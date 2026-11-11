package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

const landedSHA = "0ab0047178cd32132001eef0682d50a77a6f2622"

func landedDoneRuntime(t *testing.T, proposal resolver.Resolution, mode string, handoff *merge.Handoff) (*Runtime, *mergeIssues, Ticket) {
	t.Helper()
	var pr github.PullRequest
	if err := json.Unmarshal(recorded(t, "github-pr-merged.json"), &pr); err != nil {
		t.Fatal(err)
	}
	pr.Number = proposal.PR
	pr.MergeSHA = proposal.SHA
	pr.Merged = mode != "unmerged"
	if mode == "sha-mismatch" {
		pr.MergeSHA = strings.Repeat("f", 40)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/133"):
			_ = json.NewEncoder(w).Encode(pr)
		case strings.HasSuffix(r.URL.Path, "/commits/main"):
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": landedSHA})
		case strings.Contains(r.URL.Path, "/compare/") && strings.HasSuffix(r.URL.Path, "...main"):
			base := proposal.SHA
			if mode == "not-ancestor" {
				base = strings.Repeat("b", 40)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"merge_base_commit": map[string]string{"sha": base}})
		case strings.Contains(r.URL.Path, "/compare/main..."):
			files := []map[string]string{}
			if mode == "driver-not-installed" {
				files = append(files, map[string]string{"filename": "driver/internal/scheduler/resolver.go"})
			}
			if mode == "lane3-files" {
				files = append(files, map[string]string{"filename": "deploy/install.sh"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
		default:
			t.Errorf("unexpected GitHub request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	gh, err := github.New(github.Options{BaseURL: server.URL, HTTPClient: server.Client(), Token: func(context.Context) (string, error) { return "test", nil }})
	if err != nil {
		t.Fatal(err)
	}
	proposalJSON, _ := json.Marshal(proposal)
	recordJSON, _ := json.Marshal(resolver.Record{Version: 1, Attempt: 1, Phase: "result", Outcome: "succeeded", Result: string(proposalJSON)})
	rows := []task.Comment{
		{ID: "owner", Body: "阶段: Agent处置\n归属: 处置Agent", CreatedAt: unixSecond(1)},
		{ID: "result", Body: resolver.Prefix + string(recordJSON), CreatedAt: unixSecond(2)},
	}
	if handoff != nil {
		h, _ := json.Marshal(handoff)
		rows = append(rows, task.Comment{ID: "handoff", Body: merge.HandoffPrefix + string(h), CreatedAt: unixSecond(3)})
	}
	tracker := &mergeIssues{issue: task.Issue{ID: "id", Identifier: "OF-1", State: task.NamedID{Name: "In Progress"}}, rows: rows}
	repo := config.Repository{Path: "/repo", Env: map[string]string{"AGENT_GH_REPO": "o/r", "AGENT_LANE3_GLOBS": "deploy/*"}}
	runtime := &Runtime{GitHub: gh, Clients: map[string]issues.Store{"/repo": tracker}, Repositories: map[string]config.Repository{"/repo": repo}, Version: landedSHA, DriverPath: "/driver"}
	ticket := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-1", Repo: "/repo"}, rows)
	return runtime, tracker, ticket
}

func unixSecond(n int64) time.Time { return time.Unix(n, 0) }

func TestLandedDoneCompletesOnlyAfterAllChecks(t *testing.T) {
	proposal := resolver.Resolution{Action: "landed-done", PR: 133, SHA: landedSHA, Reason: "verified"}
	runtime, tracker, ticket := landedDoneRuntime(t, proposal, "ok", nil)
	if err := runtime.applyResolution(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if tracker.issue.State.Name != "Done" || len(tracker.bodies) != 1 {
		t.Fatalf("ticket not completed: state=%s comments=%q", tracker.issue.State.Name, tracker.bodies)
	}
	if body := tracker.bodies[0]; !strings.Contains(body, "PR #133") || !strings.Contains(body, landedSHA) || !strings.Contains(body, "ancestor=true") || !strings.Contains(body, "归属: 无") {
		t.Fatalf("completion evidence missing: %q", body)
	}
}

func TestLandedDoneRejectsUnsafeEvidenceWithoutChangingState(t *testing.T) {
	proposal := resolver.Resolution{Action: "landed-done", PR: 133, SHA: landedSHA, Reason: "verified"}
	cases := []struct {
		name string
		mode string
		h    *merge.Handoff
	}{
		{name: "unmerged", mode: "unmerged"},
		{name: "sha mismatch", mode: "sha-mismatch"},
		{name: "not ancestor", mode: "not-ancestor"},
		{name: "lane 3", mode: "ok", h: &merge.Handoff{QueueID: "q", Lane: 3}},
		{name: "lane 3 files without handoff", mode: "lane3-files"},
		{name: "frozen queue", mode: "ok", h: &merge.Handoff{QueueID: "q", Lane: 2}},
		{name: "driver not installed", mode: "driver-not-installed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runtime, tracker, ticket := landedDoneRuntime(t, proposal, tc.mode, tc.h)
			if tc.name == "driver not installed" {
				runtime.Version = strings.Repeat("c", 40)
				runtime.Run = func(context.Context, runx.Spec) ([]byte, error) { return []byte("orca-factory-driver other"), nil }
			}
			if err := runtime.applyResolution(context.Background(), ticket); err != nil {
				t.Fatal(err)
			}
			if tracker.issue.State.Name == "Done" || strings.Contains(strings.Join(tracker.bodies, "\n"), "阶段: 已合并") {
				t.Fatalf("unsafe proposal changed state: state=%s comments=%q", tracker.issue.State.Name, tracker.bodies)
			}
		})
	}
}
