package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

// WorkspaceClient lists, inspects and removes Orca workspaces (OF-96).
type WorkspaceClient interface {
	WorktreeList(context.Context, string) ([]orca.Worktree, error)
	WorktreeRemove(context.Context, string) error
	TerminalList(context.Context) ([]orca.Terminal, error)
}

// Workspace is a driver-created Orca workspace, identified by the directory
// name the executor (`<ticket>-executor`) or resolver (`<ticket>-resolver-<round>`)
// gave it. Orca appends `-<n>` when a name collides.
type Workspace struct {
	Path   string
	Ticket string
	Kind   string
	Round  int
}

// Verdict is one reclaim decision: Reason says why the workspace is
// reclaimable, Blocks why it is kept anyway. It is removed only when Blocks is empty.
type Verdict struct {
	Workspace
	Reason string
	Blocks []string
}

// closedTicket is a ticket the ledger reports Done or Canceled. Head is its
// frozen merge handoff head: a checkout at or behind that head has been pushed and merged
// even after the agent branch and its remote-tracking ref are gone.
type closedTicket struct{ State, Head string }

var workspacePattern = regexp.MustCompile(`^([A-Z][A-Z0-9]*-[0-9]+)-(executor|resolver-([1-9][0-9]*))(?:-[0-9]+)?$`)

func parseWorkspace(path string) (Workspace, bool) {
	match := workspacePattern.FindStringSubmatch(filepath.Base(path))
	if match == nil {
		return Workspace{}, false
	}
	if match[3] == "" {
		return Workspace{Path: path, Ticket: match[1], Kind: "executor"}, true
	}
	round, _ := strconv.Atoi(match[3])
	return Workspace{Path: path, Ticket: match[1], Kind: "resolver", Round: round}, true
}

// reclaimTargets selects every workspace of a closed ticket and, for any other
// ticket, the resolver rounds older than its newest round.
func reclaimTargets(workspaces []Workspace, closed map[string]closedTicket) []Verdict {
	newest := map[string]int{}
	for _, w := range workspaces {
		if w.Kind == "resolver" {
			newest[w.Ticket] = max(newest[w.Ticket], w.Round)
		}
	}
	var targets []Verdict
	for _, w := range workspaces {
		switch c, ok := closed[w.Ticket]; {
		case ok:
			targets = append(targets, Verdict{Workspace: w, Reason: "ticket " + c.State})
		case w.Kind == "resolver" && w.Round < newest[w.Ticket]:
			targets = append(targets, Verdict{Workspace: w, Reason: fmt.Sprintf("superseded by resolver round %d", newest[w.Ticket])})
		}
	}
	return targets
}

// ReclaimWorkspaces removes the reclaimable Orca workspaces of every tracked
// repository, using the ticket states of the last Snapshot. A workspace with
// uncommitted changes, commits on no remote, a live terminal or an Orca pin is
// kept and warned about once. dryRun only decides. A failed list or removal
// never affects ticket flow.
func (r *Runtime) ReclaimWorkspaces(ctx context.Context, dryRun bool, logger *slog.Logger) ([]Verdict, error) {
	repos := make([]string, 0, len(r.Repositories))
	for repo := range r.Repositories {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	var verdicts []Verdict
	var problems []error
	var terminals []orca.Terminal
	var terminalErr error
	terminalsRead := false
	for _, repo := range repos {
		trees, err := r.Workspaces.WorktreeList(ctx, repo)
		if err != nil {
			problems = append(problems, fmt.Errorf("worktree list %s: %w", repo, err))
			continue
		}
		pinned := map[string]bool{}
		var workspaces []Workspace
		for _, tree := range trees {
			if w, ok := parseWorkspace(tree.Path); ok && !tree.Main {
				workspaces = append(workspaces, w)
				pinned[tree.Path] = tree.Pinned
			}
		}
		targets := reclaimTargets(workspaces, r.closed[repo])
		if len(targets) != 0 && !terminalsRead {
			terminals, terminalErr = r.Workspaces.TerminalList(ctx)
			terminalsRead = true
		}
		for _, v := range targets {
			v.Blocks = r.reclaimBlocks(ctx, v.Workspace, pinned[v.Path], terminals, terminalErr, r.closed[repo][v.Ticket].Head)
			if len(v.Blocks) == 0 && !dryRun {
				if err := r.Workspaces.WorktreeRemove(ctx, v.Path); err != nil {
					v.Blocks = []string{"remove-failed"}
				} else {
					logger.Info("workspace removed", "path", v.Path, "reason", v.Reason)
				}
			}
			if !dryRun {
				r.warnKept(logger, v)
			}
			verdicts = append(verdicts, v)
		}
	}
	return verdicts, errors.Join(problems...)
}

func (r *Runtime) reclaimBlocks(ctx context.Context, w Workspace, pinned bool, terminals []orca.Terminal, terminalErr error, mergedHead string) []string {
	var blocks []string
	if pinned {
		blocks = append(blocks, "pinned")
	}
	if terminalErr != nil {
		blocks = append(blocks, "terminal-unknown")
	}
	for _, terminal := range terminals {
		_, idPath, _ := strings.Cut(terminal.WorktreeID, "::")
		if terminal.WorktreePath == w.Path || idPath == w.Path {
			blocks = append(blocks, "terminal-running")
			break
		}
	}
	status, err := r.command(ctx, w.Path, "git", "status", "--porcelain")
	switch {
	case err != nil:
		blocks = append(blocks, "git-status-failed")
	case strings.TrimSpace(string(status)) != "":
		blocks = append(blocks, "dirty")
	}
	// A resolver may push corrections from another workspace, leaving this
	// checkout at an ancestor of the merged head.
	if mergedHead != "" {
		if _, err := r.command(ctx, w.Path, "git", "merge-base", "--is-ancestor", "HEAD", mergedHead); err == nil {
			return blocks
		}
	}
	ahead, err := r.command(ctx, w.Path, "git", "rev-list", "--count", "HEAD", "--not", "--remotes")
	switch {
	case err != nil:
		blocks = append(blocks, "git-rev-list-failed")
	case strings.TrimSpace(string(ahead)) != "0":
		blocks = append(blocks, "unpushed")
	}
	return blocks
}

// warnKept logs a kept workspace once per distinct set of blocks.
func (r *Runtime) warnKept(logger *slog.Logger, v Verdict) {
	blocks := strings.Join(v.Blocks, ",")
	if r.reclaimWarned == nil {
		r.reclaimWarned = map[string]string{}
	}
	if blocks == "" {
		delete(r.reclaimWarned, v.Path)
		return
	}
	if r.reclaimWarned[v.Path] == blocks {
		return
	}
	r.reclaimWarned[v.Path] = blocks
	logger.Warn("workspace kept", "path", v.Path, "reason", v.Reason, "blocks", blocks)
}
