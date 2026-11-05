package intake

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// This file implements the pi --mode rpc execution path (OF-416): one
// long-lived pi process per Turn.Session, commanded over JSONL on stdin and
// read as JSONL events on stdout, instead of spawning claude -p per turn.
// The protocol speaks records like {"id":"req-1","type":"get_state"} — the
// command name lives in "type", responses repeat the request "id" — and
// frames strictly on LF, never on Unicode line separators.

const (
	defaultPiIdleTimeout = 30 * time.Minute
	piAbortGrace         = 2 * time.Second
	piShutdownGrace      = 5 * time.Second
)

var errPiExited = errors.New("intake pi process exited")

var piRequestSeq atomic.Int64

// ParsePiTier splits an "pi:<provider>/<model>:<thinking>" intake tier into
// its provider, model and thinking level; the thinking suffix is optional.
func ParsePiTier(tier string) (provider, model, thinking string, err error) {
	parts := strings.SplitN(tier, ":", 3)
	if len(parts) < 2 || parts[0] != "pi" || parts[1] == "" {
		return "", "", "", fmt.Errorf("intake pi tier must look like pi:<provider>/<model>:<thinking>: %q", tier)
	}
	provider, model, found := strings.Cut(parts[1], "/")
	if !found || provider == "" || model == "" {
		return "", "", "", fmt.Errorf("intake pi tier must look like pi:<provider>/<model>:<thinking>: %q", tier)
	}
	if len(parts) == 3 {
		thinking = parts[2]
	}
	return provider, model, thinking, nil
}

// piResponse is one protocol response record, correlated to its request by ID.
type piResponse struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`
}

// piProc is one resident pi process serving a single session. Turns serialize
// on turnMu; the reader goroutine demuxes responses to pending requests by id
// and forwards everything else as session events.
type piProc struct {
	cmd   *exec.Cmd
	stdin *bufio.Writer
	rawIn io.Closer

	writeMu sync.Mutex
	turnMu  sync.Mutex

	events chan json.RawMessage
	done   chan struct{}

	pendingMu sync.Mutex
	pending   map[string]chan piResponse

	used atomic.Int64
}

func (r *Runner) piEnabled() bool { return r.PiBinary != "" }

func (r *Runner) piIdleTimeout() time.Duration {
	if r.PiIdleTimeout > 0 {
		return r.PiIdleTimeout
	}
	return defaultPiIdleTimeout
}

// Start begins the janitor that reclaims pi processes idle past
// PiIdleTimeout. It is idempotent and a no-op without pi configuration.
func (r *Runner) Start() {
	if !r.piEnabled() {
		return
	}
	r.piMu.Lock()
	defer r.piMu.Unlock()
	if r.piStop != nil {
		return
	}
	r.piStop = make(chan struct{})
	go r.piJanitor(r.piStop)
}

// Stop ends the janitor and shuts every resident pi process down gracefully,
// escalating to SIGKILL after a grace period.
func (r *Runner) Stop() {
	r.piMu.Lock()
	stop := r.piStop
	r.piStop = nil
	procs := r.piProcs
	r.piProcs = nil
	r.piMu.Unlock()
	if stop != nil {
		close(stop)
	}
	for _, p := range procs {
		p.shutdown()
	}
}

// piJanitor ticks fast enough to notice procs past the idle deadline soon
// after they qualify, but no faster than twice per idle window.
func (r *Runner) piJanitor(stop <-chan struct{}) {
	interval := r.piIdleTimeout() / 4
	if interval > time.Minute {
		interval = time.Minute
	}
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-r.piIdleTimeout())
			r.piMu.Lock()
			for key, p := range r.piProcs {
				if time.Unix(0, p.used.Load()).Before(cutoff) {
					delete(r.piProcs, key)
					go p.shutdown()
				}
			}
			r.piMu.Unlock()
		}
	}
}

// dropPiProc removes p from the session map (only if still the registered
// process) and makes sure it is dead: after a timeout or a crash the next
// turn must spawn fresh instead of talking to a zombie.
func (r *Runner) dropPiProc(session string, p *piProc) {
	r.piMu.Lock()
	if r.piProcs[session] == p {
		delete(r.piProcs, session)
	}
	r.piMu.Unlock()
	p.kill()
}

// piProc returns the resident process for a session, respawning after death.
func (r *Runner) piProc(ctx context.Context, session string) (*piProc, error) {
	r.piMu.Lock()
	if p := r.piProcs[session]; p != nil {
		select {
		case <-p.done:
			delete(r.piProcs, session)
			r.piMu.Unlock()
			p.kill()
		default:
			r.piMu.Unlock()
			return p, nil
		}
	} else {
		r.piMu.Unlock()
	}
	p, err := r.spawnPi(ctx, session)
	if err != nil {
		return nil, err
	}
	r.piMu.Lock()
	defer r.piMu.Unlock()
	if r.piProcs == nil {
		r.piProcs = map[string]*piProc{}
	}
	if existing := r.piProcs[session]; existing != nil && existing != p {
		go p.shutdown()
		return existing, nil
	}
	r.piProcs[session] = p
	return p, nil
}

// spawnPi starts a sandboxed pi --mode rpc process and applies the configured
// model tier before the process becomes reachable, so no turn can race ahead
// of set_model/set_thinking_level.
func (r *Runner) spawnPi(ctx context.Context, session string) (*piProc, error) {
	if r.PiSessionDir == "" {
		return nil, errors.New("intake pi session dir is not configured")
	}
	if err := os.MkdirAll(r.PiSessionDir, 0o700); err != nil {
		return nil, fmt.Errorf("intake pi session dir: %w", err)
	}
	pi, err := filepath.EvalSymlinks(r.PiBinary)
	if err != nil {
		return nil, fmt.Errorf("intake sandbox cannot resolve pi: %w", err)
	}
	args, err := r.piSandboxArgs(pi, session)
	if err != nil {
		return nil, err
	}
	bwrap := r.Bwrap
	if bwrap == "" {
		bwrap = "bwrap"
	}
	cmd := exec.Command(bwrap, args...)
	cmd.Dir = r.Checkout
	cmd.Env = r.piEnv()
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("intake pi start failed: %w", err)
	}
	p := &piProc{
		cmd:     cmd,
		stdin:   bufio.NewWriter(stdin),
		rawIn:   stdin,
		events:  make(chan json.RawMessage, 256),
		done:    make(chan struct{}),
		pending: map[string]chan piResponse{},
	}
	p.touch()
	go func() { _ = cmd.Wait() }()
	go p.readLoop(stdout)
	if r.PiProvider != "" || r.PiModel != "" {
		resp, err := p.request(ctx, map[string]any{"type": "set_model", "provider": r.PiProvider, "modelId": r.PiModel})
		if err != nil || !resp.Success {
			p.kill()
			return nil, fmt.Errorf("intake pi set_model failed: %w", errors.Join(err, errors.New(resp.Error)))
		}
	}
	if r.PiThinking != "" {
		resp, err := p.request(ctx, map[string]any{"type": "set_thinking_level", "level": r.PiThinking})
		if err != nil || !resp.Success {
			p.kill()
			return nil, fmt.Errorf("intake pi set_thinking_level failed: %w", errors.Join(err, errors.New(resp.Error)))
		}
	}
	return p, nil
}

// piSandboxArgs confines pi like the claude path (no credentials on disk are
// reachable), with the session dir bound read-write: that directory is the
// only state pi may persist, and it doubles as the session-recovery anchor.
// ANTHROPIC_-prefixed variables carry the model credentials in via the
// environment instead.
func (r *Runner) piSandboxArgs(pi, session string) ([]string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		return nil, errors.New("intake sandbox requires HOME")
	}
	piMount := pi
	if pkgRoot := piPackageRoot(pi); pkgRoot != "" {
		// pi's npm package is a multi-file bundle: the entry file alone is
		// useless without its siblings (cli-runtime.js, bundled modules).
		piMount = pkgRoot
	}
	args := []string{"--unshare-pid", "--die-with-parent", "--new-session", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp"}
	for _, dir := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc"} {
		args = append(args, "--ro-bind-try", dir, dir)
	}
	if resolver, err := filepath.EvalSymlinks("/etc/resolv.conf"); err == nil {
		args = append(args, "--ro-bind-try", resolver, resolver)
	}
	args = append(args, "--tmpfs", home)
	// PiConfigDir exposes pi's non-secret configuration (provider endpoints,
	// default model) at the agent config dir pi expects under HOME. Credential
	// stores such as auth.json are deliberately never bound: model
	// credentials ride ANTHROPIC_-prefixed environment variables instead.
	for _, name := range []string{"models.json", "settings.json"} {
		if _, err := os.Stat(filepath.Join(r.PiConfigDir, name)); err == nil {
			args = append(args, "--ro-bind-try", filepath.Join(r.PiConfigDir, name), filepath.Join(home, ".pi", "agent", name))
		}
	}
	// Bind order matters: bwrap stacks mounts in argument order, so the
	// ancestor must be bound before a nested path or the later bind shadows
	// the earlier one (a session dir inside the checkout would otherwise be
	// hidden underneath the checkout's read-only bind).
	checkoutBind := []string{"--ro-bind", r.Checkout, r.Checkout}
	sessionBind := []string{"--bind", r.PiSessionDir, r.PiSessionDir}
	if pathWithin(r.Checkout, r.PiSessionDir) {
		args = append(args, sessionBind...)
		args = append(args, checkoutBind...)
	} else {
		args = append(args, checkoutBind...)
		args = append(args, sessionBind...)
	}
	return append(args,
		"--ro-bind", piMount, piMount,
		"--remount-ro", "/",
		"--chdir", r.Checkout, "--",
		pi, "--mode", "rpc", "--session-id", session, "--session-dir", r.PiSessionDir, "--name", "intake",
		"--append-system-prompt", filepath.Join(r.Checkout, "scripts/agent-gates/roles/intake-chat.md"),
		"-t", "read,grep,glob"), nil
}

// pathWithin reports whether child is strictly inside parent.
func pathWithin(child, parent string) bool {
	return child != parent && strings.HasPrefix(child, parent+string(os.PathSeparator))
}

// piPackageRoot walks up from the resolved pi entry file to the nearest
// directory holding package.json. A self-contained binary has no such marker
// and yields "", which keeps the single-file bind.
func piPackageRoot(resolved string) string {
	dir := filepath.Dir(resolved)
	for {
		if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// piEnv passes only HOME, PATH and ANTHROPIC_-prefixed variables: the model
// credentials ride the environment because the sandbox exposes no credential
// files, and nothing else of the gateway's environment is pi's business.
func (r *Runner) piEnv() []string {
	env := []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "ANTHROPIC_") {
			env = append(env, e)
		}
	}
	return env
}

func (p *piProc) touch() { p.used.Store(time.Now().UnixNano()) }

// send writes one command record, LF-framed. Concurrent shutdown closes the
// pipe underneath; that surfaces as an error, never a panic.
func (p *piProc) send(cmd map[string]any) error {
	b, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if _, err := p.stdin.Write(append(b, '\n')); err != nil {
		return err
	}
	return p.stdin.Flush()
}

// request sends a command with a fresh id and waits for its response record.
// Responses are correlated by id, never by order, because pi handles commands
// asynchronously.
func (p *piProc) request(ctx context.Context, cmd map[string]any) (piResponse, error) {
	id := fmt.Sprintf("intake-%d", piRequestSeq.Add(1))
	cmd["id"] = id
	ch := make(chan piResponse, 1)
	p.pendingMu.Lock()
	p.pending[id] = ch
	p.pendingMu.Unlock()
	defer func() {
		p.pendingMu.Lock()
		delete(p.pending, id)
		p.pendingMu.Unlock()
	}()
	if err := p.send(cmd); err != nil {
		return piResponse{}, err
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-p.done:
		return piResponse{}, errPiExited
	case <-ctx.Done():
		return piResponse{}, ctx.Err()
	}
}

// piTurn runs one conversation turn against the resident process: prompt,
// wait for agent_settled, then read the last assistant text. One turnCtx
// deadline bounds every RPC of the turn, including the process's model setup;
// any exit that leaves a run possibly in flight aborts it first, and a pi
// that never acks the abort is killed and dropped so the next turn respawns.
func (r *Runner) piTurn(ctx context.Context, t Turn) (string, error) {
	if !t.Resume {
		if err := r.update(ctx); err != nil {
			return "", err
		}
	}
	r.writeTicketContext(ctx)
	turnCtx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	p, err := r.piProc(turnCtx, t.Session)
	if err != nil {
		return "", err
	}
	p.turnMu.Lock()
	defer p.turnMu.Unlock()
	p.touch()
	resp, err := p.request(turnCtx, map[string]any{"type": "prompt", "message": t.Prompt, "streamingBehavior": "followUp"})
	if err != nil {
		if errors.Is(err, errPiExited) {
			r.dropPiProc(t.Session, p)
			return "", err
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			r.piCleanupTurn(t.Session, p)
			if errors.Is(err, context.DeadlineExceeded) {
				return "", fmt.Errorf("intake pi turn timed out after %s", r.Timeout)
			}
			return "", err
		}
		return "", err
	}
	if !resp.Success {
		return "", fmt.Errorf("intake pi prompt rejected: %s", resp.Error)
	}
	var outcome struct {
		Disposition string `json:"disposition"`
	}
	_ = json.Unmarshal(resp.Data, &outcome)
	if outcome.Disposition != "handled" {
		if err := r.piWaitSettled(turnCtx, p, t); err != nil {
			return "", err
		}
	}
	text, err := p.lastAssistantText(turnCtx)
	if err != nil {
		if errors.Is(err, errPiExited) {
			r.dropPiProc(t.Session, p)
			return "", err
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			r.piCleanupTurn(t.Session, p)
			if errors.Is(err, context.DeadlineExceeded) {
				return "", fmt.Errorf("intake pi turn timed out after %s", r.Timeout)
			}
		}
		return "", err
	}
	p.touch()
	return text, nil
}

// piWaitSettled consumes events until agent_settled. A prompt response only
// means pi accepted the work; the turn is done when pi will not continue on
// its own.
func (r *Runner) piWaitSettled(ctx context.Context, p *piProc, t Turn) error {
	remaining := r.Timeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
		if remaining <= 0 {
			remaining = time.Nanosecond
		}
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	for {
		select {
		case <-p.done:
			r.dropPiProc(t.Session, p)
			return errPiExited
		case <-timer.C:
			r.piCleanupTurn(t.Session, p)
			return fmt.Errorf("intake pi turn timed out after %s", r.Timeout)
		case <-ctx.Done():
			r.piCleanupTurn(t.Session, p)
			return ctx.Err()
		case rec, ok := <-p.events:
			if !ok {
				continue
			}
			var head struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(rec, &head); err == nil && head.Type == "agent_settled" {
				return nil
			}
		}
	}
}

// piCleanupTurn aborts a turn that will not complete; a pi that never acks
// the abort is killed and dropped so the next turn respawns, and a stale
// agent_settled from the aborted run is drained so the next turn cannot
// mistake it for its own completion.
func (r *Runner) piCleanupTurn(session string, p *piProc) {
	if !p.abort() {
		r.dropPiProc(session, p)
		return
	}
	p.drainEvents()
}

func (p *piProc) drainEvents() {
	for {
		select {
		case <-p.events:
		default:
			return
		}
	}
}

// abort asks pi to stop the current run; it only responds once idle, so a
// missing ack after the grace period means the process is stuck.
func (p *piProc) abort() bool {
	ctx, cancel := context.WithTimeout(context.Background(), piAbortGrace)
	defer cancel()
	resp, err := p.request(ctx, map[string]any{"type": "abort"})
	return err == nil && resp.Success
}

func (p *piProc) lastAssistantText(ctx context.Context) (string, error) {
	resp, err := p.request(ctx, map[string]any{"type": "get_last_assistant_text"})
	if err != nil {
		return "", err
	}
	return resp.assistantText()
}

// assistantText extracts data.text from a get_last_assistant_text response.
func (resp piResponse) assistantText() (string, error) {
	if !resp.Success {
		return "", fmt.Errorf("intake pi get_last_assistant_text failed: %s", resp.Error)
	}
	var data struct {
		Text *string `json:"text"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return "", fmt.Errorf("intake pi get_last_assistant_text data: %w", err)
	}
	if data.Text == nil || strings.TrimSpace(*data.Text) == "" {
		return "", errors.New("intake pi returned no assistant text")
	}
	return *data.Text, nil
}

// readLoop splits stdout strictly on LF (a generic line reader would also
// split on U+2028/U+2029, which are legal inside JSON strings), routes
// responses to their requester by id, and forwards the rest as session events.
func (p *piProc) readLoop(out io.ReadCloser) {
	defer close(p.done)
	reader := bufio.NewReader(out)
	for {
		line, err := reader.ReadBytes('\n')
		trimmed := bytes.TrimSuffix(line, []byte{'\n'})
		trimmed = bytes.TrimSuffix(trimmed, []byte{'\r'})
		if len(bytes.TrimSpace(trimmed)) > 0 {
			p.handleRecord(trimmed)
		}
		if err != nil {
			return
		}
	}
}

func (p *piProc) handleRecord(b []byte) {
	var head struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		fmt.Fprintf(os.Stderr, "intake pi: unparseable record: %v\n", err)
		return
	}
	if head.Type == "response" && head.ID != "" {
		var resp piResponse
		if err := json.Unmarshal(b, &resp); err == nil {
			p.pendingMu.Lock()
			ch := p.pending[head.ID]
			delete(p.pending, head.ID)
			p.pendingMu.Unlock()
			if ch != nil {
				ch <- resp
			}
			return
		}
	}
	select {
	case p.events <- json.RawMessage(b):
	case <-p.done:
	}
}

// shutdown requests an orderly exit by closing stdin, then escalates to
// SIGKILL on the whole process group after the grace period.
func (p *piProc) shutdown() {
	p.writeMu.Lock()
	_ = p.rawIn.Close()
	p.writeMu.Unlock()
	select {
	case <-p.done:
		return
	case <-time.After(piShutdownGrace):
		p.kill()
	}
}

func (p *piProc) kill() {
	if p.cmd.Process != nil {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
	select {
	case <-p.done:
	case <-time.After(piShutdownGrace):
	}
}
