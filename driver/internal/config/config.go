// Package config reads machine settings and repository profiles as data.
package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

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
	Profile     string
	Sources     map[string]string
}
type Config struct {
	MaxInFlight int
	// FactoryRoot is this machine's orca-factory production checkout, whose
	// bin/orca-factory the projects package runs to check a candidate
	// repository's pipeline (OF-42). Empty when unconfigured.
	FactoryRoot string
	TokenBucket TokenBucket
	Tuning      Tuning
}

// Load reads ~/.config/orca-factory/driver.toml relative to the supplied home.
// A missing machine file yields default limits. It lists no repositories:
// serve discovers them (projects.Discover, OF-43).
func Load(home string) (Config, error) {
	if _, err := loadProfiles(home); err != nil {
		return Config{}, err
	}
	raw := struct {
		MaxInFlight int         `toml:"max_in_flight"`
		FactoryRoot string      `toml:"factory_root"`
		TokenBucket TokenBucket `toml:"token_bucket"`
		Scheduler   *Scheduler  `toml:"scheduler"`
		GitHub      *GitHub     `toml:"github"`
		Orca        *Orca       `toml:"orca"`
		Ledger      *Ledger     `toml:"ledger"`
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
	result := Config{MaxInFlight: raw.MaxInFlight, TokenBucket: raw.TokenBucket, Tuning: Tuning{
		Scheduler: DefaultScheduler(),
		GitHub:    DefaultGitHub(),
		Orca:      DefaultOrca(),
		Ledger:    DefaultLedger(),
	}}
	// A named section overrides only the fields it names; the rest keep defaults.
	if raw.Scheduler != nil {
		result.Tuning.Scheduler = fillScheduler(*raw.Scheduler, result.Tuning.Scheduler)
	}
	if raw.GitHub != nil {
		result.Tuning.GitHub = fillGitHub(*raw.GitHub, result.Tuning.GitHub)
	}
	if raw.Orca != nil {
		result.Tuning.Orca = fillOrca(*raw.Orca, result.Tuning.Orca)
	}
	if raw.Ledger != nil {
		result.Tuning.Ledger = fillLedger(*raw.Ledger, result.Tuning.Ledger)
	}
	if raw.FactoryRoot != "" {
		root := raw.FactoryRoot
		if strings.HasPrefix(root, "~/") {
			root = filepath.Join(home, root[2:])
		}
		if !filepath.IsAbs(root) {
			return Config{}, errors.New("factory root path must be absolute")
		}
		result.FactoryRoot = filepath.Clean(root)
	}
	return result, nil
}

// LoadRegisteredRepository resolves the profile of the Orca repository
// registered under remote and applies its machine-local gear override. The
// override applies only when remote is the profile's Feishu ledger project, so
// a registration that drifted from the tickets this checkout serves, or a
// checkout on another ticket backend, has none.
func LoadRegisteredRepository(remote, path string) (Repository, error) {
	repo, err := LoadRepository(path)
	if err != nil {
		return Repository{}, err
	}
	if remote == "" || repo.Env["AGENT_ISSUE_BACKEND"] != "feishu" || remote != repo.Env["AGENT_FEISHU_PROJECT"] {
		return repo, nil
	}
	if err := applyGear(&repo, remote); err != nil {
		return Repository{}, err
	}
	return repo, nil
}

// LoadRepository reads one repository's own pipeline profile
// (scripts/agent-gates/agent-graph.env) at the given absolute, cleaned path,
// returning its resolved limits and full environment. Used by the projects
// package's discovery (OF-42) and by commands run at a repository root.
func LoadRepository(path string) (Repository, error) {
	f, err := os.Open(filepath.Join(path, "scripts", "agent-gates", "agent-graph.env"))
	if err != nil {
		return Repository{}, errors.New("cannot read repository profile")
	}
	env, parseErr := ParseEnv(f)
	closeErr := f.Close()
	if parseErr != nil || closeErr != nil {
		return Repository{}, errors.New("invalid repository profile")
	}
	if mode, explicit := env["AGENT_MODE"]; explicit && mode != "manual" && mode != "auto" {
		return Repository{}, errors.New("AGENT_MODE must be manual or auto")
	}
	limit, err := positive(env, "AGENT_MAX_IN_FLIGHT", 4)
	if err != nil {
		return Repository{}, err
	}
	stale, err := positive(env, "AGENT_STALE_HOURS", 8)
	if err != nil {
		return Repository{}, err
	}
	profile, sources, err := ApplyProfile(env)
	if err != nil {
		return Repository{}, err
	}
	return Repository{Path: path, MaxInFlight: limit, StaleHours: stale, Env: env, Profile: profile, Sources: sources}, nil
}

type gearOverride struct {
	Path      string    `json:"path"`
	Mode      string    `json:"mode"`
	Operator  string    `json:"operator"`
	Reason    string    `json:"reason"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func gearPath() (string, error) {
	root := os.Getenv("AGENT_DRIVER_STATE_ROOT")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".local", "state", "orca-factory")
	}
	return filepath.Join(root, "gear.json"), nil
}

func readGears(file string) (map[string]json.RawMessage, error) {
	entries := make(map[string]json.RawMessage)
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return entries, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	if entries == nil {
		return nil, errors.New("gear.json must contain an object")
	}
	return entries, nil
}

// HasGearOverride reports whether the machine-local gear.json holds an entry
// for repo's Feishu ledger project, the only key an override can use. A
// checkout on another ticket backend has none and does not read the file.
func HasGearOverride(repo Repository) (bool, error) {
	remote := repo.Env["AGENT_FEISHU_PROJECT"]
	if remote == "" || repo.Env["AGENT_ISSUE_BACKEND"] != "feishu" {
		return false, nil
	}
	file, err := gearPath()
	if err != nil {
		return false, err
	}
	entries, err := readGears(file)
	_, ok := entries[remote]
	return ok, err
}

func applyGear(repo *Repository, remote string) error {
	file, err := gearPath()
	if err != nil {
		return err
	}
	entries, err := readGears(file)
	if err != nil {
		return err
	}
	raw, ok := entries[remote]
	if !ok {
		return nil
	}
	var fields map[string]json.RawMessage
	var mode string
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(fields["mode"], &mode) != nil || (mode != "auto" && mode != "manual") {
		return errors.New("gear.json AGENT_MODE must be manual or auto")
	}
	repo.Env["AGENT_MODE"] = mode
	repo.Sources["AGENT_MODE"] = "override"
	return nil
}

// json.Encoder reformats RawMessage values. Keep other repositories' entries byte-for-byte.
func writeGears(w io.Writer, gears map[string]json.RawMessage) error {
	members := make([]string, 0, len(gears))
	for remote, raw := range gears {
		key, err := json.Marshal(remote)
		if err != nil {
			return err
		}
		members = append(members, string(key)+":"+string(raw))
	}
	slices.Sort(members)
	_, err := io.WriteString(w, "{"+strings.Join(members, ",")+"}\n")
	return err
}

// SetGearOverride sets a machine-local mode; an empty mode clears the override.
func SetGearOverride(remote, path, mode, operator, reason string) (err error) {
	if mode != "" && mode != "manual" && mode != "auto" {
		return errors.New("AGENT_MODE must be manual or auto")
	}
	if strings.TrimSpace(remote) == "" || path == "" || strings.TrimSpace(operator) == "" || strings.TrimSpace(reason) == "" {
		return errors.New("gear change requires remote, path, operator and reason")
	}
	path = filepath.Clean(path)
	if mode != "" {
		repo, err := LoadRepository(path)
		if err != nil {
			return err
		}
		if repo.Env["AGENT_ISSUE_BACKEND"] != "feishu" {
			return fmt.Errorf("gear: %s does not use the feishu ticket backend", path)
		}
		if repo.Env["AGENT_FEISHU_PROJECT"] != remote {
			return fmt.Errorf("gear: %s is not the ledger project AGENT_FEISHU_PROJECT of %s", remote, path)
		}
	}
	file, err := gearPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(file+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	gears, err := readGears(file)
	if err != nil {
		return err
	}
	delete(gears, remote)
	if mode != "" {
		gears[remote], err = json.Marshal(gearOverride{Path: path, Mode: mode, Operator: operator, Reason: reason, UpdatedAt: time.Now().UTC()})
		if err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".gear-*")
	if err != nil {
		return err
	}
	defer func() {
		if removeErr := os.Remove(tmp.Name()); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	writeErr := writeGears(tmp, gears)
	if writeErr == nil {
		writeErr = tmp.Sync()
	}
	if err := errors.Join(writeErr, tmp.Close()); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return err
	}
	log.Printf("gear remote=%q path=%q mode=%q operator=%q reason=%q", remote, path, mode, operator, reason)
	return nil
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

// ModelProxyURL returns AGENT_MODEL_PROXY_URL from the process environment,
// falling back to the machine driver.env: executors start in Orca terminals that
// do not inherit the service EnvironmentFile. Empty means no proxy.
func ModelProxyURL(home string) (string, error) {
	value := os.Getenv("AGENT_MODEL_PROXY_URL")
	if value == "" {
		f, err := os.Open(filepath.Join(home, ".config", "orca-factory", "driver.env"))
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", errors.New("driver.env could not be read")
		}
		env, parseErr := ParseEnv(f)
		closeErr := f.Close()
		if parseErr != nil || closeErr != nil {
			return "", errors.New("driver.env is invalid")
		}
		value = env["AGENT_MODEL_PROXY_URL"]
	}
	if value == "" {
		return "", nil
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("AGENT_MODEL_PROXY_URL must be an http(s) URL")
	}
	return value, nil
}

var assignment = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

// AssignedKeys reports the keys ParseEnv reads as explicit, so driver exec and
// LoadRepository agree. Shell statements such as export stay ignored, as in
// ParseEnv. Values are not read, so bash-only quoting does not fail it.
func AssignedKeys(data []byte) map[string]bool {
	keys := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if match := assignment.FindStringSubmatch(strings.TrimSpace(line)); match != nil {
			keys[match[1]] = true
		}
	}
	return keys
}

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

// Scheduler holds the scheduler's tunable knobs. A zero field keeps the built-in default.
type Scheduler struct {
	UnregisteredGrace time.Duration `toml:"unregistered_grace"`
}

// DefaultScheduler returns the built-in scheduler defaults.
func DefaultScheduler() Scheduler {
	return Scheduler{UnregisteredGrace: 3 * time.Minute}
}

// GitHub holds the GitHub client's tunables.
type GitHub struct {
	HTTPTimeout time.Duration `toml:"http_timeout"`
}

// DefaultGitHub returns the built-in GitHub defaults.
func DefaultGitHub() GitHub { return GitHub{HTTPTimeout: 30 * time.Second} }

// Orca holds the Orca runner's tunables.
type Orca struct {
	WorkerStartWait time.Duration `toml:"worker_start_wait"`
	CallTimeout     time.Duration `toml:"call_timeout"`
}

// DefaultOrca returns the built-in Orca defaults.
func DefaultOrca() Orca {
	return Orca{WorkerStartWait: 5 * time.Minute, CallTimeout: 30 * time.Second}
}

// Ledger holds the ledger's tunables.
type Ledger struct {
	FlushInterval time.Duration `toml:"flush_interval"`
}

// DefaultLedger returns the built-in ledger defaults.
func DefaultLedger() Ledger { return Ledger{FlushInterval: 10 * time.Minute} }

// Tuning bundles every tunable subsystem. The raw TOML may name only the sections it overrides;
// Load fills the rest with the built-in defaults.
type Tuning struct {
	Scheduler Scheduler
	GitHub    GitHub
	Orca      Orca
	Ledger    Ledger
}

// fillScheduler returns want with every zero field of over taking its value.
func fillScheduler(over, want Scheduler) Scheduler {
	if over.UnregisteredGrace != 0 {
		want.UnregisteredGrace = over.UnregisteredGrace
	}
	return want
}

func fillGitHub(over, want GitHub) GitHub {
	if over.HTTPTimeout != 0 {
		want.HTTPTimeout = over.HTTPTimeout
	}
	return want
}

func fillOrca(over, want Orca) Orca {
	if over.WorkerStartWait != 0 {
		want.WorkerStartWait = over.WorkerStartWait
	}
	if over.CallTimeout != 0 {
		want.CallTimeout = over.CallTimeout
	}
	return want
}

func fillLedger(over, want Ledger) Ledger {
	if over.FlushInterval != 0 {
		want.FlushInterval = over.FlushInterval
	}
	return want
}
