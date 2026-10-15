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
}
type Config struct {
	MaxInFlight  int
	Repositories []Repository
	// FactoryRoot is this machine's orca-factory production checkout, whose
	// bin/orca-factory the projects package runs to check a candidate
	// repository's pipeline (OF-42). Empty when unconfigured.
	FactoryRoot string
	TokenBucket TokenBucket
	Tuning      Tuning
}

// Load reads ~/.config/orca-factory/driver.toml relative to the supplied home.
// A missing machine file is an empty repository list with default limits.
func Load(home string) (Config, error) {
	raw := struct {
		MaxInFlight  int         `toml:"max_in_flight"`
		Repositories []string    `toml:"repositories"`
		FactoryRoot  string      `toml:"factory_root"`
		TokenBucket  TokenBucket `toml:"token_bucket"`
		Scheduler    *Scheduler  `toml:"scheduler"`
		Linear       *Linear     `toml:"linear"`
		GitHub       *GitHub     `toml:"github"`
		Orca         *Orca       `toml:"orca"`
		Ledger       *Ledger     `toml:"ledger"`
		Status       *Status     `toml:"status"`
		Runx         *Runx       `toml:"runx"`
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
		Linear:    DefaultLinear(),
		GitHub:    DefaultGitHub(),
		Orca:      DefaultOrca(),
		Ledger:    DefaultLedger(),
		Status:    DefaultStatus(),
		Runx:      DefaultRunx(),
	}}
	// A named section overrides only the fields it names; the rest keep defaults.
	if raw.Scheduler != nil {
		result.Tuning.Scheduler = fillScheduler(*raw.Scheduler, result.Tuning.Scheduler)
	}
	if raw.Linear != nil {
		result.Tuning.Linear = fillLinear(*raw.Linear, result.Tuning.Linear)
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
	if raw.Status != nil {
		result.Tuning.Status = fillStatus(*raw.Status, result.Tuning.Status)
	}
	if raw.Runx != nil {
		result.Tuning.Runx = fillRunx(*raw.Runx, result.Tuning.Runx)
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
		repository, err := LoadRepository(repo)
		if err != nil {
			return Config{}, err
		}
		result.Repositories = append(result.Repositories, repository)
	}
	return result, nil
}

// LoadRepository reads one repository's own pipeline profile
// (scripts/agent-gates/agent-graph.env) at the given absolute, cleaned path,
// returning its resolved limits and full environment. Shared by Load's
// driver.toml repositories list and the projects package's discovery (OF-42).
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
	limit, err := positive(env, "AGENT_MAX_IN_FLIGHT", 4)
	if err != nil {
		return Repository{}, err
	}
	stale, err := positive(env, "AGENT_STALE_HOURS", 8)
	if err != nil {
		return Repository{}, err
	}
	return Repository{Path: path, MaxInFlight: limit, StaleHours: stale, Env: env}, nil
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

// Scheduler holds the scheduler's tunable knobs. A zero field keeps the built-in default.
type Scheduler struct {
	PauseFraction       float64       `toml:"pause_fraction"`
	ResumeFraction      float64       `toml:"resume_fraction"`
	QuotaProbeBackoff   time.Duration `toml:"quota_probe_backoff"`
	UnregisteredGrace   time.Duration `toml:"unregistered_grace"`
	AdaptiveMinInterval time.Duration `toml:"adaptive_min_interval"`
	AdaptiveMaxInterval time.Duration `toml:"adaptive_max_interval"`
}

// DefaultScheduler returns the built-in scheduler defaults.
func DefaultScheduler() Scheduler {
	return Scheduler{
		PauseFraction:       .05,
		ResumeFraction:      .10,
		QuotaProbeBackoff:   5 * time.Minute,
		UnregisteredGrace:   3 * time.Minute,
		AdaptiveMinInterval: time.Minute,
		AdaptiveMaxInterval: 3 * time.Minute,
	}
}

// Linear holds the Linear client's tunables.
type Linear struct {
	HTTPTimeout time.Duration `toml:"http_timeout"`
}

// DefaultLinear returns the built-in Linear defaults.
func DefaultLinear() Linear { return Linear{HTTPTimeout: 30 * time.Second} }

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

// Status holds the status snapshot's tunables.
type Status struct {
	StaleAfter time.Duration `toml:"stale_after"`
}

// DefaultStatus returns the built-in status defaults.
func DefaultStatus() Status { return Status{StaleAfter: 5 * time.Minute} }

// Runx holds the process runner's tunables.
type Runx struct {
	WaitDelay time.Duration `toml:"wait_delay"`
}

// DefaultRunx returns the built-in runx defaults.
func DefaultRunx() Runx { return Runx{WaitDelay: 100 * time.Millisecond} }

// Tuning bundles every tunable subsystem. The raw TOML may name only the sections it overrides;
// Load fills the rest with the built-in defaults.
type Tuning struct {
	Scheduler Scheduler
	Linear    Linear
	GitHub    GitHub
	Orca      Orca
	Ledger    Ledger
	Status    Status
	Runx      Runx
}

// fillScheduler returns want with every zero field of over taking its value.
func fillScheduler(over, want Scheduler) Scheduler {
	if over.PauseFraction != 0 {
		want.PauseFraction = over.PauseFraction
	}
	if over.ResumeFraction != 0 {
		want.ResumeFraction = over.ResumeFraction
	}
	if over.QuotaProbeBackoff != 0 {
		want.QuotaProbeBackoff = over.QuotaProbeBackoff
	}
	if over.UnregisteredGrace != 0 {
		want.UnregisteredGrace = over.UnregisteredGrace
	}
	if over.AdaptiveMinInterval != 0 {
		want.AdaptiveMinInterval = over.AdaptiveMinInterval
	}
	if over.AdaptiveMaxInterval != 0 {
		want.AdaptiveMaxInterval = over.AdaptiveMaxInterval
	}
	return want
}

func fillLinear(over, want Linear) Linear {
	if over.HTTPTimeout != 0 {
		want.HTTPTimeout = over.HTTPTimeout
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

func fillStatus(over, want Status) Status {
	if over.StaleAfter != 0 {
		want.StaleAfter = over.StaleAfter
	}
	return want
}

func fillRunx(over, want Runx) Runx {
	if over.WaitDelay != 0 {
		want.WaitDelay = over.WaitDelay
	}
	return want
}
