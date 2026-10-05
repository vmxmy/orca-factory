// Package config reads machine settings and repository profiles as data.
package config

import (
	"bufio"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

type TokenBucket struct {
	Capacity        int     `toml:"capacity"`
	RefillPerSecond float64 `toml:"refill_per_second"`
}
type Repository struct {
	Path        string
	MaxInFlight int
	StaleHours  int
	Env         map[string]string
}
type Config struct {
	MaxInFlight  int
	Repositories []Repository
	TokenBucket  TokenBucket
}

// Load reads ~/.config/orca-factory/driver.toml relative to the supplied home.
// A missing machine file is an empty repository list with default limits.
func Load(home string) (Config, error) {
	raw := struct {
		MaxInFlight  int         `toml:"max_in_flight"`
		Repositories []string    `toml:"repositories"`
		TokenBucket  TokenBucket `toml:"token_bucket"`
	}{MaxInFlight: 4, TokenBucket: TokenBucket{Capacity: 4, RefillPerSecond: 1}}
	path := filepath.Join(home, ".config", "orca-factory", "driver.toml")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, errors.New("cannot read machine configuration")
	}
	if err == nil {
		if _, err = toml.Decode(string(data), &raw); err != nil {
			return Config{}, errors.New("invalid machine configuration")
		}
	}
	if raw.MaxInFlight < 1 || raw.TokenBucket.Capacity < 1 || raw.TokenBucket.RefillPerSecond <= 0 || math.IsNaN(raw.TokenBucket.RefillPerSecond) || math.IsInf(raw.TokenBucket.RefillPerSecond, 0) {
		return Config{}, errors.New("configuration limits must be positive")
	}
	result := Config{MaxInFlight: raw.MaxInFlight, TokenBucket: raw.TokenBucket}
	seen := map[string]bool{}
	for _, repo := range raw.Repositories {
		if strings.HasPrefix(repo, "~/") {
			repo = filepath.Join(home, repo[2:])
		}
		if !filepath.IsAbs(repo) {
			return Config{}, errors.New("repository path must be absolute")
		}
		repo = filepath.Clean(repo)
		if seen[repo] {
			return Config{}, errors.New("duplicate repository")
		}
		seen[repo] = true
		f, err := os.Open(filepath.Join(repo, "scripts", "agent-gates", "agent-graph.env"))
		if err != nil {
			return Config{}, errors.New("cannot read repository profile")
		}
		env, parseErr := ParseEnv(f)
		closeErr := f.Close()
		if parseErr != nil || closeErr != nil {
			return Config{}, errors.New("invalid repository profile")
		}
		limit, err := positive(env, "AGENT_MAX_IN_FLIGHT", 4)
		if err != nil {
			return Config{}, err
		}
		stale, err := positive(env, "AGENT_STALE_HOURS", 8)
		if err != nil {
			return Config{}, err
		}
		result.Repositories = append(result.Repositories, Repository{Path: repo, MaxInFlight: limit, StaleHours: stale, Env: env})
	}
	return result, nil
}
func positive(env map[string]string, key string, fallback int) (int, error) {
	value, ok := env[key]
	if !ok {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return 0, errors.New("repository limit must be positive")
	}
	return n, nil
}

var assignment = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

// ParseEnv accepts only KEY=value assignments. Quotes delimit literal strings;
// substitutions and shell statements are never evaluated. A # outside quotes
// starts a comment when preceded by whitespace (or at the start of the value).
func ParseEnv(r io.Reader) (map[string]string, error) {
	values := map[string]string{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		match := assignment.FindStringSubmatch(strings.TrimSpace(scanner.Text()))
		if match == nil {
			continue
		}
		value := strings.TrimSpace(match[2])
		if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
			end := strings.IndexByte(value[1:], value[0])
			if end < 0 {
				return nil, errors.New("unterminated profile quote")
			}
			end++
			suffix := strings.TrimSpace(value[end+1:])
			if suffix != "" && !strings.HasPrefix(suffix, "#") {
				return nil, errors.New("invalid text after profile value")
			}
			value = value[1:end]
		} else {
			for i, c := range value {
				if c == '#' && (i == 0 || value[i-1] == ' ' || value[i-1] == '\t') {
					value = value[:i]
					break
				}
			}
			value = strings.TrimSpace(value)
		}
		values[match[1]] = value
	}
	if scanner.Err() != nil {
		return nil, errors.New("cannot scan repository profile")
	}
	return values, nil
}
