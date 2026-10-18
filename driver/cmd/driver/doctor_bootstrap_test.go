package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/deploy"
)

type bootstrapFixture struct {
	bin, home string
}

func newBootstrapFixture(t *testing.T) bootstrapFixture {
	t.Helper()
	root := t.TempDir()
	return bootstrapFixture{bin: filepath.Join(root, "bin"), home: filepath.Join(root, "home")}
}

func (f bootstrapFixture) tool(t *testing.T, name, body string) {
	t.Helper()
	writeDoctorFile(t, filepath.Join(f.bin, name), "#!/bin/sh\n"+body+"\n", 0o755)
}

func (f bootstrapFixture) replay(t *testing.T, name, stdout, stderr, exit string) {
	t.Helper()
	body := ""
	if stdout != "" {
		body += "cat " + recordedPath(t, stdout) + "\n"
	}
	if stderr != "" {
		body += "cat " + recordedPath(t, stderr) + " >&2\n"
	}
	if exit != "" {
		body += "exit \"$(cat " + recordedPath(t, exit) + ")\""
	}
	f.tool(t, name, body)
}

func (f bootstrapFixture) config(t *testing.T, name, content string) {
	t.Helper()
	writeDoctorFile(t, filepath.Join(f.home, ".config", "orca-factory", name), content, 0o600)
}

func (f bootstrapFixture) probe(reachable error) bootstrapProbe {
	return bootstrapProbe{
		path:      f.bin,
		home:      f.home,
		user:      "tester",
		goMod:     filepath.Join("..", "..", "go.mod"),
		getenv:    func(string) string { return "" },
		reachable: func(context.Context, string) error { return reachable },
	}
}

func runBootstrapProbe(t *testing.T, probe bootstrapProbe) (healthReport, string, int) {
	t.Helper()
	var text, raw bytes.Buffer
	code := probe.main(context.Background(), []string{"--bootstrap"}, &text)
	probe.main(context.Background(), []string{"--bootstrap", "--json"}, &raw)
	var report healthReport
	if err := json.Unmarshal(raw.Bytes(), &report); err != nil {
		t.Fatalf("bootstrap JSON: %v\n%s", err, raw.String())
	}
	return report, text.String() + raw.String(), code
}

// TestDoctorBootstrapReportsMissingPrerequisites replays recorded gh, systemctl,
// loginctl, go and orca output: on a machine with only a logged-out gh, every
// other item is fail or human with a fix; a complete machine is all ok.
func TestDoctorBootstrapReportsMissingPrerequisites(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		f := newBootstrapFixture(t)
		f.replay(t, "gh", "", "gh-auth-status-logged-out.stderr", "gh-auth-status-logged-out.exit")
		f.replay(t, "systemctl", "", "systemctl-user-no-bus.stderr", "systemctl-user-no-bus.exit")
		f.config(t, "driver.env", "AGENT_FEISHU_APP_ID=TEST-PLACEHOLDER-CRED\nANTHROPIC_BASE_URL=http://127.0.0.1:9/TEST-PLACEHOLDER-CRED\n")
		report, out, code := runBootstrapProbe(t, f.probe(errors.New("connection refused")))
		if code != 1 {
			t.Fatalf("exit = %d, want 1\n%s", code, out)
		}
		for _, name := range bootstrapChecks {
			check, ok := report.Checks[name]
			if name == "gh" {
				continue
			}
			if !ok || (check.Status != healthFail && check.Status != healthHuman) || check.Fix == "" {
				t.Errorf("%s = %+v, want fail or human with a fix", name, check)
			}
		}
		if report.Checks["gh_auth"].Status != healthHuman || report.Checks["driver_env"].Status != healthHuman {
			t.Errorf("login and credentials must be human: %+v %+v", report.Checks["gh_auth"], report.Checks["driver_env"])
		}
		if !strings.Contains(report.Checks["driver_env"].Reason, "AGENT_FEISHU_APP_SECRET") {
			t.Errorf("driver_env reason must name the missing variable: %q", report.Checks["driver_env"].Reason)
		}
		if strings.Contains(out, "TEST-PLACEHOLDER-CRED") {
			t.Errorf("output leaks a variable value:\n%s", out)
		}
	})
	t.Run("reviewer proxy", func(t *testing.T) {
		f := newBootstrapFixture(t)
		f.config(t, "driver.env", "AGENT_MODEL_PROXY_URL=http://127.0.0.1:9/TEST-PLACEHOLDER-CRED\n")
		report, out, _ := runBootstrapProbe(t, f.probe(errors.New("connection refused")))
		check := report.Checks["model_proxy"]
		if check.Status != healthFail || !strings.Contains(check.Reason, "AGENT_MODEL_PROXY_URL") {
			t.Errorf("model_proxy = %+v, want fail naming AGENT_MODEL_PROXY_URL", check)
		}
		if strings.Contains(out, "TEST-PLACEHOLDER-CRED") {
			t.Errorf("output leaks a variable value:\n%s", out)
		}
	})
	t.Run("complete", func(t *testing.T) {
		f := newBootstrapFixture(t)
		f.replay(t, "go", "go-version.txt", "", "")
		f.replay(t, "gh", "", "", "gh-auth-status-ok.exit")
		f.tool(t, "git", "")
		f.tool(t, "jq", "")
		f.tool(t, "claude", "")
		f.replay(t, "systemctl", "systemctl-user-system-state.txt", "", "")
		f.replay(t, "loginctl", "loginctl-show-user-linger.txt", "", "")
		f.replay(t, "orca", "orca-status-ready.json", "", "")
		f.config(t, "driver.env", "AGENT_FEISHU_APP_ID=TEST-PLACEHOLDER-CRED\nAGENT_FEISHU_APP_SECRET=TEST-PLACEHOLDER-CRED\nANTHROPIC_BASE_URL=http://127.0.0.1:9\n")
		f.config(t, "driver.toml", "factory_root = \"/srv/orca-factory\"\n")
		report, out, code := runBootstrapProbe(t, f.probe(nil))
		if code != 0 {
			t.Fatalf("exit = %d, want 0\n%s", code, out)
		}
		for _, name := range bootstrapChecks {
			if report.Checks[name].Status != healthOK {
				t.Errorf("%s = %+v, want ok", name, report.Checks[name])
			}
		}
	})
}

// TestDoctorBootstrapGoFixReachesServicePath: the go fix must put go on the PATH
// the driver unit sets, or a machine set up by the checklist fails service_path.
func TestDoctorBootstrapGoFixReachesServicePath(t *testing.T) {
	unit, err := deploy.Assets.ReadFile("systemd/orca-factory-driver.service")
	if err != nil {
		t.Fatal(err)
	}
	var servicePath []string
	for line := range strings.Lines(string(unit)) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "Environment=PATH="); ok {
			servicePath = strings.Split(value, ":")
		}
	}
	fix := newBootstrapFixture(t).probe(nil).checkGo(context.Background()).Fix
	for _, dir := range servicePath {
		if !strings.HasPrefix(dir, "%h") && strings.Contains(fix, " "+dir+"/go") {
			return
		}
	}
	t.Errorf("go fix %q puts go on none of the service PATH %v", fix, servicePath)
}

// TestDoctorBootstrapGoLeavesNoTelemetry: go counts each invocation under the
// user config dir, so probing go on a machine bootstrap rejects must not reach it.
func TestDoctorBootstrapGoLeavesNoTelemetry(t *testing.T) {
	f := newBootstrapFixture(t)
	t.Setenv("HOME", f.home)
	t.Setenv("XDG_CONFIG_HOME", "")
	f.tool(t, "go", `mkdir -p "${XDG_CONFIG_HOME:-$HOME/.config}/go/telemetry" && cat `+recordedPath(t, "go-version.txt"))
	if check := f.probe(nil).checkGo(context.Background()); check.Status != healthOK {
		t.Fatalf("go = %+v, want ok", check)
	}
	if _, err := os.Stat(filepath.Join(f.home, ".config", "go")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("go version wrote under the user config dir: %v", err)
	}
}
