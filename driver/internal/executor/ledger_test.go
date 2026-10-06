package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/ledger"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/usage"
)

type finishStore struct{ bodies []string }

func (s *finishStore) ListIssues(context.Context) ([]ledger.Issue, error) {
	return []ledger.Issue{{ID: "ledger", Title: ledger.Title}}, nil
}
func (s *finishStore) ListComments(context.Context, string) ([]ledger.Comment, error) {
	return nil, nil
}
func (s *finishStore) CreateIssue(context.Context, string, string) (ledger.Issue, error) {
	return ledger.Issue{}, errors.New("unexpected issue creation")
}
func (s *finishStore) SetState(context.Context, string, string) error { return nil }
func (s *finishStore) CreateComment(_ context.Context, _ string, body string) error {
	s.bodies = append(s.bodies, body)
	return nil
}

type finishGitHub struct {
	confirmed bool
	err       error
}

func (g *finishGitHub) Snapshot(context.Context, merge.Request) (merge.Snapshot, error) {
	return merge.Snapshot{Head: "head", Main: "main", CIHead: "head", CIGreen: true, MergeableState: "clean"}, nil
}
func (g *finishGitHub) Squash(context.Context, merge.Request, string, string) (string, error) {
	g.confirmed = g.err == nil
	return "squash", g.err
}

func TestLedgerOnFinish(t *testing.T) {
	for _, path := range []string{"merged", "escalated", "branch-escalated"} {
		for _, unavailable := range []bool{false, true} {
			t.Run(path+map[bool]string{false: "/usage", true: "/unavailable"}[unavailable], func(t *testing.T) {
				ctx := context.Background()
				e, _, tools, _, _, ticket := recoveryFixture(t)
				t.Setenv("AGENT_DRIVER_STATE_ROOT", e.Config.Root)
				e.Config.Tier = "fast"
				start := time.Date(2026, 10, 6, 1, 2, 3, 123456789, time.UTC)
				now := start
				e.Now = func() time.Time { now = now.Add(time.Second); return now }
				var warnings bytes.Buffer
				e.Stderr = &warnings
				store := &finishStore{}
				l := ledger.New(store, ledger.Options{Ticks: make(chan time.Time)})
				t.Cleanup(func() {
					if err := l.Close(ctx); err != nil {
						t.Error(err)
					}
				})
				calls := 0
				writer := &merge.LedgerWriter{Ledger: l, Warnings: &warnings, Now: e.Now, Collect: func(_ context.Context, id string, paths []string) (usage.TicketUsage, error) {
					calls++
					if id != ticket.Identifier || len(paths) != 1 || paths[0] != "/original/worktree" {
						t.Fatalf("usage attribution: %s %v", id, paths)
					}
					if unavailable {
						return usage.TicketUsage{}, errors.New("unavailable")
					}
					return usage.TicketUsage{Totals: usage.Totals{Input: 123, CostUSD: 0.25}}, nil
				}}
				e.LedgerWriter = writer
				if path == "escalated" {
					tools.gateReds = 5
				}
				if path == "branch-escalated" {
					tools.head = "base-sha"
				}
				result, err := e.Execute(ctx, ticket)
				if err != nil {
					t.Fatal(err)
				}
				if path == "merged" {
					if result.State.Owner != machine.OwnerMerge {
						t.Fatalf("result: %+v", result)
					}
					if err := l.Flush(ctx); err != nil {
						t.Fatal(err)
					}
					if len(store.bodies) != 0 {
						t.Fatal("ledger written before merge confirmation")
					}
					req := merge.Request{Repository: e.Config.Owner + "/" + e.Config.Repository, Number: result.PR.Number}
					github := &finishGitHub{}
					github.err = errors.New("read-back unavailable")
					unconfirmed := merge.New(github, nil, nil, nil)
					unconfirmed.SetLedgerWriter(writer)
					if err := unconfirmed.Enqueue(req); err != nil {
						t.Fatal(err)
					}
					if _, err := unconfirmed.Next(ctx, req.Repository); err == nil {
						t.Fatal("unconfirmed squash accepted")
					}
					if err := l.Flush(ctx); err != nil {
						t.Fatal(err)
					}
					if len(store.bodies) != 0 || calls != 0 {
						t.Fatal("usage or ledger written for an unconfirmed merge")
					}
					github.err = nil
					for range 2 {
						channel := merge.New(github, nil, nil, nil)
						channel.SetLedgerWriter(&merge.LedgerWriter{Ledger: l, Warnings: &warnings, Now: e.Now, Collect: writer.Collect})
						if err := channel.Enqueue(req); err != nil {
							t.Fatal(err)
						}
						r, err := channel.Next(ctx, req.Repository)
						if err != nil || r.Status != merge.Merged || !github.confirmed {
							t.Fatalf("merge: %+v %v", r, err)
						}
					}
					// Replayed completion through a different finish path has the same claim key.
					if err := e.handoff(ctx, ticket, result.State, "replayed finish"); err != nil {
						t.Fatal(err)
					}
				} else {
					if result.State.Owner != machine.OwnerHuman {
						t.Fatalf("result: %+v", result)
					}
					if err := l.Flush(ctx); err != nil {
						t.Fatal(err)
					}
					if len(store.bodies) != 1 {
						t.Fatalf("human transition did not write ledger: %v", store.bodies)
					}
					if err := e.handoff(ctx, ticket, result.State, "retry"); err != nil {
						t.Fatal(err)
					}
				}
				if err := l.Flush(ctx); err != nil {
					t.Fatal(err)
				}
				if len(store.bodies) != 1 || strings.Count(store.bodies[0], "ledger ") != 1 || calls != 1 {
					t.Fatalf("bodies=%v usage calls=%d", store.bodies, calls)
				}
				var row map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(store.bodies[0], "ledger ")), &row); err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{"ticket", "lane", "tier", "outcome", "first_gate", "corrections", "ci_reds", "pr", "minutes", "started_at", "finished_at", "stage_secs"} {
					if _, ok := row[key]; !ok {
						t.Fatalf("missing %s: %v", key, row)
					}
				}
				outcome := path
				if path == "branch-escalated" {
					outcome = "escalated"
				}
				if row["ticket"] != ticket.Identifier || row["outcome"] != outcome || row["tier"] != "fast" || row["lane"] != float64(ticket.Lane) {
					t.Fatalf("row: %v", row)
				}
				wantGate := map[string]string{"merged": "green", "escalated": "red", "branch-escalated": ""}[path]
				if row["first_gate"] != wantGate || row["corrections"] != float64(result.State.Corrections) {
					t.Fatalf("gate/corrections: %v", row)
				}
				if path == "merged" && row["pr"] != result.PR.HTMLURL {
					t.Fatalf("PR: %v", row)
				}
				_, hasUsage := row["usage"]
				if hasUsage == unavailable {
					t.Fatalf("usage presence: %v", row)
				}
				if hasUsage {
					u := row["usage"].(map[string]any)
					if u["input"] != float64(123) || u["cost_usd"] != 0.25 {
						t.Fatalf("usage: %v", u)
					}
				}
				if unavailable && !strings.Contains(warnings.String(), "usage") {
					t.Fatal("no usage warning")
				}
				started, err := time.Parse(time.RFC3339Nano, row["started_at"].(string))
				if err != nil {
					t.Fatal(err)
				}
				finished, err := time.Parse(time.RFC3339Nano, row["finished_at"].(string))
				if err != nil {
					t.Fatal(err)
				}
				if !finished.After(started) || row["minutes"] != finished.Sub(started).Minutes() {
					t.Fatalf("duration: %v", row)
				}
			})
		}
	}
}
