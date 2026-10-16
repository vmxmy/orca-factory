package main

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLedgerInstallation(t *testing.T) {
	factory, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(factory, "driver/testdata/ledger-shell/original-ledger.sh"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	driver := filepath.Join(dir, "driver")
	shim := fmt.Sprintf("#!/bin/sh\nDRIVER_TEST_PROCESS=1 exec %q -test.run=^TestCLIProcess$ -- \"$@\"\n", os.Args[0])
	if err = os.WriteFile(driver, []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, "repo")
	dest := filepath.Join(repo, "scripts/agent-gates")
	if err = os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(dest, "ledger.sh")
	if err = os.WriteFile(entry, original, 0o755); err != nil {
		t.Fatal(err)
	}
	invoke := func(operation, binary, path string) (int, string) {
		t.Helper()
		cmd := exec.Command("bash", filepath.Join(factory, "bin/orca-factory"), operation, repo)
		cmd.Env = append(os.Environ(), "AGENT_DRIVER_BIN="+binary, "PATH="+path)
		output, err := cmd.CombinedOutput()
		if err == nil {
			return 0, string(output)
		}
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		return exit.ExitCode(), string(output)
	}
	unchanged := func() {
		t.Helper()
		body, err := os.ReadFile(entry)
		if err != nil || !bytes.Equal(body, original) {
			t.Fatalf("ledger changed on failure: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dest, "agent-graph.sh")); !os.IsNotExist(err) {
			t.Fatalf("core changed before preflight: %v", err)
		}
	}
	for _, binary := range []string{filepath.Join(dir, "missing"), "/usr/bin/true"} {
		if code, output := invoke("update", binary, os.Getenv("PATH")); code == 0 {
			t.Fatalf("preflight accepted %s: %s", binary, output)
		}
		unchanged()
	}
	custom := append([]byte(nil), original...)
	custom = append(custom, []byte("\nprintf custom\n")...)
	if err = os.WriteFile(entry, custom, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, output := invoke("init", driver, os.Getenv("PATH")); code == 0 {
		t.Fatalf("custom entry accepted: %s", output)
	}
	body, err := os.ReadFile(entry)
	if err != nil || !bytes.Equal(body, custom) {
		t.Fatal("custom entry overwritten")
	}
	if err = os.WriteFile(entry, original, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "mv"), []byte("#!/bin/sh\nexit 23\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, output := invoke("update", driver, dir+":"+os.Getenv("PATH")); code == 0 {
		t.Fatalf("replace failure accepted: %s", output)
	}
	unchanged()
	if code, output := invoke("init", driver, os.Getenv("PATH")); code != 0 {
		t.Fatalf("init failed: %d %s", code, output)
	}
	info, err := os.Stat(entry)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("installed entry permissions: %v %v", info, err)
	}
	managed, err := os.ReadFile(filepath.Join(factory, "core/ledger.sh"))
	if err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(entry)
	if err != nil || !bytes.Equal(body, managed) {
		t.Fatal("init did not install managed entry")
	}
	if code, output := invoke("update", driver, os.Getenv("PATH")); code != 0 {
		t.Fatalf("current version update failed: %d %s", code, output)
	}
	server := httptest.NewServer(&ledgerGitHubFake{})
	defer server.Close()
	command := exec.Command("bash", entry, "stats")
	command.Env = append(append(os.Environ(), isolatedGitHubEnv(t, server.URL)...), "AGENT_DRIVER_BIN="+driver)
	if output, err := command.CombinedOutput(); err != nil || string(output) != "runs=0\n" {
		t.Fatalf("managed entry failed: %q %v", output, err)
	}
	target := filepath.Join(dir, "symlink-target")
	if err = os.WriteFile(target, original, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkRepo := filepath.Join(dir, "symlink-repo")
	symlinkDest := filepath.Join(symlinkRepo, "scripts/agent-gates")
	if err = os.MkdirAll(symlinkDest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(target, filepath.Join(symlinkDest, "ledger.sh")); err != nil {
		t.Fatal(err)
	}
	command = exec.Command("bash", filepath.Join(factory, "bin/orca-factory"), "update", symlinkRepo)
	command.Env = append(os.Environ(), "AGENT_DRIVER_BIN="+driver)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("symlink accepted: %s", output)
	}
	body, err = os.ReadFile(target)
	if err != nil || !bytes.Equal(body, original) {
		t.Fatal("symlink target changed")
	}
}
