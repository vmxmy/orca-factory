package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFeishuNotifySettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{"AGENT_FEISHU_NOTIFY_CHAT", "AGENT_FEISHU_APP_ID", "AGENT_FEISHU_APP_SECRET", "AGENT_FEISHU_ENDPOINT"} {
		old, ok := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if ok {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
	settings, err := FeishuNotifySettings(nil)
	if err != nil || settings["AGENT_FEISHU_NOTIFY_CHAT"] != "" {
		t.Fatalf("unset=%v %v", settings, err)
	}
	dir := filepath.Join(home, ".config", "orca-factory")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "driver.env"), []byte("AGENT_FEISHU_NOTIFY_CHAT=oc_file\nAGENT_FEISHU_APP_ID=cli_file\nAGENT_FEISHU_APP_SECRET=file-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	settings, err = FeishuNotifySettings(nil)
	if err != nil || settings["AGENT_FEISHU_NOTIFY_CHAT"] != "oc_file" || settings["AGENT_FEISHU_APP_ID"] != "cli_file" {
		t.Fatalf("file settings not loaded: %v", err)
	}
	t.Setenv("AGENT_FEISHU_NOTIFY_CHAT", "oc_env")
	settings, err = FeishuNotifySettings(nil)
	if err != nil || settings["AGENT_FEISHU_NOTIFY_CHAT"] != "oc_env" {
		t.Fatalf("environment not applied: %v", err)
	}
	settings, err = FeishuNotifySettings(map[string]string{"AGENT_FEISHU_NOTIFY_CHAT": "oc_repo", "AGENT_FEISHU_APP_SECRET": "repo-secret"})
	if err != nil || settings["AGENT_FEISHU_NOTIFY_CHAT"] != "oc_repo" || settings["AGENT_FEISHU_APP_SECRET"] != "file-secret" {
		t.Fatalf("repository overrides invalid: %v", err)
	}
	settings, err = FeishuNotifySettings(map[string]string{"AGENT_FEISHU_NOTIFY_CHAT": ""})
	if err != nil || settings["AGENT_FEISHU_NOTIFY_CHAT"] != "" {
		t.Fatalf("explicit disable ignored: %v", err)
	}
}
