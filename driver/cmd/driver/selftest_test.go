package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/agentcli"
	"github.com/vmxmy/orca-factory/driver/internal/config"
)

func TestLiveReviewerRetainsConfiguredAgent(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "--quiet")
	writeTestFile(t, filepath.Join(repo, "scripts/agent-gates/roles/reviewer.md"), "review")
	writeTestFile(t, filepath.Join(repo, "scripts/agent-gates/constitution/constitution.md"), "rules")
	gitIn(t, repo, "add", "scripts")
	gitIn(t, repo, "commit", "--quiet", "-m", "review rules")
	gitIn(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Chdir(dir)
	// selftest has no model-health state root. A registry in its working
	// directory must not redirect the configured reviewer to a fallback.
	writeTestFile(t, filepath.Join(dir, "model-health.json"), `{"relay:chosen":{"ref":{"agent":"relay","model":"chosen"},"state":"unavailable","until":"2099-01-01T00:00:00Z"}}`)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "test-sentinel")
	binary := filepath.Join(dir, "shared-reviewer")
	writeDoctorFile(t, binary, `#!/bin/sh
if [ "$ANTHROPIC_AUTH_TOKEN" = test-sentinel ]; then echo present; else echo absent; fi > "$CAPTURE"
printf '{"verdict":"green","step":"review"}'
`, 0700)
	args := []string{"--model", "{model}"}
	agentcli.Configure(map[string]agentcli.Profile{
		"local": {Binary: binary, ReviewArgs: args},
		"relay": {Binary: binary, ReviewArgs: args, Relay: true},
	})
	t.Cleanup(func() { agentcli.Configure(nil) })

	for _, tc := range []struct{ name, main, small, want string }{
		{"main local", "local:two words:high", "", "absent"},
		{"main relay", "relay:chosen:high", "", "present"},
		{"small local", "relay:chosen:high", "local:two words:high", "absent"},
		{"small relay", "local:two words:high", "relay:chosen:high", "present"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := filepath.Join(t.TempDir(), "observed")
			t.Setenv("CAPTURE", capture)
			env := map[string]string{
				"AGENT_ROLE_REVIEWER":          tc.main,
				"AGENT_ROLE_REVIEWER_FALLBACK": "local:fallback:low",
			}
			if tc.small != "" {
				env["AGENT_ROLE_REVIEWER_SMALL"] = tc.small
				env["AGENT_ROLE_REVIEWER_SMALL_FALLBACK"] = "local:fallback:low"
			}
			if err := liveReviewer(t.Context(), config.Repository{Path: repo, Env: env}); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(capture)
			if err != nil || string(got) != tc.want+"\n" {
				t.Errorf("reviewer token = %q, %v; want %s", got, err, tc.want)
			}
		})
	}
}
