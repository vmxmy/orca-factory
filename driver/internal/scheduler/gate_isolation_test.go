package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// Reproduce an outer project gate without touching its real shared lock. The
// child executes both real-gate fixtures under an inherited, occupied TMPDIR.
func TestRealGateFixturesIsolateOuterLock(t *testing.T) {
	outer := t.TempDir()
	lock := filepath.Join(outer, "agent-gate-heavy.flock")
	held, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err = syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(lock)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", outer)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, runErr := runx.Run(context.Background(), runx.Spec{
		Path:    executable,
		Args:    []string{"-test.run=^(TestRuntimeOverlapExecutesOriginalGateScript|TestRuntimeMergedGateAcceptsLowerActualLane)$", "-test.count=1"},
		Timeout: 30 * time.Second,
	})
	after, statErr := os.Stat(lock)
	if statErr != nil || !os.SameFile(before, after) {
		t.Fatalf("inner gate removed/replaced outer lock: %v", statErr)
	}
	if runErr != nil {
		t.Fatalf("real-gate fixtures blocked on inherited outer lock: %v\n%s", runErr, out)
	}
}
