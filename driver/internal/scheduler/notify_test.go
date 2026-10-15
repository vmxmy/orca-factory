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
func (f *notifySender) start(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"test-token","expire":7200}`))
			return
		}
		var body struct {
			ReceiveID string `json:"receive_id"`
			Content   string `json:"content"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var content struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(body.Content), &content); err != nil {
			t.Error(err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := f.SendText(req.Context(), body.ReceiveID, content.Text); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	t.Cleanup(server.Close)
	t.Setenv("AGENT_FEISHU_ENDPOINT", server.URL)
	t.Setenv("AGENT_FEISHU_APP_ID", "cli_test")
	t.Setenv("AGENT_FEISHU_APP_SECRET", "secret")
}

func TestFeishuNotifyDurable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	for _, category := range []string{"转人", "证据冲突", "部署失败"} {
		t.Run(category, func(t *testing.T) {
			store := &notifyStore{}
			sender := &notifySender{store: store}
			sender.start(t)
			repo := config.Repository{Path: "/repo", Env: map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_NOTIFY_CHAT": "oc_chat"}}
			ticket := Ticket{ID: "id", Repo: repo.Path, Identifier: "OF-9", Title: "notify", URL: "https://example.feishu.cn/base/base?record=rec", Owner: machine.OwnerHuman}
			if category == "证据冲突" {
				ticket.MergeProblem = "conflicting handoff"
			}
			if category == "部署失败" {
				ticket.Progress = &merge.Progress{Deployment: &merge.DeploymentProgress{State: "paused", Reason: "workflow failed"}}
			}
			for range 2 {
				r := &Runtime{Clients: map[string]LinearClient{repo.Path: store}, Repositories: map[string]config.Repository{repo.Path: repo}}
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
			sender.start(t)
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
			r := &Runtime{Clients: map[string]LinearClient{repo.Path: store}, Repositories: map[string]config.Repository{repo.Path: repo}}
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
