package testenv

import (
	"os"
	"path/filepath"
)

// IsolateStatus redirects every status output for the lifetime of a test
// binary. Set TMPDIR as well because tests can clear DRIVER_STATUS_FILE to
// exercise the published legacy fallback. HOME stays available to Go tools.
// Call the returned cleanup after tests and any package-wide leak checks.
func IsolateStatus() func() {
	root, err := os.MkdirTemp("", "driver-status-tests-")
	if err != nil {
		panic(err)
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	for key, value := range map[string]string{
		"XDG_RUNTIME_DIR":    root,
		"TMPDIR":             root,
		"DRIVER_STATUS_FILE": filepath.Join(root, "legacy-status.json"),
	} {
		if err := os.Setenv(key, value); err != nil {
			cleanup()
			panic(err)
		}
	}
	return cleanup
}
