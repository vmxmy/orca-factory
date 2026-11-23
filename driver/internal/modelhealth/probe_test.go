package modelhealth_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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
			got := modelhealth.Probe(context.Background(), ref, dir, modelhealth.Endpoint{})
			if got.State != "ok" || got.Ref != ref || got.CheckedAt.IsZero() {
				t.Fatalf("probe = %+v", got)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "args"))
			if err != nil {
				t.Fatal(err)
			}
			want := "exec\n-m\nchosen\n-c\nmodel_reasoning_effort=low\n--sandbox\nread-only\n--ephemeral\nReply with exactly: ok\n"
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
			got = modelhealth.Probe(context.Background(), ref, dir, modelhealth.Endpoint{})
			if got.State != "unavailable" || got.Reason != "model_not_supported" || got.Until.Sub(got.CheckedAt) != 30*time.Minute {
				t.Fatalf("stderr = %+v", got)
			}
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
			got = modelhealth.Probe(ctx, ref, dir, modelhealth.Endpoint{})
			if got.State != "unknown" || got.Reason != "timeout" {
				t.Fatalf("deadline = %+v", got)
			}
		})
	}
}

func TestProbeEndpoint(t *testing.T) {
	const inherited = "https://inherited.invalid"
	const proxy = "https://proxy.invalid"
	t.Setenv("ANTHROPIC_BASE_URL", inherited)
	t.Setenv("PROBE_SERVICE_SECRET", "service")
	for _, tc := range []struct {
		name     string
		endpoint modelhealth.Endpoint
		onPath   bool
		want     string
	}{
		{"inherited", modelhealth.Endpoint{}, true, inherited + "|service"},
		{"reviewer proxy", modelhealth.Endpoint{BaseURL: proxy}, true, proxy + "|service"},
		{"isolated binary", modelhealth.Endpoint{Isolated: true}, false, "|"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			script := "#!/bin/sh\nprintf '%s|%s' \"$ANTHROPIC_BASE_URL\" \"$PROBE_SERVICE_SECRET\" > endpoint\nprintf ok\n"
			if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			endpoint := tc.endpoint
			if tc.onPath {
				t.Setenv("PATH", bin)
			} else {
				t.Setenv("PATH", dir)
				endpoint.Binary = filepath.Join(bin, "claude")
			}
			ref := modelhealth.Ref{Agent: "claude", Model: "m"}
			if got := modelhealth.Probe(context.Background(), ref, dir, endpoint); got.State != modelhealth.OK {
				t.Fatalf("probe = %+v", got)
			}
			got, err := os.ReadFile(filepath.Join(dir, "endpoint"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("Claude received %q, want %q", got, tc.want)
			}
		})
	}
}

func TestModelRelayProbe(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent+"/inherited", func(t *testing.T) {
			const token = "fake-inherited-probe-sentinel"
			t.Setenv("AGENT_MODEL_PROXY_TOKEN", token)
			t.Setenv("PROBE_INHERITED", "present")
			dir := t.TempDir()
			binary := filepath.Join(dir, "agent")
			script := "#!/bin/sh\n" +
				"test -z \"${AGENT_MODEL_PROXY_TOKEN+x}\" || exit 1\n" +
				"env | grep -Fq '" + token + "' && exit 1\n" +
				"test \"$PROBE_INHERITED\" = present && printf ok\n"
			if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			got := modelhealth.Probe(context.Background(), modelhealth.Ref{Agent: agent}, dir, modelhealth.Endpoint{Binary: binary})
			if got.State != modelhealth.OK {
				t.Errorf("non-isolated probe inherited the wrong environment: %s", got.State)
			}
		})
		for _, token := range []string{"", "fake-relay-token"} {
			t.Run(agent+"/"+token, func(t *testing.T) {
				dir := t.TempDir()
				binary := filepath.Join(dir, "agent")
				if err := os.WriteFile(binary, []byte("#!/bin/sh\n/bin/cat /proc/$$/environ > env\nprintf ok\n"), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("UNRELATED_SECRET", "must-not-reach-probe")
				endpoint := modelhealth.Endpoint{Binary: binary, BaseURL: "https://relay.invalid", AuthToken: token, Isolated: true}
				got := modelhealth.Probe(context.Background(), modelhealth.Ref{Agent: agent}, dir, endpoint)
				if got.State != modelhealth.OK {
					t.Fatalf("probe state = %s", got.State)
				}
				raw, err := os.ReadFile(filepath.Join(dir, "env"))
				if err != nil {
					t.Fatal(err)
				}
				env := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
				want := []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")}
				if agent == "claude" {
					want = append(want, "ANTHROPIC_BASE_URL=https://relay.invalid")
					if token != "" {
						want = append(want, "ANTHROPIC_AUTH_TOKEN="+token)
					}
				}
				slices.Sort(env)
				slices.Sort(want)
				if !slices.Equal(env, want) {
					t.Fatal("unexpected probe environment")
				}
			})
		}
	}
}
