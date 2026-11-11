package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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
	var compare github.Comparison
	if err := json.Unmarshal(recorded(t, "github-compare-main-files.json"), &compare); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/133"):
			_ = json.NewEncoder(w).Encode(pr)
		case strings.HasSuffix(r.URL.Path, "/commits/main"):
			_, _ = w.Write(recorded(t, "github-commit-main.json"))
		case strings.Contains(r.URL.Path, "/compare/") && strings.HasSuffix(r.URL.Path, "...main"):
			ancestor := compare
			ancestor.MergeBaseCommit.SHA = proposal.SHA
			if mode == "not-ancestor" {
				ancestor.MergeBaseCommit.SHA = strings.Repeat("b", 40)
			}
			_ = json.NewEncoder(w).Encode(ancestor)
		case strings.Contains(r.URL.Path, "/compare/main..."):
			changed := compare
			switch mode {
			case "lane3-files":
				changed.Files = append(slices.Clone(compare.Files), github.File{Filename: "deploy/install.sh"})
			case "no-files":
				changed.Files = nil
			case "file-limit":
				changed.Files = make([]github.File, 300)
				for i := range changed.Files {
					changed.Files[i] = github.File{Filename: fmt.Sprintf("docs/f%d.md", i)}
				}
			}
			_ = json.NewEncoder(w).Encode(changed)
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
	if tracker.issue.State.Name != "Done" || len(tracker.bodies) != 2 {
		t.Fatalf("ticket not completed: state=%s comments=%q", tracker.issue.State.Name, tracker.bodies)
	}
	if body := tracker.bodies[0]; !strings.Contains(body, "PR #133") || !strings.Contains(body, landedSHA) || !strings.Contains(body, "ancestor=true") || strings.Contains(body, "归属:") {
		t.Fatalf("verification evidence must precede Done without releasing ownership: %q", body)
	}
	if body := tracker.bodies[1]; !strings.Contains(body, "归属: 无") {
		t.Fatalf("ownership not released after Done: %q", body)
	}
}

func TestLandedDoneReleaseRetriesAfterDone(t *testing.T) {
	proposal := resolver.Resolution{Action: "landed-done", PR: 133, SHA: landedSHA, Reason: "verified"}
	runtime, tracker, ticket := landedDoneRuntime(t, proposal, "ok", nil)
	tracker.failFinal = true
	if err := runtime.applyResolution(context.Background(), ticket); err == nil {
		t.Fatal("release comment failure was not reported")
	}
	if tracker.issue.State.Name != "Done" {
		t.Fatalf("state=%s, want Done before the release receipt", tracker.issue.State.Name)
	}
	tracker.failFinal = false
	retry := fromEvidence(t, Ticket{ID: "id", Identifier: "OF-1", Repo: "/repo"}, tracker.rows)
	if err := runtime.applyResolution(context.Background(), retry); err != nil {
		t.Fatalf("Done ticket still owned by resolver could not finish release: %v", err)
	}
	if last := tracker.bodies[len(tracker.bodies)-1]; !strings.Contains(last, "归属: 无") {
		t.Fatalf("ownership not released on retry: %q", tracker.bodies)
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
		{name: "no changed files", mode: "no-files"},
		{name: "compare file limit", mode: "file-limit"},
		{name: "frozen queue", mode: "ok", h: &merge.Handoff{QueueID: "q", Lane: 2}},
		{name: "driver not installed", mode: "driver-not-installed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runtime, tracker, ticket := landedDoneRuntime(t, proposal, tc.mode, tc.h)
			tracker.labels = true
			tracker.issue.Labels.Nodes = []task.NamedID{{Name: "auto-impl"}}
			if tc.name == "driver not installed" {
				runtime.Version = strings.Repeat("c", 40)
				runtime.Run = func(context.Context, runx.Spec) ([]byte, error) { return []byte("orca-factory-driver other"), nil }
			}
			if err := runtime.applyResolution(context.Background(), ticket); err != nil {
				t.Fatal(err)
			}
			if tracker.issue.State.Name != "In Progress" || len(tracker.issue.Labels.Nodes) != 1 || tracker.issue.Labels.Nodes[0].Name != "auto-impl" {
				t.Fatalf("unsafe proposal changed the ticket: state=%s labels=%v", tracker.issue.State.Name, tracker.issue.Labels.Nodes)
			}
			if len(tracker.bodies) != 1 || !strings.HasPrefix(tracker.bodies[0], resolver.Prefix) || !strings.Contains(tracker.bodies[0], "landed-done rejected") {
				t.Fatalf("unsafe proposal must only record a resolver retry: %q", tracker.bodies)
			}
		})
	}
}
