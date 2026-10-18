package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

type installFakeRunner struct {
	specs []runx.Spec
	err   error
}

func (f *installFakeRunner) Run(ctx context.Context, spec runx.Spec) ([]byte, error) {
	f.specs = append(f.specs, spec)
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("missing deadline")
	}
	return nil, f.err
}

func TestInstallDryRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer
	runner := &installFakeRunner{}
	if err := installWith([]string{"--dry-run"}, &out, runner); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{filepath.Join(home, ".local/bin/orca-factory-driver"), filepath.Join(home, ".config/systemd/user/orca-factory-driver.service"), filepath.Join(home, ".config/systemd/user/orca-factory-driver-watch.timer"), "systemctl --user daemon-reload"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q: %s", want, out.String())
		}
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("dry-run changed HOME: %v %v", entries, err)
	}
	if len(runner.specs) != 0 {
		t.Fatal("dry-run executed a command")
	}
	if commands["install"] == nil {
		t.Fatal("install not registered")
	}
}

func TestInstallWritesUnitAndBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	runner := &installFakeRunner{}
	if err := installWith(nil, &bytes.Buffer{}, runner); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(filepath.Join(home, ".config/systemd/user/orca-factory-driver.service"))
	if err != nil {
		t.Fatal(err)
	}
	template, err := os.ReadFile("../../deploy/systemd/orca-factory-driver.service")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(unit, template) {
		t.Fatal("installed unit differs from template")
	}
	for _, want := range []string{"After=orca-serve.service network-online.target", "Restart=on-failure", "ExecStart=%h/.local/bin/orca-factory-driver serve", "Environment=AGENT_ORCA_BIN=%h/.local/bin/orca", "PATH=%h/.local/bin:"} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("unit missing %q", want)
		}
	}
	units := filepath.Join(home, ".config/systemd/user")
	timer, err := os.ReadFile(filepath.Join(units, "orca-factory-driver-watch.timer"))
	if err != nil || !strings.Contains(string(timer), "OnCalendar=*:0/30") || !strings.Contains(string(timer), "RandomizedDelaySec=") {
		t.Fatalf("watch timer: %v %s", err, timer)
	}
	if link, err := os.Readlink(filepath.Join(units, "orca-factory-driver-watch.service.d")); err != nil || link != filepath.Join(units, "orca-factory-driver.service.d") {
		t.Fatalf("watch drop-in link: %q %v", link, err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, ".local/bin/orca-factory-driver")
	installed, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(source, installed) {
		t.Fatalf("binary mismatch: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("binary mode: %v %v", info, err)
	}
	if len(runner.specs) != 2 {
		t.Fatalf("commands: %v", runner.specs)
	}
	spec := runner.specs[0]
	if spec.Path != "systemctl" || !reflect.DeepEqual(spec.Args, []string{"--user", "daemon-reload"}) || spec.Timeout <= 0 {
		t.Fatalf("unexpected command: %+v", spec)
	}
	// Installing again must replace an existing binary safely.
	if err := installWith(nil, &bytes.Buffer{}, &installFakeRunner{}); err != nil {
		t.Fatal(err)
	}
}

func TestInstallReloadFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	failure := errors.New("reload failed")
	if err := installWith(nil, &bytes.Buffer{}, &installFakeRunner{err: failure}); !errors.Is(err, failure) {
		t.Fatalf("got %v", err)
	}
}

func TestInstallRejectsArguments(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, args := range [][]string{{"unexpected"}, {"--unknown"}} {
		if err := installWith(args, &bytes.Buffer{}, &installFakeRunner{}); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Fatal("invalid arguments changed HOME")
	}
}

func TestInstallRejectsNonEnvironmentSharedDropIn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dropIns := filepath.Join(home, ".config/systemd/user/orca-factory-driver.service.d")
	if err := os.MkdirAll(dropIns, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropIns, "env.conf"), []byte("[Service]\nEnvironmentFile=%h/driver.env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropIns, "restart.conf"), []byte("# serve only\n[Service]\nEnvironment=A=1\nRestart=always\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &installFakeRunner{}
	err := installWith(nil, &bytes.Buffer{}, runner)
	if err == nil || !strings.Contains(err.Error(), "restart.conf") || !strings.Contains(err.Error(), "Restart") {
		t.Fatalf("install accepted non-environment drop-in: %v", err)
	}
	for _, path := range []string{".local/bin/orca-factory-driver", ".config/systemd/user/orca-factory-driver.service", ".config/systemd/user/orca-factory-driver-watch.service.d"} {
		if _, statErr := os.Lstat(filepath.Join(home, path)); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("rejected install wrote %s: %v", path, statErr)
		}
	}
	if len(runner.specs) != 0 {
		t.Fatal("rejected install ran a command")
	}
}

func TestInstallUnitRestartPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := installWith(nil, &bytes.Buffer{}, &installFakeRunner{}); err != nil {
		t.Fatal(err)
	}
	units := filepath.Join(home, ".config/systemd/user")
	for _, name := range []string{"orca-factory-driver.service", "orca-factory-feishu-gateway.service"} {
		raw, err := os.ReadFile(filepath.Join(units, name))
		if err != nil {
			t.Fatal(err)
		}
		sections := map[string][]string{}
		section := ""
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "[") {
				section = line
				continue
			}
			sections[section] = append(sections[section], line)
		}
		if !slices.Contains(sections["[Unit]"], "StartLimitIntervalSec=0") {
			t.Errorf("%s [Unit] lacks StartLimitIntervalSec=0", name)
		}
		if !slices.Contains(sections["[Service]"], "RestartSec=10s") || !slices.Contains(sections["[Service]"], "Restart=on-failure") {
			t.Errorf("%s [Service] lacks Restart=on-failure with RestartSec=10s", name)
		}
	}
}

func TestInstallWritesAllOpsAssets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := installWith(nil, &bytes.Buffer{}, &installFakeRunner{}); err != nil {
		t.Fatal(err)
	}
	units := filepath.Join(home, ".config/systemd/user")
	ops := filepath.Join(home, ".local/share/orca-factory-ops")
	want := map[string]string{
		"systemd/orca-factory-driver.service":                            filepath.Join(units, "orca-factory-driver.service"),
		"systemd/orca-factory-feishu-gateway.service":                    filepath.Join(units, "orca-factory-feishu-gateway.service"),
		"systemd/orca-factory-driver-watch.service":                      filepath.Join(units, "orca-factory-driver-watch.service"),
		"systemd/orca-factory-driver-watch.timer":                        filepath.Join(units, "orca-factory-driver-watch.timer"),
		"systemd/orca-factory-driver.service.d/env.conf":                 filepath.Join(units, "orca-factory-driver.service.d/env.conf"),
		"systemd/orca-factory-driver.service.d/git-network.conf.example": filepath.Join(units, "orca-factory-driver.service.d/git-network.conf.example"),
		"systemd/orca-factory-driver.service.d/path.conf.example":        filepath.Join(units, "orca-factory-driver.service.d/path.conf.example"),
		"systemd/orca-ci-proxy.socket.d/freebind.conf":                   filepath.Join(units, "orca-ci-proxy.socket.d/freebind.conf"),
		"ops/deploy.sh": filepath.Join(ops, "deploy.sh"),
		"ops/merge.sh":  filepath.Join(ops, "merge.sh"),
	}
	for source, target := range want {
		template, err := os.ReadFile(filepath.Join("../../deploy", source))
		if err != nil {
			t.Fatal(err)
		}
		installed, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(installed, template) {
			t.Errorf("%s not installed from %s: %v", target, source, err)
		}
	}
	for _, script := range []string{"deploy.sh", "merge.sh"} {
		if info, err := os.Stat(filepath.Join(ops, script)); err != nil || info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s not executable: %v", script, err)
		}
	}

	past := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	snapshot := func() map[string]time.Time {
		files := map[string]time.Time{}
		if err := filepath.Walk(home, func(path string, info os.FileInfo, err error) error {
			if err == nil {
				files[path] = info.ModTime()
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return files
	}
	for path := range snapshot() {
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshot()
	var out bytes.Buffer
	runner := &installFakeRunner{}
	if err := installWith(nil, &out, runner); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatalf("second install wrote files:\nbefore %v\nafter  %v\n%s", before, after, out.String())
	}
	if !slices.ContainsFunc(runner.specs, func(spec runx.Spec) bool {
		return reflect.DeepEqual(spec.Args, []string{"--user", "enable", "orca-factory-driver-watch.timer"})
	}) {
		t.Fatalf("watch timer not enabled: %+v", runner.specs)
	}
}

func TestInstallBacksUpChangedAsset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	script := filepath.Join(home, ".local/share/orca-factory-ops/deploy.sh")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("local edit\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var dry bytes.Buffer
	if err := installWith([]string{"--dry-run"}, &dry, &installFakeRunner{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dry.String(), "replace "+script) || !strings.Contains(dry.String(), "create "+filepath.Join(home, ".config/systemd/user/orca-factory-driver-watch.timer")) {
		t.Fatalf("dry-run actions: %s", dry.String())
	}
	if err := installWith(nil, &bytes.Buffer{}, &installFakeRunner{}); err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(script + ".bak-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups: %v %v", backups, err)
	}
	if old, err := os.ReadFile(backups[0]); err != nil || string(old) != "local edit\n" {
		t.Fatalf("backup content: %q %v", old, err)
	}
	if current, err := os.ReadFile(script); err != nil || string(current) == "local edit\n" {
		t.Fatalf("script not replaced: %v", err)
	}
}
