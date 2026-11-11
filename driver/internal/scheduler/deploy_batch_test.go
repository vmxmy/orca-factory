package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

type deployBatchIssues struct {
	issues.Store
	tickets []*mergeIssues
	heads   map[int]string
}

func (b *deployBatchIssues) item(id string) *mergeIssues {
	for _, t := range b.tickets {
		if t.issue.ID == id {
			return t
		}
	}
	panic("unknown ticket " + id)
}
func (b *deployBatchIssues) Snapshot(context.Context, []string, time.Time) ([]task.Issue, error) {
	var out []task.Issue
	for _, t := range b.tickets {
		out = append(out, t.issue)
	}
	return out, nil
}
func (b *deployBatchIssues) Read(ctx context.Context, id string) (issues.Task, error) {
	return b.item(id).Read(ctx, id)
}
func (b *deployBatchIssues) ListComments(ctx context.Context, id string) ([]task.Comment, error) {
	return b.item(id).ListComments(ctx, id)
}
func (b *deployBatchIssues) CreateComment(ctx context.Context, id, body string) (task.Comment, error) {
	return b.item(id).CreateComment(ctx, id, body)
}
func (b *deployBatchIssues) UpdateIssue(ctx context.Context, id, state string, add, remove []string) (task.Issue, error) {
	return b.item(id).UpdateIssue(ctx, id, state, add, remove)
}
func (b *deployBatchIssues) Repository() string { return "o/r" }

func newBatchFixture(t *testing.T, count int) (*mergeFixture, *selfDeployHost, *deployBatchIssues, *[]bool) {
	t.Helper()
	f, host := newSelfDeployFixture(t)
	store := &deployBatchIssues{heads: map[int]string{}}
	var original merge.Handoff
	raw := strings.TrimPrefix(strings.Split(f.tracker.rows[0].Body, "\n")[2], merge.HandoffPrefix)
	if err := json.Unmarshal([]byte(raw), &original); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= count; i++ {
		h := original
		h.TicketID = fmt.Sprintf("id%d", i)
		h.Identifier = fmt.Sprintf("ENG-%d", i)
		h.QueueID = fmt.Sprintf("q%d", i)
		h.PR = i
		h.EnqueuedAt = h.EnqueuedAt.Add(time.Duration(i) * time.Second)
		encoded, err := json.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		issue := f.tracker.issue
		issue.ID = h.TicketID
		issue.Identifier = h.Identifier
		store.tickets = append(store.tickets, &mergeIssues{issue: issue, rows: []task.Comment{{ID: "handoff", CreatedAt: time.Now(), Body: "阶段: 排队合并\n归属: 合并通道\n" + merge.HandoffPrefix + string(encoded)}}})
	}
	merged := make([]bool, count)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		var n int
		if strings.HasSuffix(path, "/pulls") {
			prs := []map[string]any{}
			for i := 1; i <= count; i++ {
				if !merged[i-1] {
					prs = append(prs, batchPR(f, i, false, store.heads[i]))
				}
			}
			if err := json.NewEncoder(w).Encode(prs); err != nil {
				t.Error(err)
			}
			return
		}
		if _, err := fmt.Sscanf(path, "/repos/o/r/pulls/%d", &n); err == nil && n > 0 && n <= count {
			if strings.HasSuffix(path, "/merge") {
				merged[n-1] = true
				f.puts++
				if _, err := fmt.Fprint(w, `{"merged":true}`); err != nil {
					t.Error(err)
				}
				return
			}
			if err := json.NewEncoder(w).Encode(batchPR(f, n, merged[n-1], store.heads[n])); err != nil {
				t.Error(err)
			}
			return
		}
		f.server.Config.Handler.ServeHTTP(w, req)
	}))
	t.Cleanup(server.Close)
	infra.HTTPTransport = mergeTransport{server.URL}
	f.runtime.Clients[f.repos[0].Path] = store
	return f, host, store, &merged
}
func batchPR(f *mergeFixture, n int, merged bool, head string) map[string]any {
	if head == "" {
		head = f.head
	}
	return map[string]any{"number": n, "state": "open", "draft": false, "merged": merged, "mergeable": true, "mergeable_state": "clean", "head": map[string]any{"sha": head, "ref": fmt.Sprintf("agent/ENG-%d/executor", n), "repo": map[string]any{"full_name": "o/r"}}, "base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "o/r"}}, "body": fmt.Sprintf("关联票: ENG-%d", n), "merge_commit_sha": fmt.Sprintf("%040x", n)}
}
func batchProgress(t *testing.T, store *mergeIssues) *merge.Progress {
	t.Helper()
	ticket := Ticket{ID: store.issue.ID, Identifier: store.issue.Identifier}
	readMergeHandoff(&ticket, store.rows)
	if ticket.MergeProblem != "" || ticket.Progress == nil {
		t.Fatalf("invalid progress: %+v", ticket)
	}
	return ticket.Progress
}
func TestDeployBatchSharesInstallAndSmoke(t *testing.T) {
	f, host, store, _ := newBatchFixture(t, 2)
	firstServe(t, f, host)
	if f.puts != 2 || host.builds != 1 {
		t.Fatalf("two PRs require one install: merges=%d builds=%d", f.puts, host.builds)
	}
	if err := serveAs(f, host, fmt.Sprintf("%040x", 2)); err != nil {
		t.Fatal(err)
	}
	if host.builds != 1 || host.smokes != 1 {
		t.Fatalf("builds=%d smokes=%d", host.builds, host.smokes)
	}
	a, b := batchProgress(t, store.tickets[0]), batchProgress(t, store.tickets[1])
	if a.MergeSHA == b.MergeSHA || !reflect.DeepEqual(a.Deployment, b.Deployment) || !a.Deployment.SmokePassed {
		t.Fatalf("progress not shared: %+v %+v", a, b)
	}
	for _, ticket := range store.tickets {
		if ticket.issue.State.Name != "Done" {
			t.Fatalf("not Done: %s", ticket.issue.Identifier)
		}
	}
}

func TestDeployBatchCandidateHeadChangedDeploysConfirmedPrefix(t *testing.T) {
	f, host, store, merged := newBatchFixture(t, 2)
	store.heads[2] = fmt.Sprintf("%040x", 99)
	firstServe(t, f, host)
	if f.puts != 1 || (*merged)[1] || host.builds != 1 {
		t.Fatalf("candidate head changed: merges=%d merged=%v builds=%d", f.puts, *merged, host.builds)
	}
	_ = serveAs(f, host, fmt.Sprintf("%040x", 1))
	if store.tickets[0].issue.State.Name != "Done" || host.builds != 1 || host.smokes != 1 || batchProgress(t, store.tickets[0]).Deployment.Batch != nil {
		t.Fatalf("confirmed prefix did not finish as a single deployment: state=%s builds=%d smokes=%d deployment=%+v", store.tickets[0].issue.State.Name, host.builds, host.smokes, batchProgress(t, store.tickets[0]).Deployment)
	}
	if store.tickets[1].issue.State.Name == "Done" {
		t.Fatal("candidate with changed head completed")
	}
}

func TestDeployBatchCandidateReceiptFailedDeploysConfirmedPrefix(t *testing.T) {
	f, host, store, _ := newBatchFixture(t, 2)
	store.tickets[1].failBody = `"phase":"merged"`
	firstServe(t, f, host)
	if f.puts != 2 || host.builds != 1 {
		t.Fatalf("candidate receipt failed: merges=%d builds=%d", f.puts, host.builds)
	}
	if batchProgress(t, store.tickets[0]).Deployment.Batch != nil {
		t.Fatal("unconfirmed candidate joined the deployment batch")
	}
}

func TestDeployBatchSmokeFailurePausesEveryTicket(t *testing.T) {
	f, host, store, _ := newBatchFixture(t, 2)
	firstServe(t, f, host)
	host.smokeOut = []byte("not a doctor report")
	if err := serveAs(f, host, fmt.Sprintf("%040x", 2)); err == nil {
		t.Fatal("failed smoke accepted")
	}
	for _, ticket := range store.tickets {
		p := batchProgress(t, ticket)
		if ticket.issue.State.Name == "Done" || p.Deployment.State != "paused" {
			t.Fatalf("failure not attributed to %s: %+v", ticket.issue.Identifier, p.Deployment)
		}
	}
	if host.builds != 1 || host.smokes != 1 || host.restarts != 2 {
		t.Fatalf("builds=%d smokes=%d restarts=%d", host.builds, host.smokes, host.restarts)
	}
}
func TestDeployBatchLimit(t *testing.T) {
	f, host, _, merged := newBatchFixture(t, 4)
	firstServe(t, f, host)
	if f.puts != 3 || (*merged)[3] || host.builds != 1 {
		t.Fatalf("batch limit: merged=%v builds=%d", *merged, host.builds)
	}
}

func TestDeployBatchRollbackResumesMaintenance(t *testing.T) {
	f, host, store, _ := newBatchFixture(t, 2)
	firstServe(t, f, host)
	host.smokeOut = []byte("broken doctor")
	_ = serveAs(f, host, fmt.Sprintf("%040x", 2))
	_ = serveAs(f, host, "old")
	state, err := maintenance.Status()
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != "running" {
		t.Fatalf("rollback left maintenance %s", state.Mode)
	}
	for _, ticket := range store.tickets {
		if batchProgress(t, ticket).Deployment.State != "paused" {
			t.Fatal("rollback released batch")
		}
		if batchProgress(t, ticket).Deployment.Reason != "self deployment smoke failed; previous driver restored" {
			t.Fatal("rollback discarded failure reason")
		}
	}
}

func TestDeployBatchPartialIntentNeverInstalls(t *testing.T) {
	f, host, store, _ := newBatchFixture(t, 2)
	store.tickets[1].failBody = `"state":"deploy-intent"`
	firstServe(t, f, host)
	if host.builds != 0 {
		t.Fatal("installed before all intent receipts")
	}
	store.tickets[1].failBody = ""
	_ = serveAs(f, host, "old")
	for _, ticket := range store.tickets {
		if batchProgress(t, ticket).Deployment.State != "paused" {
			t.Fatal("partial intent did not pause every member")
		}
	}
	if host.builds != 0 {
		t.Fatal("reinstalled after uncertain intent")
	}
}

func TestDeployBatchSingleKeepsLegacyReceipt(t *testing.T) {
	f, host, store, _ := newBatchFixture(t, 1)
	firstServe(t, f, host)
	if err := serveAs(f, host, fmt.Sprintf("%040x", 1)); err != nil {
		t.Fatal(err)
	}
	p := batchProgress(t, store.tickets[0])
	if p.Deployment.Batch != nil || host.builds != 1 || host.smokes != 1 || store.tickets[0].issue.State.Name != "Done" {
		t.Fatalf("single deployment changed: %+v", p.Deployment)
	}
}

func TestDeployBatchRecoveryAppliesToAllMembers(t *testing.T) {
	f, host, store, _ := newBatchFixture(t, 2)
	firstServe(t, f, host)
	host.smokeOut = []byte("broken doctor")
	_ = serveAs(f, host, fmt.Sprintf("%040x", 2))
	ctx := context.Background()
	sha := fmt.Sprintf("%040x", 1)
	if err := f.runtime.ResumeDeployment(ctx, "ENG-1", "q1", sha, "retry-smoke", "operator", "batch reconciled", true); err != nil {
		t.Fatal(err)
	}
	for _, ticket := range store.tickets {
		if batchProgress(t, ticket).Deployment.Recovery != nil {
			t.Fatal("dry run changed recovery")
		}
	}
	if err := f.runtime.ResumeDeployment(ctx, "ENG-1", "q1", sha, "retry-smoke", "operator", "batch reconciled", false); err != nil {
		t.Fatal(err)
	}
	for _, ticket := range store.tickets {
		d := batchProgress(t, ticket).Deployment
		if d.State != "deploy-intent" || d.Recovery == nil || d.Recovery.Action != "retry-smoke" {
			t.Fatalf("recovery omitted %s: %+v", ticket.issue.Identifier, d)
		}
	}
}

func TestDeployBatchPartialPassedWaitsForAllReceipts(t *testing.T) {
	f, host, store, _ := newBatchFixture(t, 2)
	firstServe(t, f, host)
	store.tickets[1].failBody = `"state":"passed"`
	_ = serveAs(f, host, fmt.Sprintf("%040x", 2))
	_ = serveAs(f, host, fmt.Sprintf("%040x", 2))
	for _, ticket := range store.tickets {
		if ticket.issue.State.Name == "Done" {
			t.Fatal("completed before all passed receipts")
		}
	}
	store.tickets[1].failBody = ""
	if err := serveAs(f, host, fmt.Sprintf("%040x", 2)); err != nil {
		t.Fatal(err)
	}
	if host.smokes != 1 {
		t.Fatalf("repeated a confirmed smoke: %d", host.smokes)
	}
	for _, ticket := range store.tickets {
		if ticket.issue.State.Name != "Done" {
			t.Fatal("did not complete repaired receipt")
		}
	}
}

func TestDeployBatchInstallFailurePausesAll(t *testing.T) {
	f, host, store, _ := newBatchFixture(t, 2)
	host.guardErr = errors.New("guard unavailable")
	firstServe(t, f, host)
	for _, ticket := range store.tickets {
		if batchProgress(t, ticket).Deployment.State != "paused" || ticket.issue.State.Name == "Done" {
			t.Fatal("install failure escaped batch pause")
		}
	}
	if host.builds != 1 || host.smokes != 0 || host.restarts != 0 {
		t.Fatalf("builds=%d smokes=%d restarts=%d", host.builds, host.smokes, host.restarts)
	}
}
func TestDeployBatchRetriesDoneWithoutDeploying(t *testing.T) {
	f, host, store, _ := newBatchFixture(t, 2)
	firstServe(t, f, host)
	store.tickets[1].failDone = true
	if err := serveAs(f, host, fmt.Sprintf("%040x", 2)); err == nil {
		t.Fatal("completion failure ignored")
	}
	store.tickets[1].failDone = false
	if err := serveAs(f, host, fmt.Sprintf("%040x", 2)); err != nil {
		t.Fatal(err)
	}
	for _, ticket := range store.tickets {
		if ticket.issue.State.Name != "Done" {
			t.Fatal("Done retry did not finish")
		}
	}
	if host.builds != 1 || host.smokes != 1 {
		t.Fatal("Done retry redeployed or repeated smoke")
	}
}

func TestDeployBatchPartialSmokeIntentCanRecover(t *testing.T) {
	for _, action := range []string{"retry-smoke", "confirm-passed"} {
		t.Run(action, func(t *testing.T) {
			f, host, store, _ := newBatchFixture(t, 2)
			firstServe(t, f, host)
			store.tickets[1].failBody = `"state":"smoke-intent"`
			_ = serveAs(f, host, fmt.Sprintf("%040x", 2))
			store.tickets[1].failBody = ""
			_ = serveAs(f, host, fmt.Sprintf("%040x", 2))
			if err := f.runtime.ResumeDeployment(context.Background(), "ENG-2", "q2", fmt.Sprintf("%040x", 2), action, "operator", "batch reconciled", false); err != nil {
				t.Fatal(err)
			}
			if err := serveAs(f, host, fmt.Sprintf("%040x", 2)); err != nil {
				t.Fatal(err)
			}
			for _, ticket := range store.tickets {
				if ticket.issue.State.Name != "Done" {
					t.Fatal("partial smoke intent cannot recover")
				}
			}
		})
	}
}
func TestDeployBatchPartialRecoveryCanReplay(t *testing.T) {
	for _, selected := range []int{1, 2} {
		t.Run(fmt.Sprint(selected), func(t *testing.T) {
			f, host, store, _ := newBatchFixture(t, 2)
			host.guardErr = errors.New("guard unavailable")
			firstServe(t, f, host)
			store.tickets[1].failBody = `"action":"confirm-passed"`
			ctx := context.Background()
			if err := f.runtime.ResumeDeployment(ctx, "ENG-1", "q1", fmt.Sprintf("%040x", 1), "confirm-passed", "operator", "batch reconciled", false); err == nil {
				t.Fatal("lost recovery write accepted")
			}
			store.tickets[1].failBody = ""
			if err := f.runtime.ResumeDeployment(ctx, fmt.Sprintf("ENG-%d", selected), fmt.Sprintf("q%d", selected), fmt.Sprintf("%040x", selected), "confirm-passed", "operator", "batch reconciled", false); err != nil {
				t.Fatal(err)
			}
			a, b := batchProgress(t, store.tickets[0]), batchProgress(t, store.tickets[1])
			if !reflect.DeepEqual(a.Deployment, b.Deployment) || a.Deployment.State != "passed" {
				t.Fatal("recovery replay did not converge")
			}
		})
	}
}
func TestDeployBatchRollbackRetainsFailedPauseWrite(t *testing.T) {
	f, host, store, _ := newBatchFixture(t, 2)
	firstServe(t, f, host)
	store.tickets[1].failBody = `"state":"paused"`
	host.smokeOut = []byte("broken doctor")
	_ = serveAs(f, host, fmt.Sprintf("%040x", 2))
	_ = serveAs(f, host, "old")
	store.tickets[1].failBody = ""
	_ = serveAs(f, host, "old")
	for _, ticket := range store.tickets {
		if batchProgress(t, ticket).Deployment.State != "paused" {
			t.Fatal("rollback lost unfinished pause write")
		}
	}
	state, err := maintenance.Status()
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != "running" {
		t.Fatalf("maintenance left %s", state.Mode)
	}
}
func TestDeployBatchKeepsHistoricalSingleBarrier(t *testing.T) {
	f, host := newSelfDeployFixture(t)
	f.merged = true
	p := merge.Progress{Version: 1, QueueID: "q1", TicketID: "id", Repository: "o/r", PR: 1, Head: f.head, Main: f.main, MergeSHA: strings.Repeat("a", 40), Phase: "merged", Deployment: newDeployment(f.repos[0].Env)}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	f.tracker.rows = append(f.tracker.rows, task.Comment{ID: "pending", CreatedAt: time.Now(), Body: "阶段: 已合并\n归属: 无\n" + merge.ProgressPrefix + string(raw)})
	f.tracker.issue.State.Name = "Done"
	firstServe(t, f, host)
	if host.builds != 1 {
		t.Fatal("historical deployment barrier stopped progressing")
	}
}

func TestDeployBatchPartialSmokeRecoverySurvivesTick(t *testing.T) {
	f, host, store, _ := newBatchFixture(t, 2)
	firstServe(t, f, host)
	store.tickets[1].failBody = `"state":"smoke-intent"`
	_ = serveAs(f, host, fmt.Sprintf("%040x", 2))
	store.tickets[1].failBody = ""
	_ = serveAs(f, host, fmt.Sprintf("%040x", 2))
	store.tickets[1].failBody = `"action":"retry-smoke"`
	ctx := context.Background()
	sha := fmt.Sprintf("%040x", 2)
	if err := f.runtime.ResumeDeployment(ctx, "ENG-2", "q2", sha, "retry-smoke", "operator", "batch reconciled", false); err == nil {
		t.Fatal("partial recovery write accepted")
	}
	_ = serveAs(f, host, sha)
	store.tickets[1].failBody = ""
	if err := f.runtime.ResumeDeployment(ctx, "ENG-2", "q2", sha, "retry-smoke", "operator", "batch reconciled", false); err != nil {
		t.Fatal(err)
	}
	if err := serveAs(f, host, sha); err != nil {
		t.Fatal(err)
	}
	for _, ticket := range store.tickets {
		if ticket.issue.State.Name != "Done" {
			t.Fatal("partial smoke recovery did not finish")
		}
	}
}
