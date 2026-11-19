package config

import (
	"fmt"
	"strings"
	"time"

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
