package config

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/agentcli"
	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
)

// FallbackKey names the ordered fallback candidates of a role or tier key.
func FallbackKey(key string) string { return key + "_FALLBACK" }

// ParseFallback reads a space-separated, ordered agent:model:effort list.
// Errors name the candidate position only, never the configured value.
func ParseFallback(value string) ([]modelhealth.Ref, error) {
	var refs []modelhealth.Ref
	for i, field := range strings.Fields(value) {
		ref, err := modelhealth.Parse(field)
		if err != nil {
			return nil, fmt.Errorf("fallback candidate %d: %w", i+1, err)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// SelectModel returns primary unless the model registry under stateRoot marks
// it unavailable; then the first fallback it does not mark unavailable. With
// no fallback, or every candidate unavailable, primary stays selected.
func SelectModel(stateRoot string, primary modelhealth.Ref, fallback []modelhealth.Ref, now time.Time) (modelhealth.Ref, error) {
	if len(fallback) == 0 {
		return primary, nil
	}
	registry, err := modelhealth.Open(stateRoot)
	if err != nil {
		return primary, fmt.Errorf("model health registry: %w", err)
	}
	if ref, ok := registry.Select(append([]modelhealth.Ref{primary}, fallback...), now); ok {
		return ref, nil
	}
	return primary, nil
}

// SelectConfigured selects among primary and the candidates env configures
// under FallbackKey(key). A selection error keeps primary.
func SelectConfigured(env map[string]string, key, stateRoot string, primary modelhealth.Ref, now time.Time) (modelhealth.Ref, error) {
	fallback, err := ParseFallback(env[FallbackKey(key)])
	if err != nil {
		return primary, fmt.Errorf("%s: %w", FallbackKey(key), err)
	}
	return SelectModel(stateRoot, primary, fallback, now)
}

// RetryAfterModelError records used unavailable in the registry under
// stateRoot when its failed command's output shows a model limit or
// unsupported model, and returns the candidate to retry with. It records even
// without another candidate, so a retry's own failure is recorded by calling
// it again without candidates. It reports false for other failures or when no
// other candidate remains available.
func RetryAfterModelError(stateRoot string, used modelhealth.Ref, candidates []modelhealth.Ref, output []byte, failure error, now time.Time) (modelhealth.Ref, bool, error) {
	if used.Agent == "" || used.Model == "" {
		return modelhealth.Ref{}, false, nil
	}
	var exit *exec.ExitError
	if errors.As(failure, &exit) {
		output = append(append(output, '\n'), exit.Stderr...)
	}
	return RetryAfterModelStatus(stateRoot, modelhealth.Classify(used, string(output), failure, now), candidates, now)
}

// RetryAfterModelStatus is RetryAfterModelError for a caller that classified
// the failure itself.
func RetryAfterModelStatus(stateRoot string, status modelhealth.Status, candidates []modelhealth.Ref, now time.Time) (modelhealth.Ref, bool, error) {
	used := status.Ref
	// Without a state root the registry has no home; a caller such as the live
	// self-test then records nothing rather than writing into its directory.
	if stateRoot == "" || status.State != modelhealth.Unavailable || used.Agent == "" || used.Model == "" {
		return modelhealth.Ref{}, false, nil
	}
	registry, err := modelhealth.Open(stateRoot)
	if err != nil {
		return modelhealth.Ref{}, false, fmt.Errorf("model health registry: %w", err)
	}
	if err = registry.Record(status); err != nil {
		return modelhealth.Ref{}, false, fmt.Errorf("model health registry: %w", err)
	}
	if len(candidates) < 2 {
		return modelhealth.Ref{}, false, nil
	}
	next, ok := registry.Select(candidates, now)
	return next, ok && next != used, nil
}

// ReviewerRef recovers the model a reviewer command runs when the command is
// exactly what GenerateReviewerCommand builds for it, so a reviewer without
// fallback still records that model's error. Any other command reports false.
func ReviewerRef(command string) (modelhealth.Ref, bool) {
	ref, _ := matchReviewer(command)
	return ref, ref.Model != ""
}

func matchReviewer(command string) (modelhealth.Ref, agentcli.Profile) {
	var found modelhealth.Ref
	var matched agentcli.Profile
	for _, profile := range agentcli.List() {
		model, effort, ok := profile.MatchReviewerCommand(command)
		if !ok {
			continue
		}
		ref, err := modelhealth.Parse(profile.Name + ":" + model + ":" + effort)
		if err == nil {
			if found.Agent != "" {
				return modelhealth.Ref{}, agentcli.Profile{}
			}
			found, matched = ref, profile
		}
	}
	return found, matched
}

// ModelParked prefixes the 搁置 reason of a ticket parked because every model
// candidate of its role or tier was unavailable.
const ModelParked = "模型不可用"

// ModelParkReason names every candidate and when its cooldown ends, so the
// scheduler can read the candidates back from the 搁置 line.
func ModelParkReason(statuses []modelhealth.Status) string {
	parts := make([]string, 0, len(statuses))
	for _, s := range statuses {
		parts = append(parts, s.Ref.String()+" 至 "+s.Until.UTC().Format(time.RFC3339))
	}
	return ModelParked + "：" + strings.Join(parts, "；")
}
