package snapshotcache_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/snapshotcache"
)

func cache(t *testing.T) snapshotcache.Store {
	t.Helper()
	return snapshotcache.Store{Path: filepath.Join(t.TempDir(), "private", "cache.sqlite")}
}
func value(s string) func(context.Context) ([]byte, error) {
	return func(context.Context) ([]byte, error) { return []byte(s), nil }
}
func read(t *testing.T, s snapshotcache.Store, want string) {
	t.Helper()
	got, err := s.Get(context.Background(), "scope", "tasks", "v1", time.Hour, value("unexpected refresh"))
	if err != nil || string(got) != want {
		t.Fatalf("want %q, got %q error=%v", want, got, err)
	}
}
func database(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

func TestWALReopenAndPrivateFiles(t *testing.T) {
	s := cache(t)
	read(t, s, "unexpected refresh")
	db := database(t, s.Path)
	var mode, version string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT sqlite_version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" || version != "3.51.3" {
		t.Fatalf("mode=%s version=%s", mode, version)
	}
	// A second real connection observes the committed projection, not process state.
	read(t, snapshotcache.Store{Path: s.Path}, "unexpected refresh")
	info, err := os.Stat(filepath.Dir(s.Path))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("directory privacy: %v %v", info, err)
	}
	for _, path := range []string{s.Path, s.Path + "-wal", s.Path + "-shm"} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("file privacy %s: %v %v", path, info, err)
		}
	}
	t.Logf("journal_mode=%s sqlite_version=%s; directory0700 DB/WAL/SHM0600", mode, version)
}

func TestFailedFetchAndTransactionNeverAdvanceFreshness(t *testing.T) {
	s := cache(t)
	var clock atomic.Int64
	epoch := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return epoch.Add(time.Duration(clock.Load())) }
	read(t, s, "unexpected refresh")
	clock.Store(int64(61 * time.Minute))
	got, err := s.Get(context.Background(), "scope", "tasks", "v1", time.Hour, func(context.Context) ([]byte, error) { return []byte("partial"), errors.New("page2 failed") })
	if err == nil || got != nil {
		t.Fatalf("partial admitted: %q %v", got, err)
	}
	clock.Store(int64(30 * time.Minute))
	read(t, s, "unexpected refresh")
	clock.Store(int64(61 * time.Minute))
	db := database(t, s.Path)
	_, err = db.Exec(`CREATE TRIGGER reject_publication BEFORE UPDATE ON snapshots WHEN CAST(NEW.payload AS TEXT)='broken' BEGIN SELECT RAISE(ABORT,'injected disk failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(context.Background(), "scope", "tasks", "v1", time.Hour, value("broken"))
	if err == nil || got != nil {
		t.Fatalf("failed transaction admitted: %q %v", got, err)
	}
	clock.Store(int64(30 * time.Minute))
	read(t, s, "unexpected refresh")
	if _, err = db.Exec("DROP TRIGGER reject_publication"); err != nil {
		t.Fatal(err)
	}
	clock.Store(int64(61 * time.Minute))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err = s.Get(ctx, "scope", "tasks", "v1", time.Hour, value("repaired"))
	if err != nil || string(got) != "repaired" {
		t.Fatalf("failed transaction poisoned refresh lease: %q %v", got, err)
	}
}

func TestRefreshExpiryAndMutationFenceOlderPublication(t *testing.T) {
	for _, mutation := range []bool{false, true} {
		t.Run(map[bool]string{false: "expiry", true: "mutation"}[mutation], func(t *testing.T) {
			s := cache(t)
			var clock atomic.Int64
			s.Now = func() time.Time { return time.Unix(1000, clock.Load()) }
			entered, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				_, err := s.Get(context.Background(), "scope", "tasks", "v1", time.Hour, func(context.Context) ([]byte, error) { close(entered); <-release; return []byte("old"), nil })
				done <- err
			}()
			<-entered
			if mutation {
				finish, err := s.BeginMutation(context.Background(), "scope")
				if err != nil {
					t.Fatal(err)
				}
				if err = finish(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else {
				clock.Store(int64(3 * time.Minute))
			}
			read(t, s, "unexpected refresh")
			close(release)
			if err := <-done; !errors.Is(err, snapshotcache.ErrSuperseded) {
				t.Fatalf("old publication allowed: %v", err)
			}
			read(t, s, "unexpected refresh")
		})
	}
}

func TestMutationCrashBlocksUntilExpiryThenRequiresRemote(t *testing.T) {
	s := cache(t)
	var clock atomic.Int64
	s.Now = func() time.Time { return time.Unix(1000, clock.Load()) }
	read(t, s, "unexpected refresh")
	finish, err := s.BeginMutation(context.Background(), "scope")
	if err != nil {
		t.Fatal(err)
	}
	// Omit finish to simulate death after the remote accepted a write.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var fetches atomic.Int32
	got, err := s.Get(ctx, "scope", "tasks", "v1", time.Hour, func(context.Context) ([]byte, error) { fetches.Add(1); return []byte("unsafe"), nil })
	if !errors.Is(err, context.DeadlineExceeded) || got != nil || fetches.Load() != 0 {
		t.Fatalf("active mutation exposed snapshot: %q %v fetches=%d", got, err, fetches.Load())
	}
	clock.Store(int64(snapshotcache.MutationLease + time.Second))
	got, err = s.Get(context.Background(), "scope", "tasks", "v1", time.Hour, value("remote after crash"))
	if err != nil || string(got) != "remote after crash" {
		t.Fatalf("expired mutation reused old cache: %q %v", got, err)
	}
	if err = finish(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLockWaitCancellationAndStorageErrors(t *testing.T) {
	s := cache(t)
	read(t, s, "unexpected refresh")
	db := database(t, s.Path)
	if _, err := db.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	got, err := s.Get(ctx, "scope", "tasks", "v1", 0, value("unsafe"))
	if !errors.Is(err, context.DeadlineExceeded) || got != nil {
		t.Fatalf("lock wait not canceled: %q %v", got, err)
	}
	if _, err = db.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"corruption", "permission", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			s := cache(t)
			if err := os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
				t.Fatal(err)
			}
			original := []byte("preserve existing data")
			mode := os.FileMode(0600)
			if kind == "permission" {
				mode = 0644
			}
			if kind == "symlink" {
				target := filepath.Join(filepath.Dir(s.Path), "target")
				if err := os.WriteFile(target, original, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, s.Path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(s.Path, original, mode); err != nil {
				t.Fatal(err)
			}
			var fetches int
			got, err := s.Get(context.Background(), "scope", "tasks", "v1", time.Hour, func(context.Context) ([]byte, error) { fetches++; return []byte("unsafe"), nil })
			if err == nil || got != nil || fetches != 0 {
				t.Fatalf("storage error ignored: %q %v fetches=%d", got, err, fetches)
			}
			preserved, err := os.ReadFile(s.Path)
			if err != nil || string(preserved) != string(original) {
				t.Fatalf("storage reset: %q %v", preserved, err)
			}
		})
	}
}

func TestUnsupportedSchemaIsRejectedWithoutChangingDatabase(t *testing.T) {
	for _, version := range []int{0, 99} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			s := cache(t)
			if err := os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.Close(); err != nil {
				t.Fatal(err)
			}
			db := database(t, s.Path)
			if _, err = db.Exec(fmt.Sprintf("CREATE TABLE preserve(value TEXT); INSERT INTO preserve VALUES('user data'); PRAGMA user_version=%d", version)); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(s.Path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.Get(context.Background(), "scope", "tasks", "v1", time.Hour, value("unsafe"))
			if err == nil || got != nil {
				t.Fatalf("unsupported database admitted: %q %v", got, err)
			}
			after, err := os.ReadFile(s.Path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("unsupported schema altered: %v", err)
			}
		})
	}
}
