package projects

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
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
func alwaysPass(context.Context, string) ([]string, error) { return nil, nil }

// TestDiscoverRecordedSample replays the real `orca-ide repo list --json`
// recording (testdata/recorded/orca-repo-list.json, OF-42): 5 registered
// repositories. orca-factory, zhiyun-ai-dashboard and ansible have their own
// pipeline installed, pass check, declare a feishu backend and have an
// enabled ledger row; ziikoo-control-plane and dujiao-next have no
// scripts/agent-gates/ directory at all, in a temp directory standing in for
// the local checkout so this test never reads a real repository on this
// machine, so OF-49 marks them ReasonOnboardable rather than
// ReasonPipelineNotInstalled.
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
	var repos []orca.Repo
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
		repos = append(repos, orca.Repo{Path: path, CanonicalKey: r.CanonicalKey})
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
		if !ok || reason != ReasonOnboardable {
			t.Errorf("remote %s reason=%q ok=%v, want %q", remote, reason, ok, ReasonOnboardable)
		}
	}
	if len(result.Rejected) != 2 {
		t.Fatalf("rejected=%+v, want 2", result.Rejected)
	}
}

func TestDiscoverProjectDisabled(t *testing.T) {
	path := t.TempDir()
	writeEnv(t, path, "AGENT_ISSUE_BACKEND=feishu\n")
	repos := []orca.Repo{{Path: path, CanonicalKey: "github.com/example/disabled"}}
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
	repos := []orca.Repo{{Path: path, CanonicalKey: "github.com/example/orphan"}}
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
	repos := []orca.Repo{{Path: path, CanonicalKey: "github.com/example/linear"}}
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
	repos := []orca.Repo{{Path: path, CanonicalKey: "github.com/example/failing"}}
	rows := []issues.LedgerProject{{Name: "failing", Remote: "github.com/example/failing", Prefix: "FAI", Enabled: true}}
	failing := func(context.Context, string) ([]string, error) { return nil, errors.New("gate red") }
	result := Discover(context.Background(), repos, rows, failing)
	reason, ok := rejection(result, "github.com/example/failing")
	if !ok || reason != ReasonCheckFailed || len(result.Eligible) != 0 {
		t.Fatalf("result=%+v, want ReasonCheckFailed", result)
	}
}

// TestDiscoverCoreBehindStillEligible is OF-53: a repository whose installed
// core copy only lags the factory (every differing file is a released core
// version) stays eligible and carries the lagging files.
func TestDiscoverCoreBehindStillEligible(t *testing.T) {
	path := t.TempDir()
	writeEnv(t, path, "AGENT_ISSUE_BACKEND=feishu\n")
	repos := []orca.Repo{{Path: path, CanonicalKey: "github.com/example/behind"}}
	rows := []issues.LedgerProject{{Name: "behind", Remote: "github.com/example/behind", Prefix: "BEH", Enabled: true}}
	behind := func(context.Context, string) ([]string, error) {
		return []string{"install.sh", "roles/onboard.md"}, nil
	}
	result := Discover(context.Background(), repos, rows, behind)
	p, ok := eligible(result, "github.com/example/behind")
	if !ok || len(result.Rejected) != 0 || !slices.Equal(p.CoreBehind, []string{"install.sh", "roles/onboard.md"}) {
		t.Fatalf("result=%+v, want eligible with CoreBehind", result)
	}
}

// TestDiscoverOnboardable is OF-49: a repository Orca knows about whose
// scripts/agent-gates/ directory does not exist at all (never onboarded) is
// rejected as ReasonOnboardable, not ReasonPipelineNotInstalled.
func TestDiscoverOnboardable(t *testing.T) {
	path := t.TempDir()
	repos := []orca.Repo{{Path: path, CanonicalKey: "github.com/example/never-onboarded"}}
	result := Discover(context.Background(), repos, nil, alwaysPass)
	reason, ok := rejection(result, "github.com/example/never-onboarded")
	if !ok || reason != ReasonOnboardable || len(result.Eligible) != 0 {
		t.Fatalf("result=%+v, want ReasonOnboardable", result)
	}
}

// TestDiscoverPartialInstallNotOnboardable: scripts/agent-gates/ already
// exists (someone started onboarding) but agent-graph.env itself is missing.
// This stays ReasonPipelineNotInstalled, not ReasonOnboardable, because the
// directory's existence means the repository is not untouched.
func TestDiscoverPartialInstallNotOnboardable(t *testing.T) {
	path := t.TempDir()
	if err := os.MkdirAll(filepath.Join(path, "scripts", "agent-gates"), 0o755); err != nil {
		t.Fatal(err)
	}
	repos := []orca.Repo{{Path: path, CanonicalKey: "github.com/example/partial"}}
	result := Discover(context.Background(), repos, nil, alwaysPass)
	reason, ok := rejection(result, "github.com/example/partial")
	if !ok || reason != ReasonPipelineNotInstalled || len(result.Eligible) != 0 {
		t.Fatalf("result=%+v, want ReasonPipelineNotInstalled", result)
	}
}

// TestDiscoverCheckFailedIsNotOnboardable is the ticket's other half: a
// fully installed repository (scripts/agent-gates/agent-graph.env present)
// whose own pipeline check fails stays ReasonCheckFailed, never
// ReasonOnboardable, even though it is also not eligible right now.
func TestDiscoverCheckFailedIsNotOnboardable(t *testing.T) {
	path := t.TempDir()
	writeEnv(t, path, "AGENT_ISSUE_BACKEND=feishu\n")
	repos := []orca.Repo{{Path: path, CanonicalKey: "github.com/example/installed-failing"}}
	failing := func(context.Context, string) ([]string, error) { return nil, errors.New("gate red") }
	result := Discover(context.Background(), repos, nil, failing)
	reason, ok := rejection(result, "github.com/example/installed-failing")
	if !ok || reason != ReasonCheckFailed || len(result.Eligible) != 0 {
		t.Fatalf("result=%+v, want ReasonCheckFailed, not ReasonOnboardable", result)
	}
}

// TestDiscoverFactoryRootUnconfigured: a nil Checker means factory_root is
// not configured, and every repository is rejected for that reason alone,
// without even checking whether its pipeline is installed.
func TestDiscoverFactoryRootUnconfigured(t *testing.T) {
	repos := []orca.Repo{{Path: "/anything", CanonicalKey: "github.com/example/anything"}}
	result := Discover(context.Background(), repos, nil, nil)
	reason, ok := rejection(result, "github.com/example/anything")
	if !ok || reason != ReasonFactoryRootUnconfigured || len(result.Eligible) != 0 {
		t.Fatalf("result=%+v, want ReasonFactoryRootUnconfigured", result)
	}
}

// containsPath reports whether repos includes one whose Path is path.
func containsPath(repos []config.Repository, path string) bool {
	for _, r := range repos {
		if r.Path == path {
			return true
		}
	}
	return false
}

// TestReadableRepositories is OF-44's manual-recovery criterion: unlike
// Discover, it never consults the ledger project table or a pipeline check,
// so a repository stays findable after it is moved out of the eligible set.
func TestReadableRepositories(t *testing.T) {
	root := t.TempDir()

	noProfile := filepath.Join(root, "no-profile")
	if err := os.MkdirAll(noProfile, 0o755); err != nil {
		t.Fatal(err)
	}

	linearBackend := filepath.Join(root, "linear-backend")
	writeEnv(t, linearBackend, "AGENT_ISSUE_BACKEND=linear\n")

	emptyBase := filepath.Join(root, "empty-base")
	writeEnv(t, emptyBase, "AGENT_ISSUE_BACKEND=feishu\nAGENT_FEISHU_BASE=\n")

	readable := filepath.Join(root, "readable")
	writeEnv(t, readable, "AGENT_ISSUE_BACKEND=feishu\nAGENT_FEISHU_BASE=BASE_TOKEN\n")

	repos := []orca.Repo{
		{Path: noProfile, CanonicalKey: "github.com/example/no-profile"},
		{Path: linearBackend, CanonicalKey: "github.com/example/linear-backend"},
		{Path: emptyBase, CanonicalKey: "github.com/example/empty-base"},
		{Path: readable, CanonicalKey: "github.com/example/readable"},
	}
	result := ReadableRepositories(repos)
	if len(result) != 1 || !containsPath(result, readable) {
		t.Fatalf("ReadableRepositories=%+v, want only %s", result, readable)
	}
}
