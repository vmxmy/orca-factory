package agentcli_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/agentcli"
	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
)

func TestAgentCliRegistryLookupAgy(t *testing.T) {
	want := agentcli.Profile{
		Name: "agy", Binary: "agy",
		ReviewArgs: []string{"--mode", "plan"}, ProbeArgs: []string{"--mode", "plan"},
		CorrectionArgs: []string{"--mode", "accept-edits", "--add-dir", "{dir}"},
	}
	agentcli.Register(agentcli.Profile{Name: "first"})
	agentcli.Register(want)
	if !agentcli.Registered("first") {
		t.Fatal("Register removed another profile")
	}
	got, ok := agentcli.Lookup("agy")
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("Lookup(agy) = %+v, %t; want %+v", got, ok, want)
	}
	if !agentcli.Registered("agy") {
		t.Fatal("agy was not registered")
	}
	list := agentcli.List()
	i := slices.IndexFunc(list, func(p agentcli.Profile) bool { return p.Name == "agy" })
	if i < 0 || !reflect.DeepEqual(list[i], want) {
		t.Fatalf("List() = %+v; want registered profile %+v", list, want)
	}
	list[i].ReviewArgs[0] = "changed"
	if stored, _ := agentcli.Lookup("agy"); !reflect.DeepEqual(stored, want) {
		t.Fatalf("editing a listed profile changed the registry: %+v", stored)
	}
}

func TestAgentCliBuiltins(t *testing.T) {
	for _, tc := range []struct {
		agent, reviewer   string
		probe, correction []string
		stdin             bool
	}{
		{"claude", "claude -p --model chosen --effort high --tools Read,Grep,Glob --permission-mode dontAsk --restricted --strict-mcp-config",
			[]string{"-p", "--model", "chosen", "--tools", "", "--permission-mode", "dontAsk", "--restricted", "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence", "Reply with exactly: ok"},
			[]string{"-p", "prompt", "--model", "chosen", "--effort", "high", "--permission-mode", "acceptEdits", "--add-dir", "/work", "--allowedTools", "Read,Grep,Glob,Write,Edit", "--tools", "Read,Grep,Glob,Write,Edit"}, false},
		{"codex", "env -u ORCA_PANE_KEY -u ORCA_AGENT_HOOK_PORT -u ORCA_AGENT_HOOK_TOKEN codex exec -m chosen -c model_reasoning_effort=high --sandbox read-only -",
			[]string{"exec", "-m", "chosen", "-c", "model_reasoning_effort=low", "--sandbox", "read-only", "--ephemeral", "Reply with exactly: ok"},
			[]string{"exec", "--model", "chosen", "-c", "model_reasoning_effort=high", "--sandbox", "workspace-write", "--add-dir", "/work", "--ephemeral", "-"}, true},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			p, ok := agentcli.Lookup(tc.agent)
			if !ok {
				t.Fatal("builtin missing")
			}
			command, err := config.GenerateReviewerCommand(tc.agent + ":chosen:high")
			if err != nil || command != tc.reviewer {
				t.Fatalf("reviewer = %q, %v; want %q", command, err, tc.reviewer)
			}
			if args := p.ProbeArgv("chosen", "high", "Reply with exactly: ok"); !slices.Equal(args, tc.probe) {
				t.Fatalf("probe = %q; want %q", args, tc.probe)
			}
			if args := p.CorrectionArgv("chosen", "high", "prompt", "/work"); !slices.Equal(args, tc.correction) {
				t.Errorf("correction = %q; want %q", args, tc.correction)
			}
			if p.Executable() != tc.agent || p.CorrectionStdin() != tc.stdin {
				t.Errorf("executable = %q, correction stdin = %t; want %q, %t", p.Executable(), p.CorrectionStdin(), tc.agent, tc.stdin)
			}
		})
	}
}

func TestRegisteredReviewerRefAndRelay(t *testing.T) {
	t.Cleanup(func() { agentcli.Configure(nil) })
	p := agentcli.Profile{
		Name: "custom", ReviewArgs: []string{"review", "--pick", "{model}", "--set", "depth={effort}", "--read-only"}, Relay: true,
	}
	withoutRelay := p
	withoutRelay.Name, withoutRelay.Relay = "local", false
	withPrompt := p
	withPrompt.Name = "prompted"
	withPrompt.ReviewArgs = slices.Concat(p.ReviewArgs, []string{"--prompt", "{prompt}"})
	agentcli.Configure(map[string]agentcli.Profile{
		p.Name: p, withoutRelay.Name: withoutRelay, withPrompt.Name: withPrompt,
		"plain": {ReviewArgs: []string{"--pick", "{model}", "--set", "{effort}"}, Relay: true},
	})

	for _, tc := range []struct {
		agent string
		relay bool
	}{
		{"custom", true}, {"local", false}, {"prompted", true}, {"plain", true},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			want := modelhealth.Ref{Agent: tc.agent, Model: "m", Effort: "high"}
			command, err := config.GenerateReviewerCommand(want.String())
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := config.ReviewerRef(command); !ok || got != want {
				t.Errorf("ReviewerRef(%q) = %+v, %t; want %+v, true", command, got, ok, want)
			}
			if got := config.RelayReviewer(command, true); got != tc.relay {
				t.Errorf("RelayReviewer(%q) = %t; want %t", command, got, tc.relay)
			}
			for _, changed := range []string{command + " && true", command + ";true", command + " "} {
				if _, ok := config.ReviewerRef(changed); ok || config.RelayReviewer(changed, true) {
					t.Errorf("accepted modified reviewer command %q", changed)
				}
			}
		})
	}
	for _, command := range []string{
		"unregistered review --pick m --set depth=high --read-only",
		p.ReviewerCommand("m", "high") + ` --prompt "$(cat)"`,
	} {
		if _, ok := config.ReviewerRef(command); ok || config.RelayReviewer(command, true) {
			t.Errorf("accepted reviewer command outside its profile %q", command)
		}
	}
	for _, command := range []string{
		withPrompt.ReviewerCommand("$MODEL", "high"),
		withPrompt.ReviewerCommand("m", "high*"),
	} {
		if config.RelayReviewer(command, true) {
			t.Errorf("accepted unsafe relay arguments in %q", command)
		}
	}
	defaultModel := withPrompt.ReviewerCommand("", "high")
	if !config.RelayReviewer(defaultModel, true) || config.RelayReviewer(defaultModel, false) {
		t.Errorf("only a structured reviewer may relay with the default model: %q", defaultModel)
	}
}

const agyDriverTOML = `[agents.agy]
binary = "agy"
probe_args = ["--model", "{model}", "--sandbox", "--mode", "plan", "--print"]
review_args = ["--model", "{model}", "--effort", "{effort}", "--disable-slash-commands", "--mode", "plan", "--print", "{prompt}"]
correction_args = ["--print", "{prompt}", "--model", "{model}", "--effort", "{effort}", "--mode", "accept-edits", "--add-dir", "{dir}"]
relay = false
`

func TestLoadAgyProfile(t *testing.T) {
	t.Cleanup(func() { agentcli.Configure(nil) })
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "orca-factory")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "driver.toml"), []byte(agyDriverTOML), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(home); err != nil {
		t.Fatal(err)
	}
	p, ok := agentcli.Lookup("agy")
	if !ok || p.Binary != "agy" || p.Relay {
		t.Fatalf("driver.toml profile = %+v, %t", p, ok)
	}
	got, err := config.GenerateReviewerCommand("agy:chosen:high")
	if err != nil || got != `orca_review_prompt=$(cat && printf .) || exit 1; orca_review_prompt=${orca_review_prompt%.}; agy --model chosen --effort high --disable-slash-commands --mode plan --print "${orca_review_prompt}"` {
		t.Fatalf("reviewer = %q, %v", got, err)
	}
	if got := p.ProbeArgv("chosen", "high", "probe"); !slices.Equal(got, []string{"--model", "chosen", "--sandbox", "--mode", "plan", "--print", "probe"}) {
		t.Fatalf("probe = %q", got)
	}
	if got := p.CorrectionArgv("chosen", "high", "update file", "/dir"); !slices.Equal(got, []string{"--print", "update file", "--model", "chosen", "--effort", "high", "--mode", "accept-edits", "--add-dir", "/dir"}) {
		t.Fatalf("correction = %q", got)
	}
}

func TestDriverUsesConfiguredAgent(t *testing.T) {
	home, bin, repo := t.TempDir(), t.TempDir(), t.TempDir()
	driver := filepath.Join(bin, "orca-factory-driver")
	build := exec.Command("go", "build", "-o", driver, "../../cmd/driver")
	// On Actions the outer race build shares a 1 GiB /tmp with this subprocess.
	if scratch := os.Getenv("RUNNER_TEMP"); scratch != "" && os.Getenv("GOTMPDIR") == "" {
		build.Env = append(build.Environ(), "GOTMPDIR="+scratch)
	}
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build driver: %v\n%s", err, out)
	}
	dir := filepath.Join(home, ".config", "orca-factory")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	const machine = "max_in_flight = 3\n" + agyDriverTOML
	configPath := filepath.Join(dir, "driver.toml")
	if err := os.WriteFile(configPath, []byte(machine), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command(driver, args...)
		cmd.Dir = repo
		cmd.Env = []string{"HOME=" + home, "PATH=" + bin, "ORCA_TERMINAL_HANDLE=test"}
		return cmd.CombinedOutput()
	}

	t.Run("exec", func(t *testing.T) {
		git, err := exec.LookPath("git")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\n[ \"$*\" = 'auth token' ] && echo fixture-token\n"), 0700); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(git, "init", "-q", repo).CombinedOutput(); err != nil {
			t.Fatalf("init repository: %v\n%s", err, out)
		}
		gates := filepath.Join(repo, "scripts", "agent-gates")
		if err := os.MkdirAll(gates, 0700); err != nil {
			t.Fatal(err)
		}
		var reads atomic.Int32
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/repos/test/demo/issues" {
				t.Errorf("unexpected ticket request: %s %s", r.Method, r.URL.Path)
			}
			reads.Add(1)
			_, _ = w.Write([]byte("[]"))
		}))
		defer backend.Close()
		for _, tc := range []struct {
			agent, message string
			reads          int32
		}{
			{"agy", "task not found", 1},
			{"unregistered", `repository profile key AGENT_ROLE_REVIEWER: unsupported reviewer agent "unregistered"`, 0},
		} {
			t.Run(tc.agent, func(t *testing.T) {
				env := fmt.Sprintf("AGENT_GH_REPO=test/demo\nAGENT_ISSUE_BACKEND=github\nAGENT_GH_API_URL=%s\nAGENT_TIER_DEFAULT=codex::\nAGENT_ROLE_REVIEWER=%s:m:high\n", backend.URL, tc.agent)
				if err := os.WriteFile(filepath.Join(gates, "agent-graph.env"), []byte(env), 0600); err != nil {
					t.Fatal(err)
				}
				before := reads.Load()
				out, err := run("exec", "TEST-1")
				exit, ok := err.(*exec.ExitError)
				if !ok || exit.ExitCode() != 1 || !slices.Contains(strings.Split(string(out), "\n"), tc.message) || reads.Load()-before != tc.reads {
					t.Fatalf("exec: %v, %d ticket reads; want exit 1, %q, %d reads\n%s", err, reads.Load()-before, tc.message, tc.reads, out)
				}
				if tc.agent == "agy" {
					before := reads.Load()
					raw, err := run("status", "--fresh", "--repo", repo, "--json")
					var view struct{ State string }
					if err != nil || json.Unmarshal(raw, &view) != nil || view.State != "ok" || reads.Load() == before {
						t.Fatalf("fresh status: %v, reads=%d: %s", err, reads.Load()-before, raw)
					}
				}

			})
		}
	})

	t.Run("doctor", func(t *testing.T) {
		raw, err := run("doctor", "--json")
		var report struct{ Agents []string }
		if json.Unmarshal(raw, &report) != nil || !slices.Equal(report.Agents, []string{"agy", "claude", "codex"}) {
			t.Fatalf("doctor agents = %v, err = %v, output = %s", report.Agents, err, raw)
		}
	})

	t.Run("config", func(t *testing.T) {
		for _, tc := range []struct {
			name, extra string
			problems    []string
		}{
			{"valid", "", nil},
			{"unknown key", "misspelled = true\n", []string{"driver.toml 的 agents.agy.misspelled 不被识别，不生效"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if err := os.WriteFile(configPath, []byte(machine+tc.extra), 0600); err != nil {
					t.Fatal(err)
				}
				raw, err := run("config", "--json")
				if err != nil {
					t.Fatalf("config --json: %v\n%s", err, raw)
				}
				type entry struct{ Key, Value, Source string }
				var snapshot struct {
					Agents    []string
					Problems  []string
					Scheduler []entry
				}
				if err := json.Unmarshal(raw, &snapshot); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(snapshot.Agents, []string{"agy", "claude", "codex"}) {
					t.Errorf("config agents = %v", snapshot.Agents)
				}
				var problems []string
				for _, problem := range snapshot.Problems {
					if strings.HasPrefix(problem, "driver.toml ") {
						problems = append(problems, problem)
					}
				}
				if !slices.Equal(problems, tc.problems) {
					t.Errorf("driver.toml problems = %q; want %q", problems, tc.problems)
				}
				for _, want := range []entry{
					{"max_in_flight", "3", "driver.toml"}, {"token_bucket.capacity", "4", "default"},
				} {
					if !slices.Contains(snapshot.Scheduler, want) {
						t.Errorf("missing scheduler entry %+v in %+v", want, snapshot.Scheduler)
					}
				}
			})
		}
	})
}

func TestConfiguredProbeBinaryAndTemplates(t *testing.T) {
	t.Cleanup(func() { agentcli.Configure(nil) })
	dir := t.TempDir()
	bin := filepath.Join(dir, "probe binary")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n[ \"$#\" = 3 ] && [ \"$1\" = 'm v' ] && [ \"$2\" = '--level=high' ] && [ \"$3\" = 'Reply with exactly: ok' ] && echo ok\n"), 0700); err != nil {
		t.Fatal(err)
	}
	agentcli.Configure(map[string]agentcli.Profile{"probe-alias": {Binary: bin, ProbeArgs: []string{"{model}", "--level={effort}"}}})
	got := modelhealth.Probe(t.Context(), modelhealth.Ref{Agent: "probe-alias", Model: "m v", Effort: "high"}, dir, modelhealth.Endpoint{})
	if got.State != modelhealth.OK {
		t.Fatalf("probe = %+v", got)
	}
}

func TestReviewerSharedBinaryRequiresUniqueRelayProfile(t *testing.T) {
	t.Cleanup(func() { agentcli.Configure(nil) })
	one := agentcli.Profile{Name: "alias-a", Binary: "shared", ReviewArgs: []string{"--model", "{model}"}, Relay: true}
	two := one
	two.Name, two.Relay = "alias-b", false
	agentcli.Configure(map[string]agentcli.Profile{one.Name: one, two.Name: two})
	command, err := config.GenerateReviewerCommand("alias-b:m:")
	if err != nil {
		t.Fatal(err)
	}
	if config.RelayReviewer(command, true) {
		t.Error("ambiguous command allowed relay credentials")
	}
}

func TestProbeKeepsFlagBeforeOmittedInlineModel(t *testing.T) {
	p := agentcli.Profile{ProbeArgs: []string{"--sandbox", "--model={model}", "--print"}}
	if got := p.ProbeArgv("", "", "ping"); !slices.Equal(got, []string{"--sandbox", "--print", "ping"}) {
		t.Fatalf("default-model probe arguments = %q", got)
	}
}

func TestProbeKeepsOperandsAfterOptionTerminator(t *testing.T) {
	p := agentcli.Profile{ProbeArgs: []string{"--sandbox", "--", "{model}", "--literal", "{effort}"}}
	if got := p.ProbeArgv("", "", "ping"); !slices.Equal(got, []string{"--sandbox", "--", "--literal", "ping"}) {
		t.Fatalf("default probe arguments = %q", got)
	}
}

func TestProbeKeepsStdinOperand(t *testing.T) {
	p := agentcli.Profile{ProbeArgs: []string{"-", "{model}", "--print"}}
	if got := p.ProbeArgv("", "", "ping"); !slices.Equal(got, []string{"-", "--print", "ping"}) {
		t.Fatalf("default probe arguments = %q", got)
	}
}

func TestProbeKeepsSandboxBeforeOptionalOperand(t *testing.T) {
	p := agentcli.Profile{ProbeArgs: []string{"--sandbox", "{model}", "--print"}}
	if got := p.ProbeArgv("", "", "ping"); !slices.Equal(got, []string{"--sandbox", "--print", "ping"}) {
		t.Fatalf("default probe arguments = %q", got)
	}
}

func TestLoadKeepsExplicitRegistration(t *testing.T) {
	const name = "registered-extension"
	agentcli.Register(agentcli.Profile{Name: name, Binary: "extension"})
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "orca-factory")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "driver.toml"), []byte("[agents.machine-only]\nbinary = 'machine'\n[agents.registered-extension]\nbinary = 'override'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(home); err != nil {
		t.Fatal(err)
	}
	if p, ok := agentcli.Lookup(name); !ok || p.Executable() != "override" || !agentcli.Registered("machine-only") {
		t.Fatalf("machine profiles after load: %v, %v", p, agentcli.List())
	}
	if _, err := config.Load(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if agentcli.Registered("machine-only") {
		t.Error("reload retained a removed machine profile")
	}
	if p, ok := agentcli.Lookup(name); !ok || p.Executable() != "extension" {
		t.Fatalf("registered extension after load = %+v, %t", p, ok)
	}
}

func TestClaudeExtendedContextReviewer(t *testing.T) {
	want := modelhealth.Ref{Agent: "claude", Model: "opus[1m]", Effort: "high"}
	command, err := config.GenerateReviewerCommand(want.String())
	const original = "claude -p --model opus[1m] --effort high --tools Read,Grep,Glob --permission-mode dontAsk --restricted --strict-mcp-config"
	if err != nil || command != original {
		t.Errorf("reviewer command = %q, %v; want %q", command, err, original)
	}
	if got, ok := config.ReviewerRef(original); !ok || got != want {
		t.Errorf("original reviewer ref = %+v, %t; want %+v", got, ok, want)
	}
	if config.RelayReviewer(original, true) {
		t.Error("extended-context alias passed the relay literal check")
	}
}

func TestClaudeReviewerLiteralModel(t *testing.T) {
	for _, name := range []string{"model$", "model$.v2", "model$[v2", "{v2.1}", "}v2", "!v2", "v2]", "model{v2}", "model{v2.1}", "team,model{v2.1}", "team%2Fopus", "team~opus", "team#opus", "team·opus"} {
		original := "claude -p --model " + name + " --effort high --tools Read,Grep,Glob --permission-mode dontAsk --restricted --strict-mcp-config"
		want := modelhealth.Ref{Agent: "claude", Model: name, Effort: "high"}
		if command, err := config.GenerateReviewerCommand(want.String()); err != nil || command != original {
			t.Errorf("reviewer command = %q, %v; want %q", command, err, original)
		}
		if got, ok := config.ReviewerRef(original); !ok || got != want {
			t.Errorf("original reviewer ref = %+v, %t; want %+v", got, ok, want)
		}
	}
}

func TestCodexReviewerBracketModel(t *testing.T) {
	const prefix = "env -u ORCA_PANE_KEY -u ORCA_AGENT_HOOK_PORT -u ORCA_AGENT_HOOK_TOKEN codex exec -m "
	for _, tc := range []struct{ model, effort, wantArgs string }{
		{"model$[v2", "high", "model$[v2 -c model_reasoning_effort=high"},
		{"model$[2]", "high", "model$[2] -c model_reasoning_effort=high"},
		{"model$[v2", "high]", "model$[v2 -c model_reasoning_effort=high]"},
	} {
		command, err := config.GenerateReviewerCommand("codex:" + tc.model + ":" + tc.effort)
		if want := prefix + tc.wantArgs + " --sandbox read-only -"; err != nil || command != want {
			t.Errorf("reviewer command = %q, %v; want %q", command, err, want)
		}
	}
}

func TestInlineAgentPrompt(t *testing.T) {
	const prompt = "Reply with exactly: ok"
	for _, template := range []string{"--print=" + prompt, "--print={prompt}"} {
		p := agentcli.Profile{ProbeArgs: []string{template, "--effort", "{effort}"}}
		if got := p.ProbeArgv("", "high", prompt); !slices.Equal(got, []string{"--print=" + prompt, "--effort", "high"}) {
			t.Errorf("probe args = %q", got)
		}
	}
	p := agentcli.Profile{Binary: "printf", ReviewArgs: []string{"%s", "--print={prompt}/{prompt}"}}
	cmd := exec.Command("/bin/sh", "-c", p.ReviewerCommand("", ""))
	const input = "two words 'quoted'\nnext line\n\n"
	cmd.Stdin = strings.NewReader(input)
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != "--print="+input+"/"+input {
		t.Errorf("reviewer argv: %q, %v", out, err)
	}
}
