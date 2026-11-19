package modelhealth_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
)

func TestClassifyModelOutputs(t *testing.T) {
	now, err := time.Parse(time.RFC3339, "2026-11-19T10:00:00+08:00")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, fixture, output string
		err                   error
		state, reason, until  string
	}{
		{name: "claude quota", fixture: "claude-model-limit.txt", state: "unavailable", reason: "usage_limit", until: "2026-11-21T01:00:00+08:00"},
		{name: "codex unsupported", fixture: "codex-model-unsupported.txt", err: errors.New("exit 1"), state: "unavailable", reason: "model_not_supported", until: "2026-11-19T10:30:00+08:00"},
		{name: "normal", output: "ok\n", state: "ok", reason: "probe_ok"},
		{name: "timeout", err: context.DeadlineExceeded, state: "unknown", reason: "timeout"},
		{name: "network error", err: errors.New("network"), state: "unknown", reason: "probe_failed"},
		{name: "bare error code", output: "model_not_found", err: errors.New("exit 1"), state: "unknown", reason: "probe_failed"},
		{name: "unexpected", output: "hello", state: "unknown", reason: "unexpected_output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := tc.output
			if tc.fixture != "" {
				b, e := os.ReadFile(filepath.Join("../../testdata/recorded", tc.fixture))
				if e != nil {
					t.Fatal(e)
				}
				output = string(b)
			}
			ref := modelhealth.Ref{Agent: "claude", Model: "fable", Effort: "high"}
			got := modelhealth.Classify(ref, output, tc.err, now)
			if got.State != tc.state || got.Reason != tc.reason || got.Ref != ref || !got.CheckedAt.Equal(now) {
				t.Fatalf("status = %+v", got)
			}
			if tc.until == "" {
				if !got.Until.IsZero() {
					t.Fatalf("unexpected until %v", got.Until)
				}
			} else if got.Until.Format(time.RFC3339) != tc.until {
				t.Fatalf("until = %s, want %s", got.Until.Format(time.RFC3339), tc.until)
			}
		})
	}
}

func TestRegistrySelectSkipsUnavailable(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 19, 2, 0, 0, 0, time.UTC)
	primary := modelhealth.Ref{Agent: "claude", Model: "fable", Effort: "high"}
	fallback := modelhealth.Ref{Agent: "codex", Model: "astra", Effort: "low"}
	registry, err := modelhealth.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	unavailable := modelhealth.Classify(primary, "You've hit your Fable limit", nil, now)
	if err := registry.Record(unavailable); err != nil {
		t.Fatal(err)
	}
	registry, err = modelhealth.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	got := registry.Status(primary, now)
	if got != unavailable {
		t.Fatalf("reopened = %+v, want %+v", got, unavailable)
	}
	// Effort does not create another availability record.
	primary.Effort = "max"
	selected, ok := registry.Select([]modelhealth.Ref{primary, fallback}, now)
	if !ok || selected != fallback {
		t.Fatalf("selected %+v, %v", selected, ok)
	}
	if _, ok := registry.Select([]modelhealth.Ref{primary}, now); ok {
		t.Fatal("selected unavailable model")
	}
	selected, ok = registry.Select([]modelhealth.Ref{primary, fallback}, now.Add(30*time.Minute))
	if !ok || selected != primary {
		t.Fatalf("expired selected %+v, %v", selected, ok)
	}
	if got := registry.Status(primary, now.Add(30*time.Minute)); got.State != "unknown" || got.Reason != "cooldown_expired" {
		t.Fatalf("expired = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(root, "model-health.json")); err != nil {
		t.Fatal(err)
	}
	second, err := modelhealth.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Record(modelhealth.Classify(fallback, "ok", nil, now)); err != nil {
		t.Fatal(err)
	}
	if err := registry.Record(unavailable); err != nil {
		t.Fatal(err)
	}
	registry, err = modelhealth.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if registry.Status(fallback, now).State != "ok" {
		t.Fatal("another writer's model lost")
	}
}

func TestRefRoundTrip(t *testing.T) {
	for _, value := range []string{"codex:gpt-6.2-astra:max", "codex::low", "claude:fable:"} {
		ref, err := modelhealth.Parse(value)
		if err != nil || ref.String() != value {
			t.Fatalf("parse %q = %+v, %v", value, ref, err)
		}
	}
	for _, value := range []string{"", ":model:low", "codex:model:low:extra", "codex"} {
		if _, err := modelhealth.Parse(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}

func TestRegistryRetainsNewerProbe(t *testing.T) {
	root := t.TempDir()
	stale, err := modelhealth.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	current, err := modelhealth.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	ref := modelhealth.Ref{Agent: "claude", Model: "m"}
	now := time.Now().UTC()
	newer := modelhealth.Status{Ref: ref, State: modelhealth.Unavailable, Reason: "usage_limit", CheckedAt: now.Add(time.Second), Until: now.Add(time.Hour)}
	if err := current.Record(newer); err != nil {
		t.Fatal(err)
	}
	if err := stale.Record(modelhealth.Classify(ref, "ok", nil, now)); err != nil {
		t.Fatal(err)
	}
	reopened, err := modelhealth.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Observe(modelhealth.Classify(ref, "ok", nil, now))
	for _, r := range []*modelhealth.Registry{stale, reopened} {
		if got := r.Status(ref, newer.CheckedAt); got != newer {
			t.Fatalf("status = %+v, want %+v", got, newer)
		}
		if _, ok := r.Select([]modelhealth.Ref{ref}, newer.CheckedAt); ok {
			t.Fatal("selected a model in cooldown")
		}
	}
}
