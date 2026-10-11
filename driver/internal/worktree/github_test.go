package worktree_test

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
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/worktree"
)

func TestGitHubPruneUsesCommentAssociationAndFreshOwnership(t *testing.T) {
	for _, test := range []struct {
		name, legacy, comment, branch    string
		changed, active, listFails, keep bool
	}{
		{name: "comment", comment: "GH-42"},
		{name: "fresh ownership", comment: "GH-42", changed: true, keep: true},
		{name: "legacy active", legacy: "ZII-42", active: true, keep: true},
		{name: "conflicting associations", legacy: "ZII-43", comment: "GH-42", keep: true},
		{name: "unmapped legacy", legacy: "ZII-99", comment: "GH-42", keep: true},
		{name: "same canonical identity", legacy: "01234567-1234-1234-1234-012345678901", comment: "GH-42"},
		{name: "branch names another task", comment: "GH-42", branch: "agent/ZII-43/executor", keep: true},
		{name: "task list unavailable", comment: "GH-42", listFails: true, keep: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// #given
			dir := t.TempDir()
			repo := filepath.Join(dir, "repo")
			if err := os.Mkdir(repo, 0700); err != nil {
				t.Fatal(err)
			}
			git := func(args ...string) string {
				t.Helper()
				b, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %s %v", args, b, err)
				}
				return strings.TrimSpace(string(b))
			}
			git("init", "-q")
			git("-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-qm", "seed")
			worker := filepath.Join(repo, "run-old")
			branch := test.branch
			if branch == "" {
				branch = "feature-worker"
			}
			git("worktree", "add", "-qb", branch, worker)
			if err := os.WriteFile(filepath.Join(repo, ".git/info/exclude"), []byte(".omc/\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(worker, ".omc"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(worker, ".omc/agent-run-finished"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			sha := git("rev-parse", "HEAD")
			calls := filepath.Join(dir, "calls")
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			comment := "driver: Done"
			if test.comment != "" {
				comment += "\nissue: " + test.comment
			}
			inventory, err := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"worktrees": []any{map[string]any{"path": worker, "comment": comment, "linkedLinearIssue": test.legacy}}}})
			if err != nil {
				t.Fatal(err)
			}
			write("orca", fmt.Sprintf("#!/bin/sh\ncase \"$1 $2\" in\n'worktree list') printf '%%s\\n' '%s';;\n'worktree rm') printf removed >> %q; printf '%%s\\n' '{\"ok\":true}';;\n*) exit 91;;\nesac\n", inventory, calls))
			write("gh", fmt.Sprintf("#!/bin/sh\ncase \"$1 $2\" in\n'auth token') printf test;;\n'api --paginate') printf '%%s\\n' '%s';;\n*) exit 92;;\nesac\n", `[{"merged_at":"2026-01-01","head":{"sha":"`+sha+`","repo":{"full_name":"acme/demo"}}}]`))
			reads := 0
			identifier, stateName := "GH-42", "Done"
			if test.active {
				identifier, stateName = "ZII-42", "In Progress"
			}
			meta, err := json.Marshal(issues.Metadata{ID: "01234567-1234-1234-1234-012345678901", Identifier: identifier, Project: "acme/demo", State: stateName, Complete: true, Blockers: []string{}, Related: []string{}})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Errorf("unexpected mutation %s", r.Method)
					http.Error(w, "forbidden", 500)
					return
				}
				if test.listFails && !strings.HasSuffix(r.URL.Path, "/comments") {
					http.Error(w, "unavailable", http.StatusBadGateway)
					return
				}
				state := "closed"
				if test.active {
					state = "open"
				}
				rows := []any{map[string]any{"number": 42, "state": state, "body": "<!-- orca-task-v1: " + string(meta) + " -->"}}
				if test.legacy == "ZII-43" || strings.Contains(test.branch, "ZII-43") {
					rows = append(rows, map[string]any{"number": 43, "state": "closed", "body": `<!-- orca-task-v1: {"id":"11234567-1234-1234-1234-012345678901","identifier":"ZII-43","project":"acme/demo","state":"Done","complete":true,"blockers":[],"related":[]} -->`})
				}
				var out any = rows
				if strings.HasSuffix(r.URL.Path, "/comments") {
					reads++
					out = []any{}
					if test.changed && reads > 1 {
						out = []any{map[string]any{"id": 1, "body": "归属: 处置Agent term=term_one run=run_one attempt=1"}}
					}
				}
				if err := json.NewEncoder(w).Encode(out); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			for key, value := range map[string]string{"HOME": dir, "PATH": dir + ":" + os.Getenv("PATH"), "AGENT_DRIVER_STATE_ROOT": filepath.Join(dir, "state"), "AGENT_REPO_PATH": repo, "AGENT_RUN_WORKTREE_PREFIX": "run-", "AGENT_ISSUE_BACKEND": "github", "AGENT_GH_REPO": "acme/demo", "AGENT_GH_API_URL": server.URL, "AGENT_LINEAR_TEAM": "", "AGENT_LINEAR_PROJECT": ""} {
				t.Setenv(key, value)
			}
			// #when
			var out, warnings bytes.Buffer
			code := worktree.RunPrune(context.Background(), nil, &out, &warnings)
			// #then
			_, removed := os.Stat(calls)
			if code != 0 && !test.listFails || (removed == nil) == test.keep || !test.keep && reads < 2 {
				t.Fatalf("prune code=%d removed=%v reads=%d warnings=%s", code, removed == nil, reads, warnings.String())
			}
		})
	}
}
