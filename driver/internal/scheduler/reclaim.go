package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

// WorkspaceClient lists, inspects and removes Orca workspaces (OF-96).
type WorkspaceClient interface {
	WorktreeList(context.Context, string) ([]orca.Worktree, error)
	WorktreeRemove(context.Context, string) error
	TerminalList(context.Context) ([]orca.Terminal, error)
	TerminalClose(context.Context, string) error
}

// OF-329 requires an hour without output: exec terminals also have no agent identity.
const reclaimTerminalIdle = time.Hour

// Workspace is a driver-created Orca workspace, identified by the directory
// name the executor (`<ticket>-executor`) or resolver (`<ticket>-resolver-<round>`)
// gave it. Orca appends `-<n>` when a name collides; Copy is that n, 0 without a suffix.
type Workspace struct {
	Path   string
	Ticket string
	Kind   string
	Round  int
	Copy   int
}

// Verdict is one reclaim decision: Reason says why the workspace is
// reclaimable, Blocks why it is kept anyway. It is removed only when Blocks is empty.
type Verdict struct {
	Workspace
	Reason string
	Blocks []string
	// Backup marks closed workspaces whose only blocks can be backed up.
	Backup bool
}

// closedTicket is a ticket the ledger reports Done or Canceled. Head is its
// frozen merge handoff head: a checkout at or behind that head has been pushed and merged
// even after the agent branch and its remote-tracking ref are gone.
type closedTicket struct{ State, Head string }

var workspacePattern = regexp.MustCompile(`^([A-Z][A-Z0-9]*-[0-9]+)-(executor|resolver-([1-9][0-9]*))(?:-([0-9]+))?$`)

func parseWorkspace(path string) (Workspace, bool) {
	match := workspacePattern.FindStringSubmatch(filepath.Base(path))
	if match == nil {
		return Workspace{}, false
	}
	copyNumber, _ := strconv.Atoi(match[4])
	if match[3] == "" {
		return Workspace{Path: path, Ticket: match[1], Kind: "executor", Copy: copyNumber}, true
	}
	round, _ := strconv.Atoi(match[3])
	return Workspace{Path: path, Ticket: match[1], Kind: "resolver", Round: round, Copy: copyNumber}, true
}

type resolverRound struct {
	Ticket string
	Round  int
}

// reclaimTargets selects every workspace of a closed ticket and, for any other
// ticket, the resolver rounds older than its newest round and the Orca
// duplicates of a round below that round's highest copy.
func reclaimTargets(workspaces []Workspace, closed map[string]closedTicket) []Verdict {
	newest := map[string]int{}
	newestCopy := map[resolverRound]int{}
	for _, w := range workspaces {
		if w.Kind == "resolver" {
			newest[w.Ticket] = max(newest[w.Ticket], w.Round)
			round := resolverRound{w.Ticket, w.Round}
			newestCopy[round] = max(newestCopy[round], w.Copy)
		}
	}
	var targets []Verdict
	for _, w := range workspaces {
		switch c, ok := closed[w.Ticket]; {
		case ok:
			targets = append(targets, Verdict{Workspace: w, Reason: "ticket " + c.State})
		case w.Kind == "resolver" && w.Round < newest[w.Ticket]:
			targets = append(targets, Verdict{Workspace: w, Reason: fmt.Sprintf("superseded by resolver round %d", newest[w.Ticket])})
		case w.Kind == "resolver" && w.Copy < newestCopy[resolverRound{w.Ticket, w.Round}]:
			targets = append(targets, Verdict{Workspace: w, Reason: fmt.Sprintf("superseded by duplicate of resolver round %d", w.Round)})
		}
	}
	return targets
}

// ReclaimWorkspaces removes the reclaimable Orca workspaces of every tracked
// repository, using the ticket states of the last Snapshot. A workspace with
// a live terminal or an Orca pin is kept and warned about once. Closed tickets
// blocked only by local changes are backed up before removal. dryRun only
// decides. A failed list or removal never affects ticket flow.
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
			var workspaceTerminals []orca.Terminal
			for _, terminal := range terminals {
				_, idPath, _ := strings.Cut(terminal.WorktreeID, "::")
				if terminal.WorktreePath == v.Path || idPath == v.Path {
					workspaceTerminals = append(workspaceTerminals, terminal)
				}
			}
			closed, canClose := r.closed[repo][v.Ticket]
			v.Blocks = r.reclaimBlocks(ctx, v.Workspace, pinned[v.Path], workspaceTerminals, terminalErr, closed.Head, canClose)
			v.Backup = canClose && len(v.Blocks) > 0
			for _, block := range v.Blocks {
				if block != "dirty" && block != "unpushed" {
					v.Backup = false
				}
			}
			if v.Backup && !dryRun {
				backup, err := r.backupWorkspace(ctx, repo, v.Workspace, workspaces)
				if err != nil {
					v.Blocks = []string{"backup-failed"}
					logger.Warn("workspace backup failed", "path", v.Path, "error", err)
				} else {
					v.Blocks = nil
					logger.Info("workspace backed up", "path", v.Path, "backup", backup)
					for _, args := range [][]string{{"reset", "--hard", "--quiet", "HEAD"}, {"clean", "-ffd", "--quiet"}} {
						if _, err := r.command(ctx, v.Path, "git", args...); err != nil {
							v.Blocks = []string{"remove-failed"}
							logger.Warn("workspace cleanup failed", "path", v.Path, "command", args[0], "error", err)
							break
						}
					}
				}
			}
			if len(v.Blocks) == 0 && !dryRun {
				for _, terminal := range workspaceTerminals {
					if err := r.Workspaces.TerminalClose(ctx, terminal.Handle); err != nil {
						var rejected *orca.RejectedError
						if !errors.As(err, &rejected) || rejected.Code != "terminal_handle_stale" {
							v.Blocks = []string{"terminal-close-failed"}
							break
						}
					}
					logger.Info("workspace terminal closed", "path", v.Path, "terminal", terminal.Handle)
				}
			}
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

func (r *Runtime) reclaimBlocks(ctx context.Context, w Workspace, pinned bool, terminals []orca.Terminal, terminalErr error, mergedHead string, canClose bool) []string {
	var blocks []string
	if pinned {
		blocks = append(blocks, "pinned")
	}
	if terminalErr != nil {
		blocks = append(blocks, "terminal-unknown")
	}
	for _, terminal := range terminals {
		if !canClose || terminal.AgentIdentity != "" || terminal.LastOutputAt <= 0 || r.clock().Sub(time.UnixMilli(terminal.LastOutputAt)) < reclaimTerminalIdle {
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

// backupWorkspace completes all artifacts outside the workspace before removal.
// Each attempt gets its own directory, so a retry cannot overwrite a backup.
func (r *Runtime) backupWorkspace(ctx context.Context, repo string, w Workspace, workspaces []Workspace) (string, error) {
	staged, err := r.command(ctx, w.Path, "git", "ls-files", "--stage", "-z")
	if err != nil {
		return "", err
	}
	for entry := range bytes.SplitSeq(staged, []byte{0}) {
		if bytes.HasPrefix(entry, []byte("160000 ")) {
			return "", errors.New("workspace contains a gitlink")
		}
	}
	root := r.StateRoot
	if root == "" {
		root = stateRoot()
	}
	if root == "" {
		return "", errors.New("cannot resolve backup state root")
	}
	parent, err := physicalBackupPath(filepath.Join(root, filepath.Base(repo), "workspace-backups"))
	if err != nil {
		return "", err
	}
	for _, workspace := range workspaces {
		path, err := physicalBackupPath(workspace.Path)
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(path, parent)
		if err != nil {
			return "", err
		}
		if filepath.IsLocal(rel) {
			return "", fmt.Errorf("backup directory is inside workspace %s", path)
		}
	}
	if err := os.MkdirAll(parent, 0700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(parent, filepath.Base(w.Path)+"-")
	if err != nil {
		return "", err
	}
	if _, err := r.command(ctx, w.Path, "git", "bundle", "create", filepath.Join(dir, "workspace.bundle"), "HEAD"); err != nil {
		return "", err
	}
	if _, err := r.command(ctx, w.Path, "git", "diff", "--binary", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", "--unified=3", "--src-prefix=a/", "--dst-prefix=b/", "--output="+filepath.Join(dir, "tracked.patch"), "HEAD", "--"); err != nil {
		return "", err
	}
	files, err := r.command(ctx, w.Path, "git", "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	changed, err := r.command(ctx, w.Path, "git", "diff", "--name-only", "--no-renames", "--no-color", "-z", "HEAD", "--")
	if err != nil {
		return "", err
	}
	untracked := make(map[string]bool)
	for path := range strings.SplitSeq(string(files), "\x00") {
		untracked[path] = true
	}
	for path := range strings.SplitSeq(string(changed), "\x00") {
		if path == "" || untracked[path] {
			continue
		}
		present, err := worktreeFilePresent(w.Path, path)
		if err != nil {
			return "", err
		}
		if present {
			files = append(files, path...)
			files = append(files, 0)
		}
	}
	list := filepath.Join(dir, "worktree.list")
	if err := os.WriteFile(list, files, 0600); err != nil {
		return "", err
	}
	if _, err := r.command(ctx, w.Path, "tar", "-cf", filepath.Join(dir, "worktree.tar"), "--null", "-T", list); err != nil {
		return "", err
	}
	if err := os.Remove(list); err != nil {
		return "", err
	}
	return dir, nil
}

// A symlink replacing a tracked directory removes its old children from the worktree.
// Archive the link itself without following it to look up those deleted paths.
func worktreeFilePresent(root, path string) (bool, error) {
	isDir := true
	for component := range strings.SplitSeq(path, "/") {
		if !isDir {
			return false, nil
		}
		root = filepath.Join(root, component)
		info, err := os.Lstat(root)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		isDir = info.IsDir()
	}
	return !isDir, nil
}

// Resolve symlinks in the existing prefix before creating any backup directories.
func physicalBackupPath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var suffix string
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(resolved, suffix), nil
		}
		if !errors.Is(err, os.ErrNotExist) || path == filepath.Dir(path) {
			return "", err
		}
		suffix = filepath.Join(filepath.Base(path), suffix)
		path = filepath.Dir(path)
	}
}
