// Package snapshotcache stores validated GitHub snapshot blobs, independently
// versioned by scope and key. A completed payload and its freshness are one row;
// network pagination belongs to the caller and never runs inside a transaction.
package snapshotcache

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// Store opens and closes a connection for each operation, including error paths.
// Scope is "hashed-host/repo:hashed-credential". Payloads and refresh leases use
// the full scope, while mutations fence every credential in that repository.
// Path must be a private local file; existing unsafe paths are rejected, not reset.
type Store struct {
	Path string
	Now  func() time.Time
}

func (s Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s Store) open(ctx context.Context) (*sql.DB, error) {
	path, err := filepath.Abs(s.Path)
	if err != nil || s.Path == "" {
		return nil, errors.New("invalid snapshot cache path")
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("snapshot cache directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("snapshot cache directory must be private (0700)")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err == nil {
		if err = f.Close(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err = os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) && p != path {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("snapshot cache files must be regular and private (0600)")
		}
	}
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite", u.String()+"?_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	err = retry(ctx, func() error {
		var version, tables int
		if e := db.QueryRowContext(ctx, "SELECT user_version,(SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%') FROM pragma_user_version").Scan(&version, &tables); e != nil {
			return e
		}
		if (version != 1 && (version != 0 || tables != 0)) || (version == 1 && tables != 3) {
			return errors.New("unsupported snapshot cache schema; preserve database for inspection")
		}
		var mode string
		if e := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); e != nil {
			return e
		}
		if mode != "wal" {
			return errors.New("snapshot cache requires WAL")
		}
		if version == 1 {
			return nil
		}
		return transaction(ctx, db, func(tx *sql.Tx) error {
			if e := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); e != nil {
				return e
			}
			if version == 1 {
				return nil
			} // another process initialized while we waited
			if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); e != nil {
				return e
			}
			if version != 0 || tables != 0 {
				return errors.New("unsupported snapshot cache schema")
			}
			_, e := tx.ExecContext(ctx, `CREATE TABLE scopes (scope TEXT PRIMARY KEY, generation INTEGER NOT NULL);
CREATE TABLE snapshots (
 namespace TEXT NOT NULL, scope TEXT NOT NULL, key TEXT NOT NULL, version TEXT NOT NULL DEFAULT '',
 payload BLOB, fetched INTEGER NOT NULL DEFAULT 0, complete INTEGER NOT NULL DEFAULT 0 CHECK(complete IN(0,1)),
 generation INTEGER NOT NULL DEFAULT 0, lease TEXT NOT NULL DEFAULT '', expires INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(scope,key));
CREATE TABLE mutations(scope TEXT NOT NULL, owner TEXT NOT NULL, expires INTEGER NOT NULL, PRIMARY KEY(scope,owner));
PRAGMA user_version=1;`)
			return e
		})
	})
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}

	return db, nil
}

// ErrSuperseded means a refresh lost its lease or was fenced by a mutation.
// Callers must preserve unknown state, not use the fetched projection.
var ErrSuperseded = errors.New("snapshot refresh superseded")

const refreshTimeout = 2 * time.Minute
const refreshLease = refreshTimeout + 5*time.Second

func wait(ctx context.Context) error {
	timer := time.NewTimer(15 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// SQLite's busy handler is disabled. Retry short transactions in Go so both lock
// contention and canceled waiters stay bounded and observable.
func retry(ctx context.Context, op func() error) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := op()
		var se *sqlite.Error
		if !errors.As(err, &se) || (se.Code()&255 != 5 && se.Code()&255 != 6) {
			return err
		}
		if err = wait(ctx); err != nil {
			return err
		}
	}
}
func transaction(ctx context.Context, db *sql.DB, op func(*sql.Tx) error) error {
	return retry(ctx, func() (err error) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() {
			e := tx.Rollback()
			if !errors.Is(e, sql.ErrTxDone) {
				err = errors.Join(err, e)
			}
		}()
		if err = op(tx); err != nil {
			return err
		}
		return tx.Commit()
	})
}
func ownerID() string { return rand.Text() }

// Get returns only a complete, unexpired projection. A bounded cross-process
// lease coalesces refreshes, and publication atomically stores payload/freshness/
// completeness at the captured generation. Network I/O has no DB transaction.
func (s Store) Get(ctx context.Context, scope, key, version string, ttl time.Duration, fetch func(context.Context) ([]byte, error)) (raw []byte, err error) {
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	db, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	owner := ownerID()
	namespace, _, _ := strings.Cut(scope, ":")
	for {
		var hit, claimed bool
		var generation int64
		now := s.now()
		err = transaction(ctx, db, func(tx *sql.Tx) error {
			hit, claimed = false, false
			// Sample after acquiring the DB lock: a waiter may have started before
			// another process committed a snapshot with a later fetched timestamp.
			now = s.now()
			if _, e := tx.ExecContext(ctx, "INSERT INTO scopes(scope,generation) VALUES(?,0) ON CONFLICT DO NOTHING", namespace); e != nil {
				return e
			}
			if e := expireMutations(ctx, tx, namespace, now.UnixNano()); e != nil {
				return e
			}
			if e := tx.QueryRowContext(ctx, "SELECT generation FROM scopes WHERE scope=?", namespace).Scan(&generation); e != nil {
				return e
			}
			var active int
			if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM mutations WHERE scope=? AND expires>?", namespace, now.UnixNano()).Scan(&active); e != nil {
				return e
			}
			if active > 0 {
				return nil
			}
			if _, e := tx.ExecContext(ctx, "INSERT INTO snapshots(namespace,scope,key) VALUES(?,?,?) ON CONFLICT DO NOTHING", namespace, scope, key); e != nil {
				return e
			}
			var storedVersion, lease string
			var fetched, epoch, expires int64
			var complete int
			if e := tx.QueryRowContext(ctx, "SELECT payload,version,fetched,complete,generation,lease,expires FROM snapshots WHERE scope=? AND key=?", scope, key).Scan(&raw, &storedVersion, &fetched, &complete, &epoch, &lease, &expires); e != nil {
				return e
			}
			if complete == 1 && epoch == generation && storedVersion == version && now.UnixNano() >= fetched && now.Sub(time.Unix(0, fetched)) < ttl {
				hit = true
				return nil
			}
			if lease != "" && expires > now.UnixNano() {
				return nil
			}
			_, e := tx.ExecContext(ctx, "UPDATE snapshots SET lease=?,expires=? WHERE scope=? AND key=?", owner, now.Add(refreshLease).UnixNano(), scope, key)
			claimed = e == nil
			return e
		})
		if err != nil {
			return nil, err
		}
		if hit {
			return raw, nil
		}
		if !claimed {
			if err = wait(ctx); err != nil {
				return nil, err
			}
			continue
		}
		// Release the lease on all failure paths, including a failed publication
		// transaction, so a recoverable disk/constraint failure does not stall
		// subsequent readers until expiration. Ownership comparison fences cleanup.
		defer func() {
			if err == nil {
				return
			}
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			err = errors.Join(err, transaction(cleanup, db, func(tx *sql.Tx) error {
				_, e := tx.ExecContext(cleanup, "UPDATE snapshots SET lease='',expires=0 WHERE scope=? AND key=? AND lease=?", scope, key, owner)
				return e
			}))
		}()
		raw, err = fetch(ctx)
		if err != nil {
			return nil, err
		}
		err = transaction(ctx, db, func(tx *sql.Tx) error {
			result, e := tx.ExecContext(ctx, `UPDATE snapshots SET payload=?,version=?,fetched=?,complete=1,generation=?,lease='',expires=0
WHERE scope=? AND key=? AND lease=? AND expires>? AND (SELECT generation FROM scopes WHERE scope=?)=? AND NOT EXISTS(SELECT 1 FROM mutations WHERE scope=? AND expires>?)`, raw, version, now.UnixNano(), generation, scope, key, owner, s.now().UnixNano(), namespace, generation, namespace, s.now().UnixNano())
			if e != nil {
				return e
			}
			n, e := result.RowsAffected()
			if e == nil && n != 1 {
				return ErrSuperseded
			}
			return e
		})
		if err != nil {
			return nil, err
		}
		return raw, nil
	}
}

// MutationLease outlives the driver's bounded remote mutation context by one
// minute. A crashed writer cannot leave a fresh entry: both start and finish
// advance the generation, and expiration also fences all outstanding refreshes.
const MutationLease = 31 * time.Minute

func fence(ctx context.Context, tx *sql.Tx, scope string) error {
	if _, err := tx.ExecContext(ctx, "UPDATE scopes SET generation=generation+1 WHERE scope=?", scope); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE snapshots SET lease='',expires=0 WHERE namespace=?", scope)
	return err
}
func expireMutations(ctx context.Context, tx *sql.Tx, scope string, now int64) error {
	result, err := tx.ExecContext(ctx, "DELETE FROM mutations WHERE scope=? AND expires<=?", scope, now)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return fence(ctx, tx, scope)
	}
	return nil
}

// BeginMutation fences old refreshes before remote I/O. Its completion callback
// must run even on ambiguous failure, with a separate bounded cleanup context.
func (s Store) BeginMutation(ctx context.Context, scope string) (finish func(context.Context) error, err error) {
	scope, _, _ = strings.Cut(scope, ":")
	db, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	owner := ownerID()
	err = transaction(ctx, db, func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(ctx, "INSERT INTO scopes(scope,generation) VALUES(?,0) ON CONFLICT DO NOTHING", scope); e != nil {
			return e
		}
		if e := fence(ctx, tx, scope); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, "INSERT INTO mutations(scope,owner,expires) VALUES(?,?,?)", scope, owner, s.now().Add(MutationLease).UnixNano())
		return e
	})
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (err error) {
		db, err := s.open(ctx)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, db.Close()) }()
		return transaction(ctx, db, func(tx *sql.Tx) error {
			if e := fence(ctx, tx, scope); e != nil {
				return e
			}
			_, e := tx.ExecContext(ctx, "DELETE FROM mutations WHERE scope=? AND owner=?", scope, owner)
			return e
		})
	}, nil
}
