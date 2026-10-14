package issues

import (
	"os"
	"path/filepath"
	"testing"
)

// Executors start in Orca terminals without the service EnvironmentFile, so
// the app credentials fall back to the machine driver.env.
func TestFeishuMachineSettingsFallBackToDriverEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "orca-factory")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "driver.env"), []byte("AGENT_FEISHU_APP_ID=cli_file\nAGENT_FEISHU_APP_SECRET=file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"AGENT_FEISHU_APP_ID", "AGENT_FEISHU_APP_SECRET", "AGENT_FEISHU_ENDPOINT"} {
		t.Setenv(k, "")
	}
	got, err := feishuMachineSettings()
	if err != nil || got["AGENT_FEISHU_APP_ID"] != "cli_file" || got["AGENT_FEISHU_APP_SECRET"] != "file-secret" || got["AGENT_FEISHU_ENDPOINT"] != "" {
		t.Fatalf("file fallback: err=%v", err)
	}
	t.Setenv("AGENT_FEISHU_APP_ID", "cli_env")
	t.Setenv("AGENT_FEISHU_APP_SECRET", "env-secret")
	got, err = feishuMachineSettings()
	if err != nil || got["AGENT_FEISHU_APP_ID"] != "cli_env" || got["AGENT_FEISHU_APP_SECRET"] != "env-secret" {
		t.Fatalf("process environment must win: err=%v", err)
	}
}
