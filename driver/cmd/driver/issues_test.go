package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGitHubCLIAndLedgerWithoutLinear(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nprintf test-token\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int64
	body := `<!-- orca-task-v1: {"id":"01234567-1234-1234-1234-012345678901","identifier":"ZII-42","project":"o/r","state":"In Progress","complete":true,"blockers":[],"related":[]} -->`
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("read command wrote %s", r.Method)
			http.Error(w, "no", http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/repos/o/r/issues" {
			_, _ = fmt.Fprintf(w, `[{"number":42,"title":"[ledger] agent-graph","body":%q,"state":"open","labels":[],"updated_at":"2026-10-06T00:00:00Z"}]`, body)
			return
		}
		if r.URL.Path == "/repos/o/r/issues/42/comments" {
			reads.Add(1)
			_, _ = fmt.Fprint(w, `[{"id":5,"body":"归属: 人","created_at":"2026-10-06T00:00:00Z","updated_at":"2026-10-06T00:00:00Z"}]`)
			return
		}
		t.Errorf("unexpected REST path %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer s.Close()
	command := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestCLIProcess$", "--"}, args...)...)
		cmd.Env = append(os.Environ(), "DRIVER_TEST_PROCESS=1", "PATH="+bin, "LINEAR_API_KEY=", "AGENT_LINEAR_TEAM=", "AGENT_LINEAR_PROJECT=", "AGENT_ISSUE_BACKEND=github", "AGENT_GH_REPO=o/r", "AGENT_GH_API_URL="+s.URL)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("command %v: %v stderr=%s", args, err, stderr.String())
		}
		return stdout.Bytes()
	}
	var snapshot struct {
		OK     bool `json:"ok"`
		Result struct {
			Complete bool `json:"complete"`
			Issues   []struct {
				ID               string `json:"id"`
				CommentsComplete bool   `json:"commentsComplete"`
			}
		} `json:"result"`
	}
	if err := json.Unmarshal(command("issues", "snapshot"), &snapshot); err != nil {
		t.Fatal(err)
	}
	if !snapshot.OK || !snapshot.Result.Complete || len(snapshot.Result.Issues) != 1 || snapshot.Result.Issues[0].CommentsComplete || reads.Load() != 0 {
		t.Fatalf("snapshot fanout or schema: %+v comment reads=%d", snapshot, reads.Load())
	}
	var read struct {
		Result struct {
			Issue struct {
				Identifier, Owner string
				Number            int
				CommentsComplete  bool
				Comments          struct{ Nodes []struct{ Body string } }
			}
		}
	}
	if err := json.Unmarshal(command("issues", "read", "--id", "ZII-42"), &read); err != nil {
		t.Fatal(err)
	}
	if read.Result.Issue.Identifier != "ZII-42" || read.Result.Issue.Number != 42 || read.Result.Issue.Owner != "人" || !read.Result.Issue.CommentsComplete || len(read.Result.Issue.Comments.Nodes) != 1 {
		t.Fatalf("read lost logical issue: %+v", read)
	}
	if out := command("ledger", "stats", "--all"); !strings.Contains(string(out), "runs=0") {
		t.Fatalf("ledger stats=%s", out)
	}
}

func TestIssueImportBudgetPreservesParentCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx, stop := issueCommandContext(parent, "import")
	defer stop()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) < 29*time.Minute || time.Until(deadline) > 30*time.Minute {
		t.Fatalf("import budget cannot cover validated paced writes: %v", deadline)
	}
	cancel()
	select {
	case <-ctx.Done():
		if ctx.Err() != context.Canceled {
			t.Fatal(ctx.Err())
		}
	default:
		t.Fatal("import detached parent cancellation")
	}
	short, shortCancel := context.WithTimeout(context.Background(), time.Second)
	defer shortCancel()
	bounded, boundedCancel := issueCommandContext(short, "import")
	defer boundedCancel()
	parentDeadline, _ := short.Deadline()
	childDeadline, _ := bounded.Deadline()
	if !parentDeadline.Equal(childDeadline) {
		t.Fatal("import extended parent deadline")
	}
}

// Both standalone CLI paths must carry the configured deadline through the
// selected backend, without needing any Linear credentials or metadata.
func TestGitHubCLIConfiguredHTTPTimeout(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nprintf test-token\n"), 0700); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	configDir := filepath.Join(home, ".config", "orca-factory")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "driver.toml"), []byte("[github]\nhttp_timeout = \"10ms\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	canceled := make(chan struct{}, 2)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			canceled <- struct{}{}
		case <-time.After(200 * time.Millisecond):
			_, _ = fmt.Fprint(w, `[]`)
		}
	}))
	defer s.Close()
	for _, args := range [][]string{{"issues", "snapshot"}, {"ledger", "stats", "--all"}} {
		t.Run(args[0], func(t *testing.T) {
			cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestCLIProcess$", "--"}, args...)...)
			cmd.Env = append(os.Environ(), "DRIVER_TEST_PROCESS=1", "HOME="+home, "PATH="+bin, "LINEAR_API_KEY=", "AGENT_LINEAR_TEAM=", "AGENT_LINEAR_PROJECT=", "AGENT_ISSUE_BACKEND=github", "AGENT_GH_REPO=o/r", "AGENT_GH_API_URL="+s.URL)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("configured HTTP deadline ignored: %s", out)
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatalf("request was not canceled by configured deadline: %v: %s", err, out)
			}
		})
	}
}
