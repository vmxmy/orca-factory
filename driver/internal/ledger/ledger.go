// Package ledger stores batched run records as comments on rotating Linear issues.
package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const Title = "[ledger] agent-graph"
const FlushInterval = 10 * time.Minute

type Issue struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	State     string    `json:"-"`
	UpdatedAt time.Time `json:"updatedAt"`
}
type Comment struct {
	Body                 string
	CreatedAt, UpdatedAt time.Time
}

// Store is scoped to one team and project. CreateIssue creates an issue without labels.
type Store interface {
	ListIssues(context.Context) ([]Issue, error)
	ListComments(context.Context, string) ([]Comment, error)
	CreateIssue(context.Context, string, string) (Issue, error)
	SetState(context.Context, string, string) error
	CreateComment(context.Context, string, string) error
}

type Options struct {
	Now func() time.Time
	// Ticks overrides the ten-minute ticker, allowing deterministic clock tests.
	Ticks    <-chan time.Time
	Warnings io.Writer
}
type Ledger struct {
	store      Store
	now        func() time.Time
	mu         sync.Mutex
	rows       []string
	retireID   string
	closed     bool
	stop, done chan struct{}
}

func New(store Store, options Options) *Ledger {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Warnings == nil {
		options.Warnings = os.Stderr
	}
	l := &Ledger{store: store, now: options.Now, stop: make(chan struct{}), done: make(chan struct{})}
	ticks := options.Ticks
	var ticker *time.Ticker
	if ticks == nil {
		ticker = time.NewTicker(FlushInterval)
		ticks = ticker.C
	}
	go func() {
		defer close(l.done)
		if ticker != nil {
			defer ticker.Stop()
		}
		for {
			select {
			case <-l.stop:
				return
			case _, ok := <-ticks:
				if !ok {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				err := l.Flush(ctx)
				cancel()
				if err != nil {
					fmt.Fprintln(options.Warnings, "ledger: warning: flush failed; buffered rows retained")
				}
			}
		}
	}()
	return l
}

func parseRow(raw string) (map[string]any, error) {
	var row map[string]any
	if json.Unmarshal([]byte(raw), &row) != nil || row == nil {
		return nil, errors.New("ledger: invalid json row")
	}
	for _, key := range []string{"corrections", "ci_reds", "minutes"} {
		if value := row[key]; value != nil && value != false {
			if _, ok := value.(float64); !ok {
				return nil, errors.New("ledger: invalid numeric field")
			}
		}
	}
	if value := row["stage_secs"]; value != nil {
		stages, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("ledger: invalid numeric field")
		}
		for _, seconds := range stages {
			if _, ok := seconds.(float64); !ok {
				return nil, errors.New("ledger: invalid numeric field")
			}
		}
	}
	return row, nil
}

func (l *Ledger) Append(raw string) error {
	if _, err := parseRow(raw); err != nil {
		return err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(raw)); err != nil {
		return errors.New("ledger: invalid json row")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("ledger: closed")
	}
	l.rows = append(l.rows, "ledger "+compact.String())
	return nil
}

// Number recognizes exact ledger titles, treating the original title as number one.
func Number(title string) (int, bool) {
	if title == Title {
		return 1, true
	}
	suffix, ok := strings.CutPrefix(title, Title+" #")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(suffix)
	return n, err == nil && n > 0 && strconv.Itoa(n) == suffix
}

// Flush retains the buffer on any failure so a caller can retry explicitly.
func (l *Ledger) Flush(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.rows) == 0 {
		return nil
	}
	if l.retireID != "" {
		if err := l.store.SetState(ctx, l.retireID, "Done"); err != nil {
			return err
		}
		l.retireID = ""
	}
	issues, err := l.store.ListIssues(ctx)
	if err != nil {
		return err
	}
	var current Issue
	max := 0
	for _, issue := range issues {
		if n, ok := Number(issue.Title); ok && n > max {
			max = n
			current = issue
		}
	}
	rotate := false
	if current.ID != "" {
		comments, err := l.store.ListComments(ctx, current.ID)
		if err != nil {
			return err
		}
		rotate = len(comments) >= 400
	}
	if current.ID == "" || rotate {
		title := Title
		if max > 0 {
			title = fmt.Sprintf("%s #%d", Title, max+1)
		}
		next, err := l.store.CreateIssue(ctx, title, "Backlog")
		if err != nil {
			return err
		}
		if next.ID == "" {
			return errors.New("ledger: created issue has no ID")
		}
		if current.ID != "" {
			l.retireID = current.ID
			if err := l.store.SetState(ctx, current.ID, "Done"); err != nil {
				return err
			}
		}
		l.retireID = ""
		current = next
	}
	if err := l.store.CreateComment(ctx, current.ID, strings.Join(l.rows, "\n")); err != nil {
		return err
	}
	l.rows = nil
	return nil
}

// Close stops the ticker and flushes remaining rows. A failed Close may be retried.
func (l *Ledger) Close(ctx context.Context) error {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.stop)
	}
	l.mu.Unlock()
	<-l.done
	return l.Flush(ctx)
}
