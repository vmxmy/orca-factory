package release

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func TestScanPinsBaseAndModVersion(t *testing.T) {
	now := time.Now()
	r := &Repository{Main: strings.Repeat("a", 40), Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
		switch s.Args[0] {
		case "ls-tree":
			if len(s.Args) > 2 && s.Args[1] == "-r" {
				return []byte("mods/demo/.claude-plugin/plugin.json\n"), nil
			}
			return []byte("100644 blob abc file"), nil
		case "log":
			if s.Args[1] == "-1" {
				return []byte("1"), nil
			}
			return []byte("fix: bug\x00\x00\n"), nil
		case "show":
			if strings.HasSuffix(s.Args[1], "plugin.json") {
				return []byte(`{"version":"1.0.0"}`), nil
			}
			if strings.HasSuffix(s.Args[1], "register.tsx") {
				return []byte("const MOD_VERSION = '1.0.0'\nkeep()\n"), nil
			}
			return nil, nil
		}
		t.Fatalf("unexpected %v", s.Args)
		return nil, nil
	}}
	plans, err := Scan(context.Background(), r, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatal(plans)
	}
	p := plans[0]
	if string(p.Files["mods/demo/hooks/register.tsx"]) != "const MOD_VERSION = '1.0.1'\nkeep()\n" {
		t.Fatalf("runtime version stale: %v", p.Files)
	}
}

func TestRecoverMergedPRBeforeEnqueue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `[]`
		if strings.HasSuffix(r.URL.Path, "/7") {
			body = `{"number":7,"merged":true,"head":{"ref":"release/demo-v1.0.1","repo":{"full_name":"o/r"}},"base":{"ref":"main","repo":{"full_name":"o/r"}}}`
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	gh, err := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &Repository{Name: "o/r", GitHub: gh, Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
		if s.Path == "gh" {
			return []byte(`[[{"number":7,"merged":true,"head":{"ref":"release/demo-v1.0.1","repo":{"full_name":"o/r"}},"base":{"ref":"main","repo":{"full_name":"o/r"}}}]]`), nil
		}
		return nil, errors.New("must recover without writing git")
	}}
	number, err := adapter.CreateReleasePR(context.Background(), merge.Request{Repository: "o/r"}, Plan{Plugin: "demo", Version: "1.0.1"})
	if err != nil || number != 7 {
		t.Fatalf("lost enqueue not recovered: PR=%d err=%v", number, err)
	}
}

func TestReleaseRejectsSymlinkFiles(t *testing.T) {
	r := &Repository{Main: strings.Repeat("a", 40), Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
		if s.Args[0] == "ls-tree" {
			return []byte("120000 blob abc\tmods/demo/CHANGELOG.md\n"), nil
		}
		return []byte("../../outside"), nil
	}}
	if _, err := r.ReadFile(context.Background(), "mods/demo/CHANGELOG.md"); err == nil {
		t.Fatal("symlink release file accepted")
	}
}
