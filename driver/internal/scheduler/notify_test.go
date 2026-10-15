package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

type notifyStore struct {
	fakeLinear
	writeErr, readErr error
}

func (f *notifyStore) ListComments(context.Context, string) ([]linear.Comment, error) {
	return f.issueComments, f.readErr
}
func (f *notifyStore) CreateComment(_ context.Context, _ string, body string) (linear.Comment, error) {
	if f.writeErr != nil {
		return linear.Comment{}, f.writeErr
	}
	c := linear.Comment{ID: "event", Body: body}
	f.issueComments = append(f.issueComments, c)
	return c, nil
}

type notifySender struct {
	store    *notifyStore
	messages []string
	err      error
}

func (f *notifySender) SendText(_ context.Context, chat, text string) error {
	if len(f.store.issueComments) == 0 {
		return errors.New("send before event")
	}
	f.messages = append(f.messages, chat+"\n"+text)
	return f.err
}
func TestFeishuNotifyDurable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	for _, category := range []string{"转人", "证据冲突", "部署失败"} {
		t.Run(category, func(t *testing.T) {
			store := &notifyStore{}
			sender := &notifySender{store: store}
			repo := config.Repository{Path: "/repo", Env: map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_NOTIFY_CHAT": "oc_chat"}}
			ticket := Ticket{ID: "id", Repo: repo.Path, Identifier: "OF-9", Title: "notify", URL: "https://example.feishu.cn/base/base?record=rec", Owner: machine.OwnerHuman}
			if category == "证据冲突" {
				ticket.MergeProblem = "conflicting handoff"
			}
			if category == "部署失败" {
				ticket.Progress = &merge.Progress{Deployment: &merge.DeploymentProgress{State: "paused", Reason: "workflow failed"}}
			}
			for range 2 {
				r := &Runtime{Clients: map[string]LinearClient{repo.Path: store}, Repositories: map[string]config.Repository{repo.Path: repo}, notifyClient: sender}
				if err := r.NotifyFeishu(ctx, ticket); err != nil {
					t.Fatal(err)
				}
			}
			if len(sender.messages) != 1 || len(store.issueComments) != 1 {
				t.Fatalf("messages=%v events=%v", sender.messages, store.issueComments)
			}
			for _, text := range []string{"oc_chat", "OF-9", "notify", category, ticket.URL} {
				if !strings.Contains(sender.messages[0], text) {
					t.Errorf("missing %q in %q", text, sender.messages[0])
				}
			}
			wantHash := map[string]string{"转人": "452f8313b94c", "证据冲突": "2e895fe4c483", "部署失败": "c11573f64f76"}[category]
			if store.issueComments[0].Body != "通知: "+category+" "+wantHash {
				t.Fatalf("marker=%s", store.issueComments[0].Body)
			}
		})
	}
}

func TestFeishuNotifyFailureAndDisabled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENT_FEISHU_NOTIFY_CHAT", "")
	ctx := context.Background()
	for _, scenario := range []string{"disabled", "send", "read", "write", "normal", "other backend"} {
		t.Run(scenario, func(t *testing.T) {
			store := &notifyStore{}
			sender := &notifySender{store: store}
			repo := config.Repository{Path: "/repo", Env: map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_NOTIFY_CHAT": "oc_chat"}}
			ticket := Ticket{ID: "id", Repo: repo.Path, Owner: machine.OwnerHuman}
			switch scenario {
			case "disabled":
				delete(repo.Env, "AGENT_FEISHU_NOTIFY_CHAT")
			case "send":
				sender.err = errors.New("send failed")
			case "read":
				store.readErr = errors.New("read failed")
			case "write":
				store.writeErr = errors.New("write failed")
			case "normal":
				ticket.Owner = machine.OwnerExecutor
			case "other backend":
				repo.Env["AGENT_ISSUE_BACKEND"] = "github"
			}
			r := &Runtime{Clients: map[string]LinearClient{repo.Path: store}, Repositories: map[string]config.Repository{repo.Path: repo}, notifyClient: sender}
			err := r.NotifyFeishu(ctx, ticket)
			wantErr := scenario == "send" || scenario == "read" || scenario == "write"
			if (err != nil) != wantErr {
				t.Fatalf("err=%v", err)
			}
			if scenario == "send" {
				if err := r.NotifyFeishu(ctx, ticket); err != nil {
					t.Fatal(err)
				}
				if len(sender.messages) != 1 {
					t.Fatalf("retried send: %v", sender.messages)
				}
			} else if len(sender.messages) != 0 || len(store.issueComments) != 0 {
				t.Fatalf("unexpected sends=%v events=%v", sender.messages, store.issueComments)
			}
		})
	}
}

func TestFeishuNotifyEditedEvidence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	base := newFeishuClaimBase(t, true)
	base.rows["tblE"] = []map[string]any{{"_id": "recEdited", "票据": []feishu.LinkRef{{ID: "recA"}}, "正文": "edited", "创建时间": "2026-10-15T00:10:00.000+08:00", "修改时间": "2026-10-15T00:11:00.000+08:00"}}
	sends := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/open-apis/im/v1/messages" {
			sends++
			if base.events != 1 {
				t.Errorf("sent before event: %d", base.events)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
			return
		}
		base.ServeHTTP(w, req)
	}))
	defer server.Close()
	t.Setenv("AGENT_FEISHU_ENDPOINT", server.URL)
	t.Setenv("AGENT_FEISHU_APP_ID", "cli_test")
	t.Setenv("AGENT_FEISHU_APP_SECRET", "secret")
	client, err := feishu.New(feishu.Options{Endpoint: server.URL, AppID: "cli_test", AppSecret: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := issues.NewFeishu(ctx, client, "BASE_TOKEN", "https://example.feishu.cn/base/BASE_TOKEN", feishuClaimRepo, issues.FeishuOptions{})
	if err != nil {
		t.Fatal(err)
	}
	repo := config.Repository{Path: "/repo", Env: map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_BASE": "BASE_TOKEN", "AGENT_FEISHU_PROJECT": feishuClaimRepo, "AGENT_FEISHU_NOTIFY_CHAT": "oc_chat"}}
	for round := range 2 {
		r := &Runtime{Clients: map[string]LinearClient{repo.Path: store}, Repositories: map[string]config.Repository{repo.Path: repo}, Orca: &fakeOrca{automations: receipt(`{"automations":[]}`)}}
		if _, err := r.Snapshot(ctx, []config.Repository{repo}); err == nil || !strings.Contains(err.Error(), "evidence conflict") {
			t.Fatalf("snapshot must still refuse: %v", err)
		}
		if sends != round {
			t.Fatal("snapshot sent a notification")
		}
		s := setup(nil)
		s.Config.Repositories = []config.Repository{repo}
		s.Backend = r
		if err := s.Tick(ctx); err == nil {
			t.Fatal("tick accepted conflicting evidence")
		}

	}
	if sends != 1 || base.events != 1 {
		t.Fatalf("sends=%d events=%d", sends, base.events)
	}
}

type failingNotifyBackend struct {
	fakeBackend
	notified int
}

func (f *failingNotifyBackend) NotifyFeishu(context.Context, Ticket) error {
	f.notified++
	return errors.New("send failed")
}
func TestFeishuNotifyWarnDoesNotBlock(t *testing.T) {
	f := &failingNotifyBackend{fakeBackend: fakeBackend{tickets: []Ticket{{ID: "ready", Repo: "a", Candidate: true}}}}
	s := setup(&f.fakeBackend)
	s.Backend = f
	var log bytes.Buffer
	s.Logger = slog.New(slog.NewTextHandler(&log, nil))
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.notified != 1 || len(f.launched) != 1 || !strings.Contains(log.String(), "level=WARN") || !strings.Contains(log.String(), "Feishu notification failed") {
		t.Fatalf("notified=%d launched=%v log=%s", f.notified, f.launched, log.String())
	}
}
