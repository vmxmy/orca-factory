package issues

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
)

func TestPersistentSnapshotsReuseAcrossClients(t *testing.T) {
	t.Setenv("AGENT_GH_CACHE_PATH", filepath.Join(t.TempDir(), "private", "cache.sqlite"))
	var issues, comments atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r/issues" {
			issues.Add(1)
			_, _ = fmt.Fprintf(w, `[{"number":42,"body":%q,"state":"open","updated_at":"2026-10-06T00:00:00Z"}]`, taskBody)
			return
		}
		comments.Add(1)
		_, _ = fmt.Fprint(w, `[{"id":3,"body":"ownership","created_at":"2026-10-06T00:00:00Z","updated_at":"2026-10-06T00:00:00Z"}]`)
	}))
	defer s.Close()
	for range 3 {
		gh, err := github.New(github.Options{BaseURL: s.URL, Token: func(context.Context) (string, error) { return "test", nil }})
		if err != nil {
			t.Fatal(err)
		}
		c, err := NewGitHub("o/r", gh)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := c.Snapshot(context.Background(), []string{"o/r"}, time.Time{})
		if err != nil || len(rows) != 1 || rows[0].Identifier != "ZII-42" || !rows[0].Blocked() {
			t.Fatalf("snapshot lost identity/unknown: %+v %v", rows, err)
		}
		history, err := c.SnapshotComments(context.Background(), "ZII-42")
		if err != nil || len(history) != 1 || history[0].Body != "ownership" {
			t.Fatalf("history %+v %v", history, err)
		}
	}
	if issues.Load() != 1 || comments.Load() != 1 {
		t.Fatalf("three independent clients: issues=%d comments=%d; want 1 each", issues.Load(), comments.Load())
	}
	t.Logf("three independent clients: issue requests=%d comment requests=%d (baseline 3 each)", issues.Load(), comments.Load())
}
