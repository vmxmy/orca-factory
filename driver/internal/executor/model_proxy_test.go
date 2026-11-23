package executor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// driver exec runs in an Orca terminal that does not inherit the service
// EnvironmentFile, so the proxy reaches the reviewer through driver.env.
func TestReviewerGetsModelProxyFromConfig(t *testing.T) {
	reviewerEnv := func(t *testing.T, driverEnv string) []string {
		t.Helper()
		t.Setenv("AGENT_MODEL_PROXY_URL", "")
		t.Setenv("AGENT_MODEL_PROXY_TOKEN", "")
		home := t.TempDir()
		if driverEnv != "" {
			dir := filepath.Join(home, ".config", "orca-factory")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "driver.env"), []byte(driverEnv), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		proxy, err := config.ModelProxyURL(home)
		if err != nil {
			t.Fatal(err)
		}
		var env []string
		c := &Commands{ReviewerCommand: "claude -p", ModelProxyURL: proxy, Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
			if s.Path == "/bin/sh" {
				env = s.Env
			}
			return []byte(`{"verdict":"green","step":"review"}`), nil
		}}
		c.Run = withReviewerRole(t, c.Run)
		if _, _, err := c.Review(context.Background(), t.TempDir(), "origin/main", "HEAD", nil); err != nil {
			t.Fatal(err)
		}
		return env
	}
	t.Run("set", func(t *testing.T) {
		env := reviewerEnv(t, "AGENT_FEISHU_APP_ID=TEST-PLACEHOLDER-CRED\nAGENT_MODEL_PROXY_URL=http://proxy.test:8080\n")
		if len(env) == 0 || env[len(env)-1] != "ANTHROPIC_BASE_URL=http://proxy.test:8080" {
			t.Fatalf("reviewer env must end with the proxy: %q", env)
		}
	})
	t.Run("unset inherits the environment unchanged", func(t *testing.T) {
		if env := reviewerEnv(t, ""); env != nil {
			t.Fatalf("reviewer env = %q; want nil (inherit)", env)
		}
	})
}

func TestModelRelayReviewerEnv(t *testing.T) {
	const token = "fake-of646-reviewer-token"
	const proxyURL = "http://proxy.test:8080"
	t.Setenv("AGENT_MODEL_PROXY_TOKEN", token)
	t.Setenv("OF646_KEEP", "1")

	smallEstimate := 2.0
	for _, tc := range []struct {
		name     string
		proxyURL string
		estimate *float64
		wantTier string
	}{
		{"reviewer with URL", proxyURL, nil, "default"},
		{"reviewer without URL", "", nil, "default"},
		{"Codex reviewer_small", proxyURL, &smallEstimate, "small"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var env []string
			c := &Commands{
				ReviewerCommand:      "claude -p",
				ReviewerSmallCommand: "codex exec -",
				ModelProxyURL:        tc.proxyURL,
				Run: withReviewerRole(t, func(_ context.Context, s runx.Spec) ([]byte, error) {
					if s.Path == "/bin/sh" {
						env = s.Env
						return []byte(`{"verdict":"green","step":"review"}`), nil
					}
					return nil, nil
				}),
			}
			_, tier, err := c.Review(context.Background(), t.TempDir(), "origin/main", "HEAD", tc.estimate)
			if err != nil {
				t.Fatal(err)
			}
			if tier != tc.wantTier {
				t.Fatalf("review tier = %q, want %q", tier, tc.wantTier)
			}
			if env == nil {
				t.Fatal("reviewer inherits the process environment")
			}
			for _, entry := range env {
				key, _, _ := strings.Cut(entry, "=")
				if key == "AGENT_MODEL_PROXY_TOKEN" || strings.Contains(entry, token) {
					t.Fatal("reviewer environment contains the relay credential")
				}
			}
			if !slices.Contains(env, "OF646_KEEP=1") {
				t.Fatal("reviewer environment lost OF646_KEEP")
			}
			wantCount := len(os.Environ()) - 1
			if tc.proxyURL != "" {
				wantCount++
				if len(env) == 0 || env[len(env)-1] != "ANTHROPIC_BASE_URL="+tc.proxyURL {
					t.Fatal("last reviewer environment entry must set the configured proxy URL")
				}
			}
			if len(env) != wantCount {
				t.Fatalf("reviewer environment has %d entries, want %d", len(env), wantCount)
			}
		})
	}
}
