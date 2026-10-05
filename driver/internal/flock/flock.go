// Adapted from junhoyeo/contrabass internal/tracker/flock.go (Apache-2.0):
// https://github.com/junhoyeo/contrabass/blob/main/internal/tracker/flock.go
// Changes: exported TryLock(path), exact caller-supplied path (no suffix),
// nonblocking contention returned as an error, private permissions, and Close
// for release. Blocking Lock and tracker-specific state have been removed.
// See ../../third_party/contrabass/LICENSE and ../../NOTICE.
package flock

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// Lock owns an advisory lock. Keep the file at its path even after release:
// unlinking it could let another process lock a different inode at that path.
type Lock struct {
	mu   sync.Mutex
	file *os.File
}

// TryLock acquires LOCK_EX|LOCK_NB. Contention returns syscall.EWOULDBLOCK.
func TryLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, errors.New("create lock directory failed")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("open lock file failed")
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return &Lock{file: file}, nil
}

// Close releases the lock and descriptor. Repeated calls are safe.
func (lock *Lock) Close() error {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.file == nil {
		return nil
	}
	err := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeErr := lock.file.Close()
	lock.file = nil
	return errors.Join(err, closeErr)
}
