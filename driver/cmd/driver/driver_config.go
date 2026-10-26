package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
	"github.com/vmxmy/orca-factory/driver/internal/status"
)

func init() { Register("config", runConfig) }

const configUsage = "Usage: orca-factory-driver config [--json]"

const (
	sourceDriverToml = "driver.toml"
	sourceDefault    = "default"
	sourceDriverEnv  = "driver.env"
	sourceRepository = "agent-graph.env"
	sourceService    = "service"
)

// configSnapshot is driver-config-v1 (driver/CONTRACTS.md「driver config 快照」).
type configSnapshot struct {
	Schema      string         `json:"schema"`
	GeneratedAt time.Time      `json:"generatedAt"`
	Overview    configOverview `json:"overview"`
	Repos       []configRepo   `json:"repos"`
	Scheduler   []configEntry  `json:"scheduler"`
	Environment []configEntry  `json:"environment"`
	Problems    []string       `json:"problems"`
}

type configOverview struct {
	Build        string `json:"build"`
	Service      string `json:"service"`
	Maintenance  string `json:"maintenance"`
	Snapshot     string `json:"snapshot"`
	GlobalPause  bool   `json:"globalPause"`
	PauseSeconds int64  `json:"pauseSeconds"`
	FactoryRoot  string `json:"factoryRoot"`
}

type configRepo struct {
	Name    string        `json:"name"`
	Remote  string        `json:"remote"`
	Path    string        `json:"path"`
	Prefix  string        `json:"prefix"`
	Roles   []configRole  `json:"roles"`
	Routing []configEntry `json:"routing"`
}

type configRole struct {
	Role   string `json:"role"`
	Agent  string `json:"agent"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
	Key    string `json:"key"`
	Raw    string `json:"raw"`
}

// configEntry carries no Value for a secret or an unset service key.
type configEntry struct {
	Key    string  `json:"key"`
	Value  *string `json:"value,omitempty"`
	Source string  `json:"source"`
	Set    bool    `json:"set"`
	Secret bool    `json:"secret,omitempty"`
}

// configRoles mirrors each role's dispatch-time fallback chain: resolver
// (internal/scheduler/resolver.go), intake (tierParts) and the worker tiers.
var configRoles = []struct {
	role     string
	keys     []string
	fallback string
	parse    func(string) (agent, model, effort string)
}{
	{"worker_fast", []string{"AGENT_TIER_FAST"}, "", splitTier},
	{"worker_default", []string{"AGENT_TIER_DEFAULT"}, "", splitTier},
	{"worker_strong", []string{"AGENT_TIER_STRONG"}, "", splitTier},
	{"reviewer", []string{"AGENT_ROLE_REVIEWER_CMD"}, "", parseReviewerCommand},
	{"reviewer_small", []string{"AGENT_ROLE_REVIEWER_CMD_SMALL"}, "", parseReviewerCommand},
	{"resolver", []string{"AGENT_ROLE_RESOLVER", "AGENT_TIER_STRONG"}, "codex", splitTier},
	{"intake", []string{"AGENT_ROLE_INTAKE", "AGENT_TIER_STRONG"}, "", splitTier},
}

var configRouting = []struct{ key, fallback string }{
	{"AGENT_TIER_BY_ESTIMATE", ""},
	{"AGENT_TIER_ESCALATE", ""},
	{"AGENT_REVIEWER_SMALL_MAX_ESTIMATE", "2"},
	{"AGENT_MAX_IN_FLIGHT", "4"},
	{"AGENT_STALE_HOURS", "8"},
	{"AGENT_ISSUE_BACKEND", ""},
	{"AGENT_LANE2_AUTOMERGE", ""},
	{"AGENT_LANE3_GLOBS", ""},
}

var configScheduler = []struct {
	key   string
	value func(config.Config) string
}{
	{"max_in_flight", func(c config.Config) string { return strconv.Itoa(c.MaxInFlight) }},
	{"token_bucket.capacity", func(c config.Config) string { return strconv.Itoa(c.TokenBucket.Capacity) }},
	{"token_bucket.refill_per_second", func(c config.Config) string { return formatFloat(c.TokenBucket.RefillPerSecond) }},
	{"scheduler.pause_fraction", func(c config.Config) string { return formatFloat(c.Tuning.Scheduler.PauseFraction) }},
	{"scheduler.quota_probe_backoff", func(c config.Config) string { return formatDuration(c.Tuning.Scheduler.QuotaProbeBackoff) }},
	{"scheduler.unregistered_grace", func(c config.Config) string { return formatDuration(c.Tuning.Scheduler.UnregisteredGrace) }},
	{"scheduler.adaptive_min_interval", func(c config.Config) string { return formatDuration(c.Tuning.Scheduler.AdaptiveMinInterval) }},
	{"scheduler.adaptive_max_interval", func(c config.Config) string { return formatDuration(c.Tuning.Scheduler.AdaptiveMaxInterval) }},
	{"github.http_timeout", func(c config.Config) string { return formatDuration(c.Tuning.GitHub.HTTPTimeout) }},
	{"orca.worker_start_wait", func(c config.Config) string { return formatDuration(c.Tuning.Orca.WorkerStartWait) }},
	{"orca.call_timeout", func(c config.Config) string { return formatDuration(c.Tuning.Orca.CallTimeout) }},
	{"ledger.flush_interval", func(c config.Config) string { return formatDuration(c.Tuning.Ledger.FlushInterval) }},
	{"status.stale_after", func(c config.Config) string { return formatDuration(c.Tuning.Status.StaleAfter) }},
	{"runx.wait_delay", func(c config.Config) string { return formatDuration(c.Tuning.Runx.WaitDelay) }},
}

var configServiceKeys = []string{"AGENT_ORCA_BIN", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY"}
var configProxyKeys = map[string]bool{"HTTPS_PROXY": true, "HTTP_PROXY": true}
var configServiceTools = []string{"go", "gh", "git", "jq", "claude", "codex"}

var shellAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

func runConfig(args []string) int {
	asJSON := len(args) == 1 && args[0] == "--json"
	if len(args) != 0 && !asJSON {
		fmt.Fprintln(os.Stderr, configUsage)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	snapshot := readConfigSnapshot(ctx, time.Now())
	var err error
	if asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetEscapeHTML(false)
		err = encoder.Encode(snapshot)
	} else {
		err = writeConfigText(os.Stdout, snapshot)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot output driver config")
		return 1
	}
	return 0
}

func readConfigSnapshot(ctx context.Context, now time.Time) configSnapshot {
	view := status.Read(now, 0)
	s := configSnapshot{
		Schema:      "driver-config-v1",
		GeneratedAt: now.UTC().Truncate(time.Second),
		Overview: configOverview{Build: version, Service: serviceActivity(ctx), Maintenance: "unknown",
			Snapshot: view.State, GlobalPause: view.GlobalPause.Active, PauseSeconds: view.GlobalPause.DurationSeconds},
		Repos: []configRepo{}, Scheduler: []configEntry{}, Environment: []configEntry{}, Problems: []string{},
	}
	if state, err := maintenance.Read(); err == nil {
		s.Overview.Maintenance = state.Mode
	} else {
		s.Problems = append(s.Problems, "维护状态读不到")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		s.Problems = append(s.Problems, "找不到用户主目录，driver.toml、driver.env 与仓库均未读取")
		return s
	}
	s.Environment = append(s.Environment, driverEnvEntries(home, &s.Problems)...)
	s.Environment = append(s.Environment, serviceEntries(ctx, &s.Problems)...)
	cfg, err := config.Load(home)
	if err != nil {
		s.Problems = append(s.Problems, "driver.toml 无法加载（"+err.Error()+"），调度项与仓库未列出")
		return s
	}
	s.Overview.FactoryRoot = cfg.FactoryRoot
	s.Scheduler = schedulerEntries(home, cfg, &s.Problems)
	result, _, err := discoverProjects(ctx, cfg, &orca.Client{Timeout: cfg.Tuning.Orca.CallTimeout})
	if err != nil {
		s.Problems = append(s.Problems, "项目发现失败，仓库未列出")
		return s
	}
	for _, p := range result.Eligible {
		s.Repos = append(s.Repos, configRepo{Name: p.Name, Remote: p.Remote, Path: p.Path, Prefix: p.Prefix,
			Roles: repositoryRoles(p.Repository.Env), Routing: repositoryRouting(p.Repository.Env)})
	}
	return s
}

func serviceActivity(ctx context.Context) string {
	out, _ := runx.Run(ctx, runx.Spec{Path: "systemctl", Args: []string{"--user", "is-active", doctorServiceUnit}, Timeout: doctorCommandTimeout})
	if state := strings.TrimSpace(string(out)); state != "" {
		return state
	}
	return "unknown"
}

// schedulerEntries decodes driver.toml once more only for its metadata: the
// same shape config.Load decodes, so Undecoded names exactly the ignored keys.
func schedulerEntries(home string, cfg config.Config, problems *[]string) []configEntry {
	var meta toml.MetaData
	data, err := os.ReadFile(filepath.Join(home, ".config", "orca-factory", "driver.toml"))
	if err == nil {
		var shape struct {
			MaxInFlight int                `toml:"max_in_flight"`
			FactoryRoot string             `toml:"factory_root"`
			TokenBucket config.TokenBucket `toml:"token_bucket"`
			Scheduler   *config.Scheduler  `toml:"scheduler"`
			GitHub      *config.GitHub     `toml:"github"`
			Orca        *config.Orca       `toml:"orca"`
			Ledger      *config.Ledger     `toml:"ledger"`
			Status      *config.Status     `toml:"status"`
			Runx        *config.Runx       `toml:"runx"`
		}
		if meta, err = toml.Decode(string(data), &shape); err != nil {
			*problems = append(*problems, "driver.toml 无法解析，调度项来源按默认值标注")
		}
		for _, key := range meta.Undecoded() {
			*problems = append(*problems, "driver.toml 的 "+key.String()+" 不被识别，不生效")
		}
	}
	entries := make([]configEntry, 0, len(configScheduler))
	for _, item := range configScheduler {
		source := sourceDefault
		if meta.IsDefined(strings.Split(item.key, ".")...) {
			source = sourceDriverToml
		}
		value := item.value(cfg)
		entries = append(entries, configEntry{Key: item.key, Value: &value, Source: source, Set: true})
	}
	return entries
}

func driverEnvEntries(home string, problems *[]string) []configEntry {
	f, err := os.Open(filepath.Join(home, ".config", "orca-factory", "driver.env"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		*problems = append(*problems, "driver.env 读不到")
		return nil
	}
	env, parseErr := config.ParseEnv(f)
	closeErr := f.Close()
	if parseErr != nil || closeErr != nil {
		*problems = append(*problems, "driver.env 无法解析")
		return nil
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]configEntry, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, configEntry{Key: key, Source: sourceDriverEnv, Set: env[key] != "", Secret: true})
	}
	return entries
}

func serviceEntries(ctx context.Context, problems *[]string) []configEntry {
	env, err := serviceEnviron(ctx)
	if err != nil {
		*problems = append(*problems, "服务环境读不到（"+err.Error()+"），未列出服务变量与工具路径")
		return nil
	}
	entries := make([]configEntry, 0, len(configServiceKeys)+len(configServiceTools))
	for _, key := range configServiceKeys {
		raw, ok := env[key]
		entry := configEntry{Key: key, Source: sourceService, Set: ok}
		if ok && configProxyKeys[key] {
			if value, valid := proxyWithoutUserinfo(raw); valid {
				entry.Value = &value
			} else {
				*problems = append(*problems, "服务环境的 "+key+" 不是带主机的 URL，值不输出")
			}
		} else if ok {
			entry.Value = &raw
		}
		entries = append(entries, entry)
	}
	for _, tool := range configServiceTools {
		entry := configEntry{Key: tool, Source: sourceService}
		if found := lookPathIn(env["PATH"], tool); found != "" {
			entry.Value, entry.Set = &found, true
		}
		entries = append(entries, entry)
	}
	return entries
}

func proxyWithoutUserinfo(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	u.User = nil
	return u.String(), true
}

func repositoryRoles(env map[string]string) []configRole {
	roles := []configRole{}
	for _, def := range configRoles {
		role := configRole{Role: def.role, Raw: def.fallback}
		for _, key := range def.keys {
			if env[key] != "" {
				role.Key, role.Raw = key, env[key]
				break
			}
		}
		if role.Raw == "" {
			continue
		}
		role.Agent, role.Model, role.Effort = def.parse(role.Raw)
		roles = append(roles, role)
	}
	return roles
}

func repositoryRouting(env map[string]string) []configEntry {
	entries := make([]configEntry, 0, len(configRouting))
	for _, item := range configRouting {
		value, ok := env[item.key]
		entry := configEntry{Key: item.key, Value: &value, Source: sourceRepository, Set: true}
		if !ok {
			fallback := item.fallback
			entry.Value, entry.Source, entry.Set = &fallback, sourceDefault, fallback != ""
		}
		entries = append(entries, entry)
	}
	return entries
}

func splitTier(tier string) (agent, model, effort string) {
	parts := append(strings.SplitN(tier, ":", 3), "", "")
	return parts[0], parts[1], parts[2]
}

// parseReviewerCommand reads a reviewer shell command word by word; whatever
// it cannot recognize stays empty and the caller keeps the original as raw.
func parseReviewerCommand(command string) (agent, model, effort string) {
	words := strings.Fields(command)
	i := 0
	if len(words) > 0 && words[0] == "env" {
		for i = 1; i < len(words); i++ {
			if words[i] == "-u" {
				i++
			} else if !shellAssignment.MatchString(words[i]) {
				break
			}
		}
	}
	if i >= len(words) {
		return "", "", ""
	}
	agent = filepath.Base(words[i])
	for j := i + 1; j < len(words); j++ {
		next := ""
		if j+1 < len(words) {
			next = words[j+1]
		}
		switch {
		case words[j] == "-m" || words[j] == "--model":
			model = next
		case words[j] == "--effort":
			effort = next
		case strings.HasPrefix(strings.Trim(words[j], `'"`), "model_reasoning_effort="):
			effort = strings.Trim(strings.TrimPrefix(strings.Trim(words[j], `'"`), "model_reasoning_effort="), `'"`)
		}
	}
	return agent, model, effort
}

func formatFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// formatDuration drops trailing zero units: 5m0s becomes 5m, 1h0m0s becomes 1h.
func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func writeConfigText(out io.Writer, s configSnapshot) error {
	var b strings.Builder
	o := s.Overview
	fmt.Fprintf(&b, "概览\n  构建        %s\n  服务        %s\n  维护状态    %s\n  状态快照    %s\n  全局暂停    %t（%ds）\n  工厂根目录  %s\n",
		o.Build, o.Service, o.Maintenance, o.Snapshot, o.GlobalPause, o.PauseSeconds, o.FactoryRoot)
	for _, repo := range s.Repos {
		fmt.Fprintf(&b, "\n模型：%s（%s，%s）\n  %s\n", repo.Name, repo.Prefix, repo.Remote, repo.Path)
		for _, r := range repo.Roles {
			model := r.Model
			if model == "" {
				model = "默认模型"
			}
			source := r.Key
			if source == "" {
				source = "内置回退"
			}
			fmt.Fprintf(&b, "  %-15s %s / %s / %s  [%s]\n", r.Role, r.Agent, model, r.Effort, source)
		}
		b.WriteString("  路由\n")
		writeConfigEntries(&b, "    ", repo.Routing)
	}
	b.WriteString("\n调度\n")
	writeConfigEntries(&b, "  ", s.Scheduler)
	b.WriteString("\n环境\n")
	writeConfigEntries(&b, "  ", s.Environment)
	if len(s.Problems) > 0 {
		b.WriteString("\n问题\n")
		for _, p := range s.Problems {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	}
	_, err := io.WriteString(out, b.String())
	return err
}

func writeConfigEntries(b *strings.Builder, indent string, entries []configEntry) {
	for _, e := range entries {
		value := "（未设置）"
		switch {
		case e.Secret && e.Set:
			value = "已设置（凭据，不显示）"
		case e.Secret:
			value = "（空）"
		case e.Value != nil && *e.Value != "":
			value = *e.Value
		case e.Value != nil && e.Set:
			value = "（空）"
		case e.Set:
			value = "（已设置，值不显示）"
		}
		fmt.Fprintf(b, "%s%-34s %s  [%s]\n", indent, e.Key, value, e.Source)
	}
}
