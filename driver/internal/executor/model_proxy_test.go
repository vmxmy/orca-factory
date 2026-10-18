package executor

import (
	"context"
	"os"
	"path/filepath"
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
