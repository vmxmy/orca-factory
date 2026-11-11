package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

type profileFile struct {
	Default  string                       `toml:"default"`
	Profiles map[string]map[string]string `toml:"profile"`
}

// DefaultTierChain is the correction escalation chain when AGENT_TIER_CHAIN is unset.
const DefaultTierChain = "default strong"

var (
	tierName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	tierKey  = regexp.MustCompile(`^AGENT_TIER_[A-Z][A-Z0-9_]*$`)
)

// TierChain parses AGENT_TIER_CHAIN: space-separated tier names, each configured
// by AGENT_TIER_<upper name>. Unset or blank means DefaultTierChain.
func TierChain(value string) ([]string, error) {
	names := strings.Fields(value)
	if len(names) == 0 {
		names = strings.Fields(DefaultTierChain)
	}
	seen := map[string]bool{}
	for _, name := range names {
		if !tierName.MatchString(name) || name == "chain" || name == "escalate" || seen[name] {
			return nil, errors.New("AGENT_TIER_CHAIN requires distinct lowercase tier names")
		}
		seen[name] = true
	}
	return names, nil
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
	for _, key := range []string{"AGENT_ROLE_REVIEWER", "AGENT_ROLE_REVIEWER_SMALL", "AGENT_ROLE_SECOND_REVIEWER"} {
		if value, ok := env[key]; ok {
			if _, err := GenerateReviewerCommand(value); err != nil {
				return "", nil, fmt.Errorf("repository profile key %s: %w", key, err)
			}
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
			case "AGENT_ROLE_REVIEWER", "AGENT_ROLE_REVIEWER_SMALL", "AGENT_ROLE_SECOND_REVIEWER":
				if _, err := GenerateReviewerCommand(value); err != nil {
					return profileFile{}, fmt.Errorf("profiles.toml: profile %s key %s: %w", name, key, err)
				}
			case "AGENT_TIER_ESCALATE":
				if value != "0" && value != "1" {
					return profileFile{}, fmt.Errorf("profiles.toml: profile %s key %s requires 0 or 1", name, key)
				}
			case "AGENT_TIER_CHAIN":
				if _, err := TierChain(value); err != nil {
					return profileFile{}, fmt.Errorf("profiles.toml: profile %s key %s: %w", name, key, err)
				}
			default:
				if !tierKey.MatchString(key) {
					return profileFile{}, fmt.Errorf("profiles.toml: unknown key profile.%s.%s", name, key)
				}
				parts := strings.Split(value, ":")
				if len(parts) != 3 || parts[0] == "" {
					return profileFile{}, fmt.Errorf("profiles.toml: profile %s key %s requires agent:model:effort", name, key)
				}
			}
		}
	}
	return file, nil
}
