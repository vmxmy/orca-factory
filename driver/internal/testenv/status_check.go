// Package testenv provides process-wide isolation for driver test binaries.
package testenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

// CheckStatusIsolation runs the package's test binary with disposable stand-ins
// for a caller's existing caches. TestMain must protect both path contracts,
// even when DRIVER_STATUS_FILE is inherited or the legacy fallback is selected.
func CheckStatusIsolation(t *testing.T, write func()) {
	t.Helper()
	const child = "DRIVER_TEST_STATUS_ISOLATION_CHILD"
	if os.Getenv(child) == "1" {
		write()
		return
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	testName := t.Name()
	for _, explicit := range []bool{true, false} {
		name := "legacy fallback"
		if explicit {
			name = "explicit legacy path"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			runtime := filepath.Join(root, "runtime")
			tmp := filepath.Join(root, "tmp")
			home := filepath.Join(root, "home")
			for _, dir := range []string{filepath.Join(runtime, "orca-factory"), tmp, home} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			pushed := filepath.Join(runtime, "orca-factory", "status.json")
			legacy := filepath.Join(tmp, "orca-factory-driver-status.json")
			selected := ""
			if explicit {
				legacy = filepath.Join(root, "selected-status.json")
				selected = legacy
			}
			const sentinel = "existing caller status\n"
			for _, path := range []string{pushed, legacy} {
				if err := os.WriteFile(path, []byte(sentinel), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(binary, "-test.run=^"+regexp.QuoteMeta(testName)+"$", "-test.count=1")
			cmd.Env = append(os.Environ(), child+"=1", "XDG_RUNTIME_DIR="+runtime, "TMPDIR="+tmp, "DRIVER_STATUS_FILE="+selected, "HOME="+home)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("test subprocess: %v\n%s", err, output)
			}
			for _, path := range []string{pushed, legacy} {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != sentinel {
					t.Errorf("test binary overwrote caller cache %s: %s", path, data)
				}
			}
			if _, err := os.Stat(pushed + ".lock"); !os.IsNotExist(err) {
				t.Errorf("test binary touched caller status lock: %v", err)
			}
		})
	}
}
