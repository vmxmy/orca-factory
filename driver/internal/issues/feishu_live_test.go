package issues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
)

const (
	liveSettleTimeout  = 90 * time.Second
	liveSettleInterval = 3 * time.Second
)

// eventually polls check until it holds; base/v3 reads lag writes (spec R2).
func eventually(ctx context.Context, t *testing.T, what string, check func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(liveSettleTimeout)
	var last error
	for {
		ok, err := check()
		if ok {
			return
		}
		last = err
		if time.Now().After(deadline) {
			t.Fatalf("%s not observed within %s (last error: %v)", what, liveSettleTimeout, last)
		}
		if err := sleepContext(ctx, liveSettleInterval); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
}

func claimable(i linear.Issue) bool {
	ready := slices.ContainsFunc(i.Labels.Nodes, func(l linear.NamedID) bool { return l.Name == "ready-for-agent" })
	return i.State.Name == "Todo" && ready && !i.Held && !i.Blocked()
}

func liveSnapshotIssue(ctx context.Context, c *Feishu, id string) (linear.Issue, bool, error) {
	rows, err := c.Snapshot(ctx, []string{c.Repository()}, time.Time{})
	if err != nil {
		return linear.Issue{}, false, err
	}
	i := slices.IndexFunc(rows, func(r linear.Issue) bool { return r.ID == id })
	if i < 0 {
		return linear.Issue{}, false, nil
	}
	return rows[i], true, nil
}

// TestFeishuLive runs the ledger flow against a real test Base as the app
// identity. It deletes the ticket and its events at the end of each run.
func TestFeishuLive(t *testing.T) {
	if os.Getenv("AGENT_FEISHU_LIVE") != "1" {
		t.Skip("AGENT_FEISHU_LIVE=1 not set")
	}
	env := map[string]string{"AGENT_ISSUE_BACKEND": "feishu"}
	for _, k := range []string{"AGENT_FEISHU_BASE", "AGENT_FEISHU_PROJECT", "AGENT_FEISHU_APP_ID", "AGENT_FEISHU_APP_SECRET"} {
		if os.Getenv(k) == "" {
			t.Skipf("%s not set", k)
		}
		env[k] = os.Getenv(k)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	client, err := New(ctx, env, config.Tuning{})
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	c := client.(*Feishu)
	direct, err := feishu.New(feishu.Options{Endpoint: os.Getenv("AGENT_FEISHU_ENDPOINT"), AppID: env["AGENT_FEISHU_APP_ID"], AppSecret: env["AGENT_FEISHU_APP_SECRET"]})
	if err != nil {
		t.Fatalf("open direct client: %v", err)
	}

	stamp := time.Now().UTC().Format("20060102T150405Z")
	brief := FeishuBrief{Todo: "TestFeishuLive 验收票，测试结束后删除。", Check: "true", Scope: "driver/internal/issues", Body: "## 现状\n真实环境测试 " + stamp}
	var task Task
	var record string
	var event string
	ok := t.Run("create adopted", func(t *testing.T) {
		task, err = c.Create(ctx, CreateInput{Title: "TestFeishuLive " + stamp, Description: brief.Description(), Labels: []string{"ready-for-agent"}, Blockers: []string{}, Related: []string{}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		record = task.nativeID
		if !task.Metadata.Complete || task.Held {
			t.Fatalf("created task not adopted: complete=%t held=%t", task.Metadata.Complete, task.Held)
		}
	})
	if record != "" {
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cleanupCancel()
			if event != "" {
				if err := deleteLiveRecords(cleanupCtx, direct, c.base, c.tables["事件"].id, []string{event}); err != nil {
					t.Errorf("delete event for %s: %v", task.Identifier, err)
				}
			}
			if err := deleteLiveRecords(cleanupCtx, direct, c.base, c.tables["票据"].id, []string{record}); err != nil {
				t.Errorf("delete ticket %s: %v", task.Identifier, err)
			}
		})
	}
	if !ok {
		return
	}
	t.Logf("ticket %s (%s)", task.Identifier, task.ID)

	steps := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"snapshot claimable", func(t *testing.T) {
			eventually(ctx, t, "claimable ticket in snapshot", func() (bool, error) {
				issue, found, err := liveSnapshotIssue(ctx, c, task.ID)
				return found && claimable(issue), err
			})
		}},
		{"claim reads back", func(t *testing.T) {
			issue, err := c.UpdateIssue(ctx, task.ID, "In Progress", []string{"auto-impl"}, nil)
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			got, err := c.Read(ctx, task.ID)
			if err != nil || got.State.Name != "In Progress" || !slices.ContainsFunc(got.Labels.Nodes, func(l linear.NamedID) bool { return l.Name == "auto-impl" }) {
				t.Fatalf("claim not read back: state=%s labels=%v err=%v", got.State.Name, got.Labels.Nodes, err)
			}
			if issue.State.Name != "In Progress" {
				t.Fatalf("claim returned state %s", issue.State.Name)
			}
		}},
	}
	for _, s := range steps {
		if !t.Run(s.name, s.run) {
			return
		}
	}

	body := "live-check-v1: {\"run\":\"" + stamp + "\"}\n多字节正文  \n末行无换行"
	if !t.Run("event round trip", func(t *testing.T) {
		comment, err := c.CreateComment(ctx, task.ID, body)
		if err != nil {
			t.Fatalf("create event: %v", err)
		}
		event = comment.ID
		eventually(ctx, t, "event "+event+" listed byte for byte", func() (bool, error) {
			comments, err := c.ListComments(ctx, task.ID)
			i := slices.IndexFunc(comments, func(x linear.Comment) bool { return x.ID == event })
			if i >= 0 && comments[i].Body != body {
				t.Fatalf("event body %q, want %q", comments[i].Body, body)
			}
			return i >= 0, err
		})
	}) {
		return
	}

	if !t.Run("scope edit holds ticket", func(t *testing.T) {
		res, err := direct.BatchUpdate(ctx, c.base, c.tables["票据"].id, map[string]map[string]any{record: {c.tables["票据"].fields["SCOPE"]: brief.Scope + "\ndocs/"}})
		if err != nil || len(res.RecordIDs) != 1 {
			t.Fatalf("direct SCOPE edit: %v", err)
		}
		eventually(ctx, t, "held ticket", func() (bool, error) {
			issue, found, err := liveSnapshotIssue(ctx, c, task.ID)
			return found && issue.Held && !claimable(issue), err
		})
	}) {
		return
	}

	t.Run("event edit is evidence conflict", func(t *testing.T) {
		res, err := direct.BatchUpdate(ctx, c.base, c.tables["事件"].id, map[string]map[string]any{event: {c.tables["事件"].fields["正文"]: body + "\ntampered"}})
		if err != nil || len(res.RecordIDs) != 1 {
			t.Fatalf("direct event edit: %v", err)
		}
		eventually(ctx, t, "evidence conflict", func() (bool, error) {
			_, err := c.ListComments(ctx, task.ID)
			return err != nil && strings.Contains(err.Error(), "evidence conflict"), nil
		})
	})
}

func deleteLiveRecords(ctx context.Context, c *feishu.Client, base, table string, ids []string) error {
	for len(ids) > 0 {
		res, err := c.BatchDelete(ctx, base, table, ids)
		if err != nil {
			return fmt.Errorf("delete %s records %v: %w", table, ids, err)
		}
		if len(res.NotFound) == 0 {
			return nil
		}
		ids = res.NotFound
		for {
			page, err := c.BatchGet(ctx, base, table, ids)
			if err != nil {
				return fmt.Errorf("read %s records %v before deletion: %w", table, ids, err)
			}
			visible := true
			for _, id := range ids {
				if !slices.ContainsFunc(page.Records, func(r feishu.Record) bool { return r.ID == id }) {
					visible = false
					break
				}
			}
			if visible {
				break
			}
			if err := sleepContext(ctx, liveSettleInterval); err != nil {
				return fmt.Errorf("delete %s records %v: %w", table, ids, err)
			}
		}
	}
	return nil
}

func TestDeleteLiveRecordsWaitsForVisibility(t *testing.T) {
	ids := []string{"recA", "recB"}
	var deletes, reads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			fmt.Fprint(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
			return
		}
		var body struct {
			IDs []string `json:"record_id_list"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !slices.Equal(body.IDs, ids) {
			t.Errorf("request IDs=%v, want %v, err=%v", body.IDs, ids, err)
		}
		data := map[string]any{"record_id_list": ids}
		switch r.URL.Path {
		case "/open-apis/base/v3/bases/base/tables/tickets/records/batch_delete":
			if deletes.Add(1) == 1 {
				data["record_not_found"] = ids
			} else if reads.Load() < 2 {
				t.Errorf("retried deletion after %d reads before both IDs were visible", reads.Load())
			}
		case "/open-apis/base/v3/bases/base/tables/tickets/records/batch_get":
			visible := ids
			if reads.Add(1) == 1 {
				visible = ids[:1]
			}
			rows := make([][]any, len(visible))
			for i := range rows {
				rows[i] = []any{}
			}
			data = map[string]any{"record_id_list": visible, "data": rows}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c, err := feishu.New(feishu.Options{Endpoint: server.URL, AppID: "app", AppSecret: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := deleteLiveRecords(ctx, c, "base", "tickets", ids); err != nil {
		t.Fatal(err)
	}
	if deletes.Load() != 2 || reads.Load() != 2 {
		t.Errorf("delete calls=%d read calls=%d, want 2 of each", deletes.Load(), reads.Load())
	}
}

func TestDeleteLiveRecordsTimeoutIdentifiesRemainingIDs(t *testing.T) {
	c, err := feishu.New(feishu.Options{AppID: "app", AppSecret: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err = deleteLiveRecords(ctx, c, "base", "tickets", []string{"recA"})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "tickets") || !strings.Contains(err.Error(), "recA") {
		t.Fatalf("error=%v, want deadline with table tickets and remaining ID recA", err)
	}
}
