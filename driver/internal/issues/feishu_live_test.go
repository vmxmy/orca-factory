package issues

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
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

// liveCreateTracker counts each submitted row, including writes whose response
// or subsequent readback fails, so cleanup cannot accept an incomplete list.
type liveCreateTracker struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *liveCreateTracker) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/records/batch_create") {
		body, err := r.GetBody()
		if err != nil {
			return nil, err
		}
		var request struct {
			Records []json.RawMessage `json:"create_records"`
		}
		err = json.NewDecoder(body).Decode(&request)
		body.Close()
		if err != nil {
			return nil, err
		}
		parts := strings.Split(r.URL.Path, "/")
		c.mu.Lock()
		c.counts[parts[len(parts)-3]] += len(request.Records)
		c.mu.Unlock()
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (c *liveCreateTracker) count(table string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[table]
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
	creates := &liveCreateTracker{counts: map[string]int{}}
	direct, err := feishu.New(feishu.Options{HTTPClient: &http.Client{Transport: creates}, Endpoint: os.Getenv("AGENT_FEISHU_ENDPOINT"), AppID: env["AGENT_FEISHU_APP_ID"], AppSecret: env["AGENT_FEISHU_APP_SECRET"]})
	if err != nil {
		t.Fatalf("open direct client: %v", err)
	}

	c.client = direct
	stamp, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	brief := FeishuBrief{Todo: "TestFeishuLive 验收票，测试结束后删除。", Check: "true", Scope: "driver/internal/issues", Body: "## 现状\n真实环境测试 " + stamp}
	var task Task
	var record string
	var event string
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		var tickets []string
		if record != "" {
			tickets = append(tickets, record)
		}
		_, own, err := c.projects(cleanupCtx)
		if err != nil {
			t.Errorf("list 项目 for cleanup of 票据 IDs %v: %v", tickets, err)
		} else {
			filter := feishu.Filter{Logic: "and", Conditions: []feishu.Condition{{Field: "项目", Operator: "intersects", Value: []feishu.LinkRef{{ID: own}}}}}
			for {
				rows, err := c.readAll(cleanupCtx, "票据", &filter)
				if err != nil {
					t.Errorf("list 票据 for cleanup IDs %v: %v", tickets, err)
					break
				}
				for _, r := range rows {
					title, err := c.text("票据", r, "标题")
					if err != nil {
						t.Errorf("read 票据 title for cleanup ID %s: %v", r.ID, err)
						continue
					}
					if title == "TestFeishuLive "+stamp && !slices.Contains(tickets, r.ID) {
						tickets = append(tickets, r.ID)
					}
				}
				if len(tickets) >= creates.count(c.tables["票据"].id) {
					break
				}
				if err := sleepContext(cleanupCtx, liveSettleInterval); err != nil {
					t.Errorf("未找到本次创建的票行: 票据 title=%q IDs %v: %v", "TestFeishuLive "+stamp, tickets, err)
					break
				}
			}
		}
		var events []string
		if event != "" {
			events = append(events, event)
		}
		for {
			for _, id := range tickets {
				filter := feishu.Filter{Logic: "and", Conditions: []feishu.Condition{{Field: "票据", Operator: "intersects", Value: []feishu.LinkRef{{ID: id}}}}}
				rows, err := c.readAll(cleanupCtx, "事件", &filter)
				if err != nil {
					t.Errorf("list 事件 for 票据 ID %s, event IDs %v: %v", id, events, err)
				}
				for _, r := range rows {
					if !slices.Contains(events, r.ID) {
						events = append(events, r.ID)
					}
				}
			}
			if len(events) >= creates.count(c.tables["事件"].id) {
				break
			}
			if err := sleepContext(cleanupCtx, liveSettleInterval); err != nil {
				t.Errorf("find 事件 for 票据 IDs %v: found %v, submitted %d: %v", tickets, events, creates.count(c.tables["事件"].id), err)
				break
			}
		}
		if err := deleteLiveRecords(cleanupCtx, direct, c.base, c.tables["事件"].id, events); err != nil {
			t.Errorf("delete 事件 IDs %v for 票据 IDs %v: %v", events, tickets, err)
		}
		if err := deleteLiveRecords(cleanupCtx, direct, c.base, c.tables["票据"].id, tickets); err != nil {
			t.Errorf("delete 票据 IDs %v: %v", tickets, err)
		}
	})
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
				if slices.Contains(page.NotFound, id) {
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

func TestFeishuLiveCleanupOnWriteFailure(t *testing.T) {
	for _, scenario := range []string{"create readback failure", "same second other run", "claim conflict event rejected", "claim conflict event delayed"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFeishuFake(t)
			f.afterWrite = func(f *feishuFake) {
				w := f.writes[len(f.writes)-1]
				if (scenario == "create readback failure" || scenario == "same second other run") && w.table == "tblT" && w.action == "batch_create" {
					f.rows["tblT"][len(f.rows["tblT"])-1]["估算"] = "invalid"
					if scenario == "same second other run" {
						row := f.rows["tblT"][len(f.rows["tblT"])-1]
						otherTitle := "TestFeishuLive " + time.Now().UTC().Format("20060102T150405Z")
						if len(row["标题"].(string)) == len(otherTitle) {
							otherTitle = row["标题"].(string)
						}
						other := feishuRow{"_id": "recOtherRun", "标题": otherTitle, "项目": row["项目"]}
						f.rows["tblT"] = append(f.rows["tblT"], other)
					}
				}
				if strings.HasPrefix(scenario, "claim conflict event") && w.table == "tblT" && w.action == "batch_update" {
					f.rows["tblT"][len(f.rows["tblT"])-1]["状态"] = []string{"Done"}
				}
			}
			var deleted []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/batch_delete") {
					var body struct {
						IDs []string `json:"record_id_list"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					f.mu.Lock()
					defer f.mu.Unlock()
					deleted = append(deleted, body.IDs...)
					for table, rows := range f.rows {
						f.rows[table] = slices.DeleteFunc(rows, func(row feishuRow) bool { return slices.Contains(body.IDs, row["_id"].(string)) })
					}
					rec := loadFeishuRecording(t, "bot-tickets-delete.json")
					f.reply(w, rec.Response.Data)
					return
				}
				if strings.HasPrefix(scenario, "claim conflict event") && strings.HasSuffix(r.URL.Path, "/tblE/records/batch_create") {
					response := httptest.NewRecorder()
					f.ServeHTTP(response, r)
					if scenario == "claim conflict event delayed" {
						f.mu.Lock()
						f.stale = map[string][]feishuRow{"tblE": nil}
						f.staleReads = 1
						f.mu.Unlock()
					}
					// The event is stored, but checkWrite cannot confirm its ID.
					f.reply(w, map[string]any{"record_id_list": []string{}})
					return
				}
				f.ServeHTTP(w, r)
			}))
			defer server.Close()
			t.Setenv("AGENT_FEISHU_LIVE", "1")
			t.Setenv("AGENT_FEISHU_ENDPOINT", server.URL)
			t.Setenv("AGENT_FEISHU_APP_ID", "cli_test")
			t.Setenv("AGENT_FEISHU_APP_SECRET", "secret")
			t.Setenv("AGENT_FEISHU_BASE", "BASE_TOKEN")
			t.Setenv("AGENT_FEISHU_PROJECT", feishuRepo)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, executable, "-test.run=^TestFeishuLive$", "-test.v").CombinedOutput()
			failure := "feishu 估算 is not a number"
			want := []string{"recNew1"}
			if strings.HasPrefix(scenario, "claim conflict event") {
				failure = "feishu write returned an unexpected record count"
				want = []string{"recNew2", "recNew1"}
			}
			if err == nil || !strings.Contains(string(out), failure) || ctx.Err() != nil {
				t.Fatalf("live test did not reach injected failure: %v\n%s", err, out)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if !slices.Equal(deleted, want) {
				t.Fatalf("deleted %v, want %v\n%s", deleted, want, out)
			}
			remaining := 3
			if scenario == "same second other run" {
				remaining++
			}
			if len(f.rows["tblT"]) != remaining || len(f.rows["tblE"]) != 0 {
				t.Fatalf("remaining tickets=%v events=%v", f.rows["tblT"], f.rows["tblE"])
			}
		})
	}
}
