package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
)

type scopeStore struct {
	notifyStore
	face  string
	edits int
}

func (s *scopeStore) Read(context.Context, string) (issues.Task, error) {
	return issues.Task{Description: s.face, Metadata: issues.Metadata{ID: "id", State: "In Progress"}}, nil
}
func (s *scopeStore) Edit(_ context.Context, _ string, raw []byte) (issues.Task, error) {
	var patch struct{ Description string }
	if err := json.Unmarshal(raw, &patch); err != nil {
		return issues.Task{}, err
	}
	s.face = patch.Description
	s.edits++
	return issues.Task{}, nil
}

func TestAutoScopeBeforeHumanCard(t *testing.T) {
	for _, tc := range []struct {
		name, flags, path, refusal string
		previous                   int
		approve                    bool
	}{
		{name: "enabled", flags: "scope", path: "src/new.go", approve: true},
		{name: "comma list", flags: "other, scope", path: "src/new.go", approve: true},
		{name: "second", flags: "scope", path: "src/new.go", previous: 1, approve: true},
		{name: "default", path: "src/new.go"},
		{name: "not a substring", flags: "scopes", path: "src/new.go"},
		{name: "third", flags: "scope", path: "src/new.go", previous: 2, refusal: "2 次"},
		{name: "too many", flags: "scope", path: "a b c d e f g h i j k", refusal: "10"},
		{name: "lane 3", flags: "scope", path: "billing/key", refusal: "lane 3"},
		{name: "traversal", flags: "scope", path: "src/../x", refusal: "字面路径"},
		{name: "github", flags: "scope", path: ".github/workflows/ci.yml", refusal: "控制文件"},
		{name: "core gate", flags: "scope", path: "core/gate.sh", refusal: "控制文件"},
		{name: "project gate", flags: "scope", path: "scripts/agent-gates/gate.project.sh", refusal: "控制文件"},
		{name: "env config", flags: "scope", path: "scripts/agent-gates/agent-graph.env", refusal: "控制文件"},
		{name: "deploy", flags: "scope", path: "driver/deploy/unit", refusal: "控制文件"},
		{name: "makefile", flags: "scope", path: "Makefile", refusal: "控制文件"},
		{name: "dotenv", flags: "scope", path: ".env.local", refusal: "控制文件"},
		{name: "secret", flags: "scope", path: "src/secrets.go", refusal: "控制文件"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			face := "## LANE 预判\n2\n\n## SCOPE\nsrc/old.go\n"
			store := &scopeStore{face: face}
			sender := &notifySender{store: &store.notifyStore}
			sender.start(t)
			result, _ := json.Marshal(map[string]any{"action": "escalate", "reason": "needs scope", "body": "proof", "class": "scope-extension", "scope": strings.Fields(tc.path)})
			record := resolver.Record{Version: 1, Attempt: 7, Phase: "human", Result: string(result)}
			raw, _ := json.Marshal(record)
			var bodies []string
			for i := 0; i < tc.previous; i++ {
				bodies = append(bodies, fmt.Sprintf("扩SCOPE：auto 2026-11-01T00:00:00Z\n路径：old%d.go\nresolver-state {\"version\":1,\"attempt\":%d,\"phase\":\"waiting\"}", i, i+1))
			}
			bodies = append(bodies, resolver.Prefix+string(raw))
			store.issueComments = evidenceComments(bodies...)
			repo := config.Repository{Path: "/repo", Env: map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_NOTIFY_CHAT": "oc_chat", "AGENT_AUTO_REVIEW": tc.flags, "AGENT_LANE3_GLOBS": "billing/*"}}
			runtime := &Runtime{Clients: map[string]issues.Store{repo.Path: store}, Repositories: map[string]config.Repository{repo.Path: repo}, Now: func() time.Time { return time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC) }}
			ticket := Ticket{ID: "id", Identifier: "OF-9", Repo: repo.Path, Owner: machine.OwnerHuman, NotifyReason: "needs scope", Resolution: &record}
			if err := runtime.NotifyFeishu(context.Background(), ticket); err != nil {
				t.Fatal(err)
			}
			if tc.approve {
				if len(sender.messages) != 0 || store.edits != 1 || store.face != "## LANE 预判\n2\n\n## SCOPE\nsrc/old.go src/new.go\n" {
					t.Fatalf("edits=%d face=%q cards=%q", store.edits, store.face, sender.messages)
				}
				last := store.issueComments[len(store.issueComments)-1].Body
				for _, want := range []string{"扩SCOPE：auto 2026-11-10T00:00:00Z", "条件：", "路径：src/new.go", `resolver-state {"version":1,"attempt":7,"phase":"waiting"}`} {
					if !strings.Contains(last, want) {
						t.Errorf("missing %q in %q", want, last)
					}
				}
			} else {
				if store.edits != 0 || store.face != face || len(sender.messages) != 1 {
					t.Fatalf("edits=%d face=%q cards=%q", store.edits, store.face, sender.messages)
				}
				if tc.refusal != "" && (!strings.Contains(sender.messages[0], "未自动批准") || !strings.Contains(sender.messages[0], tc.refusal)) {
					t.Fatalf("card=%q", sender.messages[0])
				}
			}
		})
	}
}

func TestAutoScopeSkipsNotifiedHumanTicket(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := &scopeStore{face: "## LANE 预判\n2\n\n## SCOPE\nsrc/old.go\n"}
	sender := &notifySender{store: &store.notifyStore}
	sender.start(t)
	result, _ := json.Marshal(map[string]any{"action": "escalate", "reason": "needs ruling", "body": "proof", "class": "brief-interpretation"})
	record := resolver.Record{Version: 1, Attempt: 7, Phase: "human", Result: string(result)}
	raw, _ := json.Marshal(record)
	store.issueComments = evidenceComments(resolver.Prefix + string(raw))
	repo := config.Repository{Path: "/repo", Env: map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_NOTIFY_CHAT": "oc_chat", "AGENT_AUTO_REVIEW": "scope"}}
	runtime := &Runtime{Clients: map[string]issues.Store{repo.Path: store}, Repositories: map[string]config.Repository{repo.Path: repo}}
	ticket := Ticket{ID: "id", Identifier: "OF-9", Repo: repo.Path, Owner: machine.OwnerHuman, NotifyReason: "needs ruling", Resolution: &record}
	if err := runtime.NotifyFeishu(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	ticket.NotifyEvents = map[string]bool{}
	for _, comment := range store.issueComments {
		if strings.HasPrefix(comment.Body, "通知: ") {
			ticket.NotifyEvents[comment.Body] = true
		}
	}
	reads := store.reads
	if err := runtime.NotifyFeishu(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if len(ticket.NotifyEvents) != 1 || store.reads != reads || len(sender.messages) != 1 {
		t.Fatalf("markers=%v reads %d -> %d cards=%d", ticket.NotifyEvents, reads, store.reads, len(sender.messages))
	}
}
