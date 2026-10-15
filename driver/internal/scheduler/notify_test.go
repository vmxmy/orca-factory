package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
)

type notifyStore struct {
	fakeLinear
	writeErr, readErr error
	reads, writes     int
}

func (f *notifyStore) ListComments(context.Context, string) ([]linear.Comment, error) {
	f.reads++
	return f.issueComments, f.readErr
}
func (f *notifyStore) CreateComment(_ context.Context, _ string, body string) (linear.Comment, error) {
	f.writes++
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
	raw, err := os.ReadFile("../../testdata/recorded/feishu/bot-message-send.json")
	if err != nil {
		t.Fatal(err)
	}
	var recording struct {
		Response struct {
			Data json.RawMessage `json:"data"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &recording); err != nil {
		t.Fatal(err)
	}
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
		if err := json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": recording.Response.Data}); err != nil {
			t.Error(err)
		}
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
			ticket := Ticket{ID: "id", Repo: repo.Path, Identifier: "OF-9", Title: "notify", URL: "https://example.feishu.cn/base/base?record=rec", Owner: machine.OwnerHuman, NotifyReason: "等待处置"}
			if category == "证据冲突" {
				ticket.MergeProblem = "conflicting handoff"
			}
			if category == "部署失败" {
				ticket.Progress = &merge.Progress{Deployment: &merge.DeploymentProgress{State: "paused", Reason: "workflow failed"}}
			}
			for range 2 {
				ticket.NotifyEvents = map[string]bool{}
				for _, event := range store.issueComments {
					ticket.NotifyEvents[event.Body] = true
				}
				r := &Runtime{Clients: map[string]LinearClient{repo.Path: store}, Repositories: map[string]config.Repository{repo.Path: repo}}
				if err := r.NotifyFeishu(ctx, ticket); err != nil {
					t.Fatal(err)
				}
			}
			if store.reads != 0 {
				t.Fatalf("notification reread comments %d times", store.reads)
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
	for _, scenario := range []string{"disabled", "send", "cached events", "write", "normal", "other backend"} {
		t.Run(scenario, func(t *testing.T) {
			store := &notifyStore{}
			sender := &notifySender{store: store}
			sender.start(t)
			repo := config.Repository{Path: "/repo", Env: map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_NOTIFY_CHAT": "oc_chat"}}
			ticket := Ticket{ID: "id", Repo: repo.Path, Owner: machine.OwnerHuman, NotifyReason: "等待处置"}
			switch scenario {
			case "disabled":
				delete(repo.Env, "AGENT_FEISHU_NOTIFY_CHAT")
			case "send":
				sender.err = errors.New("send failed")
			case "cached events":
				store.readErr = errors.New("must not read comments")
				ticket.NotifyEvents = map[string]bool{"通知: 转人 452f8313b94c": true}
			case "write":
				store.writeErr = errors.New("write failed")
			case "normal":
				ticket.Owner = machine.OwnerExecutor
			case "other backend":
				repo.Env["AGENT_ISSUE_BACKEND"] = "github"
			}
			r := &Runtime{Clients: map[string]LinearClient{repo.Path: store}, Repositories: map[string]config.Repository{repo.Path: repo}}
			err := r.NotifyFeishu(ctx, ticket)
			wantErr := scenario == "send" || scenario == "write"
			if (err != nil) != wantErr {
				t.Fatalf("err=%v", err)
			}
			if store.reads != 0 {
				t.Fatalf("notification read comments %d times", store.reads)
			}
			if scenario == "send" {
				ticket.NotifyEvents = map[string]bool{store.issueComments[0].Body: true}
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
	f := &failingNotifyBackend{fakeBackend: fakeBackend{tickets: []Ticket{{ID: "ready", Repo: "a", Candidate: true}, {ID: "human", Repo: "a", Owner: machine.OwnerHuman}}}}
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

type notifyTickBackend struct {
	fakeBackend
	runtime  *Runtime
	snapshot func() ([]Ticket, error)
}

func (f *notifyTickBackend) Snapshot(context.Context, []config.Repository) ([]Ticket, error) {
	f.snapshots++
	return f.snapshot()
}
func (f *notifyTickBackend) NotifyFeishu(ctx context.Context, ticket Ticket) error {
	return f.runtime.NotifyFeishu(ctx, ticket)
}
func TestFeishuNotifyPausedSkips(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, timing := range []string{"before snapshot", "after snapshot", "failed snapshot"} {
		t.Run(timing, func(t *testing.T) {
			store := &notifyStore{}
			sender := &notifySender{store: store}
			sender.start(t)
			repo := config.Repository{Path: "a", MaxInFlight: 1, Env: map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_NOTIFY_CHAT": "oc_chat"}}
			f := &notifyTickBackend{runtime: &Runtime{Clients: map[string]LinearClient{repo.Path: store}, Repositories: map[string]config.Repository{repo.Path: repo}}}
			s := setup(&f.fakeBackend)
			s.Backend = f
			s.Config.Repositories = []config.Repository{repo}
			low := []linear.RateLimit{{Limit: 100, Remaining: 1, Reset: s.Now().Add(time.Hour)}}
			if timing == "before snapshot" {
				f.limits = low
			}
			f.snapshot = func() ([]Ticket, error) {
				f.limits = low
				if timing == "failed snapshot" {
					return nil, &TicketEvidenceError{Ticket: Ticket{ID: "id", Repo: repo.Path}, Err: errors.New("invalid ownership comment")}
				}
				return []Ticket{{ID: "id", Repo: repo.Path, Owner: machine.OwnerHuman}}, nil
			}
			err := s.Tick(context.Background())
			if timing == "failed snapshot" {
				if err == nil || err.Error() != "scheduler snapshot failed" {
					t.Fatalf("err=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if store.reads != 0 || store.writes != 0 || len(sender.messages) != 0 {
				t.Fatalf("paused notification reads=%d writes=%d messages=%v", store.reads, store.writes, sender.messages)
			}
		})
	}
}

func TestFeishuNotifySnapshotConflict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, body := range []string{"归属: unknown", resolver.Prefix + "{}"} {
		t.Run(body, func(t *testing.T) {
			store := &notifyStore{fakeLinear: fakeLinear{issueComments: []linear.Comment{{ID: "bad", Body: body}}, issues: []linear.Issue{{ID: "id", Identifier: "OF-9", Title: "conflicting ticket", URL: "https://example.feishu.cn/base/base?record=rec", Project: linear.NamedID{ID: "project"}}}}}
			sender := &notifySender{store: store}
			sender.start(t)
			repo := config.Repository{Path: "a", MaxInFlight: 1, Env: map[string]string{"AGENT_ISSUE_BACKEND": "feishu", "AGENT_FEISHU_NOTIFY_CHAT": "oc_chat", "AGENT_FEISHU_PROJECT": "project", "AGENT_SCOPE_AVOID": "0"}}

			r := &Runtime{Clients: map[string]LinearClient{repo.Path: store}, Repositories: map[string]config.Repository{repo.Path: repo}}
			rows, err := r.Snapshot(context.Background(), []config.Repository{repo})
			var evidence *TicketEvidenceError
			if !errors.As(err, &evidence) || len(rows) != 0 {
				t.Fatalf("snapshot rows=%v err=%v", rows, err)
			}
			if evidence.Ticket.ID != "id" || evidence.Ticket.Identifier != "OF-9" || evidence.Ticket.Repo != repo.Path || evidence.Ticket.Title != "conflicting ticket" || evidence.Ticket.URL != store.issues[0].URL || evidence.Err == nil {
				t.Fatalf("evidence=%+v", evidence)
			}
			for round := 0; round < 2; round++ {
				f := &notifyTickBackend{runtime: r}
				f.snapshot = func() ([]Ticket, error) { return r.Snapshot(context.Background(), []config.Repository{repo}) }
				s := setup(&f.fakeBackend)
				s.Config.Repositories = []config.Repository{repo}
				s.Backend = f
				before := len(sender.messages)
				if err := s.Tick(context.Background()); err == nil || err.Error() != "scheduler snapshot failed" {
					t.Fatalf("tick error=%v", err)
				}
				want := 1
				if round == 1 {
					want = 0
				}
				if len(sender.messages)-before != want || len(f.actions) != 0 || len(f.launched) != 0 {
					t.Fatalf("round=%d messages=%v actions=%v launches=%v", round, sender.messages, f.actions, f.launched)
				}
			}
			for _, part := range []string{"OF-9 conflicting ticket", "类别: 证据冲突", evidence.Err.Error(), store.issues[0].URL} {
				if !strings.Contains(sender.messages[0], part) {
					t.Errorf("message missing %q: %s", part, sender.messages[0])
				}
			}
			// Every Snapshot reads once; notifications must not reread the history.
			if store.reads != 3 || store.writes != 1 {
				t.Fatalf("reads=%d writes=%d", store.reads, store.writes)
			}
		})
	}
}

func TestFeishuNotifySnapshotReason(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		comments []linear.Comment
		want     string
	}{
		{"latest ID", []linear.Comment{{ID: "z", CreatedAt: now, Body: "Agent处置: latest reason\n证据: proof"}, {ID: "a", CreatedAt: now, Body: "转人: older reason"}, {ID: "new", CreatedAt: now.Add(time.Minute), Body: "通知: 转人 existing"}}, "latest reason"},
		{"latest time", []linear.Comment{{ID: "z", CreatedAt: now, Body: "转人: old"}, {ID: "a", CreatedAt: now.Add(time.Minute), Body: "Agent处置: new"}}, "new"},
		{"default", nil, "等待处置"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &notifyStore{fakeLinear: fakeLinear{issueComments: tc.comments, issues: []linear.Issue{{ID: "id", Project: linear.NamedID{ID: "p"}, Labels: linear.Connection[linear.NamedID]{Nodes: []linear.NamedID{{Name: "auto-failed"}}}}}}}
			repo := config.Repository{Path: "a", Env: map[string]string{"AGENT_LINEAR_PROJECT": "p", "AGENT_SCOPE_AVOID": "0"}}
			r := &Runtime{Clients: map[string]LinearClient{repo.Path: store}}
			tickets, err := r.Snapshot(context.Background(), []config.Repository{repo})
			if err != nil || len(tickets) != 1 {
				t.Fatalf("tickets=%v err=%v", tickets, err)
			}
			if tickets[0].NotifyReason != tc.want || tickets[0].NotifyEvents["通知: 转人 existing"] != (tc.name == "latest ID") {
				t.Fatalf("ticket=%+v", tickets[0])
			}
			if store.reads != 1 {
				t.Fatalf("snapshot reads=%d", store.reads)
			}
		})
	}
}

func TestFeishuNotifyDeploymentReason(t *testing.T) {
	ticket := Ticket{NotifyReason: "handoff reason", Progress: &merge.Progress{Deployment: &merge.DeploymentProgress{State: "paused"}}}
	category, reason := feishuNotification(ticket)
	if category != "部署失败" || reason != "handoff reason" {
		t.Fatalf("category=%q reason=%q", category, reason)
	}
	readFeishuNotification(&ticket, nil)
	if ticket.NotifyReason != "等待处置" {
		t.Fatalf("default reason=%q", ticket.NotifyReason)
	}
	ticket.Progress.Reason = "progress reason"
	readFeishuNotification(&ticket, nil)
	if ticket.NotifyReason != "progress reason" {
		t.Fatalf("progress reason=%q", ticket.NotifyReason)
	}
}
