package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
)

type request struct {
	Query     string
	Variables map[string]any
}

func fixture(t *testing.T, handle func(http.ResponseWriter, request)) (*Client, *int) {
	t.Helper()
	t.Setenv("LINEAR_API_KEY", "test-private-key")
	calls := new(int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if r.Header.Get("Authorization") != "test-private-key" {
			t.Error("authorization missing")
		}
		if r.Method != "POST" {
			t.Error("expected POST")
		}
		var q request
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
		}
		w.Header().Set("X-Complexity", "7")
		w.Header().Set("X-RateLimit-Requests-Limit", "2500")
		w.Header().Set("X-RateLimit-Requests-Remaining", "2490")
		w.Header().Set("X-RateLimit-Requests-Reset", "1800000000000")
		w.Header().Set("X-RateLimit-Complexity-Limit", "3000000")
		w.Header().Set("X-RateLimit-Complexity-Remaining", "2999990")
		w.Header().Set("X-RateLimit-Complexity-Reset", "1800000000001")
		if strings.Contains(q.Query, "Metadata") {
			if q.Variables["team"] != "team-id" {
				t.Error("missing team scope")
			}
			field := "states"
			nodes := `[{"id":"s1","name":"Done"}]`
			if strings.Contains(q.Query, "labels(") {
				field = "labels"
				nodes = `[{"id":"l1","name":"bug"},{"id":"l2","name":"old"}]`
			}
			if strings.Contains(q.Query, "projects(") {
				field = "projects"
				nodes = `[{"id":"p1","name":"Alpha"},{"id":"p2","name":"Beta"}]`
			}
			if _, err := fmt.Fprintf(w, `{"data":{"team":{"%s":{"nodes":%s,"pageInfo":{"hasNextPage":false}}}}}`, field, nodes); err != nil {
				t.Error(err)
				return
			}
			return
		}
		handle(w, q)
	}))
	t.Cleanup(server.Close)
	c, err := New(context.Background(), Options{TeamID: "team-id", BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return c, calls
}
func TestMissingKey(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "")
	_, err := New(context.Background(), Options{TeamID: "team-id"})
	if err == nil || !strings.Contains(err.Error(), "LINEAR_API_KEY") {
		t.Fatalf("missing key error: %v", err)
	}
}
func TestSnapshot(t *testing.T) {
	for _, since := range []time.Time{{}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)} {
		t.Run(fmt.Sprint(since.IsZero()), func(t *testing.T) {
			pages := 0
			c, calls := fixture(t, func(w http.ResponseWriter, q request) {
				pages++
				filter := q.Variables["filter"].(map[string]any)
				if !reflect.DeepEqual(filter["project"], map[string]any{"id": map[string]any{"in": []any{"p1", "p2"}}}) {
					t.Errorf("project filter: %v", filter)
				}
				if since.IsZero() {
					if _, ok := filter["updatedAt"]; ok {
						t.Error("zero time filtered")
					}
				} else if !reflect.DeepEqual(filter["updatedAt"], map[string]any{"gt": "2026-01-02T03:04:05Z"}) {
					t.Errorf("time filter: %v", filter)
				}
				if pages == 1 {
					if _, err := fmt.Fprint(w, `{"data":{"issues":{"nodes":[{"id":"i1","identifier":"ENG-1","updatedAt":"2026-01-03T00:00:00Z","state":{"id":"s1","name":"Done"},"labels":{"nodes":[{"id":"l1","name":"bug"}]},"relations":{"nodes":[{"id":"r1","type":"blocks","relatedIssue":{"id":"i2","identifier":"ENG-2"}}]}}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}`); err != nil {
						t.Error(err)
						return
					}
				} else {
					if q.Variables["after"] != "next" {
						t.Error("missing cursor")
					}
					if _, err := fmt.Fprint(w, `{"data":{"issues":{"nodes":[{"id":"i2","identifier":"ENG-2"}],"pageInfo":{"hasNextPage":false}}}}`); err != nil {
						t.Error(err)
						return
					}
				}
			})
			issues, err := c.Snapshot(context.Background(), []string{"Alpha", "Beta"}, since)
			if err != nil {
				t.Fatal(err)
			}
			if len(issues) != 2 || issues[0].Identifier != "ENG-1" || issues[0].State.Name != "Done" || issues[0].Labels.Nodes[0].Name != "bug" || issues[0].Relations.Nodes[0].RelatedIssue.Identifier != "ENG-2" || issues[0].UpdatedAt.Format(time.RFC3339) != "2026-01-03T00:00:00Z" {
				t.Fatalf("issues: %+v", issues)
			}
			if *calls != 5 {
				t.Errorf("calls=%d", *calls)
			}
		})
	}
}
func TestUpdateAndUsage(t *testing.T) {
	c, calls := fixture(t, func(w http.ResponseWriter, q request) {
		if strings.Contains(q.Query, "IssueLabels") {
			if _, err := fmt.Fprint(w, `{"data":{"issue":{"labels":{"nodes":[{"id":"l2"}],"pageInfo":{"hasNextPage":false}}}}}`); err != nil {
				t.Error(err)
				return
			}
			return
		}
		want := map[string]any{"stateId": "s1", "addedLabelIds": []any{"l1"}, "removedLabelIds": []any{"l2"}}
		if !strings.Contains(q.Query, "issueUpdate") || q.Variables["id"] != "i1" || !reflect.DeepEqual(q.Variables["input"], want) {
			t.Errorf("mutation: %+v", q)
		}
		if _, err := fmt.Fprint(w, `{"data":{"issueUpdate":{"success":true,"issue":{"id":"i1","identifier":"ENG-1","state":{"id":"s1","name":"Done"}}}}}`); err != nil {
			t.Error(err)
			return
		}
	})
	issue, err := c.UpdateIssue(context.Background(), "i1", "Done", []string{"bug"}, []string{"old"})
	if err != nil || issue.Identifier != "ENG-1" {
		t.Fatalf("update: %+v %v", issue, err)
	}
	if *calls != 5 {
		t.Errorf("update must read labels once and mutate once, total=%d", *calls)
	}
	u := c.Usage()
	if u.Requests != 5 || u.Complexity != 35 || u.RequestLimit.Limit != 2500 || u.RequestLimit.Remaining != 2490 || u.RequestLimit.Reset.UnixMilli() != 1800000000000 || u.ComplexityLimit.Limit != 3000000 || u.ComplexityLimit.Remaining != 2999990 || u.ComplexityLimit.Reset.UnixMilli() != 1800000000001 {
		t.Fatalf("usage: %+v", u)
	}
	_, err = c.UpdateIssue(context.Background(), "i1", "unknown", nil, nil)
	if err == nil || *calls != 5 {
		t.Error("unknown name must fail locally")
	}
}
func TestComments(t *testing.T) {
	pages := 0
	c, calls := fixture(t, func(w http.ResponseWriter, q request) {
		if strings.Contains(q.Query, "commentCreate") {
			if !reflect.DeepEqual(q.Variables["input"], map[string]any{"issueId": "i1", "body": "hello"}) {
				t.Error("comment input")
			}
			if _, err := fmt.Fprint(w, `{"data":{"commentCreate":{"success":true,"comment":{"id":"c1","body":"hello"}}}}`); err != nil {
				t.Error(err)
				return
			}
			return
		}
		pages++
		if q.Variables["id"] != "i1" {
			t.Error("issue id")
		}
		if pages == 1 {
			if _, err := fmt.Fprint(w, `{"data":{"issue":{"comments":{"nodes":[{"id":"c1","body":"hello"}],"pageInfo":{"hasNextPage":true,"endCursor":"c-next"}}}}}`); err != nil {
				t.Error(err)
				return
			}
		} else {
			if q.Variables["after"] != "c-next" {
				t.Error("comments cursor")
			}
			if _, err := fmt.Fprint(w, `{"data":{"issue":{"comments":{"nodes":[{"id":"c2","body":"world"}],"pageInfo":{"hasNextPage":false}}}}}`); err != nil {
				t.Error(err)
				return
			}
		}
	})
	comment, err := c.CreateComment(context.Background(), "i1", "hello")
	if err != nil || comment.ID != "c1" {
		t.Fatalf("create: %+v %v", comment, err)
	}
	comments, err := c.ListComments(context.Background(), "i1")
	if err != nil || len(comments) != 2 || comments[1].Body != "world" || *calls != 6 {
		t.Fatalf("list: %+v %v", comments, err)
	}
}

func TestMetricsRecordRequestCacheForcedVerificationAndQuota(t *testing.T) {
	c, _ := fixture(t, func(w http.ResponseWriter, q request) {
		if strings.Contains(q.Query, "QuotaProbe") {
			_, _ = fmt.Fprint(w, `{"data":{"viewer":{"id":"viewer"}}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":{"issue":{"comments":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}`)
	})
	ctx := BeginRound(context.Background())
	if _, err := c.ListComments(ctx, "i1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListComments(ctx, "i1"); err != nil {
		t.Fatal(err)
	}
	if _, err := CriticalComments(context.Background(), c, "i1"); err != nil {
		t.Fatal(err)
	}
	if err := c.ProbeQuota(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := c.Metrics()
	if got.Requests != 6 || got.CacheHits != 1 || got.ForcedVerifications != 1 || got.QuotaProbes != 1 {
		t.Fatalf("metrics=%+v", got)
	}
}
func TestErrorsAreSanitized(t *testing.T) {
	for _, tc := range []struct {
		status    int
		body      string
		global    bool
		transient bool
	}{
		{400, `{"errors":[{"message":"test-private-key","extensions":{"code":"RATELIMITED"}}]}`, true, false},
		{200, `{"errors":[{"message":"test-private-key"}]}`, false, false},
		{500, `test-private-key`, false, true},
		{200, `test-private-key`, false, false},
		{200, `{"data":{"issueUpdate":{"success":false}}}`, false, false},
	} {
		t.Run(fmt.Sprint(tc.status, tc.global, tc.transient, tc.body[:1]), func(t *testing.T) {
			c, _ := fixture(t, func(w http.ResponseWriter, q request) {
				w.WriteHeader(tc.status)
				if _, err := fmt.Fprint(w, tc.body); err != nil {
					t.Error(err)
					return
				}
			})
			_, err := c.UpdateIssue(context.Background(), "i1", "Done", nil, nil)
			if err == nil {
				t.Fatal("expected error")
			}
			for e := err; e != nil; e = errors.Unwrap(e) {
				if strings.Contains(e.Error(), "test-private-key") {
					t.Fatal("credential leaked")
				}
			}
			var g *infra.Global
			if errors.As(err, &g) != tc.global || (g != nil && g.Dep != infra.Linear) {
				t.Fatalf("classification: %T", err)
			}
			var tr *infra.Transient
			if errors.As(err, &tr) != tc.transient {
				t.Fatalf("classification: %T", err)
			}
		})
	}
}
func TestRedirectDoesNotForwardKey(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer target.Close()
	c, _ := fixture(t, func(w http.ResponseWriter, q request) { w.Header().Set("Location", target.URL); w.WriteHeader(307) })
	_, err := c.CreateComment(context.Background(), "i1", "hello")
	if err == nil || reached {
		t.Fatal("redirect followed")
	}
}

func TestMetadataPaginationAndCache(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test-private-key")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var q request
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
		}
		if strings.Contains(q.Query, "IssueLabels") {
			if _, err := fmt.Fprint(w, `{"data":{"issue":{"labels":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}`); err != nil {
				t.Error(err)
				return
			}
			return
		}
		if strings.Contains(q.Query, "issueUpdate") {
			if !reflect.DeepEqual(q.Variables["input"], map[string]any{"stateId": "s2", "addedLabelIds": []any{"l2"}, "removedLabelIds": []any{}}) {
				t.Error("second page not cached")
			}
			if _, err := fmt.Fprint(w, `{"data":{"issueUpdate":{"success":true,"issue":{"id":"i1"}}}}`); err != nil {
				t.Error(err)
				return
			}
			return
		}
		field, prefix := "states", "s"
		if strings.Contains(q.Query, "labels(") {
			field, prefix = "labels", "l"
		}
		if strings.Contains(q.Query, "projects(") {
			field, prefix = "projects", "p"
		}
		if q.Variables["after"] == nil {
			if _, err := fmt.Fprintf(w, `{"data":{"team":{"%s":{"nodes":[{"id":"%s1","name":"first"}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}}`, field, prefix); err != nil {
				t.Error(err)
				return
			}
		} else {
			if q.Variables["after"] != "next" {
				t.Error("metadata cursor")
			}
			if _, err := fmt.Fprintf(w, `{"data":{"team":{"%s":{"nodes":[{"id":"%s2","name":"second"}],"pageInfo":{"hasNextPage":false}}}}}`, field, prefix); err != nil {
				t.Error(err)
				return
			}
		}
	}))
	defer server.Close()
	c, err := New(context.Background(), Options{TeamID: "team-id", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 6 || c.projects["second"] != "p2" {
		t.Fatalf("metadata requests: %d", requests)
	}
	for i := 0; i < 2; i++ {
		if _, err = c.UpdateIssue(context.Background(), "i1", "second", []string{"second"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if requests != 10 {
		t.Fatalf("metadata refetched: %d", requests)
	}
}

func TestUsageHourRollover(t *testing.T) {
	c, _ := fixture(t, func(w http.ResponseWriter, q request) {
		if _, err := fmt.Fprint(w, `{"data":{"commentCreate":{"success":true,"comment":{"id":"c1"}}}}`); err != nil {
			t.Error(err)
			return
		}
	})
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	if u := c.Usage(); u.Requests != 0 || u.Complexity != 0 {
		t.Fatalf("new hour: %+v", u)
	}
	if _, err := c.CreateComment(context.Background(), "i1", "x"); err != nil {
		t.Fatal(err)
	}
	if u := c.Usage(); u.Requests != 1 || u.Complexity != 7 {
		t.Fatalf("new request: %+v", u)
	}
	now = now.Add(time.Hour)
	if u := c.Usage(); u.Requests != 0 || u.Complexity != 0 || u.Hour.Format(time.RFC3339) != "2026-01-02T04:00:00Z" {
		t.Fatalf("rollover: %+v", u)
	}
}

func TestCanceledRequestAndUnknownProjects(t *testing.T) {
	c, calls := fixture(t, func(w http.ResponseWriter, q request) { t.Error("unexpected API call") })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.ListComments(ctx, "i1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err = c.Snapshot(context.Background(), []string{"missing"}, time.Time{}); err == nil {
		t.Error("unknown project accepted")
	}
	if issues, err := c.Snapshot(context.Background(), nil, time.Time{}); err != nil || len(issues) != 0 {
		t.Errorf("empty snapshot: %v %v", issues, err)
	}
	if *calls != 3 {
		t.Errorf("unexpected requests: %d", *calls)
	}
}

func TestSnapshotNestedPagination(t *testing.T) {
	c, calls := fixture(t, func(w http.ResponseWriter, q request) {
		switch {
		case strings.Contains(q.Query, "Snapshot"):
			if _, err := fmt.Fprint(w, `{"data":{"issues":{"nodes":[{"id":"i1","labels":{"nodes":[{"id":"l1","name":"first"}],"pageInfo":{"hasNextPage":true,"endCursor":"labels-next"}},"relations":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"relations-next"}}}],"pageInfo":{"hasNextPage":false}}}}`); err != nil {
				t.Error(err)
				return
			}
		case strings.Contains(q.Query, "labels("):
			if q.Variables["after"] != "labels-next" || q.Variables["id"] != "i1" {
				t.Error("nested label cursor")
			}
			if _, err := fmt.Fprint(w, `{"data":{"issue":{"labels":{"nodes":[{"id":"l2","name":"second"}],"pageInfo":{"hasNextPage":false}}}}}`); err != nil {
				t.Error(err)
				return
			}
		default:
			if q.Variables["after"] != "relations-next" || q.Variables["id"] != "i1" {
				t.Error("nested relation cursor")
			}
			if _, err := fmt.Fprint(w, `{"data":{"issue":{"relations":{"nodes":[{"id":"r2","type":"blocks","relatedIssue":{"id":"i2","identifier":"ENG-2"}}],"pageInfo":{"hasNextPage":false}}}}}`); err != nil {
				t.Error(err)
				return
			}
		}
	})
	issues, err := c.Snapshot(context.Background(), []string{"Alpha"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || len(issues[0].Labels.Nodes) != 2 || issues[0].Labels.Nodes[1].Name != "second" || len(issues[0].Relations.Nodes) != 1 || issues[0].Relations.Nodes[0].RelatedIssue.Identifier != "ENG-2" || *calls != 6 {
		t.Fatalf("nested snapshot: %+v", issues)
	}
}

func TestRepeatedCursorFails(t *testing.T) {
	c, calls := fixture(t, func(w http.ResponseWriter, q request) {
		if _, err := fmt.Fprint(w, `{"data":{"issue":{"comments":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"same"}}}}}`); err != nil {
			t.Error(err)
			return
		}
	})
	if _, err := c.ListComments(context.Background(), "i1"); err == nil {
		t.Error("repeated cursor accepted")
	}
	if *calls != 5 {
		t.Errorf("unbounded pagination: %d", *calls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestTransportErrorDoesNotLeakKey(t *testing.T) {
	c, _ := fixture(t, func(w http.ResponseWriter, q request) { t.Error("unexpected request") })
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("transport echoed %s", r.Header.Get("Authorization"))
	})
	_, err := c.ListComments(context.Background(), "i1")
	if err == nil {
		t.Fatal("expected error")
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		if strings.Contains(e.Error(), "test-private-key") {
			t.Fatal("credential leaked")
		}
	}
}

func TestSchedulerSnapshotFields(t *testing.T) {
	c, _ := fixture(t, func(w http.ResponseWriter, q request) {
		for _, field := range []string{"priority", "createdAt", "project { id name }"} {
			if !strings.Contains(q.Query, field) {
				t.Errorf("missing %s", field)
			}
		}
		if _, err := fmt.Fprint(w, `{"data":{"issues":{"nodes":[{"id":"i1","priority":2,"createdAt":"2026-01-01T00:00:00Z","project":{"id":"p1","name":"Alpha"}}],"pageInfo":{"hasNextPage":false}}}}`); err != nil {
			t.Error(err)
			return
		}
	})
	issues, err := c.Snapshot(context.Background(), []string{"Alpha"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Priority != 2 || issues[0].CreatedAt.Format(time.RFC3339) != "2026-01-01T00:00:00Z" || issues[0].Project.ID != "p1" {
		t.Fatalf("issues=%+v", issues)
	}
}

func TestSnapshotAcceptsProjectUUIDAlongsideName(t *testing.T) {
	project := "480a07e8-8b39-4b8f-92b9-dbf1997cbd8c"
	c, _ := fixture(t, func(w http.ResponseWriter, q request) {
		filter := q.Variables["filter"].(map[string]any)
		want := map[string]any{"id": map[string]any{"in": []any{project, "p1"}}}
		if !reflect.DeepEqual(filter["project"], want) {
			t.Errorf("project filter=%v", filter)
		}
		if _, err := fmt.Fprint(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}`); err != nil {
			t.Error(err)
			return
		}
	})
	if _, err := c.Snapshot(context.Background(), []string{project, "Alpha"}, time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestIssueBlocked(t *testing.T) {
	blocker := func(kind, stateType string) InverseRelation {
		r := InverseRelation{Type: kind}
		r.Issue.Identifier = "X-1"
		r.Issue.State.Type = stateType
		return r
	}
	cases := []struct {
		name  string
		nodes []InverseRelation
		more  bool
		want  bool
	}{
		{"none", nil, false, false},
		{"open blocker", []InverseRelation{blocker("blocks", "started")}, false, true},
		{"done blocker", []InverseRelation{blocker("blocks", "completed")}, false, false},
		{"canceled blocker", []InverseRelation{blocker("blocks", "canceled")}, false, false},
		{"duplicate blocker", []InverseRelation{blocker("blocks", "duplicate")}, false, false},
		{"related only", []InverseRelation{blocker("related", "started")}, false, false},
		{"truncated is conservative", nil, true, true},
	}
	for _, c := range cases {
		issue := Issue{InverseRelations: Connection[InverseRelation]{Nodes: c.nodes, PageInfo: PageInfo{HasNextPage: c.more}}}
		if got := issue.Blocked(); got != c.want {
			t.Fatalf("%s: Blocked()=%v want %v", c.name, got, c.want)
		}
	}
}

func TestSameRoundCommentReadsAreCoalesced(t *testing.T) {
	requests := 0
	t.Setenv("LINEAR_API_KEY", "test-private-key")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var q request
		if err := json.Unmarshal(body, &q); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		response := `{"data":{"team":{"states":{"nodes":[{"id":"s1","name":"Done"}],"pageInfo":{"hasNextPage":false}}}}}`
		if strings.Contains(q.Query, "labels(") {
			response = `{"data":{"team":{"labels":{"nodes":[{"id":"l1","name":"bug"}],"pageInfo":{"hasNextPage":false}}}}}`
		} else if strings.Contains(q.Query, "projects(") {
			response = `{"data":{"team":{"projects":{"nodes":[{"id":"p1","name":"Alpha"}],"pageInfo":{"hasNextPage":false}}}}}`
		} else if strings.Contains(q.Query, "Metadata") {
		} else if strings.Contains(q.Query, "ListComments") {
			response = `{"data":{"issue":{"comments":{"nodes":[{"id":"c1","body":"hello"}],"pageInfo":{"hasNextPage":false}}}}}`
		} else {
			t.Fatalf("unexpected query: %s", q.Query)
			return nil, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
	})}
	c, err := New(context.Background(), Options{TeamID: "team-id", BaseURL: "http://linear.test/graphql", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	ctx := BeginRound(context.Background())
	first, err := c.ListComments(ctx, "i1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.ListComments(ctx, "i1")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 || first[0].Body != second[0].Body {
		t.Fatalf("comments: %+v %+v", first, second)
	}
	if requests != 4 {
		t.Fatalf("same-round read made %d requests, want metadata plus one comment request", requests)
	}
}

func TestCriticalCommentsUsesFreshRemoteSeam(t *testing.T) {
	reader := criticalReader{remote: []Comment{{ID: "fresh"}}, cached: []Comment{{ID: "cached"}}}
	got, err := CriticalComments(context.Background(), &reader, "issue")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "fresh" || reader.remoteCalls != 1 || reader.cachedCalls != 0 {
		t.Fatalf("got=%v remote=%d cached=%d", got, reader.remoteCalls, reader.cachedCalls)
	}
}

type criticalReader struct {
	remote, cached           []Comment
	remoteCalls, cachedCalls int
}

func (r *criticalReader) ListComments(context.Context, string) ([]Comment, error) {
	r.cachedCalls++
	return r.cached, nil
}
func (r *criticalReader) ListCommentsRemote(context.Context, string) ([]Comment, error) {
	r.remoteCalls++
	return r.remote, nil
}

func TestRateLimitEffectiveRemainingReplenishesFromHeadersConservatively(t *testing.T) {
	observed := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	limit := RateLimit{Limit: 100, Remaining: 20, Reset: observed.Add(80 * time.Second), ObservedAt: observed}
	if got := limit.EffectiveRemaining(observed.Add(40 * time.Second)); got != 60 {
		t.Fatalf("effective remaining=%d, want 60", got)
	}
	if got := (RateLimit{}).EffectiveRemaining(observed.Add(time.Hour)); got != 0 {
		t.Fatalf("unknown effective remaining=%d, want conservative zero", got)
	}
	if limit.CriticalCapacity(observed.Add(40*time.Second), 60) {
		t.Fatal("critical reserve was not preserved")
	}
	if (RateLimit{}).CriticalCapacity(observed, 0) {
		t.Fatal("unknown quota admitted critical action")
	}
}

func TestCriticalReservationSerializesContention(t *testing.T) {
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	c := &Client{now: func() time.Time { return now }, usage: Usage{RequestLimit: RateLimit{Limit: 2, Remaining: 1, Reset: now.Add(time.Hour), ObservedAt: now}}}
	start := make(chan struct{})
	results := make(chan bool, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; results <- c.ReserveCritical(now, 0) }()
	}
	close(start)
	if first, second := <-results, <-results; first == second {
		t.Fatalf("reservations=%v,%v; exactly one caller should pass", first, second)
	}
}
