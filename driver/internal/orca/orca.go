// Package orca adapts the Orca CLI through the bounded runx process runner.
// Mutations require a stable RequestID chosen before invocation. An uncertain
// result must be reconciled before replay; this package never launches retries
// automatically or interprets absence as proof that a mutation had no effects.
package orca

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// Client may be used with its zero value. Path, when provided, must be absolute;
// otherwise runx.OrcaPath resolves it. Timeout defaults to 30 seconds.
type Client struct {
	Path    string
	Timeout time.Duration
}

// Receipt retains the complete, decoded command result, including evolving
// nested recovery, launch, projection and pagination fields. Decode individual
// fields with json.Unmarshal; no service response is included in an error.
type Receipt map[string]json.RawMessage

// RequestID is RFC 4122 UUIDv5 in the standard URL namespace for the exact name
// <ticket>-<unit>-<step>-<attempt>. Persist those inputs across restarts; change
// attempt only for a new operation (a failed Dispatch retry), never an unknown
// outcome replay. Names and namespace form a stable on-disk/external contract.
func RequestID(ticket, unit, step string, attempt int) string {
	namespace := []byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	h := sha1.New()
	h.Write(namespace)
	h.Write([]byte(fmt.Sprintf("%s-%s-%s-%d", ticket, unit, step, attempt)))
	sum := h.Sum(nil)[:16]
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:])
}

func invalid(message string) error { return &infra.Ticket{Err: errors.New(message)} }

func (c *Client) call(ctx context.Context, args []string) (Receipt, error) {
	path := c.Path
	if path == "" {
		var err error
		path, err = runx.OrcaPath()
		if err != nil {
			return nil, &infra.Global{Dep: infra.Orca, Err: err}
		}
	}
	if !filepath.IsAbs(path) {
		return nil, invalid("orca path must be absolute")
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	if timeout < 0 {
		return nil, invalid("orca timeout must be positive")
	}
	output, runErr := runx.Run(ctx, runx.Spec{Path: path, Args: append(args, "--json"), Timeout: timeout})
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
		return nil, infra.Classify(runErr)
	}
	if errors.Is(runErr, os.ErrNotExist) || errors.Is(runErr, os.ErrPermission) {
		return nil, &infra.Global{Dep: infra.Orca, Err: errors.New("orca executable unavailable")}
	}
	var envelope struct {
		OK     *bool   `json:"ok"`
		Result Receipt `json:"result"`
		Error  *struct {
			Code string  `json:"code"`
			Data Receipt `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(output, &envelope); err != nil || envelope.OK == nil {
		// Discard parser errors: invalid JSON may contain credentials.
		return nil, invalid("invalid orca response")
	}
	if !*envelope.OK {
		if envelope.Error != nil {
			if envelope.Result == nil {
				envelope.Result = envelope.Error.Data
			}
			switch envelope.Error.Code {
			case "runtime_unavailable", "runtime_access_denied", "authentication_required", "unauthorized":
				return envelope.Result, &infra.Global{Dep: infra.Orca, Err: errors.New("orca dependency unavailable")}
			}
		}
		if envelope.Error != nil && validCode(envelope.Error.Code) {
			return envelope.Result, invalid("orca command rejected: " + envelope.Error.Code)
		}
		return envelope.Result, invalid("orca command rejected")
	}
	// worker-start can emit a valid receipt but exit nonzero (failed or unknown).
	if runErr != nil {
		return envelope.Result, infra.Classify(runErr)
	}
	if envelope.Result == nil {
		return nil, invalid("missing orca result")
	}
	return envelope.Result, nil
}

func (c *Client) mutate(ctx context.Context, request string, args []string) (Receipt, error) {
	if !validUUID(request) {
		return nil, invalid("mutation requires a UUID request ID")
	}
	return c.call(ctx, append(args, "--retry-request", request))
}
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}
func add(args []string, flag, value string) []string {
	if value != "" {
		return append(args, flag, value)
	}
	return args
}

func (c *Client) RunCreate(ctx context.Context, request, objective string) (Receipt, error) {
	return c.mutate(ctx, request, []string{"orchestration", "run-create", "--objective", objective})
}

// RunUse binds the calling terminal to an existing Run and fences its previous consumer.
func (c *Client) RunUse(ctx context.Context, request, run string) (Receipt, error) {
	return c.mutate(ctx, request, []string{"orchestration", "run-use", "--id", run})
}

type StartOptions struct {
	Run, Task, Spec, Worktree, Agent, Model, Effort, Setup string
	Name, Repo, BaseBranch, RetryOf                        string
}

// WorkerStart requires exactly one of Task and Spec. For a positively failed
// Dispatch, retry with Task, RetryOf and a new RequestID attempt, repeating the
// desired placement. Unknown outcomes instead retain the original request ID.
func (c *Client) WorkerStart(ctx context.Context, request string, o StartOptions) (Receipt, error) {
	if (o.Task == "") == (o.Spec == "") {
		return nil, invalid("choose task or spec")
	}
	if o.RetryOf != "" && o.Task == "" {
		return nil, invalid("retry requires task")
	}
	if o.Effort != "" && o.Model == "" {
		return nil, invalid("effort requires model")
	}
	args := []string{"orchestration", "worker-start"}
	for _, pair := range [][2]string{{"--run", o.Run}, {"--task", o.Task}, {"--spec", o.Spec}, {"--worktree", o.Worktree}, {"--agent", o.Agent}, {"--model", o.Model}, {"--effort", o.Effort}, {"--setup", o.Setup}, {"--name", o.Name}, {"--repo", o.Repo}, {"--base-branch", o.BaseBranch}, {"--retry-of", o.RetryOf}} {
		args = add(args, pair[0], pair[1])
	}
	return c.mutate(ctx, request, args)
}

type Task struct {
	ID         string          `json:"id"`
	RunID      string          `json:"run_id"`
	DispatchID string          `json:"dispatch_id"`
	Status     string          `json:"status"`
	Spec       string          `json:"spec"`
	Result     json.RawMessage `json:"result"`
}

type WorkerReport struct {
	Provenance    string   `json:"provenance"`
	Outcome       string   `json:"outcome"`
	Body          string   `json:"body"`
	FilesModified []string `json:"filesModified"`
}

// WorkerReport accepts the CLI's JSON-encoded result string, or an already
// decoded object. Null and results with different provenance are not reports.
func (t Task) WorkerReport() (*WorkerReport, error) {
	raw := t.Result
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		raw = []byte(encoded)
	}
	var report WorkerReport
	if json.Unmarshal(raw, &report) != nil {
		return nil, invalid("invalid task result")
	}
	if report.Provenance != "worker_report" {
		return nil, nil
	}
	return &report, nil
}

func (c *Client) TaskList(ctx context.Context, run string) ([]Task, error) {
	if run == "" {
		return nil, invalid("task list requires run")
	}
	receipt, err := c.call(ctx, []string{"orchestration", "task-list", "--run", run})
	if err != nil {
		return nil, err
	}
	var tasks []Task
	if json.Unmarshal(receipt["tasks"], &tasks) != nil {
		return nil, invalid("invalid task list")
	}
	return tasks, nil
}

type ReadOptions struct {
	Dispatch, Source, Cursor string
	Limit                    int
}

func (c *Client) WorkerRead(ctx context.Context, o ReadOptions) (Receipt, error) {
	args := []string{"orchestration", "worker-read", "--dispatch", o.Dispatch}
	args = add(args, "--source", o.Source)
	args = add(args, "--cursor", o.Cursor)
	if o.Limit > 0 {
		args = append(args, "--limit", strconv.Itoa(o.Limit))
	}
	return c.call(ctx, args)
}

type ListOptions struct {
	Run, TerminalState, Cursor string
	Limit                      int
	IncludeRemote              bool
}

func (c *Client) WorkerList(ctx context.Context, o ListOptions) (Receipt, error) {
	args := []string{"orchestration", "worker-list"}
	args = add(args, "--run", o.Run)
	args = add(args, "--terminal-state", o.TerminalState)
	args = add(args, "--cursor", o.Cursor)
	if o.Limit > 0 {
		args = append(args, "--limit", strconv.Itoa(o.Limit))
	}
	if o.IncludeRemote {
		args = append(args, "--include-remote")
	}
	return c.call(ctx, args)
}
func (c *Client) WorkerRelease(ctx context.Context, request, dispatch string) (Receipt, error) {
	return c.mutate(ctx, request, []string{"orchestration", "worker-release", "--dispatch", dispatch})
}

type TerminalOptions struct{ Worktree, Title, Command string }

// TerminalCreate has no retry-request support in the Orca CLI. It is not
// automatically retried and must not be treated as an idempotent mutation.
func (c *Client) TerminalCreate(ctx context.Context, o TerminalOptions) (Receipt, error) {
	args := []string{"terminal", "create"}
	args = add(args, "--worktree", o.Worktree)
	args = add(args, "--title", o.Title)
	args = add(args, "--command", o.Command)
	return c.call(ctx, args)
}

// TerminalSend has no retry-request support and is never retried automatically.
func (c *Client) TerminalSend(ctx context.Context, terminal, text string) (Receipt, error) {
	if terminal == "" || text == "" {
		return nil, invalid("terminal send requires handle and text")
	}
	return c.call(ctx, []string{"terminal", "send", "--terminal", terminal, "--text", text, "--enter"})
}
func (c *Client) TerminalShow(ctx context.Context, terminal string) (Receipt, error) {
	return c.call(ctx, []string{"terminal", "show", "--terminal", terminal})
}
func (c *Client) AutomationsList(ctx context.Context) (Receipt, error) {
	return c.call(ctx, []string{"automations", "list"})
}

type Request struct {
	RequestID string          `json:"requestId"`
	State     string          `json:"state"`
	Receipt   json.RawMessage `json:"receipt"`
}

func (c *Client) RequestShow(ctx context.Context, request string) (Request, error) {
	receipt, err := c.call(ctx, []string{"orchestration", "request-show", "--request", request})
	if err != nil {
		return Request{}, err
	}
	var result Request
	if json.Unmarshal(receipt["state"], &result.State) != nil || json.Unmarshal(receipt["requestId"], &result.RequestID) != nil {
		return Request{}, invalid("invalid request receipt")
	}
	result.Receipt = receipt["receipt"]
	return result, nil
}

type Reconciliation struct {
	State   string
	Receipt json.RawMessage
	Tasks   []Task
}

// Reconcile uses a completed receipt without executing any mutation or task
// lookup. Pending/absent enumerate Tasks in the explicit Run for caller matching;
// even an empty Tasks result does NOT authorize replay or prove process exit.
func (c *Client) Reconcile(ctx context.Context, request, run string) (Reconciliation, error) {
	observed, err := c.RequestShow(ctx, request)
	if err != nil {
		return Reconciliation{}, err
	}
	result := Reconciliation{State: observed.State}
	switch observed.State {
	case "completed":
		var receipt Receipt
		if json.Unmarshal(observed.Receipt, &receipt) != nil || receipt == nil {
			return result, invalid("completed request lacks receipt")
		}
		result.Receipt = observed.Receipt
	case "pending", "absent":
		result.Tasks, err = c.TaskList(ctx, run)
	default:
		err = invalid("unknown request state")
	}
	return result, err
}

// Terminal is a terminal output observation; timestamps are epoch milliseconds.
type Terminal struct {
	Handle       string `json:"handle"`
	WorktreeID   string `json:"worktreeId"`
	WorktreePath string `json:"worktreePath"`
	LastOutputAt int64  `json:"lastOutputAt"`
}

func (c *Client) TerminalList(ctx context.Context) ([]Terminal, error) {
	receipt, err := c.call(ctx, []string{"terminal", "list"})
	if err != nil {
		return nil, err
	}
	var truncated bool
	if raw, ok := receipt["truncated"]; ok {
		if json.Unmarshal(raw, &truncated) != nil || truncated {
			return nil, invalid("invalid terminal list")
		}
	}
	var rows []json.RawMessage
	raw := receipt["terminals"]
	if json.Unmarshal(raw, &rows) != nil || rows == nil {
		return nil, invalid("invalid terminal list")
	}
	terminals := make([]Terminal, 0, len(rows))
	for _, row := range rows {
		var terminal Terminal
		var timestamp struct {
			LastOutputAt *int64 `json:"lastOutputAt"`
		}
		if json.Unmarshal(row, &terminal) != nil || json.Unmarshal(row, &timestamp) != nil || timestamp.LastOutputAt == nil {
			return nil, invalid("invalid terminal timestamp")
		}
		terminals = append(terminals, terminal)
	}
	return terminals, nil
}

// TerminalForWorktree selects the most recent output for an absolute worktree path.
func TerminalForWorktree(terminals []Terminal, path string) (Terminal, bool) {
	var latest Terminal
	found := false
	if !filepath.IsAbs(path) {
		return latest, false
	}
	for _, terminal := range terminals {
		_, idPath, _ := strings.Cut(terminal.WorktreeID, "::")
		if terminal.WorktreePath != path && idPath != path {
			continue
		}
		if !found || terminal.LastOutputAt > latest.LastOutputAt {
			latest, found = terminal, true
		}
	}
	return latest, found
}

// validCode admits only identifier-shaped error codes into error text.
func validCode(code string) bool {
	if code == "" || len(code) > 64 {
		return false
	}
	for _, r := range code {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
