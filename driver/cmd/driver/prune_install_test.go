package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPruneInstallation(t *testing.T) {
	factory, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(factory, "driver/testdata/prune-worktrees/original-prune-run-worktrees.sh"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	driver := filepath.Join(dir, "driver")
	if err := os.WriteFile(driver, []byte(fmt.Sprintf("#!/bin/sh\nDRIVER_TEST_PROCESS=1 exec %q -test.run=^TestCLIProcess$ -- \"$@\"\n", os.Args[0])), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, "repo")
	dest := filepath.Join(repo, "scripts/agent-gates")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(dest, "prune-run-worktrees.sh")
	write := func(body []byte) {
		t.Helper()
		if err := os.WriteFile(entry, body, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(original)
	invoke := func(binary, path string) error {
		t.Helper()
		c := exec.Command("bash", filepath.Join(factory, "bin/orca-factory"), "update", repo)
		c.Env = append(os.Environ(), "AGENT_DRIVER_BIN="+binary, "PATH="+path)
		b, e := c.CombinedOutput()
		if e == nil {
			return nil
		}
		return fmt.Errorf("%w: %s", e, b)
	}
	retained := func(want []byte) {
		t.Helper()
		b, e := os.ReadFile(entry)
		if e != nil || !bytes.Equal(b, want) {
			t.Fatal("prune entry changed on failure")
		}
		if _, e := os.Stat(filepath.Join(dest, "agent-graph.sh")); !os.IsNotExist(e) {
			t.Fatal("other core copied before prune preflight")
		}
	}
	if e := invoke("/usr/bin/true", os.Getenv("PATH")); e == nil {
		t.Fatal("incompatible driver accepted")
	}
	retained(original)
	custom := append(append([]byte(nil), original...), []byte("\nprintf custom\n")...)
	write(custom)
	if e := invoke(driver, os.Getenv("PATH")); e == nil {
		t.Fatal("custom prune accepted")
	}
	retained(custom)
	write(original)
	if e := os.WriteFile(filepath.Join(dir, "mv"), []byte("#!/bin/sh\ncase \"$*\" in *prune-run-worktrees.sh*) exit 23 ;; esac\nexec /bin/mv \"$@\"\n"), 0o755); e != nil {
		t.Fatal(e)
	}
	if e := invoke(driver, dir+":"+os.Getenv("PATH")); e == nil {
		t.Fatal("rename failure accepted")
	}
	retained(original)
	if e := invoke(driver, os.Getenv("PATH")); e != nil {
		t.Fatal(e)
	}
	managed, e := os.ReadFile(filepath.Join(factory, "core/prune-run-worktrees.sh"))
	if e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(entry)
	if e != nil || !bytes.Equal(b, managed) {
		t.Fatal("update did not install managed prune")
	}
	info, e := os.Stat(entry)
	if e != nil || info.Mode().Perm() != 0o755 {
		t.Fatal("prune entry not executable")
	}
	fresh := filepath.Join(dir, "fresh")
	if e := os.Mkdir(fresh, 0o755); e != nil {
		t.Fatal(e)
	}
	c := exec.Command("bash", filepath.Join(factory, "bin/orca-factory"), "init", fresh)
	c.Env = append(os.Environ(), "AGENT_DRIVER_BIN="+driver)
	if b, e := c.CombinedOutput(); e != nil {
		t.Fatalf("init: %s %v", b, e)
	}
	if e := invoke(driver, os.Getenv("PATH")); e != nil {
		t.Fatal(e)
	}
	symlinkEntry := filepath.Join(fresh, "scripts/agent-gates/prune-run-worktrees.sh")
	if e := os.Remove(symlinkEntry); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(entry, symlinkEntry); e != nil {
		t.Fatal(e)
	}
	c = exec.Command("bash", filepath.Join(factory, "bin/orca-factory"), "update", fresh)
	c.Env = append(os.Environ(), "AGENT_DRIVER_BIN="+driver)
	if b, e := c.CombinedOutput(); e == nil {
		t.Fatalf("symlink accepted: %s", b)
	}
	b, e = os.ReadFile(entry)
	if e != nil || !bytes.Equal(b, managed) {
		t.Fatal("symlink target changed")
	}
}
