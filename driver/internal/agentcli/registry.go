// Package agentcli describes the commands used by driver model roles.
package agentcli

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
)

// Profile describes one executable. PromptFlag takes the prompt as its argument.
// CorrectionArgs may contain a "{dir}" argument for the correction file's directory.
type Profile struct {
	Args                []string `toml:"args"`
	ModelFlag           string   `toml:"model_flag"`
	EffortFlag          string   `toml:"effort_flag"`
	EffortPrefix        string   `toml:"effort_prefix"`
	PromptFlag          string   `toml:"prompt_flag"`
	ReviewerPrefix      string   `toml:"reviewer_prefix"`
	ReviewerArgs        string   `toml:"reviewer_args"`
	ProbeArgs           []string `toml:"probe_args"`
	CorrectionModelFlag string   `toml:"correction_model_flag"`
	CorrectionArgs      []string `toml:"correction_args"`
	CorrectionStdin     bool     `toml:"correction_stdin"`
	Relay               bool     `toml:"relay"`
}

var builtins = map[string]Profile{
	"claude": {
		Args: []string{"-p"}, ModelFlag: "--model", EffortFlag: "--effort", Relay: true,
		ReviewerArgs:   "--tools Read,Grep,Glob --permission-mode dontAsk --restricted --strict-mcp-config",
		ProbeArgs:      []string{"--tools", "", "--permission-mode", "dontAsk", "--restricted", "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence"},
		CorrectionArgs: []string{"--permission-mode", "acceptEdits", "--add-dir", "{dir}", "--allowedTools", "Read,Grep,Glob,Write,Edit", "--tools", "Read,Grep,Glob,Write,Edit"},
	},
	"codex": {
		Args: []string{"exec"}, ModelFlag: "-m", EffortFlag: "-c", EffortPrefix: "model_reasoning_effort=",
		ReviewerPrefix: "env -u ORCA_PANE_KEY -u ORCA_AGENT_HOOK_PORT -u ORCA_AGENT_HOOK_TOKEN", ReviewerArgs: "--sandbox read-only -",
		ProbeArgs:           []string{"-c", "model_reasoning_effort=low", "--sandbox", "read-only", "--ephemeral"},
		CorrectionModelFlag: "--model", CorrectionStdin: true,
		CorrectionArgs: []string{"--sandbox", "workspace-write", "--add-dir", "{dir}", "--ephemeral", "-"},
	},
}

var profiles atomic.Pointer[map[string]Profile]

func init() { Register(nil) }

// Register installs the machine's complete set of agent definitions over the builtins.
func Register(agents map[string]Profile) {
	next := maps.Clone(builtins)
	for name, p := range agents {
		next[name] = p.clone()
	}
	profiles.Store(&next)
}

func (p Profile) clone() Profile {
	p.Args = slices.Clone(p.Args)
	p.ProbeArgs = slices.Clone(p.ProbeArgs)
	p.CorrectionArgs = slices.Clone(p.CorrectionArgs)
	return p
}

func Lookup(name string) (Profile, bool) {
	p, ok := (*profiles.Load())[name]
	return p.clone(), ok
}

func Registered(name string) bool {
	_, ok := (*profiles.Load())[name]
	return ok
}

func List() []string { return slices.Sorted(maps.Keys(*profiles.Load())) }

func (p Profile) options(modelFlag, model, effort string, includeEmpty bool) []string {
	var args []string
	if modelFlag != "" && (model != "" || includeEmpty) {
		args = append(args, modelFlag, model)
	}
	if p.EffortFlag != "" && (effort != "" || includeEmpty) {
		args = append(args, p.EffortFlag, p.EffortPrefix+effort)
	}
	return args
}

const reviewerStdin = `"$(cat)"`

func (p Profile) reviewerParts(agent string) (prefix, suffix string) {
	prefix = strings.Join(append([]string{p.ReviewerPrefix, agent}, p.Args...), " ")
	if p.ReviewerArgs != "" {
		suffix = " " + p.ReviewerArgs
	}
	if p.PromptFlag != "" {
		suffix += " " + p.PromptFlag + " " + reviewerStdin
	}
	return prefix, suffix
}

func (p Profile) ReviewerCommand(agent, model, effort string) string {
	prefix, suffix := p.reviewerParts(agent)
	args := append([]string{prefix}, p.options(p.ModelFlag, model, effort, false)...)
	return strings.TrimSpace(strings.Join(args, " ") + suffix)
}

// MatchReviewerCommand recovers the options of an exact generated command.
func (p Profile) MatchReviewerCommand(agent, command string) (model, effort string, ok bool) {
	prefix, suffix := p.reviewerParts(agent)
	options, hasPrefix := strings.CutPrefix(command, strings.TrimSpace(prefix))
	options, hasSuffix := strings.CutSuffix(options, strings.TrimSpace(suffix))
	if !hasPrefix || !hasSuffix || strings.ContainsAny(options, ";&|<>()") {
		return "", "", false
	}
	literal := command
	if p.PromptFlag != "" {
		literal = strings.TrimSuffix(literal, reviewerStdin)
	}
	if strings.ContainsAny(literal, "'\"\\$`") {
		return "", "", false
	}
	fields := strings.Fields(options)
	if len(fields) >= 2 && fields[0] == p.ModelFlag {
		model, fields = fields[1], fields[2:]
	}
	if len(fields) == 2 && fields[0] == p.EffortFlag {
		effort = strings.TrimPrefix(fields[1], p.EffortPrefix)
	}
	return model, effort, p.ReviewerCommand(agent, model, effort) == command
}

func (p Profile) promptArgv(prompt string) []string {
	if p.PromptFlag != "" {
		return []string{p.PromptFlag, prompt}
	}
	return []string{prompt}
}

func (p Profile) ProbeArgv(model, prompt string) []string {
	return slices.Concat(p.Args, p.options(p.ModelFlag, model, "", false), p.ProbeArgs, p.promptArgv(prompt))
}

func (p Profile) CorrectionArgv(model, effort, prompt, dir string) []string {
	args := slices.Clone(p.Args)
	if !p.CorrectionStdin {
		args = append(args, p.promptArgv(prompt)...)
	}
	args = append(args, p.options(cmp.Or(p.CorrectionModelFlag, p.ModelFlag), model, effort, true)...)
	for _, arg := range p.CorrectionArgs {
		if arg == "{dir}" {
			arg = dir
		}
		args = append(args, arg)
	}
	return args
}
