package testsupport

import (
	"os"
	"path/filepath"
	"strings"
)

// IsolateStatus redirects every status output for the lifetime of a test
// binary. Set TMPDIR as well because tests can clear DRIVER_STATUS_FILE to
// exercise the published legacy fallback. HOME stays available to Go tools, so
// the driver state and agent session roots are redirected explicitly.
// Call the returned cleanup after tests and any package-wide leak checks.
func IsolateStatus() func() {
	root, err := os.MkdirTemp("", "driver-status-tests-")
	if err != nil {
		panic(err)
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	values := map[string]string{
		"XDG_RUNTIME_DIR":         root,
		"TMPDIR":                  root,
		"DRIVER_STATUS_FILE":      filepath.Join(root, "legacy-status.json"),
		"AGENT_DRIVER_STATE_ROOT": filepath.Join(root, "state"),
		"CLAUDE_CONFIG_DIR":       filepath.Join(root, "claude"),
		"CODEX_HOME":              filepath.Join(root, "codex"),
	}
	for key, value := range values {
		if err := os.Setenv(key, value); err != nil {
			cleanup()
			panic(err)
		}
	}
	// Production configuration must never steer a unit test: AGENT_ISSUE_BACKEND
	// makes ledger subcommands re-exec the test binary without bound, and the
	// AGENT_TIER_* agent choice changes executor correction paths.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "AGENT_") {
			continue
		}
		if err := os.Unsetenv(key); err != nil {
			cleanup()
			panic(err)
		}
	}
	return cleanup
}
