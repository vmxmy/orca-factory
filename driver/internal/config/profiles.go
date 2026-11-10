package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

type profileFile struct {
	Default  string                       `toml:"default"`
	Profiles map[string]map[string]string `toml:"profile"`
}

// Cache each machine file for the process lifetime: repository discovery repeats,
// but changes to profiles.toml require a restart, including creation or removal.
var machineProfiles sync.Map

// ApplyProfile fills only keys absent from env, preserving explicitly empty
// assignments. Every profile is validated before any is applied. It is the single
// profile overlay shared by LoadRepository and driver exec.
func ApplyProfile(env map[string]string) (string, map[string]string, error) {
	selected := env["AGENT_PROFILE"]
	sources := make(map[string]string, len(env))
	for key := range env {
		sources[key] = "repository"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil, err
	}
	file, err := loadProfiles(home)
	if err != nil {
		return "", nil, err
	}
	if _, explicit := env["AGENT_PROFILE"]; !explicit {
		selected = file.Default
	}
	for key, value := range file.Profiles[selected] {
		if _, explicit := env[key]; !explicit {
			env[key] = value
			sources[key] = "profile:" + selected
		}
	}
	return selected, sources, nil
}

func loadProfiles(home string) (profileFile, error) {
	path := filepath.Join(home, ".config", "orca-factory", "profiles.toml")
	load, _ := machineProfiles.LoadOrStore(path, sync.OnceValues(func() (profileFile, error) { return readProfiles(path) }))
	return load.(func() (profileFile, error))()
}

func readProfiles(path string) (profileFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return profileFile{}, nil
	}
	if err != nil {
		return profileFile{}, errors.New("cannot read profiles.toml")
	}
	var file profileFile
	meta, err := toml.Decode(string(data), &file)
	if err != nil {
		return profileFile{}, errors.New("invalid profiles.toml")
	}
	if unknown := meta.Undecoded(); len(unknown) > 0 {
		return profileFile{}, fmt.Errorf("profiles.toml: unknown key %s", unknown[0])
	}
	for name, profile := range file.Profiles {
		for key, value := range profile {
			switch key {
			case "AGENT_TIER_DEFAULT", "AGENT_TIER_STRONG", "AGENT_ROLE_RESOLVER", "AGENT_ROLE_INTAKE":
				parts := strings.Split(value, ":")
				if len(parts) != 3 || parts[0] == "" {
					return profileFile{}, fmt.Errorf("profiles.toml: profile %s key %s requires agent:model:effort", name, key)
				}
			case "AGENT_TIER_ESCALATE":
				if value != "0" && value != "1" {
					return profileFile{}, fmt.Errorf("profiles.toml: profile %s key %s requires 0 or 1", name, key)
				}
			default:
				return profileFile{}, fmt.Errorf("profiles.toml: unknown key profile.%s.%s", name, key)
			}
		}
	}
	return file, nil
}
