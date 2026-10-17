package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
	if len(runner.specs) != 1 {
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
