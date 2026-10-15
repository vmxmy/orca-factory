package projects

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

// writeEnv installs path/scripts/agent-gates/agent-graph.env with the given
// content, so Discover's os.Stat and config.LoadRepository see a real file.
func writeEnv(t *testing.T, path, content string) {
	t.Helper()
	dir := filepath.Join(path, "scripts", "agent-gates")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-graph.env"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rejection(result Result, remote string) (Reason, bool) {
	for _, r := range result.Rejected {
		if r.Remote == remote {
			return r.Reason, true
		}
	}
	return "", false
}

func eligible(result Result, remote string) (Project, bool) {
	for _, p := range result.Eligible {
		if p.Remote == remote {
			return p, true
		}
	}
	return Project{}, false
}

// alwaysPass is the Checker for repos whose pipeline check succeeds.
func alwaysPass(context.Context, string) error { return nil }

// TestDiscoverRecordedSample replays the real `orca-ide repo list --json`
// recording (testdata/recorded/orca-repo-list.json, OF-42): 5 registered
// repositories. orca-factory, zhiyun-ai-dashboard and ansible have their own
// pipeline installed, pass check, declare a feishu backend and have an
// enabled ledger row; ziikoo-control-plane and dujiao-next have no pipeline
// installed at all, in a temp directory standing in for the local checkout
// so this test never reads a real repository on this machine.
func TestDiscoverRecordedSample(t *testing.T) {
	fixture, err := filepath.Abs("../../testdata/recorded/orca-repo-list.json")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "orca")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat '"+fixture+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	recorded, err := (&orca.Client{Path: script}).RepoList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 5 {
		t.Fatalf("recorded repos=%d, want 5", len(recorded))
	}
	root := t.TempDir()
	eligibleRemotes := map[string]bool{
		"github.com/vmxmy/orca-factory":        true,
		"github.com/vmxmy/zhiyun-ai-dashboard": true,
		"github.com/vmxmy/ansible":             true,
	}
	var repos []Repo
	var rows []issues.LedgerProject
	for _, r := range recorded {
		path := filepath.Join(root, filepath.Base(r.Path))
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if eligibleRemotes[r.CanonicalKey] {
			writeEnv(t, path, "AGENT_ISSUE_BACKEND=feishu\n")
			rows = append(rows, issues.LedgerProject{Name: filepath.Base(r.Path), Remote: r.CanonicalKey, Prefix: filepath.Base(r.Path), Enabled: true})
		}
		repos = append(repos, Repo{Path: path, CanonicalKey: r.CanonicalKey})
	}
	result := Discover(context.Background(), repos, rows, alwaysPass)
	if len(result.Eligible) != 3 {
		t.Fatalf("eligible=%+v, want 3", result.Eligible)
	}
	for remote := range eligibleRemotes {
		if _, ok := eligible(result, remote); !ok {
			t.Errorf("remote %s not eligible: %+v", remote, result)
		}
	}
	for _, remote := range []string{"github.com/vmxmy/ziikoo-control-plane", "github.com/dujiao-next/dujiao-next"} {
		reason, ok := rejection(result, remote)
		if !ok || reason != ReasonPipelineNotInstalled {
			t.Errorf("remote %s reason=%q ok=%v, want %q", remote, reason, ok, ReasonPipelineNotInstalled)
		}
	}
	if len(result.Rejected) != 2 {
		t.Fatalf("rejected=%+v, want 2", result.Rejected)
	}
}

func TestDiscoverProjectDisabled(t *testing.T) {
	path := t.TempDir()
	writeEnv(t, path, "AGENT_ISSUE_BACKEND=feishu\n")
	repos := []Repo{{Path: path, CanonicalKey: "github.com/example/disabled"}}
	rows := []issues.LedgerProject{{Name: "disabled", Remote: "github.com/example/disabled", Prefix: "DIS", Enabled: false}}
	result := Discover(context.Background(), repos, rows, alwaysPass)
	reason, ok := rejection(result, "github.com/example/disabled")
	if !ok || reason != ReasonProjectDisabled || len(result.Eligible) != 0 {
		t.Fatalf("result=%+v, want ReasonProjectDisabled", result)
	}
}

func TestDiscoverNoLedgerRow(t *testing.T) {
	path := t.TempDir()
	writeEnv(t, path, "AGENT_ISSUE_BACKEND=feishu\n")
	repos := []Repo{{Path: path, CanonicalKey: "github.com/example/orphan"}}
	rows := []issues.LedgerProject{{Name: "other", Remote: "github.com/example/other", Prefix: "OTH", Enabled: true}}
	result := Discover(context.Background(), repos, rows, alwaysPass)
	reason, ok := rejection(result, "github.com/example/orphan")
	if !ok || reason != ReasonNoLedgerRow || len(result.Eligible) != 0 {
		t.Fatalf("result=%+v, want ReasonNoLedgerRow", result)
	}
}

func TestDiscoverLedgerNotFeishu(t *testing.T) {
	path := t.TempDir()
	writeEnv(t, path, "AGENT_ISSUE_BACKEND=linear\n")
	repos := []Repo{{Path: path, CanonicalKey: "github.com/example/linear"}}
	rows := []issues.LedgerProject{{Name: "linear", Remote: "github.com/example/linear", Prefix: "LIN", Enabled: true}}
	result := Discover(context.Background(), repos, rows, alwaysPass)
	reason, ok := rejection(result, "github.com/example/linear")
	if !ok || reason != ReasonLedgerNotFeishu || len(result.Eligible) != 0 {
		t.Fatalf("result=%+v, want ReasonLedgerNotFeishu", result)
	}
}

func TestDiscoverCheckFailed(t *testing.T) {
	path := t.TempDir()
	writeEnv(t, path, "AGENT_ISSUE_BACKEND=feishu\n")
	repos := []Repo{{Path: path, CanonicalKey: "github.com/example/failing"}}
	rows := []issues.LedgerProject{{Name: "failing", Remote: "github.com/example/failing", Prefix: "FAI", Enabled: true}}
	failing := func(context.Context, string) error { return errors.New("gate red") }
	result := Discover(context.Background(), repos, rows, failing)
	reason, ok := rejection(result, "github.com/example/failing")
	if !ok || reason != ReasonCheckFailed || len(result.Eligible) != 0 {
		t.Fatalf("result=%+v, want ReasonCheckFailed", result)
	}
}

// TestDiscoverFactoryRootUnconfigured: a nil Checker means factory_root is
// not configured, and every repository is rejected for that reason alone,
// without even checking whether its pipeline is installed.
func TestDiscoverFactoryRootUnconfigured(t *testing.T) {
	repos := []Repo{{Path: "/anything", CanonicalKey: "github.com/example/anything"}}
	result := Discover(context.Background(), repos, nil, nil)
	reason, ok := rejection(result, "github.com/example/anything")
	if !ok || reason != ReasonFactoryRootUnconfigured || len(result.Eligible) != 0 {
		t.Fatalf("result=%+v, want ReasonFactoryRootUnconfigured", result)
	}
}
