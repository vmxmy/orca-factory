package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/projects"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// TestWarnRemoteMismatchOnce is OF-56: a long-running serve re-discovers
// projects every interval, but a persistent live-remote drift must log only
// once, not on every rediscovery; a changed drift value logs again, and a
// resolved drift clears so a later recurrence is re-logged.
func TestWarnRemoteMismatchOnce(t *testing.T) {
	var logs bytes.Buffer
	ctx := runx.WithLogger(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)))
	warned := map[string]string{}
	mismatched := []projects.Project{{Path: "/repo/dujiao-next", Remote: "github.com/dujiao-next/dujiao-next", RemoteMismatch: "github.com/vmxmy/dujiao-next"}}

	warnRemoteMismatch(ctx, mismatched, warned)
	warnRemoteMismatch(ctx, mismatched, warned)
	if n := strings.Count(logs.String(), "live remote drifted"); n != 1 {
		t.Fatalf("warned %d times for an unchanged drift, want 1\nlog:\n%s", n, logs.String())
	}

	clean := []projects.Project{{Path: "/repo/dujiao-next", Remote: "github.com/dujiao-next/dujiao-next"}}
	warnRemoteMismatch(ctx, clean, warned)
	if n := strings.Count(logs.String(), "live remote drifted"); n != 1 {
		t.Fatalf("warned again after the drift resolved, want still 1\nlog:\n%s", logs.String())
	}

	warnRemoteMismatch(ctx, mismatched, warned)
	if n := strings.Count(logs.String(), "live remote drifted"); n != 2 {
		t.Fatalf("warned %d times after the drift recurred, want 2\nlog:\n%s", n, logs.String())
	}

	other := []projects.Project{{Path: "/repo/dujiao-next", Remote: "github.com/dujiao-next/dujiao-next", RemoteMismatch: "github.com/someone-else/dujiao-next"}}
	warnRemoteMismatch(ctx, other, warned)
	if n := strings.Count(logs.String(), "live remote drifted"); n != 3 {
		t.Fatalf("warned %d times for a changed drift value, want 3\nlog:\n%s", n, logs.String())
	}

	if !strings.Contains(logs.String(), "/repo/dujiao-next") || !strings.Contains(logs.String(), "github.com/dujiao-next/dujiao-next") {
		t.Fatalf("log missing path or canonicalKey: %s", logs.String())
	}
}
