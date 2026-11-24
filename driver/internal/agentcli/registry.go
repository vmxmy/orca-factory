// Package agentcli describes the commands used by driver model roles.
package agentcli

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
)

type Profile struct {
	Name           string   `toml:"-"`
	Binary         string   `toml:"binary"`
	ProbeArgs      []string `toml:"probe_args"`
	ReviewArgs     []string `toml:"review_args"`
	CorrectionArgs []string `toml:"correction_args"`
	Relay          bool     `toml:"relay"`
	reviewPrefix   string
}

var builtins = map[string]Profile{
	"claude": {
		Name: "claude", Relay: true,
		ProbeArgs:      []string{"-p", "--model", "{model}", "--tools", "", "--permission-mode", "dontAsk", "--restricted", "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence"},
		ReviewArgs:     strings.Fields("-p --model {model} --effort {effort} --tools Read,Grep,Glob --permission-mode dontAsk --restricted --strict-mcp-config"),
		CorrectionArgs: strings.Fields("-p {prompt} --model {model} --effort {effort} --permission-mode acceptEdits --add-dir {dir} --allowedTools Read,Grep,Glob,Write,Edit --tools Read,Grep,Glob,Write,Edit"),
	},
	"codex": {
		Name: "codex", reviewPrefix: "env -u ORCA_PANE_KEY -u ORCA_AGENT_HOOK_PORT -u ORCA_AGENT_HOOK_TOKEN ",
		ProbeArgs:      strings.Fields("exec -m {model} -c model_reasoning_effort=low --sandbox read-only --ephemeral"),
		ReviewArgs:     strings.Fields("exec -m {model} -c model_reasoning_effort={effort} --sandbox read-only -"),
		CorrectionArgs: strings.Fields("exec --model {model} -c model_reasoning_effort={effort} --sandbox workspace-write --add-dir {dir} --ephemeral -"),
	},
}

var mu sync.RWMutex
var profiles = maps.Clone(builtins)

func (p Profile) clone() Profile {
	p.ProbeArgs = slices.Clone(p.ProbeArgs)
	p.ReviewArgs = slices.Clone(p.ReviewArgs)
	p.CorrectionArgs = slices.Clone(p.CorrectionArgs)
	return p
}

func Register(p Profile) {
	mu.Lock()
	defer mu.Unlock()
	profiles[p.Name] = p.clone()
}

// Configure replaces the previous machine configuration in a single update.
func Configure(defs map[string]Profile) {
	next := maps.Clone(builtins)
	for name, p := range defs {
		p.Name = name
		next[name] = p.clone()
	}
	mu.Lock()
	defer mu.Unlock()
	profiles = next
}

func Lookup(name string) (Profile, bool) {
	mu.RLock()
	defer mu.RUnlock()
	p, ok := profiles[name]
	return p.clone(), ok
}

func Registered(name string) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := profiles[name]
	return ok
}

func List() []string {
	mu.RLock()
	defer mu.RUnlock()
	return slices.Sorted(maps.Keys(profiles))
}

func (p Profile) Executable() string { return cmp.Or(p.Binary, p.Name) }

func expand(template []string, model, effort, prompt, dir string, omitEmpty bool) []string {
	sub := strings.NewReplacer("{model}", model, "{effort}", effort, "{prompt}", prompt, "{dir}", dir)
	args := make([]string, 0, len(template))
	for i, arg := range template {
		if omitEmpty && ((model == "" && strings.Contains(arg, "{model}")) || (effort == "" && strings.Contains(arg, "{effort}"))) {
			if i > 0 && len(args) > 0 && strings.HasPrefix(template[i-1], "-") && !strings.ContainsAny(template[i-1], "={}") {
				args = args[:len(args)-1]
			}
			continue
		}
		args = append(args, sub.Replace(arg))
	}
	return args
}

func (p Profile) ProbeArgv(model, effort, prompt string) []string {
	return append(expand(p.ProbeArgs, model, effort, "", "", true), prompt)
}

func (p Profile) CorrectionArgv(model, effort, prompt, dir string) []string {
	return expand(p.CorrectionArgs, model, effort, prompt, dir, false)
}

func (p Profile) CorrectionStdin() bool {
	for _, arg := range p.CorrectionArgs {
		if strings.Contains(arg, "{prompt}") {
			return false
		}
	}
	return true
}

var shellWord = regexp.MustCompile(`^[a-zA-Z0-9_.,/@+=:-]+$`)

func quote(arg string) string {
	if shellWord.MatchString(arg) {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

func (p Profile) ReviewerCommand(model, effort string) string {
	words := []string{quote(p.Executable())}
	// NUL cannot occur in an executable shell argument. Keep the stdin slot
	// distinct from a literal role value such as "{prompt}".
	const stdin = "\x00"
	args := expand(p.ReviewArgs, model, effort, stdin, "", true)
	for _, arg := range args {
		if arg == stdin {
			words = append(words, `"$(cat)"`)
		} else {
			words = append(words, quote(arg))
		}
	}
	return p.reviewPrefix + strings.Join(words, " ")
}

// MatchReviewerCommand recovers literal role arguments only from a complete generated command.
func (p Profile) MatchReviewerCommand(command string) (model, effort string, ok bool) {
	const modelMark = "ORCA_MODEL_PLACEHOLDER"
	const effortMark = "ORCA_EFFORT_PLACEHOLDER"
	for _, m := range []string{modelMark, ""} {
		for _, e := range []string{effortMark, ""} {
			pattern := regexp.QuoteMeta(p.ReviewerCommand(m, e))
			pattern = strings.NewReplacer(modelMark, "(?P<m>[A-Za-z0-9._/@+-]+)", effortMark, "(?P<e>[A-Za-z0-9._/@+-]+)").Replace(pattern)
			re := regexp.MustCompile("^" + pattern + "$")
			match := re.FindStringSubmatch(command)
			if match == nil {
				continue
			}
			model, effort = "", ""
			if i := re.SubexpIndex("m"); i >= 0 {
				model = match[i]
			}
			if i := re.SubexpIndex("e"); i >= 0 {
				effort = match[i]
			}
			if p.ReviewerCommand(model, effort) == command {
				return model, effort, true
			}
		}
	}
	return "", "", false
}
