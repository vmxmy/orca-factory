package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeploymentResolvesExistingWorkflowDisplayName(t *testing.T) {
	sha := strings.Repeat("a", 40)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("deployment mutated GitHub: %s", r.Method)
		}
		switch r.URL.Path {
		case "/repos/o/r/actions/workflows":
			fmt.Fprint(w, `{"total_count":2,"workflows":[{"id":7,"name":"Deploy to Cloudflare","path":".github/workflows/deploy.yml"},{"id":8,"name":"Other","path":".github/workflows/other.yml"}]}`)
		case "/repos/o/r/actions/workflows/7/runs":
			if r.URL.Query().Get("head_sha") != sha {
				t.Errorf("wrong SHA %s", r.URL)
			}
			fmt.Fprintf(w, `{"total_count":1,"workflow_runs":[{"id":10,"workflow_id":7,"path":".github/workflows/deploy.yml@main","head_sha":%q,"status":"completed","conclusion":"success"}]}`, sha)
		default:
			t.Errorf("wrong workflow endpoint %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c, _ := New(Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	state, err := c.Deployment(context.Background(), "o", "r", "Deploy to Cloudflare", sha)
	if err != nil || state != "passed" {
		t.Fatalf("existing workflow name rejected: %s %v", state, err)
	}
}

func TestDeploymentConservativeReceipts(t *testing.T) {
	sha := strings.Repeat("a", 40)
	row := func(id int, status, conclusion, head string) string {
		return fmt.Sprintf(`{"id":%d,"workflow_id":7,"path":".github/workflows/deploy.yml","head_sha":%q,"status":%q,"conclusion":%q}`, id, head, status, conclusion)
	}
	for _, tc := range []struct{ name, body, want string }{
		{"no runs", `{"total_count":0,"workflow_runs":[]}`, "pending"},
		{"unknown", `{"total_count":1,"workflow_runs":[` + row(1, "mystery", "success", sha) + `]}`, "pending"},
		{"unknown conclusion", `{"total_count":1,"workflow_runs":[` + row(1, "completed", "neutral", sha) + `]}`, "pending"},
		{"wrong SHA", `{"total_count":1,"workflow_runs":[` + row(1, "completed", "success", strings.Repeat("b", 40)) + `]}`, "pending"},
		{"missing count", `{"workflow_runs":[` + row(1, "completed", "success", sha) + `]}`, "pending"},
		{"truncated", `{"total_count":2,"workflow_runs":[]}`, "pending"},
		{"cancelled", `{"total_count":1,"workflow_runs":[` + row(1, "completed", "cancelled", sha) + `]}`, "failed"},
		{"older run still deploying", `{"total_count":2,"workflow_runs":[` + row(2, "completed", "success", sha) + `,` + row(1, "in_progress", "", sha) + `]}`, "pending"},
		{"successful rerun", `{"total_count":2,"workflow_runs":[` + row(2, "completed", "success", sha) + `,` + row(1, "completed", "failure", sha) + `]}`, "passed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }))
			defer server.Close()
			c, _ := New(Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
			state, err := c.Deployment(context.Background(), "o", "r", "deploy.yml", sha)
			if err != nil || state != tc.want {
				t.Fatalf("state=%s err=%v want=%s", state, err, tc.want)
			}
		})
	}
}
