package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/projects"
)

func TestProjectsUsageError(t *testing.T) {
	if code := runProjects(nil); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if code := runProjects([]string{"--bogus"}); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

// fakeOrcaRepoList writes a stub orca binary that answers `repo list --json`
// with one repository, and returns its path for AGENT_ORCA_BIN.
func fakeOrcaRepoList(t *testing.T, path, canonicalKey string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "orca")
	body := `{"id":"3b3ee508-1f2b-4b8e-9b5a-6fbb2c5b8a3b","ok":true,"result":{"repos":[{"path":"` + path + `","gitRemoteIdentity":{"canonicalKey":"` + canonicalKey + `"}}]}}`
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho '"+body+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// Without a configured factory_root, discoverProjects rejects every
// discovered repository for that reason alone (OF-42).
func TestDiscoverProjectsFactoryRootUnconfigured(t *testing.T) {
	t.Setenv("AGENT_ORCA_BIN", fakeOrcaRepoList(t, "/repo/orca-factory", "github.com/vmxmy/orca-factory"))
	result, _, err := discoverProjects(context.Background(), config.Config{}, &orca.Client{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Eligible) != 0 || len(result.Rejected) != 1 || result.Rejected[0].Reason != projects.ReasonFactoryRootUnconfigured {
		t.Fatalf("result=%+v, want one rejection for ReasonFactoryRootUnconfigured", result)
	}
}

// TestProjectsCommandJSON drives the `projects --json` command end to end
// against a fake Orca binary, with no factory_root configured.
func TestProjectsCommandJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENT_ORCA_BIN", fakeOrcaRepoList(t, "/repo/orca-factory", "github.com/vmxmy/orca-factory"))
	stdout := captureStdout(t, func() {
		if code := runProjects([]string{"--json"}); code != 0 {
			t.Fatalf("exit code = %d", code)
		}
	})
	var result projects.Result
	if err := json.Unmarshal(stdout, &result); err != nil {
		t.Fatalf("invalid JSON %s: %v", stdout, err)
	}
	if len(result.Eligible) != 0 || len(result.Rejected) != 1 || result.Rejected[0].Reason != projects.ReasonFactoryRootUnconfigured {
		t.Fatalf("result=%+v", result)
	}
}

func captureStdout(t *testing.T, run func()) []byte {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = original }()
	run()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
