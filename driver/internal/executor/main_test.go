package executor

import (
	"os"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// Every transition now writes a disposable status cache. Keep the entire
	// package away from the running user's XDG runtime directory.
	root, err := os.MkdirTemp("", "driver-status-tests-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("XDG_RUNTIME_DIR", root); err != nil {
		panic(err)
	}
	goleak.VerifyTestMain(m, goleak.Cleanup(func(code int) {
		_ = os.RemoveAll(root)
		os.Exit(code)
	}))
}
