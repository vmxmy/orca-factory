package intake

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
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
const agentSettings = `{"permissions":{"deny":["Read(~/.claude/**)","Read(~/.config/**)","Read(~/.local/state/**)","Read(~/.ssh/**)","Read(/proc/**)"]}}`

// Runner runs the read-only intake agent with claude -p in a detached checkout
// of origin/main. The agent receives only configured model relay credentials
// and no write tools. When PiBinary is set, turns run against a resident pi --mode rpc process per
// session (OF-416) instead; every other field below applies only to the
// claude path, and the zero value selects it exactly as before.
type Runner struct {
	Claude, Repo, Checkout, Model, Effort string
	// Agent runs Model: claude when empty, or antigravity.
	Agent                string
	ModelURL, ModelToken string
	// Fallback lists the claude or antigravity models each turn tries in order
	// when the model registry under StateRoot marks Model unavailable. A
	// session keeps the agent its first turn ran on.
	Fallback  []modelhealth.Ref
	StateRoot string
	// Bwrap overrides the sandbox executable, found on PATH when empty.
	Bwrap   string
	Timeout time.Duration
	// Tickets lists the project's ledger tickets for the .intake/tickets.md
	// context (OF-33); nil for a gateway with no projects configured
	// (pre-OF-32), which skips writing it.
	Tickets func(context.Context) ([]TicketContext, error)
	// PiBinary is the pi executable for the RPC execution path; empty keeps
	// the per-turn claude -p process model.
	PiBinary string
	// PiProvider and PiModel select the model tier applied over RPC
	// (set_model); PiThinking maps to set_thinking_level.
	PiProvider, PiModel, PiThinking string
	// PiSessionDir anchors pi session storage; it must outlive individual
	// Runner instances so a gateway restart resumes conversations.
	PiSessionDir string
	// PiConfigDir is a host directory whose models.json/settings.json are
	// exposed inside the sandbox as pi configuration; credential files are
	// never bound.
	PiConfigDir   string
	PiIdleTimeout time.Duration
	refresh       sync.Mutex
	piMu          sync.Mutex
	piProcs       map[string]*piProc
	piStop        chan struct{}
}

// turnModel is the first candidate unless the model registry marks it
// unavailable; then it is the first available later candidate. The switch
// goes to the gateway's stderr log, like the other intake turn records.
func (r *Runner) turnModel(candidates []modelhealth.Ref) modelhealth.Ref {
	primary := candidates[0]
	selected, err := config.SelectModel(r.StateRoot, primary, candidates[1:], time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "intake model fallback unavailable; keeping configured model: %v\n", err)
	}
	if selected != primary {
		fmt.Fprintf(os.Stderr, "intake model falls back: configured=%s selected=%s\n", primary, selected)
	}
	return selected
}

func (r *Runner) Turn(ctx context.Context, t Turn) (string, error) {
	if r.piEnabled() {
		return r.piTurn(ctx, t)
	}
	candidates := append([]modelhealth.Ref{{Agent: cmp.Or(r.Agent, "claude"), Model: r.Model, Effort: r.Effort}}, r.Fallback...)
	var session sessionAgent
	if t.Resume {
		var err error
		if session, err = r.readSessionAgent(t.Session); err != nil {
			return "", err
		}
		candidates = slices.DeleteFunc(candidates, func(ref modelhealth.Ref) bool { return ref.Agent != session.Agent })
		if len(candidates) == 0 {
			return "", fmt.Errorf("intake session agent %s has no configured candidate", session.Agent)
		}
	} else if err := r.update(ctx); err != nil {
		return "", err
	}
	r.writeTicketContext(ctx)
	model := r.turnModel(candidates)
	if session.Agent == antigravity && !r.available(model) {
		return "", fmt.Errorf("intake session agent %s unavailable", session.Agent)
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	b, err := r.run(ctx, t, model, session.Conversation)
	if err != nil {
		next, retry, modelErr := config.RetryAfterModelStatus(r.StateRoot, turnStatus(model, b, err, time.Now()), candidates, time.Now())
		if modelErr != nil {
			fmt.Fprintf(os.Stderr, "intake model error not recorded: %v\n", modelErr)
		}
		if retry {
			fmt.Fprintf(os.Stderr, "intake model unavailable; retrying: unavailable=%s selected=%s\n", model, next)
			model = next
			if b, err = r.run(ctx, t, next, session.Conversation); err != nil {
				if _, _, modelErr = config.RetryAfterModelStatus(r.StateRoot, turnStatus(next, b, err, time.Now()), nil, time.Now()); modelErr != nil {
					fmt.Fprintf(os.Stderr, "intake model error not recorded: %v\n", modelErr)
				}
			}
		}
	}
	if err != nil {
		return "", fmt.Errorf("intake agent failed: %w", errors.Join(err, ctx.Err()))
	}
	if model.Agent == antigravity {
		return r.antigravityAnswer(t.Session, b)
	}
	result, ok := printResult(b)
	if !ok || result.Subtype != "success" || result.IsError || strings.TrimSpace(result.Result) == "" {
		return "", errors.New("intake agent returned no result")
	}
	if !t.Resume {
		if err := r.recordSessionAgent(t.Session, sessionAgent{Agent: "claude"}); err != nil {
			return "", fmt.Errorf("intake session agent not recorded: %w", err)
		}
	}
	return result.Result, nil
}

// run sends one turn to model's agent; conversation is the antigravity
// conversation a resumed turn continues.
func (r *Runner) run(ctx context.Context, t Turn, model modelhealth.Ref, conversation string) ([]byte, error) {
	if model.Agent == antigravity {
		return r.antigravity(ctx, t, model, conversation)
	}
	flag := "--resume"
	if !t.Resume {
		flag = "--session-id"
	}
	return r.claude(ctx, flag, t, model)
}

// available reports whether the model registry leaves ref eligible; an
// unreadable registry keeps it, as SelectModel does.
func (r *Runner) available(ref modelhealth.Ref) bool {
	registry, err := modelhealth.Open(r.StateRoot)
	return err != nil || registry.Status(ref, time.Now()).State != modelhealth.Unavailable
}

type claudePrintResult struct {
	Type         string `json:"type"`
	Subtype      string `json:"subtype"`
	IsError      bool   `json:"is_error"`
	Result       string `json:"result"`
	APIErrorType string `json:"api_error_type"`
}

func printResult(b []byte) (claudePrintResult, bool) {
	var result claudePrintResult
	return result, json.Unmarshal(b, &result) == nil && result.Type == "result"
}

// turnStatus classifies a failed claude -p --output-format json turn. The run
// prints an is_error result object before exiting non-zero: its result field
// carries the message, and api_error_type not_found_error marks a model that
// does not exist or is not accessible
// (testdata/recorded/claude-print-json-model-error.json).
func turnStatus(model modelhealth.Ref, output []byte, failure error, now time.Time) modelhealth.Status {
	result, ok := printResult(output)
	if !ok || !result.IsError {
		var exit *exec.ExitError
		if errors.As(failure, &exit) {
			output = append(append(slices.Clone(output), '\n'), exit.Stderr...)
		}
		return modelhealth.Classify(model, string(output), failure, now)
	}
	if result.APIErrorType == "not_found_error" {
		return modelhealth.Status{Ref: model, State: modelhealth.Unavailable, Reason: "model_not_found", Until: now.Add(modelhealth.DefaultCooldown), CheckedAt: now}
	}
	return modelhealth.Classify(model, result.Result, failure, now)
}

// claude runs one sandboxed claude -p turn with model; on failure it returns
// the output read so far for model error classification.
func (r *Runner) claude(ctx context.Context, flag string, t Turn, model modelhealth.Ref) ([]byte, error) {
	args := []string{"-p", "--output-format", "json", flag, t.Session}
	if model.Model != "" {
		args = append(args, "--model", model.Model)
	}
	if model.Effort != "" {
		args = append(args, "--effort", model.Effort)
	}
	args = append(args, "--append-system-prompt-file", filepath.Join(r.Checkout, "scripts/agent-gates/roles/intake-chat.md"),
		"--tools", "Read,Grep,Glob,Skill", "--permission-mode", "dontAsk", "--restricted", "--strict-mcp-config", "--settings", agentSettings)
	// The prompt travels on stdin: --tools is variadic, and an untrusted message
	// starting with "-" must never be parsed as a flag.
	home := os.Getenv("HOME")
	config, state := filepath.Join(home, ".claude"), filepath.Join(home, ".claude.json")
	sandbox, err := r.sandboxArgs(r.Claude, config, config, state, state)
	if err != nil {
		return nil, err
	}
	env := []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")}
	if r.ModelURL != "" && r.ModelToken != "" {
		env = append(env, "ANTHROPIC_BASE_URL="+r.ModelURL, "ANTHROPIC_AUTH_TOKEN="+r.ModelToken)
	}
	return r.sandboxed(ctx, append(sandbox, args...), env, t.Prompt)
}

// sandboxed runs bwrap with args in its own process group, killed as a whole
// when ctx ends.
func (r *Runner) sandboxed(ctx context.Context, args, env []string, stdin string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, cmp.Or(r.Bwrap, "bwrap"), args...)
	cmd.Dir = r.Checkout
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

// sandboxArgs confines the agent binary with the operating system, under its
// own tool restrictions: the agent handles untrusted group messages on a
// machine holding production credentials. A missing bwrap fails the turn
// instead of running unconfined. Only system directories are exposed, so a
// repository is hidden wherever it lives. Home holds nothing but the agent's
// own state, binds given as source and target pairs, because a token refresh
// must be able to replace its credentials file. /etc/resolv.conf may link into
// the otherwise absent /run.
func (r *Runner) sandboxArgs(binary string, binds ...string) ([]string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		return nil, errors.New("intake sandbox requires HOME")
	}
	agent, err := filepath.EvalSymlinks(binary)
	if err != nil {
		return nil, fmt.Errorf("intake sandbox cannot resolve %s: %w", filepath.Base(binary), err)
	}
	args := []string{"--unshare-pid", "--die-with-parent", "--new-session", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp"}
	for _, dir := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc"} {
		args = append(args, "--ro-bind-try", dir, dir)
	}
	if resolver, err := filepath.EvalSymlinks("/etc/resolv.conf"); err == nil {
		args = append(args, "--ro-bind-try", resolver, resolver)
	}
	args = append(args, "--tmpfs", home)
	for i := 0; i+1 < len(binds); i += 2 {
		args = append(args, "--bind-try", binds[i], binds[i+1])
	}
	return append(args, "--ro-bind", r.Checkout, r.Checkout, "--ro-bind", agent, agent, "--remount-ro", "/", "--chdir", r.Checkout, "--", agent), nil
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
