package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/agentcli"
	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func TestCorrectionSessionSelectsModelFromRegistry(t *testing.T) {
	primary := modelhealth.Ref{Agent: "claude", Model: "claude-opus-5-5", Effort: "high"}
	fallback := modelhealth.Ref{Agent: "codex", Model: "gpt-6.2-astra", Effort: "high"}
	custom := modelhealth.Ref{Agent: "codex", Model: "configured-model", Effort: "medium"}
	for _, tc := range []struct {
		name, primaryEnv, fallbackEnv string
		unavailable, wantCalls        []modelhealth.Ref
		weeklyLimit, brokenRegistry   bool
	}{
		{name: "primary available", wantCalls: []modelhealth.Ref{primary}},
		{name: "primary weekly limit", unavailable: []modelhealth.Ref{primary}, wantCalls: []modelhealth.Ref{fallback}},
		{name: "unreadable registry", brokenRegistry: true},
		{name: "all unavailable", unavailable: []modelhealth.Ref{primary, fallback}},
		{name: "session weekly limit selects fallback next round", weeklyLimit: true, wantCalls: []modelhealth.Ref{primary, fallback}},
		{name: "configured primary", primaryEnv: custom.String(), wantCalls: []modelhealth.Ref{custom}},
		{name: "configured fallback", fallbackEnv: custom.String(), unavailable: []modelhealth.Ref{primary}, wantCalls: []modelhealth.Ref{custom}},
		{name: "invalid primary uses default", primaryEnv: "invalid", wantCalls: []modelhealth.Ref{primary}},
		{name: "invalid fallback uses default", fallbackEnv: "invalid", unavailable: []modelhealth.Ref{primary}, wantCalls: []modelhealth.Ref{fallback}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ANTHROPIC_BASE_URL", "https://inherited.invalid")
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "sentinel")
			state := t.TempDir()
			registry, err := modelhealth.Open(state)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if err := registry.Record(modelhealth.Status{Ref: primary, State: modelhealth.OK, CheckedAt: now}); err != nil {
				t.Fatal(err)
			}
			for _, ref := range tc.unavailable {
				if err := registry.Record(modelhealth.Status{Ref: ref, State: modelhealth.Unavailable, Reason: "usage_limit", CheckedAt: now, Until: now.Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.brokenRegistry {
				if err := os.WriteFile(filepath.Join(state, "model-health.json"), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			e, _, tools, _, _, ticket := recoveryFixture(t)
			writeRole(t, e.Config.Repo, "correction.md", "CORRECTION ROLE\n")
			var calls []runx.Spec
			limitErr := errors.New("exit status 1")
			commands := &Commands{StateRoot: state, CorrectionCandidates: correctionCandidates(tc.primaryEnv, tc.fallbackEnv), Run: func(_ context.Context, spec runx.Spec) ([]byte, error) {
				calls = append(calls, spec)
				if tc.weeklyLimit && len(calls) == 1 {
					return []byte("You've hit your weekly limit"), limitErr
				}
				return nil, nil
			}}
			if len(tc.wantCalls) == 0 {
				tools.gateReds = 1
				recovery := &fallbackRecovery{correction: commands.Correction}
				e.Recovery = recovery
				result, err := e.Execute(context.Background(), ticket)
				if err != nil || result.State.Owner != machine.OwnerMerge || result.State.Corrections != 1 {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				if len(calls) != 0 || len(recovery.requests) != 1 || len(recovery.specs) != 1 {
					t.Fatalf("model calls=%d correction requests=%d child tasks=%d", len(calls), len(recovery.requests), len(recovery.specs))
				}
				req := recovery.requests[0]
				summary, err := os.ReadFile(failLogPath(e.Config, ticket))
				if err != nil {
					t.Fatal(err)
				}
				want := "执行器代写：correction 会话未写出修复步骤（会话失败），按下列失败证据修复。\n失败原因：" + req.Cause + "\n失败摘要：" + strings.TrimSpace(string(summary))
				if !strings.Contains(recovery.specs[0], want+"\n") {
					t.Fatalf("child task missing fallback with gate summary %q: %q", want, recovery.specs[0])
				}
				return
			}
			req := CorrectionRequest{Repo: e.Config.Repo, Path: filepath.Join(state, "correction.md"), Cause: "gate red", Number: 1}
			if tc.weeklyLimit {
				err := commands.Correction(context.Background(), req)
				if !errors.Is(err, limitErr) || len(calls) != 1 {
					t.Fatalf("first correction: err=%v calls=%d, want exit status 1 after one call", err, len(calls))
				}
				req.Number++
			}
			if err := commands.Correction(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if len(calls) != len(tc.wantCalls) {
				t.Fatalf("calls=%d want=%d", len(calls), len(tc.wantCalls))
			}
			for i, want := range tc.wantCalls {
				spec := calls[i]
				for _, entry := range []string{"ANTHROPIC_BASE_URL=https://inherited.invalid", "ANTHROPIC_AUTH_TOKEN=sentinel"} {
					if slices.Contains(spec.Env, entry) != (want.Agent == "claude") {
						t.Errorf("unexpected relay environment for %s", want.Agent)
					}
				}

				mode, flags := "-p", map[string]string{"--model": want.Model, "--effort": want.Effort}
				if want.Agent == "codex" {
					if spec.Path != "/bin/sh" || len(spec.Args) < 5 {
						t.Fatalf("missing stdin wrapper in %+v", spec.Args)
					}
					spec.Path, spec.Args = spec.Args[3], spec.Args[4:]
					mode = "exec"
					flags = map[string]string{"--model": want.Model, "-c": "model_reasoning_effort=" + want.Effort,
						"--sandbox": "workspace-write", "--add-dir": filepath.Dir(req.Path)}
				}
				if spec.Path != want.Agent || spec.Timeout != 10*time.Minute || spec.Dir != req.Repo {
					t.Fatalf("call %d: path=%q want=%q timeout=%v dir=%q", i, spec.Path, want.Agent, spec.Timeout, spec.Dir)
				}
				if len(spec.Args) == 0 || spec.Args[0] != mode || (want.Agent == "claude" && !strings.Contains(strings.Join(spec.Args, " "), req.Path)) {
					t.Errorf("expected %s and correction path in %q", mode, spec.Args)
				}
				for flag, value := range flags {
					pos := slices.Index(spec.Args, flag)
					if pos < 0 || pos+1 >= len(spec.Args) || spec.Args[pos+1] != value {
						t.Errorf("expected %s %s in %q", flag, value, spec.Args)
					}
				}
			}
			if tc.weeklyLimit {
				registry, err = modelhealth.Open(state)
				if err != nil {
					t.Fatal(err)
				}
				if status := registry.Status(primary, time.Now()); status.State != modelhealth.Unavailable || status.Reason != "usage_limit" {
					t.Errorf("primary after weekly limit: %+v", status)
				}
			}
		})
	}
	t.Run("codex receives prompt on stdin", func(t *testing.T) {
		repo, state := t.TempDir(), t.TempDir()
		req := CorrectionRequest{Repo: repo, Path: filepath.Join(state, "correction.md"), Number: 1, Cause: "gate red", Brief: "first line\n$(exit 91) 'second line'"}
		writeRole(t, repo, "correction.md", "CORRECTION ROLE\n")
		probe := []byte("#!/bin/sh\ncat > \"$CAPTURE\"\n")
		if err := os.WriteFile(filepath.Join(repo, "codex"), probe, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", repo+string(os.PathListSeparator)+os.Getenv("PATH"))
		capture := filepath.Join(state, "stdin")
		t.Setenv("CAPTURE", capture)
		commands := Commands{StateRoot: state, CorrectionCandidates: correctionCandidates(fallback.String(), "")}
		if err := commands.Correction(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		input, err := os.ReadFile(capture)
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("CORRECTION ROLE\n\nCorrection file: %q\nCorrection: 1/3\nFailure: gate red\nBrief:\n%s", req.Path, req.Brief)
		if string(input) != want {
			t.Fatalf("Codex stdin = %q, want %q", input, want)
		}
	})
	t.Run("stalled before weekly limit", func(t *testing.T) {
		state := t.TempDir()
		req := CorrectionRequest{Repo: t.TempDir(), Path: filepath.Join(state, "correction.md"), Number: 2}
		writeRole(t, req.Repo, "correction.md", "CORRECTION ROLE\n")
		const verdict = "STALLED: 同一断言重复失败"
		limitErr := errors.New("exit status 1")
		calls := 0
		commands := Commands{StateRoot: state, CorrectionCandidates: correctionCandidates(primary.String(), fallback.String()), Run: func(_ context.Context, spec runx.Spec) ([]byte, error) {
			calls++
			if calls > 1 {
				t.Fatalf("started %q after the primary wrote STALLED", spec.Path)
			}
			if err := os.WriteFile(req.Path, []byte(verdict), 0o600); err != nil {
				t.Fatal(err)
			}
			return []byte("You've hit your weekly limit"), limitErr
		}}
		if err := commands.Correction(context.Background(), req); !errors.Is(err, limitErr) {
			t.Fatalf("session error = %v, want %v", err, limitErr)
		}
		if data, err := os.ReadFile(req.Path); err != nil || string(data) != verdict {
			t.Fatalf("correction file = %q, err = %v", data, err)
		}
		registry, err := modelhealth.Open(state)
		if err != nil {
			t.Fatal(err)
		}
		if status := registry.Status(primary, time.Now()); status.State != modelhealth.Unavailable || status.Reason != "usage_limit" {
			t.Errorf("primary after STALLED and weekly limit: %+v", status)
		}
	})
}

func TestReviewerRelayChangesWithCandidate(t *testing.T) {
	const proxy = "https://configured.invalid"
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sentinel")
	t.Setenv("ANTHROPIC_BASE_URL", "https://inherited.invalid")
	limit, err := os.ReadFile("../../testdata/recorded/claude-model-limit.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range [][]string{{"claude", "codex"}, {"codex", "claude"}} {
		refs := []modelhealth.Ref{{Agent: order[0], Model: "first", Effort: "high"}, {Agent: order[1], Model: "second", Effort: "high"}}
		command, err := config.GenerateReviewerCommand(refs[0].String())
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		c := Commands{ReviewerCommand: command, ReviewerCandidates: refs, StateRoot: t.TempDir(), ModelProxyURL: proxy}
		c.Run = withReviewerRole(t, func(_ context.Context, s runx.Spec) ([]byte, error) {
			if s.Path != "/bin/sh" {
				return nil, nil
			}
			agent := order[calls]
			calls++
			for _, entry := range []string{"ANTHROPIC_BASE_URL=" + proxy, "ANTHROPIC_AUTH_TOKEN=sentinel"} {
				if slices.Contains(s.Env, entry) != (agent == "claude") {
					t.Errorf("wrong relay environment for %s on call %d", agent, calls)
				}
			}
			if agent == "codex" && slices.Contains(s.Env, "ANTHROPIC_BASE_URL=https://inherited.invalid") {
				t.Error("non-relay reviewer inherited the proxy URL")
			}
			if calls == 1 {
				return limit, errors.New("model unavailable")
			}
			return nil, nil
		})
		if _, _, err := c.Review(t.Context(), t.TempDir(), "origin/main", "HEAD", nil); err != nil || calls != 2 {
			t.Fatalf("review calls=%d, err=%v", calls, err)
		}
	}
}

func TestReviewerWithoutCandidatesUsesProfileRelay(t *testing.T) {
	agentcli.Configure(map[string]agentcli.Profile{
		"local":  {Binary: "shared", ReviewArgs: []string{"--model", "{model}"}},
		"inline": {Binary: "inline", ReviewArgs: []string{"--model={model}", "--level={effort}"}},
		"remote": {Binary: "shared", ReviewArgs: []string{"--model", "{model}"}, Relay: true},
	})
	t.Cleanup(func() { agentcli.Configure(nil) })
	for _, role := range []string{"codex::", "codex:model$[2]:high", "codex:model with spaces:high", "local:chosen:high", "local:two words:high", "inline:two words:level with quote'"} {
		t.Run(role, func(t *testing.T) {
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "sentinel")
			command, err := config.GenerateReviewerCommand(role)
			if err != nil {
				t.Fatal(err)
			}
			const proxy = "https://configured.invalid"
			c := Commands{ReviewerCommand: command, ModelProxyURL: proxy}
			called := false
			c.Run = withReviewerRole(t, func(_ context.Context, s runx.Spec) ([]byte, error) {
				if s.Path == "/bin/sh" {
					called = true
					if s.Env == nil || slices.Contains(s.Env, "ANTHROPIC_AUTH_TOKEN=sentinel") || slices.Contains(s.Env, "ANTHROPIC_BASE_URL="+proxy) {
						t.Error("non-relay reviewer received relay settings")
					}
				}
				return nil, nil
			})
			if _, _, err := c.Review(t.Context(), t.TempDir(), "origin/main", "HEAD", nil); err != nil || !called {
				t.Fatalf("review called=%t, err=%v", called, err)
			}
		})
	}
}

func TestReviewerKeepsModelArgumentOnRetry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, file := range []string{"primary1", "fallback1"} {
		if err := os.WriteFile(filepath.Join(dir, file), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nprintf '%s' \"$3\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	limit, err := os.ReadFile("../../testdata/recorded/claude-model-limit.txt")
	if err != nil {
		t.Fatal(err)
	}
	refs := []modelhealth.Ref{{Agent: "claude", Model: "primary[1m]"}, {Agent: "claude", Model: "fallback[1m]"}}
	command, err := config.GenerateReviewerCommand(refs[0].String())
	if err != nil {
		t.Fatal(err)
	}
	c := Commands{ReviewerCommand: command, ReviewerCandidates: refs, StateRoot: t.TempDir()}
	var models []string
	c.Run = withReviewerRole(t, func(ctx context.Context, s runx.Spec) ([]byte, error) {
		if s.Path != "/bin/sh" {
			return nil, nil
		}
		ref := refs[len(models)]
		wantArgv := []string{"claude", "-p", "--model", ref.Model, "--tools", "Read,Grep,Glob", "--permission-mode", "dontAsk", "--restricted", "--strict-mcp-config"}
		if !slices.Equal(s.Args[3:], wantArgv) {
			t.Errorf("reviewer executable and arguments = %q, want %q", s.Args[3:], wantArgv)
		}
		s.Dir = dir
		out, err := runx.Run(ctx, s)
		if err != nil {
			return out, err
		}
		models = append(models, string(out))
		if len(models) == 1 {
			return limit, errors.New("model unavailable")
		}
		return out, nil
	})
	if _, _, err = c.Review(t.Context(), dir, "origin/main", "HEAD", nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(models, []string{refs[0].Model, refs[1].Model}) {
		t.Errorf("reviewer received models %q", models)
	}
}

func TestCorrectionFallbackCandidateList(t *testing.T) {
	home, repo, state := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENT_DRIVER_STATE_ROOT", state)
	primary := modelhealth.Ref{Agent: "claude", Model: "repo-primary", Effort: "high"}
	first := modelhealth.Ref{Agent: "codex", Model: "first-fallback", Effort: "high"}
	second := modelhealth.Ref{Agent: "claude", Model: "second-fallback", Effort: "medium"}
	for path, body := range map[string]string{
		filepath.Join(home, ".config/orca-factory/profiles.toml"):  "default='daily'\n[profile.daily]\nAGENT_CORRECTION_FALLBACK='" + first.String() + " " + second.String() + "'\n",
		filepath.Join(repo, "scripts/agent-gates/agent-graph.env"): "AGENT_GH_REPO=owner/repo\nAGENT_ROLE_REVIEWER_CMD='review command'\nAGENT_TIER_DEFAULT=codex:worker:high\nAGENT_CORRECTION=" + primary.String() + "\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeRole(t, repo, "correction.md", "CORRECTION ROLE\n")
	registry, err := modelhealth.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, ref := range []modelhealth.Ref{primary, first} {
		if err := registry.Record(modelhealth.Status{Ref: ref, State: modelhealth.Unavailable, Reason: "usage_limit", CheckedAt: now, Until: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	executor, _, err := repositoryExecutor(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	commands := executor.Tools.(*Commands)
	var calls []runx.Spec
	commands.Run = func(_ context.Context, spec runx.Spec) ([]byte, error) {
		calls = append(calls, spec)
		return nil, nil
	}
	if err := commands.Correction(context.Background(), CorrectionRequest{Repo: repo, Path: filepath.Join(state, "correction.md"), Cause: "gate red", Number: 1}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Path != second.Agent {
		t.Fatalf("correction calls=%+v, want one %s session", calls, second.Agent)
	}
	if pos := slices.Index(calls[0].Args, "--model"); pos < 0 || pos+1 >= len(calls[0].Args) || calls[0].Args[pos+1] != second.Model {
		t.Fatalf("correction args=%q, want --model %s", calls[0].Args, second.Model)
	}
}
