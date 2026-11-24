package scheduler

import (
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

const maxReclaimFailures = 5

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

type repositoryWorkspaces struct {
	repo       string
	workspaces []Workspace
	pinned     map[string]bool
	err        error
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
	var problems []error
	listings := make([]repositoryWorkspaces, len(repos))
	for i, repo := range repos {
		list := &listings[i]
		list.repo = repo
		trees, err := r.Workspaces.WorktreeList(ctx, repo)
		if err != nil {
			list.err = fmt.Errorf("worktree list %s: %w", repo, err)
			problems = append(problems, list.err)
			continue
		}
		list.pinned = map[string]bool{}
		for _, tree := range trees {
			if w, ok := parseWorkspace(tree.Path); ok && !tree.Main {
				list.workspaces = append(list.workspaces, w)
				list.pinned[tree.Path] = tree.Pinned
			}
		}
	}
	var verdicts []Verdict
	var terminals []orca.Terminal
	var terminalErr error
	terminalsRead := false
	for _, list := range listings {
		repo := list.repo
		targets := reclaimTargets(list.workspaces, r.closed[repo])
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
			v.Blocks = r.reclaimBlocks(ctx, v.Workspace, list.pinned[v.Path], workspaceTerminals, terminalErr, closed.Head, canClose)
			v.Backup = canClose && len(v.Blocks) > 0
			for _, block := range v.Blocks {
				if block != "dirty" && block != "unpushed" {
					v.Backup = false
				}
			}
			if !dryRun {
				if v.Backup || len(v.Blocks) == 0 {
					v.Blocks = r.removeWorkspace(ctx, v, workspaceTerminals, listings, logger)
				}
				r.warnKept(logger, v)
			}
			verdicts = append(verdicts, v)
		}
	}
	return verdicts, errors.Join(problems...)
}

func (r *Runtime) removeWorkspace(ctx context.Context, v Verdict, terminals []orca.Terminal, listings []repositoryWorkspaces, logger *slog.Logger) []string {
	if r.reclaimFailures[v.Path] >= maxReclaimFailures {
		return []string{"remove-failed-giving-up"}
	}
	for _, terminal := range terminals {
		if err := r.Workspaces.TerminalClose(ctx, terminal.Handle); err != nil {
			var rejected *orca.RejectedError
			if !errors.As(err, &rejected) || rejected.Code != "terminal_handle_stale" {
				return []string{"terminal-close-failed"}
			}
		}
		logger.Info("workspace terminal closed", "path", v.Path, "terminal", terminal.Handle)
	}
	if v.Backup {
		backup, err := r.backupWorkspace(ctx, v.Workspace, listings)
		if err != nil {
			logger.Warn("workspace backup failed", "path", v.Path, "error", err)
			return []string{"backup-failed"}
		}
		logger.Info("workspace backed up", "path", v.Path, "backup", backup)
		for _, args := range [][]string{{"reset", "--hard", "--quiet", "HEAD"}, {"clean", "-ffd", "--quiet"}} {
			if _, err := r.command(ctx, v.Path, "git", args...); err != nil {
				logger.Warn("workspace cleanup failed", "path", v.Path, "command", args[0], "error", err)
				return []string{"remove-failed"}
			}
		}
	}
	if err := r.Workspaces.WorktreeRemove(ctx, v.Path); err != nil {
		if r.reclaimFailures == nil {
			r.reclaimFailures = make(map[string]int)
		}
		r.reclaimFailures[v.Path]++
		if r.reclaimFailures[v.Path] >= maxReclaimFailures {
			return []string{"remove-failed-giving-up"}
		}
		return []string{"remove-failed"}
	}
	delete(r.reclaimFailures, v.Path)
	logger.Info("workspace removed", "path", v.Path, "reason", v.Reason)
	return nil
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
func (r *Runtime) backupWorkspace(ctx context.Context, w Workspace, listings []repositoryWorkspaces) (string, error) {
	root := r.StateRoot
	if root == "" {
		root = stateRoot()
	}
	if root == "" {
		return "", errors.New("cannot resolve backup state root")
	}
	parent, err := physicalBackupPath(filepath.Join(root, "branch-backups"))
	if err != nil {
		return "", err
	}
	mountInfo := r.MountInfo
	if mountInfo == "" {
		mountInfo = "/proc/self/mountinfo"
	}
	table, err := os.ReadFile(mountInfo)
	if err != nil {
		return "", err
	}
	mounts, err := parseMountInfo(string(table))
	if err != nil {
		return "", err
	}
	if err := checkBackupDirectory(parent, listings, mounts); err != nil {
		return "", err
	}
	files, err := r.workspaceArchiveFiles(ctx, w.Path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(parent, 0700); err != nil {
		return "", err
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	dir := filepath.Join(parent, now.UTC().Format("20060102T150405Z")+"-"+filepath.Base(w.Path))
	if err := os.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	if _, err := r.command(ctx, w.Path, "git", "bundle", "create", filepath.Join(dir, "workspace.bundle"), "HEAD"); err != nil {
		return "", err
	}
	if _, err := r.command(ctx, w.Path, "git", "diff", "--binary", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", "--unified=3", "--src-prefix=a/", "--dst-prefix=b/", "--output="+filepath.Join(dir, "tracked.patch"), "HEAD", "--"); err != nil {
		return "", err
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

func checkBackupDirectory(parent string, listings []repositoryWorkspaces, mounts []mount) error {
	backupDevice, backupSource := mountSource(mounts, parent)
	var ancestors []os.FileInfo
	for {
		info, err := os.Stat(parent)
		if err == nil {
			ancestors = append(ancestors, info)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		next := filepath.Dir(parent)
		if next == parent {
			break
		}
		parent = next
	}
	for _, list := range listings {
		if list.err != nil {
			return list.err
		}
		for _, workspace := range list.workspaces {
			info, err := os.Stat(workspace.Path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			for _, ancestor := range ancestors {
				if os.SameFile(ancestor, info) {
					return fmt.Errorf("backup directory is inside workspace %s", workspace.Path)
				}
			}
			physical, err := filepath.EvalSymlinks(workspace.Path)
			if err != nil {
				return err
			}
			device, source := mountSource(mounts, physical)
			if device == backupDevice && pathWithin(backupSource, source) {
				return fmt.Errorf("backup directory is mounted from inside workspace %s", workspace.Path)
			}
		}
	}
	return nil
}

// mount is one /proc/self/mountinfo line: the directory root of device is
// visible at point. A bind mount of a subdirectory has a root other than "/".
type mount struct {
	device, root, point string
}

func parseMountInfo(table string) ([]mount, error) {
	var mounts []mount
	for line := range strings.Lines(table) {
		// The kernel separates fields with one ASCII space and escapes it inside
		// paths; other whitespace, such as U+00A0, belongs to the path.
		fields := strings.Split(strings.TrimSuffix(line, "\n"), " ")
		if len(fields) < 5 {
			return nil, fmt.Errorf("mountinfo line %q has fewer than 5 fields", line)
		}
		mounts = append(mounts, mount{device: fields[2], root: mountPathEscapes.Replace(fields[3]), point: mountPathEscapes.Replace(fields[4])})
	}
	if len(mounts) == 0 {
		return nil, errors.New("mountinfo lists no mounts")
	}
	return mounts, nil
}

// The kernel writes space, tab, newline and backslash in mountinfo paths as
// three-digit octal escapes.
var mountPathEscapes = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

// mountSource names the device and the path on it that hold the physical
// path. The last mount over the longest matching point is the visible one.
func mountSource(mounts []mount, path string) (device, source string) {
	var best mount
	for _, m := range mounts {
		if pathWithin(path, m.point) && len(m.point) >= len(best.point) {
			best = m
		}
	}
	return best.device, filepath.Join(best.root, strings.TrimPrefix(path, best.point))
}

func pathWithin(path, dir string) bool {
	return path == dir || dir == "/" || strings.HasPrefix(path, dir+"/")
}

func (r *Runtime) workspaceArchiveFiles(ctx context.Context, root string) ([]byte, error) {
	tracked, err := r.command(ctx, root, "git", "ls-files", "--cached", "-z")
	if err != nil {
		return nil, err
	}
	files, err := r.command(ctx, root, "git", "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	listed := make(map[string]bool)
	for path := range strings.SplitSeq(string(files), "\x00") {
		listed[path] = true
	}
	for path := range strings.SplitSeq(string(tracked), "\x00") {
		if path == "" || listed[path] {
			continue
		}
		listed[path] = true
		present, err := worktreeFilePresent(root, path)
		if err != nil {
			return nil, err
		}
		if present {
			files = append(files, path...)
			files = append(files, 0)
		}
	}
	return files, nil
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
	if !isDir {
		return true, nil
	}
	_, err := os.Lstat(filepath.Join(root, ".git"))
	if errors.Is(err, os.ErrNotExist) {
		entries, err := os.ReadDir(root)
		return len(entries) > 0, err
	}
	if err != nil {
		return false, err
	}
	return false, errors.New("workspace contains a populated submodule")
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
