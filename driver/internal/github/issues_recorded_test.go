package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

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
