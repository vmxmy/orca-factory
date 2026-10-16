package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoWithOrigin creates a real local git repository whose origin remote is
// set to remote, without any network access: `git remote get-url origin`
// only reads local config.
func repoWithOrigin(t *testing.T, remote string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, path, "init", "--quiet", "-b", "main")
	gitIn(t, path, "remote", "add", "origin", remote)
	return path
}

// TestRemoteIdentityCheck is OF-56's preflight comparison: Orca's cached
// canonicalKey against a live `git remote get-url origin`, both normalized
// the same way.
func TestRemoteIdentityCheck(t *testing.T) {
	t.Run("agrees", func(t *testing.T) {
		path := repoWithOrigin(t, "https://github.com/vmxmy/dujiao-next.git")
		ok, detail := remoteIdentityCheck(context.Background(), path, "github.com/vmxmy/dujiao-next")
		if !ok || detail == "" {
			t.Fatalf("ok=%v detail=%q, want ok with a detail", ok, detail)
		}
	})

	t.Run("mismatch names both values", func(t *testing.T) {
		// The real-world case this ticket fixes: Orca's canonicalKey still
		// names the old upstream after the local origin moved to a fork.
		path := repoWithOrigin(t, "https://github.com/vmxmy/dujiao-next.git")
		ok, detail := remoteIdentityCheck(context.Background(), path, "github.com/dujiao-next/dujiao-next")
		if ok {
			t.Fatalf("ok=true, want a reported mismatch")
		}
		for _, want := range []string{"github.com/dujiao-next/dujiao-next", "github.com/vmxmy/dujiao-next"} {
			if !strings.Contains(detail, want) {
				t.Fatalf("detail=%q, want it to name %q", detail, want)
			}
		}
	})

	t.Run("mismatch never leaks credentials", func(t *testing.T) {
		path := repoWithOrigin(t, "https://x-access-token:TEST-PLACEHOLDER-CRED@github.com/attacker/other-repo.git")
		ok, detail := remoteIdentityCheck(context.Background(), path, "github.com/vmxmy/dujiao-next")
		if ok {
			t.Fatalf("ok=true, want a reported mismatch")
		}
		if strings.Contains(detail, "SECRETVALUE") || strings.Contains(detail, "TEST-PLACEHOLDER-CRED") {
			t.Fatalf("detail=%q leaks the credential", detail)
		}
		if !strings.Contains(detail, "github.com/attacker/other-repo") {
			t.Fatalf("detail=%q, want the normalized live value without credentials", detail)
		}
	})

	t.Run("no origin configured", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "repo")
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		gitIn(t, path, "init", "--quiet", "-b", "main")
		ok, detail := remoteIdentityCheck(context.Background(), path, "github.com/vmxmy/dujiao-next")
		if ok || detail == "" {
			t.Fatalf("ok=%v detail=%q, want a failure with a detail", ok, detail)
		}
	})

	t.Run("unnormalizable origin never echoes raw value", func(t *testing.T) {
		path := repoWithOrigin(t, "/local/bare/path/without/a/host")
		ok, detail := remoteIdentityCheck(context.Background(), path, "github.com/vmxmy/dujiao-next")
		if ok {
			t.Fatalf("ok=true, want a failure (origin does not normalize)")
		}
		if strings.Contains(detail, "/local/bare/path/without/a/host") {
			t.Fatalf("detail=%q echoes the raw unnormalizable origin", detail)
		}
	})
}

// TestCheckOnboardRepoAccessRemoteIdentity proves checkOnboardRepoAccess (the
// real preflight group (c), not a stand-in) wires the new comparison in: a
// fake gh confirms push access, and the target repository's origin does not
// normalize, so the "目标仓库" check fails without ever reaching a real
// network ls-remote.
func TestCheckOnboardRepoAccessRemoteIdentity(t *testing.T) {
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "gh"), `case "$1 $2" in
"api repos/vmxmy/dujiao-next") echo '{"permissions":{"push":true}}' ;;
*) exit 9 ;;
esac
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	path := repoWithOrigin(t, "/local/bare/path/without/a/host")
	plan := onboardPlan{Repo: path, Remote: "github.com/vmxmy/dujiao-next"}
	check := checkOnboardRepoAccess(context.Background(), onboarder{}, plan)
	if check.OK || check.Name != "目标仓库" || check.Detail == "" {
		t.Fatalf("check=%+v, want a failing 目标仓库 check with a detail", check)
	}
	if strings.Contains(check.Detail, "/local/bare/path/without/a/host") {
		t.Fatalf("check.Detail=%q echoes the raw origin", check.Detail)
	}
}

// A stale Orca canonicalKey must be reported before the push-permission
// probe: the probe targets the stale repository, so its failure (no push to
// someone else's upstream) would hide the real cause.
func TestCheckOnboardRepoAccessReportsMismatchBeforePush(t *testing.T) {
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "gh"), `case "$1 $2" in
"api repos/dujiao-next/dujiao-next") echo '{"permissions":{"push":false}}' ;;
*) exit 9 ;;
esac
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	path := repoWithOrigin(t, "https://github.com/vmxmy/dujiao-next.git")
	plan := onboardPlan{Repo: path, Remote: "github.com/dujiao-next/dujiao-next"}
	check := checkOnboardRepoAccess(context.Background(), onboarder{}, plan)
	if check.OK || !strings.Contains(check.Detail, "github.com/vmxmy/dujiao-next") || strings.Contains(check.Detail, "permissions.push") {
		t.Fatalf("check=%+v, want the Orca/origin mismatch reported instead of the push probe", check)
	}
}
