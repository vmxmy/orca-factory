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
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// Client may be used with its zero value. Path, when provided, must be absolute;
// otherwise runx.OrcaPath resolves it. Timeout defaults to 30 seconds.
type Client struct {
	Path            string
	Timeout         time.Duration
	WorkerStartWait time.Duration
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
	digest := sha1.Sum(append(namespace, fmt.Sprintf("%s-%s-%s-%d", ticket, unit, step, attempt)...))
	sum := digest[:16]
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:])
}

func invalid(message string) error { return &infra.Ticket{Err: errors.New(message)} }

// RejectedError carries the Orca mutation rejection code (see recorded samples).
// Unwrapping to infra.Ticket keeps the existing non-retryable classification.
type RejectedError struct{ Code string }

func (e *RejectedError) Error() string { return "orca command rejected: " + e.Code }

func (e *RejectedError) Unwrap() error { return &infra.Ticket{Err: errors.New(e.Code)} }

// IsTerminalStale reports a terminal-reuse rejection: the handle is gone, so the
// caller must fall back to a fresh agent instead of replaying the request.
func IsTerminalStale(err error) bool {
	var rejected *RejectedError
	return errors.As(err, &rejected) && rejected.Code == "terminal_handle_stale"
}

func (c *Client) call(ctx context.Context, args []string) (Receipt, error) {
	started := time.Now()
	command := strings.Join(args[:min(2, len(args))], " ")
	request, result := "", "runtime_error"
	for i := 2; i+1 < len(args); i++ {
		if args[i] == "--retry-request" {
			request = args[i+1]
		}
	}
	defer func() {
		level := slog.LevelInfo
		if result != "ok" {
			level = slog.LevelWarn
		}
		runx.LoggerFrom(ctx).Log(ctx, level, "Orca call", "command", command, "result", result,
			"duration", time.Since(started), "request_id", request)
	}()
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
	// runx redacts free-text arguments and records process diagnostics.
	output, runErr := runx.Run(ctx, runx.Spec{Path: path, Args: append(args, "--json"), Timeout: timeout})
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
		return nil, infra.Classify(runErr)
	}
	if errors.Is(runErr, os.ErrNotExist) || errors.Is(runErr, os.ErrPermission) {
		return nil, &infra.Global{Dep: infra.Orca, Err: errors.New("orca executable unavailable")}
	}
	var envelope struct {
		ID     string  `json:"id"`
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
	if request == "" && validUUID(envelope.ID) {
		request = envelope.ID
	}
	if !*envelope.OK {
		if envelope.Error != nil {
			if validCode(envelope.Error.Code) {
				result = envelope.Error.Code
			}
			if envelope.Result == nil {
				envelope.Result = envelope.Error.Data
			}
			switch envelope.Error.Code {
			case "runtime_unavailable", "runtime_access_denied", "authentication_required", "unauthorized":
				return envelope.Result, &infra.Global{Dep: infra.Orca, Err: errors.New("orca dependency unavailable")}
			}
		}
		if envelope.Error != nil && validCode(envelope.Error.Code) {
			return envelope.Result, &RejectedError{Code: envelope.Error.Code}
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
	result = "ok"
	return envelope.Result, nil
}

func (c *Client) mutate(ctx context.Context, request string, args []string) (Receipt, error) {
	guard := maintenance.Write
	if len(args) > 1 && args[1] == "worker-start" {
		guard = maintenance.Admission
	}
	release, err := guard()
	if err != nil {
		return nil, err
	}
	defer release()
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
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
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
	Run, Task, Spec, Worktree, Agent, Model, Effort, Setup, Terminal string
	Name, Repo, BaseBranch, RetryOf                                  string
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
	// Terminal reuse drives an already-running agent; launch flags would conflict.
	if o.Terminal != "" && (o.Agent != "" || o.Model != "" || o.Effort != "") {
		return nil, invalid("terminal reuse excludes agent flags")
	}
	args := []string{"orchestration", "worker-start"}
	for _, pair := range [][2]string{{"--run", o.Run}, {"--task", o.Task}, {"--spec", o.Spec}, {"--worktree", o.Worktree}, {"--terminal", o.Terminal}, {"--agent", o.Agent}, {"--model", o.Model}, {"--effort", o.Effort}, {"--setup", o.Setup}, {"--name", o.Name}, {"--repo", o.Repo}, {"--base-branch", o.BaseBranch}, {"--retry-of", o.RetryOf}} {
		args = add(args, pair[0], pair[1])
	}
	// Agent readiness on an existing worktree can exceed the default call
	// timeout; a client-side timeout would report a started worker as uncertain.
	wait := c.WorkerStartWait
	if wait == 0 {
		wait = defaultWorkerStartWait
	}
	args = append(args, "--timeout-ms", strconv.FormatInt(wait.Milliseconds(), 10))
	long := *c
	long.Timeout = wait + 30*time.Second
	return long.mutate(ctx, request, args)
}

// workerStartWait defaults the agent-readiness window; the Client field overrides it.
const defaultWorkerStartWait = 5 * time.Minute

type Task struct {
	ID         string          `json:"id"`
	ParentID   string          `json:"parent_id"`
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
	if json.Unmarshal(receipt["tasks"], &tasks) != nil || tasks == nil {
		return nil, invalid("invalid task list")
	}
	// A partial enumeration cannot authorize dispatch recovery. Older CLI
	// versions omit metadata; retain that contract while checking any supplied.
	if raw, ok := receipt["truncated"]; ok {
		var truncated bool
		if json.Unmarshal(raw, &truncated) != nil || truncated {
			return nil, invalid("incomplete task list")
		}
	}
	for _, key := range []string{"count", "totalCount"} {
		if raw, ok := receipt[key]; ok {
			var count int
			if json.Unmarshal(raw, &count) != nil || count != len(tasks) {
				return nil, invalid("incomplete task list")
			}
		}
	}
	if raw, ok := receipt["page"]; ok {
		var page struct {
			HasMore    bool   `json:"hasMore"`
			NextCursor string `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &page) != nil || page.HasMore || page.NextCursor != "" {
			return nil, invalid("incomplete task list")
		}
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

// Worker is the fleet projection, not the terminal's PTY observation.
// Unverifiable means unknown, never proof of process exit.
type Worker struct {
	DispatchID          string `json:"dispatchId"`
	DispatchStatus      string `json:"dispatchStatus"`
	AgentTerminalHandle string `json:"agentTerminalHandle"`
	Resource            struct {
		WorktreeID string `json:"worktreeId"`
	} `json:"resource"`
	Projection struct {
		Liveness struct {
			Verdict string `json:"verdict"`
			Reason  string `json:"reason"`
		} `json:"liveness"`
		Stage struct {
			Worker string `json:"worker"`
			Detail string `json:"detail"`
		} `json:"stage"`
	} `json:"projection"`
}

func (w Worker) WorktreePath() string {
	_, path, ok := strings.Cut(w.Resource.WorktreeID, "::")
	if !ok || !filepath.IsAbs(path) {
		return ""
	}
	return path
}

// Workers decodes fleet rows while preserving WorkerList receipts for scheduler callers.
func (c *Client) Workers(ctx context.Context, o ListOptions) ([]Worker, error) {
	var workers []Worker
	seen := map[string]bool{o.Cursor: true}
	for {
		receipt, err := c.WorkerList(ctx, o)
		if err != nil {
			return nil, err
		}
		var rows []Worker
		var page struct {
			HasMore    bool   `json:"hasMore"`
			NextCursor string `json:"nextCursor"`
		}
		if json.Unmarshal(receipt["workers"], &rows) != nil || json.Unmarshal(receipt["page"], &page) != nil {
			return nil, invalid("invalid worker list")
		}
		workers = append(workers, rows...)
		if !page.HasMore {
			return workers, nil
		}
		if page.NextCursor == "" || seen[page.NextCursor] {
			return nil, invalid("invalid worker cursor")
		}
		o.Cursor = page.NextCursor
		seen[o.Cursor] = true
	}
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
func (c *Client) WorkerStop(ctx context.Context, request, dispatch string) (Receipt, error) {
	return c.mutate(ctx, request, []string{"orchestration", "worker-stop", "--dispatch", dispatch})
}

// WorkerAbandon fences a dispatch without claiming its process stopped.
func (c *Client) WorkerAbandon(ctx context.Context, request, dispatch string) (Receipt, error) {
	return c.mutate(ctx, request, []string{"orchestration", "worker-abandon", "--dispatch", dispatch})
}

// TerminalClose closes one terminal pane.
func (c *Client) TerminalClose(ctx context.Context, terminal string) error {
	release, err := maintenance.Write()
	if err != nil {
		return err
	}
	defer release()
	if terminal == "" || terminal == "-" {
		return invalid("terminal close requires a handle")
	}
	_, err = c.call(ctx, []string{"terminal", "close", "--terminal", terminal})
	return err
}

func (c *Client) WorkerRelease(ctx context.Context, request, dispatch string) (Receipt, error) {
	return c.mutate(ctx, request, []string{"orchestration", "worker-release", "--dispatch", dispatch})
}

type TerminalOptions struct{ Worktree, Title, Command string }

// TerminalCreate has no retry-request support in the Orca CLI. It is not
// automatically retried and must not be treated as an idempotent mutation.
func (c *Client) TerminalCreate(ctx context.Context, o TerminalOptions) (Receipt, error) {
	release, err := maintenance.Admission()
	if err != nil {
		return nil, err
	}
	defer release()
	args := []string{"terminal", "create"}
	args = add(args, "--worktree", o.Worktree)
	args = add(args, "--title", o.Title)
	args = add(args, "--command", o.Command)
	return c.call(ctx, args)
}

// TerminalSend is never retried automatically. It waits for the agent to start
// a turn; TurnStarted tells whether the receipt proves that.
func (c *Client) TerminalSend(ctx context.Context, terminal, text string) (Receipt, error) {
	release, err := maintenance.Write()
	if err != nil {
		return nil, err
	}
	defer release()
	if terminal == "" || text == "" {
		return nil, invalid("terminal send requires handle and text")
	}
	return c.call(ctx, []string{"terminal", "send", "--terminal", terminal, "--text", text, "--enter", "--wait-submit", "30"})
}

// TurnStarted reports whether a terminal send receipt observed the agent start a turn.
// Providers that cannot observe delivery never prove it.
func TurnStarted(r Receipt) bool {
	var send struct {
		Prompt struct {
			Stages []string `json:"stages"`
		} `json:"prompt"`
	}
	if json.Unmarshal(r["send"], &send) != nil {
		return false
	}
	for _, stage := range send.Prompt.Stages {
		if stage == "turn_started" {
			return true
		}
	}
	return false
}

// BoardCard is the Orca workspace board metadata of one ticket worktree.
// Zero Issue/PR and empty strings leave those Orca fields unchanged.
type BoardCard struct {
	Path, Status, DisplayName, Comment, Parent string
	Issue, PR                                  int
	Unread                                     bool
}

// TicketCard builds the card for a ticket phase. A GitHub issue number links
// natively; other tickets link only through the comment, which always ends
// with exactly one `issue:` line so worktree pruning can map the card back to
// its ticket.
func TicketCard(path, identifier, title string, issue int, phase machine.Phase, detail string) BoardCard {
	card := BoardCard{Path: path, Status: machine.BoardColumn(phase), DisplayName: strings.TrimSpace(identifier + " " + title), Issue: issue, Unread: phase == machine.PhaseResolver}
	card.Comment = "driver: " + string(phase)
	if detail != "" {
		card.Comment += " · " + detail
	}
	card.Comment += "\nissue: " + identifier
	return card
}

// WorktreeSet updates the Orca board metadata of the worktree at an absolute path.
func (c *Client) WorktreeSet(ctx context.Context, card BoardCard) (Receipt, error) {
	release, err := maintenance.Write()
	if err != nil {
		return nil, err
	}
	defer release()
	if !filepath.IsAbs(card.Path) || card.Status == "" || (card.Parent != "" && !filepath.IsAbs(card.Parent)) {
		return nil, invalid("worktree set requires absolute paths and status")
	}
	args := []string{"worktree", "set", "--worktree", "path:" + card.Path, "--workspace-status", card.Status}
	args = add(args, "--display-name", card.DisplayName)
	args = add(args, "--comment", card.Comment)
	if card.Issue > 0 {
		args = append(args, "--issue", strconv.Itoa(card.Issue))
	}
	if card.PR > 0 {
		args = append(args, "--pr", strconv.Itoa(card.PR))
	}
	if card.Parent != "" {
		args = append(args, "--parent-worktree", "path:"+card.Parent)
	}
	if card.Unread {
		args = append(args, "--unread")
	} else {
		args = append(args, "--read")
	}
	return c.call(ctx, args)
}

// Worktree is one Orca-managed checkout of a repository. Pinned is the only
// mark a user can put on an Orca workspace (Orca has no lock flag).
type Worktree struct {
	Path   string `json:"path"`
	Pinned bool   `json:"isPinned"`
	Main   bool   `json:"isMainWorktree"`
}

// WorktreeList lists every Orca workspace of the repository at an absolute path.
// A truncated page is rejected: absence from it proves nothing.
func (c *Client) WorktreeList(ctx context.Context, repo string) ([]Worktree, error) {
	if !filepath.IsAbs(repo) {
		return nil, invalid("worktree list requires an absolute repository path")
	}
	receipt, err := c.call(ctx, []string{"worktree", "list", "--repo", "path:" + repo, "--limit", "1000"})
	if err != nil {
		return nil, err
	}
	var truncated bool
	var trees []Worktree
	if json.Unmarshal(receipt["truncated"], &truncated) != nil || truncated || json.Unmarshal(receipt["worktrees"], &trees) != nil || trees == nil {
		return nil, invalid("invalid worktree list")
	}
	for _, tree := range trees {
		if !filepath.IsAbs(tree.Path) {
			return nil, invalid("invalid worktree list entry")
		}
	}
	return trees, nil
}

// WorktreeRemove removes the Orca workspace at an absolute path, its checkout
// and, when Orca can prove it merged, its local branch. It never passes
// --force: Orca refuses a checkout with uncommitted changes (runtime_error).
func (c *Client) WorktreeRemove(ctx context.Context, path string) error {
	release, err := maintenance.Write()
	if err != nil {
		return err
	}
	defer release()
	if !filepath.IsAbs(path) {
		return invalid("worktree rm requires an absolute path")
	}
	receipt, err := c.call(ctx, []string{"worktree", "rm", "--worktree", "path:" + path})
	if err != nil {
		return err
	}
	var removed bool
	if json.Unmarshal(receipt["removed"], &removed) != nil || !removed {
		return invalid("worktree rm unconfirmed")
	}
	return nil
}

// ReceiptWorktree returns the absolute worktree path a worker-start receipt
// placed the worker in, or "" when the receipt does not name one.
func ReceiptWorktree(r Receipt) string {
	field := func(raw json.RawMessage, key string) string {
		var row map[string]json.RawMessage
		var s string
		if json.Unmarshal(raw, &row) == nil {
			_ = json.Unmarshal(row[key], &s)
		}
		return s
	}
	var top string
	_ = json.Unmarshal(r["worktreePath"], &top)
	for _, path := range []string{top, field(r["worktree"], "path"), field(r["placement"], "worktreePath")} {
		if filepath.IsAbs(path) {
			return path
		}
	}
	return ""
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

// Repo is one repository Orca already manages locally. CanonicalKey is
// gitRemoteIdentity.canonicalKey, stable across a repo's display name or
// remote URL changes; the projects package joins it to the ledger's
// 仓库远端 column (OF-42).
type Repo struct {
	Path         string
	CanonicalKey string
}

// RepoList lists every repository registered in this machine's Orca client.
func (c *Client) RepoList(ctx context.Context) ([]Repo, error) {
	receipt, err := c.call(ctx, []string{"repo", "list"})
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Path              string `json:"path"`
		GitRemoteIdentity struct {
			CanonicalKey string `json:"canonicalKey"`
		} `json:"gitRemoteIdentity"`
	}
	if json.Unmarshal(receipt["repos"], &rows) != nil || rows == nil {
		return nil, invalid("invalid repo list")
	}
	repos := make([]Repo, 0, len(rows))
	for _, row := range rows {
		if !filepath.IsAbs(row.Path) {
			return nil, invalid("invalid repo list entry")
		}
		repos = append(repos, Repo{Path: filepath.Clean(row.Path), CanonicalKey: row.GitRemoteIdentity.CanonicalKey})
	}
	return repos, nil
}

// Terminal is a terminal output observation; timestamps are epoch milliseconds.
// A null output timestamp decodes to zero and cannot establish idle duration.
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
			LastOutputAt json.RawMessage `json:"lastOutputAt"`
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
		if terminal.LastOutputAt <= 0 {
			return Terminal{}, false
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
		if r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
