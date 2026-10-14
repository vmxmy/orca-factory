package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/ledger"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func init() { Register("ledger", runLedger) }

func runLedger(args []string) int {
	appendRow := len(args) == 2 && args[0] == "append"
	stats := len(args) == 1 && args[0] == "stats" || len(args) == 2 && args[0] == "stats" && args[1] == "--all"
	if !appendRow && !stats {
		fmt.Fprintln(os.Stderr, "usage: driver ledger append '<json>' | driver ledger stats [--all]")
		return 2
	}
	env := issueEnvironment()
	backend, err := issues.Backend(env)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	team, project := env["AGENT_LINEAR_TEAM"], env["AGENT_LINEAR_PROJECT"]
	if backend == "linear" && (team == "" || project == "") {
		fmt.Fprintln(os.Stderr, "ledger: AGENT_LINEAR_TEAM and AGENT_LINEAR_PROJECT required")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ledger: home directory unavailable")
		return 1
	}
	cfg, err := config.Load(home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ledger: driver configuration could not be loaded")
		return 1
	}
	var store ledger.Store
	if issues.Native(env) {
		client, err := issues.New(ctx, env, cfg.Tuning)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		tasks, ok := client.(issues.Store)
		if !ok {
			fmt.Fprintln(os.Stderr, "ledger: backend keeps no canonical task records")
			return 1
		}
		store = &issues.LedgerStore{Client: tasks}
	} else {
		client, err := linear.New(ctx, linear.Options{TeamID: team, HTTPTimeout: cfg.Tuning.Linear.HTTPTimeout})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		store = &ledgerStore{client: client, team: team, project: project}
	}
	l := ledger.New(store, ledger.Options{FlushInterval: cfg.Tuning.Ledger.FlushInterval})
	if appendRow {
		if err = l.Append(args[1]); err != nil {
			_ = l.Close(ctx) // Invalid input was never buffered; preserve the validation error.
			fmt.Fprintln(os.Stderr, "ledger: invalid json row")
			return 2
		}
		// Close synchronously flushes before os.Exit in main.
		err = l.Close(ctx)
	} else {
		err = l.Stats(ctx, len(args) == 2, os.Stdout, os.Stderr)
		closeErr := l.Close(ctx)
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ledger: operation failed")
		return 1
	}
	return 0
}

type ledgerStore struct {
	client        *linear.Client
	team, project string
}

// Snapshot omits titles, so issue discovery and creation use Orca. All comment
// reads/writes and state mutations reuse the paginated, team-scoped Linear client.
func (s *ledgerStore) ListIssues(ctx context.Context) ([]ledger.Issue, error) {
	var result struct {
		Issues    []ledger.Issue `json:"issues"`
		Truncated bool           `json:"truncated"`
	}
	err := ledgerOrca(ctx, &result, "list-issues", "--team", s.team, "--project", s.project, "--query", ledger.Title, "--include-archived", "--json")
	if err != nil {
		return nil, err
	}
	if result.Truncated {
		return nil, errors.New("ledger: issue list truncated")
	}
	issues := make([]ledger.Issue, 0, len(result.Issues))
	for _, issue := range result.Issues {
		if _, ok := ledger.Number(issue.Title); ok {
			if issue.ID == "" {
				return nil, errors.New("ledger: issue ID missing")
			}
			issues = append(issues, issue)
		}
	}
	return issues, nil
}
func (s *ledgerStore) CreateIssue(ctx context.Context, title, state string) (ledger.Issue, error) {
	var result struct {
		Issue ledger.Issue `json:"issue"`
	}
	// No --label arguments: a new ledger issue has no labels.
	err := ledgerOrca(ctx, &result, "save-issue", "--team", s.team, "--project", s.project, "--title", title, "--state", state, "--json")
	if err == nil && result.Issue.ID == "" {
		err = errors.New("ledger: created issue ID missing")
	}
	return result.Issue, err
}
func (s *ledgerStore) ListComments(ctx context.Context, id string) ([]ledger.Comment, error) {
	comments, err := s.client.ListComments(ctx, id)
	if err != nil {
		return nil, err
	}
	result := make([]ledger.Comment, 0, len(comments))
	for _, c := range comments {
		result = append(result, ledger.Comment{Body: c.Body, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
	}
	return result, nil
}
func (s *ledgerStore) SetState(ctx context.Context, id, state string) error {
	_, err := s.client.UpdateIssue(ctx, id, state, nil, nil)
	return err
}
func (s *ledgerStore) CreateComment(ctx context.Context, id, body string) error {
	_, err := s.client.CreateComment(ctx, id, body)
	return err
}
func ledgerOrca(ctx context.Context, result any, args ...string) error {
	path, err := runx.OrcaPath()
	if err != nil {
		return err
	}
	raw, err := runx.Run(ctx, runx.Spec{Path: path, Args: append([]string{"linear"}, args...), Timeout: 30 * time.Second})
	if err != nil {
		return err
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &envelope) != nil || !envelope.OK || len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return errors.New("ledger: invalid Orca response")
	}
	if json.Unmarshal(envelope.Result, result) != nil {
		return errors.New("ledger: invalid Orca result")
	}
	return nil
}
