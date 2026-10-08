package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestRecordedGitHubIssuePagination(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/recorded/github/issues-pagination-link.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Repository json.RawMessage `json:"repository"`
		Link       string          `json:"link"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/repos/vmxmy/orca-factory":
			_, _ = w.Write(fixture.Repository)
		case "/repos/vmxmy/orca-factory/issues":
			w.Header().Set("Link", strings.Replace(fixture.Link, "https://api.github.com", "http://"+r.Host, 1))
			_, _ = fmt.Fprint(w, `[{"number":1}]`)
		case "/repositories/1403187767/issues":
			if !strings.Contains(fixture.Link, r.URL.RawQuery) || r.URL.Query().Get("after") == "" {
				t.Error("recorded next query not preserved")
			}
			_, _ = fmt.Fprint(w, `[{"number":2}]`)
		default:
			t.Errorf("unexpected request: %s", r.URL)
		}
	}))
	defer s.Close()
	c, _ := New(Options{BaseURL: s.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	rows, err := c.Issues(context.Background(), "vmxmy", "orca-factory")
	if err != nil || len(rows) != 2 || calls != 3 {
		t.Fatalf("recorded pagination: calls=%d rows=%d err=%v", calls, len(rows), err)
	}
}

// Fixture recorded through gh api repos/vmxmy/orca-factory/issues/172 on
// 2026-10-06. Only public issue fields used by this adapter were retained.
func TestRecordedGitHubIssueREST(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/recorded/github/issues-172.json")
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(raw) }))
	defer s.Close()
	c, err := New(Options{BaseURL: s.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := c.Issues(context.Background(), "vmxmy", "orca-factory")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Number != 172 || rows[0].URL != "https://github.com/vmxmy/orca-factory/issues/172" || rows[0].CreatedAt.IsZero() || rows[0].Body == "" {
		t.Fatalf("recorded issue contract lost: %+v", rows)
	}
}
