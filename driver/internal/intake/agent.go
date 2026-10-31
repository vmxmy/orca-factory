package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// excludeMarker keeps .intake/ (OF-33's per-turn ticket context) out of a
// checkout's git status: it is Agent-visible working state, not a tracked
// or even an untracked change the Agent could mistake for something to
// report on.
const excludeMarker = "/.intake/"

// Turn is one agent invocation inside a thread conversation. Resume is false
// only for the first turn, which creates the claude session with this ID.
// Project is the zero value when the gateway has no projects configured.
type Turn struct {
	Session string
	Resume  bool
	Prompt  string
	Project Project
}

// agentSettings deny reads of credential and state directories on top of
// --restricted, which already confines file tools to the checkout.
const agentSettings = `{"permissions":{"deny":["Read(~/.claude/**)","Read(~/.config/**)","Read(~/.local/state/**)","Read(~/.ssh/**)"]}}`

// Runner runs the read-only intake agent with claude -p in a detached checkout
// of origin/main. The agent receives no credentials and no write tools.
type Runner struct {
	Claude, Repo, Checkout, Model, Effort string
	// Bwrap overrides the sandbox executable, found on PATH when empty.
	Bwrap   string
	Timeout time.Duration
	// Tickets lists the project's ledger tickets for the .intake/tickets.md
	// context (OF-33); nil for a gateway with no projects configured
	// (pre-OF-32), which skips writing it.
	Tickets func(context.Context) ([]TicketContext, error)
	refresh sync.Mutex
}

func (r *Runner) Turn(ctx context.Context, t Turn) (string, error) {
	flag := "--resume"
	if !t.Resume {
		flag = "--session-id"
		if err := r.update(ctx); err != nil {
			return "", err
		}
	}
	r.writeTicketContext(ctx)
	args := []string{"-p", "--output-format", "json", flag, t.Session}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	if r.Effort != "" {
		args = append(args, "--effort", r.Effort)
	}
	args = append(args, "--append-system-prompt-file", filepath.Join(r.Checkout, "scripts/agent-gates/roles/intake-chat.md"),
		"--tools", "Read,Grep,Glob,Skill", "--permission-mode", "dontAsk", "--restricted", "--strict-mcp-config", "--settings", agentSettings)
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	// The prompt travels on stdin: --tools is variadic, and an untrusted message
	// starting with "-" must never be parsed as a flag.
	sandbox, err := r.sandboxArgs()
	if err != nil {
		return "", err
	}
	bwrap := r.Bwrap
	if bwrap == "" {
		bwrap = "bwrap"
	}
	cmd := exec.CommandContext(ctx, bwrap, append(sandbox, args...)...)
	cmd.Dir = r.Checkout
	cmd.Env = []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")}
	cmd.Stdin = strings.NewReader(t.Prompt)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	b, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("intake agent failed: %w", errors.Join(err, ctx.Err()))
	}
	var result struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
	}
	if json.Unmarshal(b, &result) != nil || result.Type != "result" || result.Subtype != "success" || result.IsError || strings.TrimSpace(result.Result) == "" {
		return "", errors.New("intake agent returned no result")
	}
	return result.Result, nil
}

// sandboxArgs confines claude with the operating system, under --restricted:
// the agent handles untrusted group messages on a machine holding production
// credentials. A missing bwrap fails the turn instead of running unconfined.
// Only system directories are exposed, so a repository is hidden wherever it
// lives. Home holds nothing but claude's own directory, because a token refresh
// must be able to replace its credentials file. /etc/resolv.conf may link into
// the otherwise absent /run.
func (r *Runner) sandboxArgs() ([]string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		return nil, errors.New("intake sandbox requires HOME")
	}
	claude, err := filepath.EvalSymlinks(r.Claude)
	if err != nil {
		return nil, fmt.Errorf("intake sandbox cannot resolve claude: %w", err)
	}
	args := []string{"--unshare-pid", "--die-with-parent", "--new-session", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp"}
	for _, dir := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc"} {
		args = append(args, "--ro-bind-try", dir, dir)
	}
	if resolver, err := filepath.EvalSymlinks("/etc/resolv.conf"); err == nil {
		args = append(args, "--ro-bind-try", resolver, resolver)
	}
	config, state := filepath.Join(home, ".claude"), filepath.Join(home, ".claude.json")
	return append(args, "--tmpfs", home, "--bind-try", config, config, "--bind-try", state, state,
		"--ro-bind", r.Checkout, r.Checkout, "--ro-bind", claude, claude, "--remount-ro", "/", "--chdir", r.Checkout, "--", claude), nil
}

// update moves the shared checkout to origin/main; a missing checkout is
// re-added even when git still registers it.
func (r *Runner) update(ctx context.Context) error {
	r.refresh.Lock()
	defer r.refresh.Unlock()
	if _, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", r.Repo, "fetch", "--quiet", "origin", "main"}, Timeout: time.Minute}); err != nil {
		return errors.New("intake checkout fetch failed")
	}
	args := []string{"-C", r.Checkout, "checkout", "--quiet", "--force", "--detach", "origin/main"}
	if _, err := os.Stat(filepath.Join(r.Checkout, ".git")); errors.Is(err, os.ErrNotExist) {
		args = []string{"-C", r.Repo, "worktree", "add", "--quiet", "--force", "--detach", r.Checkout, "origin/main"}
	}
	if _, err := runx.Run(ctx, runx.Spec{Path: "git", Args: args, Timeout: time.Minute}); err != nil {
		return errors.New("intake checkout update failed")
	}
	if err := excludeIntakeDir(ctx, r.Checkout); err != nil {
		return errors.New("intake checkout exclude setup failed")
	}
	return nil
}

// excludeIntakeDir appends excludeMarker to the checkout's own
// info/exclude, so .intake/ never shows as untracked. --git-path resolves
// to a worktree's own info/exclude, not the main repository's; the call is
// idempotent, since a fresh worktree gets a fresh info/exclude but a reused
// one already carries the marker after its first update.
func excludeIntakeDir(ctx context.Context, checkout string) error {
	out, err := runx.Run(ctx, runx.Spec{Path: "git", Args: []string{"-C", checkout, "rev-parse", "--git-path", "info/exclude"}, Timeout: time.Minute})
	if err != nil {
		return err
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		path = filepath.Join(checkout, path)
	}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if strings.Contains(string(b), excludeMarker) {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(excludeMarker + "\n")
	return errors.Join(err, f.Close())
}
