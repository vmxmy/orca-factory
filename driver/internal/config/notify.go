package config

import (
	"errors"
	"os"
	"path/filepath"
)

// FeishuNotifySettings reads machine credentials and the optional notification
// chat. A repository chat overrides the machine default; credentials stay local.
func FeishuNotifySettings(env map[string]string) (map[string]string, error) {
	keys := []string{"AGENT_FEISHU_NOTIFY_CHAT", "AGENT_FEISHU_APP_ID", "AGENT_FEISHU_APP_SECRET", "AGENT_FEISHU_ENDPOINT"}
	settings := map[string]string{}
	if home, err := os.UserHomeDir(); err == nil {
		f, err := os.Open(filepath.Join(home, ".config", "orca-factory", "driver.env"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("notification driver.env could not be read")
		}
		if err == nil {
			values, parseErr := ParseEnv(f)
			closeErr := f.Close()
			if parseErr != nil || closeErr != nil {
				return nil, errors.New("notification driver.env is invalid")
			}
			for _, key := range keys {
				settings[key] = values[key]
			}
		}
	}
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			settings[key] = value
		}
	}
	if value, ok := env["AGENT_FEISHU_NOTIFY_CHAT"]; ok {
		settings["AGENT_FEISHU_NOTIFY_CHAT"] = value
	}
	return settings, nil
}
