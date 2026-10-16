package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/runnerhealth"
)

// wrappedStore is a ledger store that is not the GitHub backend type.
type wrappedStore struct{ *issues.GitHub }

func TestStoreClaimRefreshesOwnershipBeforeMutation(t *testing.T) {
	ctx := context.Background()
	body := `<!-- orca-task-v1: {"id":"01234567-1234-1234-1234-012345678901","identifier":"ZII-42","project":"o/r","state":"Todo","complete":true,"blockers":[],"related":[]} -->`
	changed := false
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes++
		}
		if strings.HasSuffix(r.URL.Path, "/comments") {
			if changed {
				_, _ = fmt.Fprint(w, `[{"id":4,"body":"转人: manual stop\n归属: 人","created_at":"2026-10-06T00:00:01Z","updated_at":"2026-10-06T00:00:01Z"}]`)
			} else {
				_, _ = fmt.Fprint(w, `[]`)
			}
			return
		}
		row := fmt.Sprintf(`{"number":42,"body":%q,"state":"open","labels":[{"name":"ready-for-agent"},{"name":"auto-impl"}],"updated_at":"2026-10-06T00:00:00Z"}`, body)
		if r.Method == "GET" {
			_, _ = fmt.Fprintf(w, "[%s]", row)
		} else {
			_, _ = fmt.Fprint(w, row)
		}
	}))
	defer server.Close()
	gh, _ := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	client, _ := issues.NewGitHub("o/r", gh)
	if _, err := client.List(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SnapshotComments(ctx, "ZII-42"); err != nil {
		t.Fatal(err)
	}
	changed = true
	r := &Runtime{Clients: map[string]IssueClient{"/repo": wrappedStore{client}}}
	err := r.Apply(ctx, Ticket{ID: "01234567-1234-1234-1234-012345678901", Identifier: "ZII-42", Repo: "/repo", Candidate: true}, Action{Kind: Claim})
	if err == nil || !strings.Contains(err.Error(), "ownership") || writes != 0 {
		t.Fatalf("claim used cached ownership: error=%v writes=%d", err, writes)
	}
}

// The runner alert follows the code repository, whichever ledger holds tickets.
func TestRunnerMonitorFollowsCodeRepository(t *testing.T) {
	env := map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_GH_REPO": runnerhealth.Repository, "AGENT_FEISHU_PROJECT": "github.com/" + runnerhealth.Repository}
	r := &Runtime{runnerState: t.TempDir(), Clients: map[string]IssueClient{"/repo": wrappedStore{}}}
	r.watchRunners(config.Repository{Path: "/repo", Env: env})
	if r.Runners == nil {
		t.Fatal("runner monitor disabled for a non-GitHub ledger")
	}
}
