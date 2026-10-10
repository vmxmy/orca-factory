package scheduler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

type liveBatchFixture struct {
	entered                                         chan struct{}
	claimed                                         atomic.Bool
	comments, snapshots, writes, freshReads, probes atomic.Int64
	conflict                                        atomic.Bool
	mode                                            atomic.Value
	client                                          *linear.Client
	count                                           int
	updated                                         atomic.Value
}

func liveBatchSetup(t *testing.T, projects ...string) *liveBatchFixture {
	t.Helper()
	project := "Alpha"
	if len(projects) > 0 {
		project = projects[0]
	}
	f := &liveBatchFixture{entered: make(chan struct{}), count: 100}
	f.mode.Store("")
	f.updated.Store("2026-01-01T00:00:00Z")
	t.Setenv("LINEAR_API_KEY", "isolated-fixture")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Requests-Limit", "2500")
		w.Header().Set("X-RateLimit-Requests-Remaining", "2400")
		w.Header().Set("X-RateLimit-Requests-Reset", "1800000000000")
		w.Header().Set("X-RateLimit-Complexity-Limit", "3000000")
		w.Header().Set("X-RateLimit-Complexity-Remaining", "2999999")
		w.Header().Set("X-RateLimit-Complexity-Reset", "1800000000000")
		var q struct {
			Query     string
			Variables map[string]any
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			return
		}
		var data any
		issue := func(id string, claimed bool) map[string]any {
			labels := []any{map[string]any{"id": "ready", "name": "ready-for-agent"}}
			state := "Todo"
			if claimed || (id == "0" && f.claimed.Load()) {
				labels = append(labels, map[string]any{"id": "auto", "name": "auto-impl"})
				state = "In Progress"
			}
			mode := f.mode.Load().(string)
			if mode == "done" {
				state = "Done"
			}
			if mode == "canceled" {
				state = "Canceled"
			}
			if mode == "retained" && (id == "0" || id == "1") {
				labels = []any{}
				if id == "1" {
					state = "Done"
				}
			}
			if mode == "removed-label" {
				labels = []any{}
			}
			if mode == "failed" {
				labels = append(labels, map[string]any{"id": "failed", "name": "auto-failed"})
			}
			blockers := []any{}
			if mode == "blocker" {
				blockers = append(blockers, map[string]any{"id": "rel", "type": "blocks", "issue": map[string]any{"id": "block", "identifier": "ENG-block", "state": map[string]any{"type": "started"}}})
			}
			return map[string]any{"relations": map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}, "inverseRelations": map[string]any{"nodes": blockers, "pageInfo": map[string]any{"hasNextPage": false}}, "id": id, "identifier": "ENG-" + id, "updatedAt": f.updated.Load().(string), "project": map[string]any{"id": "p1", "name": project}, "state": map[string]any{"id": "state", "name": state}, "labels": map[string]any{"nodes": labels, "pageInfo": map[string]any{"hasNextPage": false}}}
		}
		switch {
		case strings.Contains(q.Query, "QuotaProbe"):
			f.probes.Add(1)
			switch f.mode.Load().(string) {
			case "probe-failed":
				http.Error(w, "probe failed", http.StatusServiceUnavailable)
				return
			case "probe-low":
				w.Header().Set("X-RateLimit-Complexity-Remaining", "180000")
			}
			data = map[string]any{"viewer": map[string]any{"id": "viewer"}}
		case strings.Contains(q.Query, "Metadata"):
			field := "states"
			nodes := []any{map[string]any{"id": "progress", "name": "In Progress"}}
			if strings.Contains(q.Query, "labels(") {
				field = "labels"
				nodes = []any{map[string]any{"id": "ready", "name": "ready-for-agent"}, map[string]any{"id": "auto", "name": "auto-impl"}}
			}
			if strings.Contains(q.Query, "projects(") {
				field = "projects"
				nodes = []any{map[string]any{"id": "p1", "name": project}}
			}
			data = map[string]any{"team": map[string]any{field: map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false}}}}
		case strings.Contains(q.Query, "ListCommentsBatch"):
			f.comments.Add(1)
			mode := f.mode.Load().(string)
			if mode == "cancel" {
				close(f.entered)
				<-r.Context().Done()
				return
			}
			aliases := map[string]any{}
			for i := 0; i < 5; i++ {
				id, ok := q.Variables[fmt.Sprintf("id%d", i)].(string)
				if !ok {
					continue
				}
				page := 0
				if after, ok := q.Variables[fmt.Sprintf("after%d", i)].(string); ok && mode != "repeat-cursor" {
					if _, err := fmt.Sscanf(after, id+"-page-%d", &page); err != nil {
						t.Errorf("crossed cursor: %v", q.Variables)
					}
				}
				next := mode == "uneven" && page < i
				nodes := []any{}
				if mode == "uneven" && !next {
					nodes = append(nodes, map[string]any{"id": "owned-" + id, "body": "归属: 执行器 term=term-" + id + " run=run-" + id, "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z"})
				}
				if mode == "edited-comment" && id == "0" {
					nodes = append(nodes, map[string]any{"id": "edited", "body": "归属: 执行器 term=edited run=edited", "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-02T00:00:00Z"})
				}
				if mode == "retained" && (id == "0" || id == "1") {
					body := "归属: 执行器 term=owned run=existing attempt=3 scheduler-attempt=3"
					if id == "1" {
						body = `归属: 无
merge-progress-v1: {"queue_id":"q","phase":"merged","deployment":{"state":"pending"}}`
					}
					nodes = append(nodes, map[string]any{"id": "retained-" + id, "body": body, "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z"})
				}
				if mode == "later-failure" {
					if page > 0 {
						http.Error(w, "later page failed", 500)
						return
					}
					next = true
				}
				aliases[fmt.Sprintf("i%d", i)] = map[string]any{"id": id, "comments": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": fmt.Sprintf("%s-page-%d", id, page+1)}}}
			}
			switch f.mode.Load().(string) {
			case "missing-alias":
				delete(aliases, "i0")
			case "null-alias":
				aliases["i0"] = nil
			case "wrong-id":
				aliases["i0"].(map[string]any)["id"] = "other"
			case "missing-comments":
				delete(aliases["i0"].(map[string]any), "comments")
			case "null-comments":
				aliases["i0"].(map[string]any)["comments"] = nil
			case "missing-pageInfo":
				delete(aliases["i0"].(map[string]any)["comments"].(map[string]any), "pageInfo")
			case "null-node":
				aliases["i0"].(map[string]any)["comments"].(map[string]any)["nodes"] = []any{nil}
			case "missing-body":
				aliases["i0"].(map[string]any)["comments"].(map[string]any)["nodes"] = []any{map[string]any{"id": "bad"}}
			case "missing-nodes":
				delete(aliases["i0"].(map[string]any)["comments"].(map[string]any), "nodes")
			case "missing-next":
				aliases["i0"].(map[string]any)["comments"].(map[string]any)["pageInfo"] = map[string]any{"endCursor": nil}
			case "partial-error":
				_ = json.NewEncoder(w).Encode(map[string]any{"data": aliases, "errors": []any{map[string]any{"message": "partial"}}})
				return
			case "repeat-cursor":
				aliases["i0"].(map[string]any)["comments"].(map[string]any)["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": "same"}
			}
			if mode := f.mode.Load().(string); mode == "quota-requests" {
				w.Header().Set("X-RateLimit-Requests-Remaining", "125")
			} else if mode == "quota-above-resume" {
				w.Header().Set("X-RateLimit-Complexity-Limit", "1000")
				w.Header().Set("X-RateLimit-Complexity-Remaining", "600")
			} else if mode == "quota-complexity" {
				w.Header().Set("X-RateLimit-Complexity-Remaining", "150500")
			}
			data = aliases
		case strings.Contains(q.Query, "ListComments"):
			f.comments.Add(1)
			nodes := []any{}
			if f.conflict.Load() {
				nodes = append(nodes, map[string]any{"createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z", "id": "owned", "body": "阶段: worker 运行中\n归属: 执行器 term=other run=other attempt=1 scheduler-attempt=1 since=2026-10-07T00:00:00Z"})
			}
			connection := map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}
			if f.mode.Load().(string) == "critical-missing-page" {
				delete(connection, "pageInfo")
			}
			if f.mode.Load().(string) == "barrier" {
				connection["nodes"] = []any{map[string]any{"createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z", "id": "barrier", "body": `merge-progress-v1: {"queue_id":"q","phase":"merged","deployment":{"state":"pending"}}`}}
			}
			data = map[string]any{"issue": map[string]any{"comments": connection}}
		case strings.Contains(q.Query, "FreshIssue"):
			f.freshReads.Add(1)
			fresh := issue(q.Variables["id"].(string), false)
			switch f.mode.Load().(string) {
			case "missing-issue":
				fresh = nil
			case "missing-labels":
				delete(fresh, "labels")
			case "missing-blockers":
				delete(fresh, "inverseRelations")
			case "wrong-project":
				fresh["project"] = map[string]any{"id": "other", "name": "Other"}
			case "wrong-issue":
				fresh["id"] = "other"
			}
			data = map[string]any{"issue": fresh}
		case strings.Contains(q.Query, "IssueLabels"):
			data = map[string]any{"issue": map[string]any{"labels": map[string]any{"nodes": []any{}}}}
		case strings.Contains(q.Query, "mutation UpdateIssue"):
			f.writes.Add(1)
			f.claimed.Store(true)
			data = map[string]any{"issueUpdate": map[string]any{"success": true, "issue": issue(q.Variables["id"].(string), true)}}
		case strings.Contains(q.Query, "mutation CreateComment"):
			f.writes.Add(1)
			data = map[string]any{"commentCreate": map[string]any{"success": true, "comment": map[string]any{"id": "new", "body": "confirmed"}}}
		case strings.Contains(q.Query, "issues("):
			if f.mode.Load().(string) == "quota-issue-page" {
				w.Header().Set("X-RateLimit-Complexity-Remaining", "155500")
			}
			filter := q.Variables["filter"].(map[string]any)["project"].(map[string]any)["id"].(map[string]any)["in"].([]any)
			if len(filter) != 1 || filter[0] != "p1" {
				t.Errorf("client received foreign scope: %v", filter)
			}
			f.snapshots.Add(1)
			start := 0
			if cursor, ok := q.Variables["after"].(string); ok {
				_, _ = fmt.Sscan(cursor, &start)
			}
			nodes := []any{}
			for i := start; i < min(start+20, f.count); i++ {
				nodes = append(nodes, issue(fmt.Sprint(i), false))
			}
			data = map[string]any{"issues": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": start+20 < f.count, "endCursor": fmt.Sprint(start + 20)}}}
		default:
			t.Errorf("unexpected operation: %s", q.Query)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(s.Close)
	var err error
	f.client, err = linear.New(context.Background(), linear.Options{TeamID: "team", BaseURL: s.URL, HTTPClient: s.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func liveBatchRuntime(t *testing.T, f *liveBatchFixture) (*scheduler.Runtime, config.Repository) {
	repo := config.Repository{Path: "/fixture", Env: map[string]string{"AGENT_LINEAR_PROJECT": "Alpha", "AGENT_SCOPE_AVOID": "0"}}
	return &scheduler.Runtime{Clients: map[string]scheduler.LinearClient{repo.Path: f.client}, Repositories: map[string]config.Repository{repo.Path: repo}}, repo
}
func TestLiveBatchRepeatedDiscovery(t *testing.T) {
	f := liveBatchSetup(t)
	f.count = 123
	r, repo := liveBatchRuntime(t, f)
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { return now }
	for round := 1; round <= 4; round++ {
		got, err := r.Snapshot(context.Background(), []config.Repository{repo})
		if err != nil || len(got) != 123 {
			t.Fatalf("round %d tickets=%d err=%v", round, len(got), err)
		}
		for _, ticket := range got {
			if !ticket.Candidate || ticket.Owner != "" {
				t.Fatalf("unexpected candidate: %+v", ticket)
			}
		}
		if f.snapshots.Load() != int64(round*7) || f.comments.Load() != 25 {
			t.Fatalf("round=%d issue requests=%d comments=%d; want %d + 25", round, f.snapshots.Load(), f.comments.Load(), round*7)
		}
		now = now.Add(time.Minute)
	}
	f.mode.Store("edited-comment")
	now = now.Add(6 * time.Minute)
	rows, err := r.Snapshot(context.Background(), []config.Repository{repo})
	if err != nil || len(rows) != 123 || rows[0].Owner != machine.OwnerExecutor || rows[0].Run != "edited" || f.comments.Load() != 50 {
		t.Fatalf("bounded comment refresh: rows=%d comments=%d err=%v", len(rows), f.comments.Load(), err)
	}
	f.updated.Store("2026-01-02T00:00:00Z")
	if _, err := r.Snapshot(context.Background(), []config.Repository{repo}); err != nil || f.comments.Load() != 75 {
		t.Fatalf("changed issue refresh: comments=%d err=%v", f.comments.Load(), err)
	}
}

func TestLiveBatchIncompleteDiscoveryIsUnknown(t *testing.T) {
	for _, mode := range []string{"missing-alias", "null-alias", "wrong-id", "missing-comments", "null-comments", "missing-pageInfo", "missing-nodes", "missing-next", "partial-error", "repeat-cursor", "null-node", "missing-body", "later-failure"} {
		t.Run(mode, func(t *testing.T) {
			f := liveBatchSetup(t)
			r, repo := liveBatchRuntime(t, f)
			f.mode.Store(mode)
			rows, err := r.Snapshot(context.Background(), []config.Repository{repo})
			if err == nil || rows != nil || f.writes.Load() != 0 {
				t.Fatalf("incomplete discovery published tickets=%d writes=%d err=%v", len(rows), f.writes.Load(), err)
			}
		})
	}
}

func TestLiveBatchClaimRejectsFreshOwner(t *testing.T) {
	f := liveBatchSetup(t)
	r, repo := liveBatchRuntime(t, f)
	tickets, err := r.Snapshot(context.Background(), []config.Repository{repo})
	if err != nil {
		t.Fatal(err)
	}
	f.conflict.Store(true)
	for _, action := range []scheduler.ActionKind{scheduler.Claim, scheduler.Resume, scheduler.Merge, scheduler.Release, scheduler.Resolve} {
		before := f.comments.Load()
		err = r.Apply(context.Background(), tickets[0], scheduler.Action{Kind: action, Attempt: 1})
		if err == nil || f.writes.Load() != 0 || f.comments.Load() != before+1 {
			t.Fatalf("%s fresh owner must prohibit writes: writes=%d fresh comments=%d err=%v", action, f.writes.Load(), f.comments.Load()-before, err)
		}
	}
}

func TestLiveBatchClaimRevalidatesIssue(t *testing.T) {
	for _, mode := range []string{"done", "canceled", "removed-label", "blocker", "missing-issue", "missing-labels", "missing-blockers", "wrong-project", "wrong-issue", "critical-missing-page", "barrier"} {
		t.Run(mode, func(t *testing.T) {
			f := liveBatchSetup(t)
			r, repo := liveBatchRuntime(t, f)
			tickets, err := r.Snapshot(context.Background(), []config.Repository{repo})
			if err != nil {
				t.Fatal(err)
			}
			f.mode.Store(mode)
			err = r.Apply(context.Background(), tickets[0], scheduler.Action{Kind: scheduler.Claim, Attempt: 1})
			if err == nil || f.writes.Load() != 0 || f.freshReads.Load() != 1 {
				t.Fatalf("changed issue must prohibit writes: writes=%d fresh issue reads=%d err=%v", f.writes.Load(), f.freshReads.Load(), err)
			}
		})
	}
}

func TestLiveBatchPreservesClientScopes(t *testing.T) {
	a, b := liveBatchSetup(t, "Alpha"), liveBatchSetup(t, "Beta")
	r, ra := liveBatchRuntime(t, a)
	rb := config.Repository{Path: "/other", Env: map[string]string{"AGENT_LINEAR_PROJECT": "Beta", "AGENT_SCOPE_AVOID": "0"}}
	r.Clients[rb.Path] = b.client
	r.Repositories[rb.Path] = rb
	rows, err := r.Snapshot(context.Background(), []config.Repository{ra, rb})
	if err != nil || len(rows) != 200 || a.snapshots.Load() != 5 || b.snapshots.Load() != 5 || a.comments.Load() != 20 || b.comments.Load() != 20 {
		t.Fatalf("scopes lost: rows=%d a=%d/%d b=%d/%d err=%v", len(rows), a.snapshots.Load(), a.comments.Load(), b.snapshots.Load(), b.comments.Load(), err)
	}
	counts := map[string]int{}
	for _, row := range rows {
		counts[row.Repo]++
	}
	if counts[ra.Path] != 100 || counts[rb.Path] != 100 {
		t.Fatalf("wrong repositories: %v", counts)
	}
}

func TestLiveBatchStopsBeforeSpendingQuotaReserve(t *testing.T) {
	for _, mode := range []string{"quota-requests", "quota-complexity"} {
		t.Run(mode, func(t *testing.T) {
			f := liveBatchSetup(t)
			r, repo := liveBatchRuntime(t, f)
			f.mode.Store(mode)
			rows, err := r.Snapshot(context.Background(), []config.Repository{repo})
			if err == nil || rows != nil || f.comments.Load() != 1 {
				t.Fatalf("quota must stop after first batch: rows=%d comments=%d err=%v", len(rows), f.comments.Load(), err)
			}
			if err := r.ProbeLinearQuota(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLiveBatchCompletesEveryAliasIndependently(t *testing.T) {
	f := liveBatchSetup(t)
	r, repo := liveBatchRuntime(t, f)
	f.mode.Store("uneven")
	rows, err := r.Snapshot(context.Background(), []config.Repository{repo})
	if err != nil || len(rows) != 100 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	for _, row := range rows {
		if row.Owner != machine.OwnerExecutor || row.Terminal != "term-"+row.ID || row.Run != "run-"+row.ID {
			t.Fatalf("lost later ownership: %+v", row)
		}
	}
	if f.comments.Load() != 100 {
		t.Fatalf("uneven pages need 5 requests per group: %d", f.comments.Load())
	}
}
func TestLiveBatchCancellationPublishesNothing(t *testing.T) {
	f := liveBatchSetup(t)
	r, repo := liveBatchRuntime(t, f)
	f.mode.Store("cancel")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		rows, err := r.Snapshot(ctx, []config.Repository{repo})
		if rows != nil {
			done <- fmt.Errorf("published %d tickets", len(rows))
			return
		}
		done <- err
	}()
	select {
	case <-f.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request not reached")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation became empty success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not finish")
	}
}
func TestLiveBatchValidClaimVisibleNextRound(t *testing.T) {
	f := liveBatchSetup(t)
	r, repo := liveBatchRuntime(t, f)
	rows, err := r.Snapshot(context.Background(), []config.Repository{repo})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(context.Background(), rows[0], scheduler.Action{Kind: scheduler.Claim, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if f.writes.Load() != 2 || f.freshReads.Load() != 1 {
		t.Fatalf("valid claim writes=%d fresh=%d", f.writes.Load(), f.freshReads.Load())
	}
	rows, err = r.Snapshot(context.Background(), []config.Repository{repo})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == "0" && row.Candidate {
			t.Fatal("claim invisible next round")
		}
	}
}

func TestLiveBatchRetainsOwnedLabelRemovedAndDoneBarrier(t *testing.T) {
	f := liveBatchSetup(t)
	r, repo := liveBatchRuntime(t, f)
	f.mode.Store("retained")
	rows, err := r.Snapshot(context.Background(), []config.Repository{repo})
	if err != nil || len(rows) != 100 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if rows[0].Candidate || rows[0].Owner != machine.OwnerExecutor || rows[0].Attempt != 3 || rows[0].Run != "existing" {
		t.Fatalf("owned label-removed ticket lost: %+v", rows[0])
	}
	if rows[1].Candidate || !scheduler.DeploymentBarrier(rows[1]) {
		t.Fatalf("Done barrier lost: %+v", rows[1])
	}
}

// Keep repositories refused so this quota recovery test cannot launch workers;
// discovery and quota probing still use the production Runtime and HTTP client.
type refusedLiveRuntime struct{ *scheduler.Runtime }

func (r refusedLiveRuntime) AutomationDisabled(context.Context, config.Repository) (bool, error) {
	return false, nil
}

func TestLiveBatchHeadroomRefusalUsesCheapProbeRecovery(t *testing.T) {
	f := liveBatchSetup(t)
	r, repo := liveBatchRuntime(t, f)
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	s := &scheduler.Scheduler{Backend: refusedLiveRuntime{r}, Now: func() time.Time { return now }, Config: config.Config{MaxInFlight: 1, TokenBucket: config.TokenBucket{Capacity: 1, RefillPerSecond: 1}, Repositories: []config.Repository{repo}, Tuning: config.Tuning{Scheduler: config.DefaultScheduler()}}}
	f.mode.Store("quota-complexity")
	if err := s.Tick(context.Background()); err == nil {
		t.Fatal("headroom refusal was hidden")
	}
	if !s.Pauses.Paused("") {
		t.Fatal("headroom refusal above pause floor must enter quota recovery")
	}
	beforeIssues, beforeComments := f.snapshots.Load(), f.comments.Load()
	now = now.Add(3 * time.Minute)
	_ = s.Tick(context.Background())
	if f.probes.Load() != 0 || f.snapshots.Load() != beforeIssues || f.comments.Load() != beforeComments {
		t.Fatal("paused tick read discovery before backoff")
	}
	f.mode.Store("")
	now = now.Add(2 * time.Minute)
	_ = s.Tick(context.Background())
	if f.probes.Load() != 1 || s.Pauses.Paused("") || f.snapshots.Load() != beforeIssues+5 || f.comments.Load() != beforeComments+20 || f.writes.Load() != 0 {
		t.Fatalf("cheap probe failed to recover: probes=%d paused=%v issues=%d comments=%d writes=%d", f.probes.Load(), s.Pauses.Paused(""), f.snapshots.Load(), f.comments.Load(), f.writes.Load())
	}
}

func TestLiveBatchReservesIssuePageHeadroom(t *testing.T) {
	f := liveBatchSetup(t)
	r, repo := liveBatchRuntime(t, f)
	f.mode.Store("quota-issue-page")
	rows, err := r.Snapshot(context.Background(), []config.Repository{repo})
	if err == nil || rows != nil || f.snapshots.Load() != 1 || f.comments.Load() != 0 {
		t.Fatalf("issue pagination exceeded known headroom: issues=%d comments=%d rows=%d err=%v", f.snapshots.Load(), f.comments.Load(), len(rows), err)
	}
}

func TestLiveBatchAdmissionPauseRequiresSuccessfulProbe(t *testing.T) {
	for _, mode := range []string{"quota-complexity", "quota-above-resume"} {
		t.Run(mode, func(t *testing.T) {
			f := liveBatchSetup(t)
			r, repo := liveBatchRuntime(t, f)
			now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
			start := now
			s := &scheduler.Scheduler{Backend: refusedLiveRuntime{r}, Now: func() time.Time { return now }, Config: config.Config{MaxInFlight: 1, TokenBucket: config.TokenBucket{Capacity: 1, RefillPerSecond: 1}, Repositories: []config.Repository{repo}, Tuning: config.Tuning{Scheduler: config.DefaultScheduler()}}}
			f.mode.Store(mode)
			if err := s.Tick(context.Background()); err == nil || !s.Pauses.Paused("") {
				t.Fatalf("refusal did not pause: %v", err)
			}
			beforeIssues, beforeComments := f.snapshots.Load(), f.comments.Load()
			for _, step := range []struct {
				minute int
				mode   string
				probes int64
				paused bool
			}{{3, mode, 0, true}, {5, "probe-failed", 1, true}, {8, "probe-failed", 1, true}, {10, "probe-low", 2, true}, {13, "probe-low", 2, true}, {15, "", 3, false}} {
				now = start.Add(time.Duration(step.minute) * time.Minute)
				f.mode.Store(step.mode)
				_ = s.Tick(context.Background())
				if f.probes.Load() != step.probes || s.Pauses.Paused("") != step.paused || s.NextInterval <= 0 {
					t.Fatalf("minute %d: probes=%d paused=%v next=%v", step.minute, f.probes.Load(), s.Pauses.Paused(""), s.NextInterval)
				}
				issues, comments := beforeIssues, beforeComments
				if !step.paused {
					issues += 5
					comments += 20
				}
				if f.snapshots.Load() != issues || f.comments.Load() != comments || f.writes.Load() != 0 {
					t.Fatalf("minute %d: unexpected discovery/writes: issues=%d comments=%d writes=%d", step.minute, f.snapshots.Load(), f.comments.Load(), f.writes.Load())
				}
			}
		})
	}
}

func TestLiveBatchCanceledProbeKeepsAdmissionPauseAndBackoff(t *testing.T) {
	f := liveBatchSetup(t)
	r, repo := liveBatchRuntime(t, f)
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	s := &scheduler.Scheduler{Backend: refusedLiveRuntime{r}, Now: func() time.Time { return now }, Config: config.Config{MaxInFlight: 1, TokenBucket: config.TokenBucket{Capacity: 1, RefillPerSecond: 1}, Repositories: []config.Repository{repo}, Tuning: config.Tuning{Scheduler: config.DefaultScheduler()}}}
	f.mode.Store("quota-above-resume")
	_ = s.Tick(context.Background())
	beforeIssues, beforeComments := f.snapshots.Load(), f.comments.Load()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	now = now.Add(5 * time.Minute)
	_ = s.Tick(canceled)
	now = now.Add(3 * time.Minute)
	_ = s.Tick(context.Background())
	if !s.Pauses.Paused("") || f.probes.Load() != 0 || f.snapshots.Load() != beforeIssues || f.comments.Load() != beforeComments || s.NextInterval <= 0 {
		t.Fatal("canceled probe released pause or bypassed backoff")
	}
	f.mode.Store("")
	now = now.Add(2 * time.Minute)
	_ = s.Tick(context.Background())
	if s.Pauses.Paused("") || f.probes.Load() != 1 || f.snapshots.Load() != beforeIssues+5 || f.comments.Load() != beforeComments+20 || f.writes.Load() != 0 {
		t.Fatal("fresh successful probe did not recover after cancellation")
	}
}

func TestLiveBatchMergeRevalidatesCachedOwner(t *testing.T) {
	f := liveBatchSetup(t)
	r, repo := liveBatchRuntime(t, f)
	repo.Env["AGENT_GH_REPO"] = "o/r"
	f.conflict.Store(true)
	before := f.comments.Load()
	ticket := scheduler.Ticket{ID: "0", Identifier: "ENG-0", Repo: repo.Path, Owner: machine.OwnerMerge, Handoff: &merge.Handoff{QueueID: "q", EnqueuedAt: time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)}}
	err := r.AdvanceMerges(context.Background(), []scheduler.Ticket{ticket})
	if err == nil || !strings.Contains(err.Error(), "resnapshot required") || f.comments.Load() != before+1 || f.writes.Load() != 0 {
		t.Fatalf("cached merge owner must be reread before merge: fresh comments=%d writes=%d err=%v", f.comments.Load()-before, f.writes.Load(), err)
	}
}

func TestLiveBatchFailedTaskRejectsExecution(t *testing.T) {
	f := liveBatchSetup(t)
	r, repo := liveBatchRuntime(t, f)
	tickets, err := r.Snapshot(context.Background(), []config.Repository{repo})
	if err != nil {
		t.Fatal(err)
	}
	f.mode.Store("failed")
	for _, action := range []scheduler.ActionKind{scheduler.Resume, scheduler.Merge} {
		if err = r.Apply(context.Background(), tickets[0], scheduler.Action{Kind: action, Attempt: 1}); err == nil || f.writes.Load() != 0 {
			t.Fatalf("%s on failed task must not write: writes=%d err=%v", action, f.writes.Load(), err)
		}
	}
	executor := tickets[0]
	executor.Owner, executor.Terminal = machine.OwnerExecutor, "-"
	if err = r.Launch(context.Background(), executor); err == nil || !strings.Contains(err.Error(), "failed task") {
		t.Fatalf("failed task launch must be refused: %v", err)
	}
}
