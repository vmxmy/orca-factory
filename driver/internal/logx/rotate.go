package logx

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

type rotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	file     *os.File
	size     int64
}

// RotatingFile appends to path and rotates before a write exceeds maxBytes.
// A single oversized record stays intact. Nonpositive limits default to 10 MiB
// and five archives. The returned writer is safe for concurrent use.
func RotatingFile(path string, maxBytes int64, keep int) (io.WriteCloser, error) {
	if maxBytes <= 0 {
		maxBytes = 10 << 20
	}
	if keep <= 0 {
		keep = 5
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	w := &rotatingFile{path: path, maxBytes: maxBytes, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}
func (w *rotatingFile) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.file, w.size = f, info.Size()
	return nil
}
func (w *rotatingFile) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if len(p) > 0 && w.size > 0 && int64(len(p)) > w.maxBytes-w.size {
		if err := w.file.Close(); err != nil {
			return 0, err
		}
		w.file = nil
		for i := w.keep; i >= 1; i-- {
			source := w.path
			if i > 1 {
				source += "." + strconv.Itoa(i-1)
			}
			if err := os.Rename(source, w.path+"."+strconv.Itoa(i)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return 0, err
			}
		}
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}
func (w *rotatingFile) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return os.ErrClosed
	}
	err := w.file.Close()
	w.file = nil
	return err
}
