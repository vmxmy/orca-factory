package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIssueRateLimitStopsSubsequentRequests(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
	}))
	defer s.Close()
	c, err := New(Options{BaseURL: s.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = c.Issues(context.Background(), "o", "r"); err == nil {
			t.Fatal("rate limit accepted")
		}
	}
	if calls != 1 {
		t.Fatalf("rate protection made %d requests, want 1", calls)
	}
}

func TestIssuePermissionFailureDoesNotCreateQuotaPause(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
	}))
	defer s.Close()
	c, err := New(Options{BaseURL: s.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := c.Issues(context.Background(), "o", "r"); err == nil {
			t.Fatal("permission failure accepted")
		}
	}
	if calls != 2 {
		t.Fatalf("permission failure incorrectly created quota pause: calls=%d", calls)
	}
}

func TestIssuesCanonicalPagination(t *testing.T) {
	calls := 0
	wrongRepository := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/repos/o/r":
			_, _ = fmt.Fprint(w, `{"id":42,"full_name":"o/r"}`)
		case "/repos/o/r/issues":
			id := 42
			if wrongRepository {
				id = 43
			}
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/repositories/%d/issues?state=all&per_page=100&page=2&sort=created&direction=asc&after=cursor%%2Bvalue>; rel="next"`, r.Host, id))
			_, _ = fmt.Fprint(w, `[{"number":1}]`)
		case "/repositories/42/issues":
			if r.URL.Query().Get("after") != "cursor+value" || r.URL.Query().Get("state") != "all" {
				t.Errorf("pagination query lost: %s", r.URL)
			}
			_, _ = fmt.Fprint(w, `[{"number":2}]`)
		default:
			t.Errorf("unsafe request: %s", r.URL)
			w.WriteHeader(400)
		}
	}))
	defer s.Close()
	c, _ := New(Options{BaseURL: s.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	rows, err := c.Issues(context.Background(), "o", "r")
	if err != nil || len(rows) != 2 || calls != 3 {
		t.Fatalf("canonical pagination: rows=%v calls=%d error=%v", rows, calls, err)
	}
	wrongRepository = true
	before := calls
	if _, err = c.Issues(context.Background(), "o", "r"); err == nil || calls-before != 2 {
		t.Fatalf("foreign repository accepted: calls=%d error=%v", calls-before, err)
	}
}

func TestIssuesFollowNextLinkEvenForShortPage(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/o/r/issues?state=all&per_page=100&page=2&sort=created&direction=asc>; rel="next"`, r.Host))
			_, _ = fmt.Fprint(w, `[{"number":1}]`)
		} else {
			_, _ = fmt.Fprint(w, `[{"number":2}]`)
		}
	}))
	defer s.Close()
	c, _ := New(Options{BaseURL: s.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	rows, err := c.Issues(context.Background(), "o", "r")
	if err != nil || len(rows) != 2 || calls != 2 {
		t.Fatalf("short page truncated: rows=%v calls=%d error=%v", rows, calls, err)
	}
}
