package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// A serve restart leaves Runtime.GitHub nil until the merge lane first runs;
// an open-pr resolution arriving in that window must still reach GitHub.
func TestResolverOpenPRInitializesGitHub(t *testing.T) {
	ctx := context.Background()
	prRaw := recorded(t, "github-pr-open-clean.json")
	var pr github.PullRequest
	if err := json.Unmarshal(prRaw, &pr); err != nil {
		t.Fatal(err)
	}
	base := newFeishuClaimBase(t, true)
	feishuServer := httptest.NewServer(base)
	t.Cleanup(feishuServer.Close)
	client, err := feishu.New(feishu.Options{Endpoint: feishuServer.URL, AppID: "cli_test", AppSecret: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := issues.NewFeishu(ctx, client, "BASE_TOKEN", "https://ledger.example/base/BASE_TOKEN", feishuClaimRepo, issues.FeishuOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := store.List(ctx)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("ticket list: %+v %v", tasks, err)
	}
	issue := tasks[0]

	lookups, creates := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/commits/agent/OF-5/executor"):
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": pr.Head.SHA})
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls"):
			lookups++
			_, _ = w.Write([]byte("[]"))
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
			creates++
			var input github.CreatePRRequest
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			created := pr
			created.Body = input.Body
			_ = json.NewEncoder(w).Encode(created)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	original := githubNew
	t.Cleanup(func() { githubNew = original })
	githubNew = func(o github.Options) (*github.Client, error) {
		o.BaseURL, o.HTTPClient = server.URL, server.Client()
		o.Token = func(context.Context) (string, error) { return "test", nil }
		return github.New(o)
	}

	runtime := &Runtime{GitHub: nil, Clients: map[string]issues.Store{"/repo": store}, Repositories: map[string]config.Repository{"/repo": {Env: map[string]string{"AGENT_GH_REPO": "o/r"}}}}
	record := &resolver.Record{Version: 1, Attempt: 2, Phase: "result"}
	ticket := Ticket{ID: issue.ID, Identifier: issue.Identifier, Repo: "/repo", Owner: machine.OwnerResolver, Phase: machine.PhaseResolver, Resolution: record}
	proposal := resolver.Resolution{Action: "open-pr", Branch: "agent/OF-5/executor", Head: pr.Head.SHA, Title: "Recover PR", Body: "Verified fix", Reason: "gate passed"}
	if err := runtime.openResolutionPR(ctx, ticket, record, proposal); err != nil {
		t.Fatal(err)
	}

	if lookups == 0 || creates != 1 {
		t.Fatalf("GitHub calls: lookups=%d creates=%d, want lookups>0 and creates=1", lookups, creates)
	}
	base.mu.Lock()
	body := ""
	for _, row := range base.rows["tblE"] {
		body = row["正文"].(string)
	}
	base.mu.Unlock()
	if !strings.Contains(body, "阶段: Agent处置\n归属: 处置Agent\n") {
		t.Fatalf("resolution handed to human: %q", body)
	}
	got, err := readResolution([]task.Comment{{Body: body}})
	if err != nil || got == nil || got.Phase != "waiting" || got.Outcome != "succeeded" {
		t.Fatalf("resolution receipt: %+v %v", got, err)
	}
}
