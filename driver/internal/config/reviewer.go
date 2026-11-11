package config

import (
	"fmt"
	"strings"
)

// ReviewerCommands are the effective shell commands used by the three review
// roles. Structured role values are converted here so every caller uses the
// same agent-specific sandbox and read-only flags.
type ReviewerCommands struct {
	Main   string
	Small  string
	Second string
}

// GenerateReviewerCommand converts agent:model:effort into the command used by
// the corresponding reviewer backend. Only the two supported reviewer agents
// are accepted.
func GenerateReviewerCommand(value string) (string, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 || parts[0] == "" {
		return "", fmt.Errorf("reviewer role requires agent:model:effort")
	}
	agent, model, effort := parts[0], parts[1], parts[2]
	switch agent {
	case "codex":
		command := "env -u ORCA_PANE_KEY -u ORCA_AGENT_HOOK_PORT -u ORCA_AGENT_HOOK_TOKEN codex exec"
		if model != "" {
			command += " -m " + model
		}
		if effort != "" {
			command += " -c model_reasoning_effort=" + effort
		}
		return command + " --sandbox read-only -", nil
	case "claude":
		command := "claude -p"
		if model != "" {
			command += " --model " + model
		}
		if effort != "" {
			command += " --effort " + effort
		}
		return command + " --tools Read,Grep,Glob --permission-mode dontAsk --restricted --strict-mcp-config", nil
	default:
		return "", fmt.Errorf("unsupported reviewer agent %q", agent)
	}
}

// ReviewerKey picks the key that configures one reviewer role: an explicit
// repository structured key, an explicit repository command key, then a
// structured value supplied by the selected machine profile. A key missing
// from sources but present in env counts as repository-assigned.
func ReviewerKey(env, sources map[string]string, structuredKey, commandKey string) string {
	source := func(key string) string {
		if value, ok := sources[key]; ok {
			return value
		}
		if _, ok := env[key]; ok {
			return "repository"
		}
		return ""
	}
	switch {
	case source(structuredKey) == "repository":
		return structuredKey
	case source(commandKey) == "repository":
		return commandKey
	case strings.HasPrefix(source(structuredKey), "profile:"):
		return structuredKey
	}
	return ""
}

// ResolveReviewerCommand returns the command for the key ReviewerKey picks.
func ResolveReviewerCommand(env, sources map[string]string, structuredKey, commandKey string) (string, error) {
	switch ReviewerKey(env, sources, structuredKey, commandKey) {
	case structuredKey:
		return GenerateReviewerCommand(env[structuredKey])
	case commandKey:
		return strings.TrimSpace(env[commandKey]), nil
	}
	return "", nil
}

// ResolveReviewerCommands resolves the main, small-ticket and cross-review
// commands from one repository profile.
func ResolveReviewerCommands(env, sources map[string]string) (ReviewerCommands, error) {
	main, err := ResolveReviewerCommand(env, sources, "AGENT_ROLE_REVIEWER", "AGENT_ROLE_REVIEWER_CMD")
	if err != nil {
		return ReviewerCommands{}, err
	}
	small, err := ResolveReviewerCommand(env, sources, "AGENT_ROLE_REVIEWER_SMALL", "AGENT_ROLE_REVIEWER_CMD_SMALL")
	if err != nil {
		return ReviewerCommands{}, err
	}
	second, err := ResolveReviewerCommand(env, sources, "AGENT_ROLE_SECOND_REVIEWER", "AGENT_ROLE_SECOND_REVIEWER_CMD")
	if err != nil {
		return ReviewerCommands{}, err
	}
	return ReviewerCommands{Main: main, Small: small, Second: second}, nil
}
