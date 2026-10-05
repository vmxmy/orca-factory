package flock

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
)

func TestExclusiveAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "worker.lock")
	first, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := first.Close(); err != nil {
			t.Error(err)
		}
	}()
	second, err := TryLock(path)
	if second != nil || !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("second lock: %v %v", second, err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := third.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err = first.Close(); err != nil {
		t.Fatal("close not idempotent", err)
	}
}
