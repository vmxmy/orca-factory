package mergelane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/release"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

type publisher struct{ tags, releases int }

func (p *publisher) CreateTag(context.Context, string, string, string, string) error {
	p.tags++
	return nil
}
func (p *publisher) CreateRelease(context.Context, string, string, github.ReleaseRequest) (github.Release, error) {
	p.releases++
	return github.Release{}, nil
}

type writer struct{ calls *int }

func (w writer) CreateReleasePR(context.Context, merge.Request, release.Plan) (int, error) {
	if w.calls != nil {
		*w.calls++
	}
	return 8, nil
}
func TestReleaseSharesFIFOAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	feature := openPR()
	rel := openPR()
	rel.Number = 8
	gh := &fakeGitHub{prs: []github.PullRequest{feature, rel}}
	l, _, tk, _, ticket := lane(t, gh)
	pub := &publisher{}
	l.Publisher = pub
	if err := l.Process(ctx, []scheduler.Ticket{ticket}); err != nil {
		t.Fatal(err)
	}
	if err := l.SubmitRelease(ctx, writer{}, merge.Request{Repository: "vmxmy/orca-factory", Path: "/repo"}, release.Plan{Plugin: "demo", Version: "1.0.1"}); err != nil {
		t.Fatal(err)
	}
	// Restore FIFO, then supply authoritative current merge ownership.
	restored := *l
	restored.entries = nil
	restored.channels = nil
	restored.loaded = false
	l = &restored
	if err := l.Process(ctx, []scheduler.Ticket{ticket}); err != nil {
		t.Fatal(err)
	}
	if len(gh.merges) != 0 || pub.tags != 0 {
		t.Fatal("release overtook waiting feature")
	}
	gh.runs = green()
	if err := l.Process(ctx, []scheduler.Ticket{ticket}); err != nil {
		t.Fatal(err)
	}
	if len(tk.finished) != 1 || pub.tags != 0 {
		t.Fatal("feature must finish first")
	}
	if err := l.Process(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if len(gh.merges) != 2 || pub.tags != 1 || pub.releases != 1 {
		t.Fatalf("merges=%v publisher=%+v", gh.merges, pub)
	}
	if err := l.Process(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if pub.tags != 1 || pub.releases != 1 {
		t.Fatal("publication repeated")
	}
	t.Run("deployment waits across ticks and restart", func(t *testing.T) {
		gh := &fakeGitHub{prs: []github.PullRequest{feature, rel}, runs: green()}
		l, _, _, _, ticket := lane(t, gh)
		repo := l.Repositories["/repo"]
		repo.Env["AGENT_DEPLOY_WORKFLOW"], repo.Env["AGENT_SMOKE_CMD"] = "deploy.yml", "./smoke"
		l.Repositories["/repo"] = repo
		status, smoke := "in_progress", 0
		previousExec := l.Exec
		l.Exec = func(ctx context.Context, s runx.Spec) ([]byte, error) {
			if s.Path == "gh" {
				if strings.Join(s.Args, " ") != "api repos/vmxmy/orca-factory/actions/workflows/deploy.yml/runs?head_sha="+sha+"&per_page=100&page=1" || s.Timeout <= 0 {
					t.Fatalf("deployment command=%+v", s)
				}
				return []byte(`{"workflow_runs":[{"id":9,"head_sha":"` + sha + `","status":"` + status + `","conclusion":"success"}]}`), nil
			}
			if s.Path == "bash" {
				if s.Dir != "/repo" || strings.Join(s.Args, " ") != "-lc ./smoke" || s.Timeout <= 0 {
					t.Fatalf("smoke command=%+v", s)
				}
				smoke++
				return nil, nil
			}
			return previousExec(ctx, s)
		}
		l.Publisher = &publisher{}
		if err := l.Admit(ctx, []scheduler.Ticket{ticket}); err != nil {
			t.Fatal(err)
		}
		if err := l.SubmitRelease(ctx, writer{}, merge.Request{Repository: "vmxmy/orca-factory", Path: "/repo"}, release.Plan{Plugin: "demo", Version: "1.0.1"}); err != nil {
			t.Fatal(err)
		}
		if err := l.Process(ctx, []scheduler.Ticket{ticket}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err := l.Process(ctx, []scheduler.Ticket{ticket}); err != nil {
				t.Fatal(err)
			}
		}
		if len(gh.merges) != 1 || smoke != 0 {
			t.Fatalf("deployment barrier lost: merges=%v smoke=%d", gh.merges, smoke)
		}
		restored := *l
		restored.entries, restored.loaded, restored.channels = nil, false, nil
		l = &restored
		if err := l.Process(ctx, []scheduler.Ticket{ticket}); err != nil {
			t.Fatal(err)
		}
		if len(gh.merges) != 1 {
			t.Fatal("restart lost deployment barrier")
		}
		status = "completed"
		if err := l.Process(ctx, []scheduler.Ticket{ticket}); err != nil {
			t.Fatal(err)
		}
		if err := l.Process(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if len(gh.merges) != 2 || smoke != 1 {
			t.Fatalf("did not resume after deployment: merges=%v smoke=%d", gh.merges, smoke)
		}
	})
	for _, failure := range []string{"workflow failed", "smoke failed", "workflow unavailable", "wrong SHA"} {
		t.Run(failure+" isolates repository", func(t *testing.T) {
			gh := &fakeGitHub{prs: []github.PullRequest{feature, rel}, runs: green()}
			l, _, tk, _, ticket := lane(t, gh)
			repo := l.Repositories["/repo"]
			repo.Env["AGENT_DEPLOY_WORKFLOW"], repo.Env["AGENT_SMOKE_CMD"] = "deploy.yml", "./smoke"
			l.Repositories["/repo"] = repo
			pub := &publisher{}
			l.Publisher = pub
			previousExec := l.Exec
			l.Exec = func(ctx context.Context, s runx.Spec) ([]byte, error) {
				if s.Path == "gh" {
					if failure == "workflow unavailable" {
						return nil, errors.New("workflow read unavailable")
					}
					conclusion, commit := "success", sha
					if failure == "workflow failed" {
						conclusion = "failure"
					}
					if failure == "wrong SHA" {
						commit = head
					}
					return []byte(`{"workflow_runs":[{"id":9,"head_sha":"` + commit + `","status":"completed","conclusion":"` + conclusion + `"}]}`), nil
				}
				if s.Path == "bash" {
					return nil, errors.New("smoke failed")
				}
				return previousExec(ctx, s)
			}
			if err := l.Admit(ctx, []scheduler.Ticket{ticket}); err != nil {
				t.Fatal(err)
			}
			if err := l.SubmitRelease(ctx, writer{}, merge.Request{Repository: "vmxmy/orca-factory", Path: "/repo"}, release.Plan{Plugin: "demo", Version: "1.0.1"}); err != nil {
				t.Fatal(err)
			}
			if err := l.Process(ctx, []scheduler.Ticket{ticket}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				_ = l.Process(ctx, []scheduler.Ticket{ticket})
			}
			wantEscalations := 1
			if failure == "workflow unavailable" || failure == "wrong SHA" {
				wantEscalations = 0
			}
			if len(gh.merges) != 1 || len(l.entries) != 2 || len(tk.escalated) != wantEscalations || pub.tags != 0 {
				t.Fatalf("failed deployment lost barrier: merges=%v entries=%d escalations=%v tags=%d", gh.merges, len(l.entries), tk.escalated, pub.tags)
			}
			// A paused deployment must not freeze a different repository's lane.
			if err := l.SubmitRelease(ctx, writer{}, merge.Request{Repository: "owner/other", Path: "/other"}, release.Plan{Plugin: "other", Version: "1.0.1"}); err != nil {
				t.Fatal(err)
			}
			_ = l.Process(ctx, []scheduler.Ticket{ticket})
			if len(gh.merges) != 2 || pub.tags != 1 || pub.releases != 1 {
				t.Fatalf("other repository stopped: merges=%v publication=%+v", gh.merges, pub)
			}
			restored := *l
			restored.entries, restored.loaded, restored.channels = nil, false, nil
			_ = restored.Process(ctx, []scheduler.Ticket{ticket})
			if len(gh.merges) != 2 || len(tk.escalated) != wantEscalations {
				t.Fatal("restart repeated escalation or bypassed wait")
			}
		})
	}
	for _, state := range []string{"missing", "human", "wrong phase", "blocked"} {
		t.Run("restored "+state, func(t *testing.T) {
			gh := &fakeGitHub{prs: []github.PullRequest{feature, rel}}
			l, _, _, _, ticket := lane(t, gh)
			pub := &publisher{}
			l.Publisher = pub
			if err := l.Admit(ctx, []scheduler.Ticket{ticket}); err != nil {
				t.Fatal(err)
			}
			if err := l.SubmitRelease(ctx, writer{}, merge.Request{Repository: "vmxmy/orca-factory", Path: "/repo"}, release.Plan{Plugin: "demo", Version: "1.0.1"}); err != nil {
				t.Fatal(err)
			}
			restored := *l
			restored.entries, restored.loaded, restored.channels = nil, false, nil
			l = &restored
			gh.runs = green()
			current := []scheduler.Ticket{ticket}
			switch state {
			case "missing":
				current = nil
			case "human":
				current[0].Owner = machine.OwnerHuman
				current[0].Phase = machine.PhaseHuman
			case "wrong phase":
				current[0].Phase = machine.PhaseReview
			case "blocked":
				current[0].Blocked = true
			}
			_ = l.Process(ctx, current)
			_ = l.Process(ctx, current)
			if len(gh.merges) != 0 || pub.tags != 0 || pub.releases != 0 || len(l.entries) != 2 {
				t.Fatalf("lost ownership barrier: merges=%v tags=%d releases=%d entries=%d", gh.merges, pub.tags, pub.releases, len(l.entries))
			}
		})
	}
	t.Run("correction stays ahead of release", func(t *testing.T) {
		gh := &fakeGitHub{prs: []github.PullRequest{feature, rel}, runs: green(), behind: 1}
		l, _, tk, _, ticket := lane(t, gh)
		l.Exec = func(context.Context, runx.Spec) ([]byte, error) { return nil, conflictExit{} }
		l.Publisher = &publisher{}
		if err := l.Admit(ctx, []scheduler.Ticket{ticket}); err != nil {
			t.Fatal(err)
		}
		if err := l.SubmitRelease(ctx, writer{}, merge.Request{Repository: "vmxmy/orca-factory", Path: "/repo"}, release.Plan{Plugin: "demo", Version: "1.0.1"}); err != nil {
			t.Fatal(err)
		}
		_ = l.Process(ctx, []scheduler.Ticket{ticket})
		gh.behind = 0
		for i := 0; i < 2; i++ {
			_ = l.Process(ctx, []scheduler.Ticket{ticket})
		}
		if len(gh.merges) != 0 || len(l.entries) != 2 || len(tk.escalated) != 1 {
			t.Fatalf("correction barrier lost: merges=%v entries=%d escalations=%v", gh.merges, len(l.entries), tk.escalated)
		}
		if l.entries[0].Request.CatchupRounds != 1 || l.entries[0].Blocked == "" {
			t.Fatal("correction recovery evidence lost")
		}
		// Reaffirmed merge ownership and a corrected head release the same slot.
		ticket.Attempt++
		ticket.UpdatedAt = time.Now()
		gh.prs[0].Head.SHA = main
		gh.runs = []github.CheckRun{{HeadSHA: main, Status: "completed", Conclusion: "success"}}
		if err := l.Process(ctx, []scheduler.Ticket{ticket}); err != nil {
			t.Fatal(err)
		}
		if len(gh.merges) != 1 || gh.merges[0] != main {
			t.Fatal("corrected head did not resume")
		}
	})
}

type conflictExit struct{}

func (conflictExit) Error() string { return "text conflict" }
func (conflictExit) ExitCode() int { return 1 }

func TestReleaseGateUsesRepositoryGate(t *testing.T) {
	l, _, _, _, _ := lane(t, &fakeGitHub{})
	r := merge.Request{Repository: "vmxmy/orca-factory", Path: t.TempDir(), Number: 8}
	l.entries = []laneEntry{{Request: r, Release: &release.Pending{Request: r, Plan: release.Plan{Plugin: "demo", Version: "1.0.1", Files: map[string][]byte{"mods/demo/.claude-plugin/plugin.json": []byte(`{"version":"1.0.1"}`), "mods/demo/CHANGELOG.md": []byte("## 1.0.1\n")}}}}}
	var spec string
	l.Exec = func(_ context.Context, s runx.Spec) ([]byte, error) {
		if s.Path != filepath.Join(r.Path, "scripts/agent-gates/gate.sh") || len(s.Args) != 2 || s.Args[0] != "/validated" {
			t.Fatalf("gate command=%+v", s)
		}
		b, err := os.ReadFile(s.Args[1])
		if err != nil {
			t.Fatal(err)
		}
		spec = string(b)
		return nil, nil
	}
	if err := l.Run(context.Background(), r, "/validated"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spec, "SCOPE: mods/demo/.claude-plugin/plugin.json mods/demo/CHANGELOG.md") || !strings.Contains(spec, "CHECK: git diff --check") {
		t.Fatal(spec)
	}
}

type interruptedPublisher struct {
	publisher
	fail bool
}

func (p *interruptedPublisher) CreateRelease(ctx context.Context, owner, repo string, r github.ReleaseRequest) (github.Release, error) {
	if p.fail {
		p.fail = false
		return github.Release{}, errors.New("lost publication")
	}
	return p.publisher.CreateRelease(ctx, owner, repo, r)
}
func TestReleasePublicationRecovery(t *testing.T) {
	t.Run("save failure has no merge side effects", func(t *testing.T) {
		ctx := context.Background()
		pr := openPR()
		pr.Number = 8
		gh := &fakeGitHub{prs: []github.PullRequest{pr}, runs: green()}
		l, _, _, _, _ := lane(t, gh)
		pub := &publisher{}
		l.Publisher = pub
		creates := 0
		if err := l.load(); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(l.Root, "merge-queue.json"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := l.SubmitRelease(ctx, writer{&creates}, merge.Request{Repository: "vmxmy/orca-factory", Path: "/repo"}, release.Plan{Plugin: "demo", Version: "1.0.1"}); err == nil {
			t.Fatal("save failure hidden")
		}
		_ = l.Process(ctx, nil)
		if len(gh.merges) != 0 || pub.tags != 0 || pub.releases != 0 {
			t.Fatalf("unsaved release consumed: merges=%v tags=%d releases=%d", gh.merges, pub.tags, pub.releases)
		}
		if len(l.entries) != 0 {
			t.Fatal("save failure accepted an unsaved queue entry")
		}
		if len(l.pending) != 1 || l.pending[0].Release.Request.Number != 8 {
			t.Fatal("created PR recovery Pending lost")
		}
		if err := os.Remove(filepath.Join(l.Root, "merge-queue.json")); err != nil {
			t.Fatal(err)
		}
		if err := l.SubmitRelease(ctx, writer{&creates}, merge.Request{Repository: "vmxmy/orca-factory", Path: "/repo"}, release.Plan{Plugin: "demo", Version: "1.0.1"}); err != nil {
			t.Fatal(err)
		}
		if creates != 1 {
			t.Fatal("recreated PR instead of saving Pending")
		}
		if err := l.Process(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if len(gh.merges) != 1 || pub.tags != 1 || pub.releases != 1 {
			t.Fatal("saved Pending did not resume")
		}
	})
	ctx := context.Background()
	pr := openPR()
	pr.Number = 8
	gh := &fakeGitHub{prs: []github.PullRequest{pr}, runs: green()}
	l, _, _, _, _ := lane(t, gh)
	pub := &interruptedPublisher{fail: true}
	l.Publisher = pub
	if err := l.SubmitRelease(ctx, writer{}, merge.Request{Repository: "vmxmy/orca-factory", Path: "/repo"}, release.Plan{Plugin: "demo", Version: "1.0.1"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Process(ctx, nil); err == nil {
		t.Fatal("publication error disappeared")
	}
	pr.Merged = true
	pr.State = "closed"
	pr.MergeCommitSHA = sha
	gh.prs = []github.PullRequest{pr}
	restored := *l
	restored.entries = nil
	restored.channels = nil
	restored.loaded = false
	l = &restored
	if err := l.Process(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if len(gh.merges) != 1 || pub.releases != 1 {
		t.Fatalf("merges=%v published=%d", gh.merges, pub.releases)
	}
	pending, err := l.HasRelease("vmxmy/orca-factory", "demo")
	if err != nil || pending {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
}
