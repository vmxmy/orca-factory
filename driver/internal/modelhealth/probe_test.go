package modelhealth_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
)

func TestProbeCommands(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > args\nprintf ok\n"
			if err := os.WriteFile(filepath.Join(dir, agent), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			ref := modelhealth.Ref{Agent: agent, Model: "chosen", Effort: "max"}
			got := modelhealth.Probe(context.Background(), ref, dir)
			if got.State != "ok" || got.Ref != ref || got.CheckedAt.IsZero() {
				t.Fatalf("probe = %+v", got)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "args"))
			if err != nil {
				t.Fatal(err)
			}
			want := "exec\n-m\nchosen\n-c\nmodel_reasoning_effort=low\n--sandbox\nread-only\nReply with exactly: ok\n"
			if agent == "claude" {
				want = "-p\n--model\nchosen\n--tools\n\n--permission-mode\ndontAsk\n--restricted\n--strict-mcp-config\n--disable-slash-commands\n--no-session-persistence\nReply with exactly: ok\n"
			}
			if string(raw) != want {
				t.Fatalf("args = %q, want %q", raw, want)
			}
			fixture, err := os.ReadFile("../../testdata/recorded/codex-model-unsupported.txt")
			if err != nil {
				t.Fatal(err)
			}
			script = "#!/bin/sh\ncat <<'MODEL_ERROR' >&2\n" + string(fixture) + "MODEL_ERROR\nexit 1\n"
			script = strings.Replace(script, "cat <<", "/bin/cat <<", 1)
			if err := os.WriteFile(filepath.Join(dir, agent), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			got = modelhealth.Probe(context.Background(), ref, dir)
			if got.State != "unavailable" || got.Reason != "model_not_supported" || got.Until.Sub(got.CheckedAt) != 30*time.Minute {
				t.Fatalf("stderr = %+v", got)
			}
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
			got = modelhealth.Probe(ctx, ref, dir)
			if got.State != "unknown" || got.Reason != "timeout" {
				t.Fatalf("deadline = %+v", got)
			}
		})
	}
}
