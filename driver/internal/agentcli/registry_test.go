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
	t.Cleanup(func() { agentcli.Configure(nil) })
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
	if !agentcli.Registered("agy") || !slices.Contains(agentcli.List(), "agy") {
		t.Fatal("registered agy missing from registry")
	}
}

func TestAgentCliBuiltins(t *testing.T) {
	for _, tc := range []struct {
		agent, reviewer string
		probe           []string
	}{
		{"claude", "claude -p --model chosen --effort high --tools Read,Grep,Glob --permission-mode dontAsk --restricted --strict-mcp-config",
			[]string{"-p", "--model", "chosen", "--tools", "", "--permission-mode", "dontAsk", "--restricted", "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence", "Reply with exactly: ok"}},
		{"codex", "env -u ORCA_PANE_KEY -u ORCA_AGENT_HOOK_PORT -u ORCA_AGENT_HOOK_TOKEN codex exec -m chosen -c model_reasoning_effort=high --sandbox read-only -",
			[]string{"exec", "-m", "chosen", "-c", "model_reasoning_effort=low", "--sandbox", "read-only", "--ephemeral", "Reply with exactly: ok"}},
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
	for _, profile := range []agentcli.Profile{p, withoutRelay, withPrompt, {Name: "plain", ReviewArgs: []string{"--pick", "{model}", "--set", "{effort}"}, Relay: true}} {
		agentcli.Register(profile)
	}

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
	if err != nil || got != `agy --model chosen --effort high --disable-slash-commands --mode plan --print "$(cat)"` {
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
	if out, err := exec.Command("go", "build", "-o", driver, "../../cmd/driver").CombinedOutput(); err != nil {
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
	agentcli.Register(agentcli.Profile{Name: "probe-alias", Binary: bin, ProbeArgs: []string{"{model}", "--level={effort}"}})
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
	agentcli.Register(one)
	agentcli.Register(two)
	command, err := config.GenerateReviewerCommand("alias-b:m:")
	if err != nil {
		t.Fatal(err)
	}
	if config.RelayReviewer(command, true) {
		t.Error("ambiguous command allowed relay credentials")
	}
}
