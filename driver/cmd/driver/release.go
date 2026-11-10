package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

const releaseHelp = `  release <票号> --pr <编号> --sha <合并提交> [--done]
      [写入] 人工合并后的部署与 doctor 检查，通过后写评论；--done 才置 Done。
`

type releaseDeps struct {
	paths    func() (script, driver string, err error)
	run      func(context.Context, runx.Spec) ([]byte, error)
	evidence func(context.Context, string, int, string) (merge.Progress, error)
	comment  func(context.Context, string, string) error
	done     func(context.Context, string) error
}

func init() {
	Register("release", func(args []string) int {
		var ledger issues.Store
		return runRelease(args, os.Stderr, releaseDeps{
			paths: func() (string, string, error) {
				home, err := os.UserHomeDir()
				return filepath.Join(home, ".local/share/orca-factory-ops/deploy.sh"), filepath.Join(home, ".local/bin/orca-factory-driver"), err
			},
			run: runx.Run,
			evidence: func(ctx context.Context, ticket string, pr int, sha string) (merge.Progress, error) {
				home, err := os.UserHomeDir()
				if err != nil {
					return merge.Progress{}, err
				}
				cfg, err := config.Load(home)
				if err != nil {
					return merge.Progress{}, err
				}
				env := issueEnvironment()
				ledger, err = issues.New(ctx, env, cfg.Tuning)
				if err != nil {
					return merge.Progress{}, err
				}
				gh, err := github.New(github.Options{BaseURL: env["AGENT_GH_API_URL"]})
				if err != nil {
					return merge.Progress{}, err
				}
				owner, repo, ok := strings.Cut(env["AGENT_GH_REPO"], "/")
				if !ok || owner == "" || repo == "" {
					return merge.Progress{}, errors.New("release: AGENT_GH_REPO must be owner/repo")
				}
				merged, err := gh.GetPR(ctx, owner, repo, pr)
				if err != nil {
					return merge.Progress{}, err
				}
				return releaseEvidence(ctx, ledger, ticket, pr, sha, env["AGENT_GH_REPO"], merged)
			},
			comment: func(ctx context.Context, id, body string) error {
				_, err := ledger.CreateComment(ctx, id, body)
				return err
			},
			done: func(ctx context.Context, id string) error {
				_, err := ledger.UpdateIssue(ctx, id, "Done", nil, nil)
				return err
			},
		})
	})
}

func runRelease(args []string, out io.Writer, deps releaseDeps) int {
	usage := func() int {
		_, _ = fmt.Fprintln(out, "Usage: driver release <ticket> --pr <number> --sha <merge SHA> [--done]")
		return 2
	}
	if len(args) == 0 || !usageTicket.MatchString(args[0]) {
		return usage()
	}
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	fs.SetOutput(out)
	pr := fs.Int("pr", 0, "merged pull request number")
	sha := fs.String("sha", "", "full merge commit SHA")
	done := fs.Bool("done", false, "mark the ticket Done after the comment is confirmed")
	if fs.Parse(args[1:]) != nil || fs.NArg() != 0 || *pr <= 0 || len(*sha) != 40 || strings.Trim(*sha, "0123456789abcdefABCDEF") != "" {
		return usage()
	}
	*sha = strings.ToLower(*sha)
	fail := func(step string, err error) int {
		_, _ = fmt.Fprintf(out, "release: %s: %v\n", step, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	defer cancel()
	progress, err := deps.evidence(ctx, args[0], *pr, *sha)
	if err != nil {
		return fail("evidence", err)
	}
	script, driver, err := deps.paths()
	if err != nil {
		return fail("paths", err)
	}
	if _, err = deps.run(ctx, runx.Spec{Path: script, Timeout: 70 * time.Minute}); err != nil {
		return fail("deploy", err)
	}
	raw, commandErr := deps.run(ctx, runx.Spec{Path: driver, Args: []string{"doctor", "--json"}, Timeout: 5 * time.Minute})
	deployed, err := releaseSmoke(raw)
	if err != nil {
		return fail("doctor", err)
	}
	if commandErr != nil {
		return fail("doctor", commandErr)
	}
	fresh, err := deps.evidence(ctx, args[0], *pr, *sha)
	if err != nil {
		return fail("evidence", err)
	}
	if fresh != progress {
		return fail("evidence", errors.New("merge evidence changed during deployment"))
	}
	body, err := json.Marshal(progress)
	if err != nil {
		return fail("comment", err)
	}
	comment := merge.ProgressPrefix + string(body) + fmt.Sprintf("\nPR: #%d\n合并提交: %s\n部署版本: %s\nSmoke: doctor --json passed（无 fail 或 unknown）", *pr, *sha, deployed)
	if err = deps.comment(ctx, progress.TicketID, comment); err != nil {
		return fail("comment", err)
	}
	if *done {
		if err = deps.done(ctx, progress.TicketID); err != nil {
			return fail("done", err)
		}
	}
	if _, err := fmt.Fprintf(out, "release: %s comment recorded; deployed %s; done=%t\n", args[0], deployed, *done); err != nil {
		return 1
	}
	return 0
}

func releaseSmoke(raw []byte) (string, error) {
	var report healthReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return "", fmt.Errorf("invalid doctor JSON: %w", err)
	}
	if len(report.Checks) == 0 {
		return "", errors.New("doctor report has no checks")
	}
	problems := append([]string(nil), report.Errors...)
	for name, check := range report.Checks {
		if check.Status != healthOK && check.Status != healthWarn {
			problems = append(problems, fmt.Sprintf("%s: %s: %s", name, check.Status, check.Reason))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return "", errors.New(strings.Join(problems, "; "))
	}
	details, _ := report.Checks["version"].Details.(map[string]any)
	running, _ := details["running"].(string)
	if len(running) < 7 || len(running) > 40 || strings.Trim(running, "0123456789abcdefABCDEF") != "" {
		return "", errors.New("doctor did not confirm a deployed version")
	}
	return running, nil
}

func releaseEvidence(ctx context.Context, ledger issues.Store, identifier string, number int, sha, repository string, pr github.PullRequest) (merge.Progress, error) {
	if !pr.Merged || pr.Number != number || pr.MergeSHA != sha || pr.Base.Ref != "main" || len(pr.Head.SHA) != 40 {
		return merge.Progress{}, errors.New("PR read-back does not confirm the merged PR and SHA")
	}
	ticket, err := ledger.Read(ctx, identifier)
	if err != nil {
		return merge.Progress{}, err
	}
	if ticket.ID == "" || ticket.Identifier != identifier || ticket.Metadata.ImportHold {
		return merge.Progress{}, errors.New("ticket identity is missing or held")
	}
	comments, err := ledger.ListComments(ctx, ticket.ID)
	if err != nil {
		return merge.Progress{}, err
	}
	sort.SliceStable(comments, func(i, j int) bool {
		if comments[i].CreatedAt.Equal(comments[j].CreatedAt) {
			return comments[i].ID < comments[j].ID
		}
		return comments[i].CreatedAt.Before(comments[j].CreatedAt)
	})
	var handoff *merge.Handoff
	var prior *merge.Progress
	for _, comment := range comments {
		for _, line := range strings.Split(comment.Body, "\n") {
			if raw, ok := strings.CutPrefix(line, merge.HandoffPrefix); ok {
				var h merge.Handoff
				if err := json.Unmarshal([]byte(raw), &h); err != nil {
					return merge.Progress{}, fmt.Errorf("invalid handoff: %w", err)
				}
				handoff = &h
			}
			if raw, ok := strings.CutPrefix(line, merge.ProgressPrefix); ok {
				var p merge.Progress
				if err := json.Unmarshal([]byte(raw), &p); err != nil {
					return merge.Progress{}, fmt.Errorf("invalid progress: %w", err)
				}
				if p.Deployment != nil {
					return merge.Progress{}, errors.New("deployment evidence exists; use resume-deployment")
				}
				switch p.Phase {
				case "waiting", "correction", "human", "merge-intent", "merged":
				default:
					return merge.Progress{}, errors.New("unknown merge progress phase")
				}
				if p.Main != "" && (len(p.Main) != 40 || strings.Trim(p.Main, "0123456789abcdefABCDEF") != "" || p.MainUnknown) {
					return merge.Progress{}, errors.New("invalid historical main")
				}
				prior = &p
			}
		}
	}
	p := merge.Progress{Version: 1, TicketID: ticket.ID, Repository: repository, PR: number, Head: pr.Head.SHA, MainUnknown: true, MergeSHA: sha, Phase: "merged"}
	if handoff != nil {
		if handoff.Version != 1 || handoff.TicketID != ticket.ID || handoff.Identifier != identifier || handoff.Repository != repository || handoff.PR != number || handoff.Head != pr.Head.SHA || handoff.QueueID == "" || handoff.Lane < 1 || handoff.Lane > 2 {
			return merge.Progress{}, errors.New("handoff identity mismatch or unsupported lane")
		}
		p.QueueID, p.Corrections, p.Infra, p.Attempt, p.CatchupRounds = handoff.QueueID, handoff.Corrections, handoff.Infra, handoff.Attempt, handoff.CatchupRounds
	}
	if prior != nil && prior.QueueID == p.QueueID {
		if prior.Version != 1 || prior.TicketID != p.TicketID || prior.Repository != repository || prior.PR != number || prior.Head != p.Head || (prior.MergeSHA != "" && prior.MergeSHA != sha) {
			return merge.Progress{}, errors.New("merge progress identity mismatch")
		}
		p.Corrections, p.Infra, p.Attempt, p.CatchupRounds = prior.Corrections, prior.Infra, prior.Attempt, prior.CatchupRounds
		if prior.Main != "" {
			p.Main, p.MainUnknown = prior.Main, false
		}
	}
	return p, nil
}
