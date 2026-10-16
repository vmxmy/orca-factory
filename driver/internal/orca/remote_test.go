package orca

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNormalizeRemote covers OF-56's normalization rules: https and ssh
// URLs, a trailing ".git", and embedded credentials all reduce to the same
// host/owner/repo form CanonicalKey already uses.
func TestNormalizeRemote(t *testing.T) {
	cases := []struct {
		name, remote, want string
	}{
		{"https", "https://github.com/vmxmy/orca-factory", "github.com/vmxmy/orca-factory"},
		{"https with .git", "https://github.com/vmxmy/orca-factory.git", "github.com/vmxmy/orca-factory"},
		{"https with trailing slash", "https://github.com/vmxmy/orca-factory/", "github.com/vmxmy/orca-factory"},
		{"https with credentials", "https://x-access-token:TEST-PLACEHOLDER-CRED@github.com/vmxmy/orca-factory.git", "github.com/vmxmy/orca-factory"},
		{"ssh scp-like", "git@github.com:vmxmy/orca-factory.git", "github.com/vmxmy/orca-factory"},
		{"ssh scp-like no .git", "git@github.com:vmxmy/orca-factory", "github.com/vmxmy/orca-factory"},
		{"ssh url scheme", "ssh://git@github.com/vmxmy/orca-factory.git", "github.com/vmxmy/orca-factory"},
		{"ssh url scheme with port", "ssh://git@github.com:22/vmxmy/orca-factory.git", "github.com/vmxmy/orca-factory"},
		{"mixed case host", "https://GitHub.com/vmxmy/orca-factory.git", "github.com/vmxmy/orca-factory"},
		{"trailing whitespace", "  https://github.com/vmxmy/orca-factory.git\n", "github.com/vmxmy/orca-factory"},
		// OF-56's real-world case: dujiao-next's origin moved to a fork.
		{"real-world fork", "https://github.com/vmxmy/dujiao-next.git", "github.com/vmxmy/dujiao-next"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeRemote(tc.remote)
			if err != nil {
				t.Fatalf("NormalizeRemote(%q) err=%v", tc.remote, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeRemote(%q)=%q, want %q", tc.remote, got, tc.want)
			}
		})
	}
}

// TestNormalizeRemoteCredentialsNeverLeak proves a malformed or unrecognizable
// remote never echoes the raw input (which may carry a credential) back in
// the error.
func TestNormalizeRemoteCredentialsNeverLeak(t *testing.T) {
	cases := []string{
		"",
		"not a url at all",
		"https://x-access-token:TEST-PLACEHOLDER-CRED@github.com",
		"/local/path/with/no/host",
	}
	for _, remote := range cases {
		if _, err := NormalizeRemote(remote); err == nil {
			t.Fatalf("NormalizeRemote(%q) err=nil, want an error", remote)
		} else if strings.Contains(err.Error(), "SECRETVALUE") || (remote != "" && strings.Contains(err.Error(), remote)) {
			t.Fatalf("NormalizeRemote(%q) err=%q leaks the raw input", remote, err)
		}
	}
}

// TestRecordedDujiaoNextRemoteMismatch replays the real `orca-ide repo list
// --json` recording's dujiao-next entry alongside a real `git -C
// /home/laoxu/github/dujiao-next remote get-url origin` capture (OF-56):
// Orca's cached canonicalKey still names the old upstream, but the local
// checkout's origin was repointed to a fork, 2026-10-16.
func TestRecordedDujiaoNextRemoteMismatch(t *testing.T) {
	fixture, err := filepath.Abs("../../testdata/recorded/orca-repo-list.json")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "orca")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat '"+fixture+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	repos, err := (&Client{Path: script}).RepoList(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var canonicalKey string
	for _, r := range repos {
		if strings.HasSuffix(r.Path, "dujiao-next") {
			canonicalKey = r.CanonicalKey
		}
	}
	if canonicalKey == "" {
		t.Fatal("recorded sample has no dujiao-next entry")
	}
	liveRemote, err := os.ReadFile("../../testdata/recorded/git-remote-get-url-dujiao-next.txt")
	if err != nil {
		t.Fatal(err)
	}
	live, err := NormalizeRemote(string(liveRemote))
	if err != nil {
		t.Fatalf("NormalizeRemote(recorded live remote) err=%v", err)
	}
	if live == canonicalKey {
		t.Fatalf("live=%q equals canonicalKey=%q, want a mismatch (the real-world drift this ticket fixes)", live, canonicalKey)
	}
	if live != "github.com/vmxmy/dujiao-next" || canonicalKey != "github.com/dujiao-next/dujiao-next" {
		t.Fatalf("live=%q canonicalKey=%q, want the recorded fork mismatch", live, canonicalKey)
	}
}
