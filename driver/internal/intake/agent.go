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

// Turn is one agent invocation inside a thread conversation. Resume is false
// only for the first turn, which creates the claude session with this ID.
type Turn struct {
	Session string
	Resume  bool
	Prompt  string
}

// agentSettings deny reads of credential and state directories on top of
// --restricted, which already confines file tools to the checkout.
const agentSettings = `{"permissions":{"deny":["Read(~/.claude/**)","Read(~/.config/**)","Read(~/.local/state/**)","Read(~/.ssh/**)"]}}`

// Runner runs the read-only intake agent with claude -p in a detached checkout
// of origin/main. The agent receives no credentials and no write tools.
type Runner struct {
	Claude, Repo, Checkout, Model, Effort string
	Timeout                               time.Duration
	refresh                               sync.Mutex
}

func (r *Runner) Turn(ctx context.Context, t Turn) (string, error) {
	flag := "--resume"
	if !t.Resume {
		flag = "--session-id"
		if err := r.update(ctx); err != nil {
			return "", err
		}
	}
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
	cmd := exec.CommandContext(ctx, r.Claude, args...)
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
	return nil
}
