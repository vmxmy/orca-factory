package merge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/ledger"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/usage"
)

// LedgerRow preserves the existing ledger format. Nanosecond claim timestamps
// distinguish separate claims made within the same second.
type LedgerRow struct {
	Ticket      string             `json:"ticket"`
	Lane        int                `json:"lane"`
	Tier        string             `json:"tier"`
	Outcome     string             `json:"outcome"`
	FirstGate   string             `json:"first_gate"`
	Corrections int                `json:"corrections"`
	CIReds      int                `json:"ci_reds"`
	PR          string             `json:"pr"`
	Minutes     float64            `json:"minutes"`
	StartedAt   time.Time          `json:"started_at"`
	FinishedAt  time.Time          `json:"finished_at"`
	StageSecs   map[string]float64 `json:"stage_secs"`
	Usage       *usage.TicketUsage `json:"usage,omitempty"`
}

// LedgerRecord carries attribution from the executor to the merge process.
// It lives beside executor recovery state, never in a mutable issue timestamp.
type LedgerRecord struct {
	CompletionPath string    `json:"completion_path"`
	Row            LedgerRow `json:"row"`
	Repository     string    `json:"repository"`
	Worktrees      []string  `json:"worktrees"`
	Team           string    `json:"team"`
	Project        string    `json:"project"`
	Stage          string    `json:"stage"`
	StageAt        time.Time `json:"stage_at"`
	Written        bool      `json:"written"`
}

// LedgerWriter allows tests and long-lived drivers to inject the existing
// buffered Ledger. The command fallback uses the current driver executable,
// whose ledger entrypoint closes and flushes before returning.
type LedgerWriter struct {
	Ledger   *ledger.Ledger
	Collect  func(context.Context, string, []string) (usage.TicketUsage, error)
	Now      func() time.Time
	Warnings io.Writer
	Run      func(context.Context, runx.Spec) ([]byte, error)
	mu       sync.Mutex
	written  map[string]bool
}

func (w *LedgerWriter) warn(message string) {
	output := w.Warnings
	if output == nil {
		output = os.Stderr
	}
	slog.New(slog.NewTextHandler(output, nil)).Warn(message)
}

func (w *LedgerWriter) Append(ctx context.Context, record LedgerRecord, outcome string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	row := record.Row
	key := row.Ticket + "\x00" + row.StartedAt.UTC().Format(time.RFC3339Nano)
	if w.written[key] {
		return nil
	}
	now := w.Now
	if now == nil {
		now = time.Now
	}
	row.FinishedAt = now().UTC()
	row.StartedAt = row.StartedAt.UTC()
	row.Outcome = outcome
	row.Minutes = max(0, row.FinishedAt.Sub(row.StartedAt).Minutes())
	row.StageSecs = make(map[string]float64, len(record.Row.StageSecs))
	for stage, seconds := range record.Row.StageSecs {
		row.StageSecs[stage] = seconds
	}
	if record.Stage != "" {
		row.StageSecs[record.Stage] += max(0, row.FinishedAt.Sub(record.StageAt).Seconds())
	}
	collect := w.Collect
	if collect == nil {
		collect = func(ctx context.Context, ticket string, paths []string) (usage.TicketUsage, error) {
			return usage.Collect(ctx, ticket, paths)
		}
	}
	data, err := collect(ctx, row.Ticket, record.Worktrees)
	if err != nil {
		w.warn("ledger: usage collection failed; omitting usage for " + row.Ticket)
		row.Usage = nil
	} else {
		// Empty slices must remain JSON arrays; ledger rejects null usage fields.
		if data.Items == nil {
			data.Items = []usage.Item{}
		}
		if data.Skipped == nil {
			data.Skipped = []usage.FileError{}
		}
		row.Usage = &data
	}
	raw, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("ledger: encode finished row: %w", err)
	}
	if w.Ledger != nil {
		err = w.Ledger.Append(string(raw))
	} else {
		path, pathErr := os.Executable()
		if pathErr != nil {
			return pathErr
		}
		env := make([]string, 0, len(os.Environ())+2)
		for _, entry := range os.Environ() {
			if strings.HasPrefix(entry, "AGENT_LINEAR_TEAM=") || strings.HasPrefix(entry, "AGENT_LINEAR_PROJECT=") {
				continue
			}
			env = append(env, entry)
		}
		env = append(env, "AGENT_LINEAR_TEAM="+record.Team, "AGENT_LINEAR_PROJECT="+record.Project)
		run := w.Run
		if run == nil {
			run = runx.Run
		}
		_, err = run(ctx, runx.Spec{Path: path, Args: []string{"ledger", "append", string(raw)}, Env: env, Timeout: 2 * time.Minute})
	}
	if err != nil {
		return err
	}
	if w.written == nil {
		w.written = make(map[string]bool)
	}
	w.written[key] = true
	return nil
}

// Record operations serialize competing channels and repeated handoffs. The
// completion marker survives channel reconstruction and driver restarts.
var ledgerRecordsMu sync.Mutex

func SaveLedgerRecord(path string, record LedgerRecord) error {
	ledgerRecordsMu.Lock()
	defer ledgerRecordsMu.Unlock()
	old, err := readLedgerRecord(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if old != nil && old.Row.Ticket == record.Row.Ticket && old.Row.StartedAt.Equal(record.Row.StartedAt) && old.Written {
		return nil
	}
	return saveLedgerRecord(path, record)
}

func saveLedgerRecord(path string, record LedgerRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(path+".new", raw, 0600); err != nil {
		return err
	}
	return os.Rename(path+".new", path)
}

func readLedgerRecord(path string) (*LedgerRecord, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var record LedgerRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	if record.Row.Ticket == "" || record.Row.StartedAt.IsZero() {
		return nil, errors.New("ledger: missing claim identity")
	}
	return &record, nil
}

func ReadLedgerRecord(path string) (*LedgerRecord, error) {
	ledgerRecordsMu.Lock()
	defer ledgerRecordsMu.Unlock()
	return readLedgerRecord(path)
}

func FinishLedger(ctx context.Context, path string, writer *LedgerWriter, outcome string) error {
	ledgerRecordsMu.Lock()
	defer ledgerRecordsMu.Unlock()
	record, err := readLedgerRecord(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} // Older handoffs have no attribution record.
	if err != nil {
		return err
	}
	if record.Written {
		return nil
	}
	if record.CompletionPath != "" {
		completed, err := readLedgerRecord(record.CompletionPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if completed != nil {
			if completed.Row.Ticket != record.Row.Ticket || !completed.Row.StartedAt.Equal(record.Row.StartedAt) {
				return errors.New("ledger: conflicting completion identity")
			}
			if completed.Written {
				return nil
			}
		}
	}
	if writer == nil {
		writer = &LedgerWriter{}
	}
	if err := writer.Append(ctx, *record, outcome); err != nil {
		return err
	}
	record.Written = true
	if record.CompletionPath != "" {
		if err := saveLedgerRecord(record.CompletionPath, *record); err != nil {
			return err
		}
	}
	return saveLedgerRecord(path, *record)
}

func MergeLedgerPath(root, repository string, number int) string {
	return filepath.Join(root, filepath.Base(repository), "merge-ledger", strconv.Itoa(number)+".json")
}

func (c *Channel) SetLedgerWriter(writer *LedgerWriter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ledgerWriter = writer
}

func (c *Channel) finishLedger(ctx context.Context, request Request) {
	root := os.Getenv("AGENT_DRIVER_STATE_ROOT")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			slog.Warn("ledger: state root unavailable")
			return
		}
		root = filepath.Join(home, ".local", "state", "orca-factory")
	}
	c.mu.Lock()
	writer := c.ledgerWriter
	c.mu.Unlock()
	if writer == nil {
		writer = &LedgerWriter{}
	}
	if err := FinishLedger(ctx, MergeLedgerPath(root, request.Repository, request.Number), writer, "merged"); err != nil {
		writer.warn("ledger: merged row failed; retain attribution for retry")
	}
}
