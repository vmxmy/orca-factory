package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/agentcli"
	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
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
// the corresponding reviewer backend.
func GenerateReviewerCommand(value string) (string, error) {
	ref, err := modelhealth.Parse(value)
	if err != nil {
		return "", fmt.Errorf("reviewer role requires agent:model:effort")
	}
	profile, ok := agentcli.Lookup(ref.Agent)
	if !ok {
		return "", fmt.Errorf("unsupported reviewer agent %q", ref.Agent)
	}
	return profile.ReviewerCommand(ref.Model, ref.Effort), nil
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

// ReviewerCandidates returns a structured reviewer role's configured model
// followed by its FallbackKey candidates. It returns nil when the role is
// configured by a command key.
func ReviewerCandidates(env, sources map[string]string, structuredKey, commandKey string) ([]modelhealth.Ref, error) {
	if ReviewerKey(env, sources, structuredKey, commandKey) != structuredKey {
		return nil, nil
	}
	fallback, err := ParseFallback(env[FallbackKey(structuredKey)])
	if err != nil {
		return nil, wrapKey(FallbackKey(structuredKey), err)
	}
	primary, err := modelhealth.Parse(env[structuredKey])
	if err != nil {
		return nil, wrapKey(structuredKey, err)
	}
	if err := validFallback(structuredKey, env[FallbackKey(structuredKey)]); err != nil {
		return nil, wrapKey(FallbackKey(structuredKey), err)
	}
	return append([]modelhealth.Ref{primary}, fallback...), nil
}

// SelectReviewerCommand keeps command unless the registry marks candidates[0]
// unavailable; then it returns the first available fallback's command and model.
func SelectReviewerCommand(command string, candidates []modelhealth.Ref, stateRoot string, now time.Time) (string, modelhealth.Ref, error) {
	if len(candidates) == 0 {
		return command, modelhealth.Ref{}, nil
	}
	ref, err := SelectModel(stateRoot, candidates[0], candidates[1:], now)
	if ref == candidates[0] {
		return command, ref, err
	}
	generated, err := GenerateReviewerCommand(ref.String())
	if err != nil {
		return command, candidates[0], err
	}
	return generated, ref, nil
}

func wrapKey(key string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", key, err)
}
