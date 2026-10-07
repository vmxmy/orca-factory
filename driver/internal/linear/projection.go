package linear

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const ProjectionSource = "projection"

const projectionRetention = 30 * 24 * time.Hour

var ErrIncompleteProjection = errors.New("linear projection is incomplete")

type Projection struct {
	db    *sql.DB
	scope string
}

type ProjectionSnapshot struct {
	Issues    []Issue
	Comments  map[string][]Comment
	Complete  bool
	Watermark string
}

type ReadOptions struct {
	MaxAge          time.Duration
	RequireComplete bool
}

type ProjectionMeta struct {
	Fresh       bool
	Complete    bool
	Source      string
	Scope       string
	Generation  int64
	Watermark   string
	RefreshedAt time.Time
}

// MarkDirty records an object that must be refreshed from Linear. It is
// idempotent so repeated webhook deliveries cannot grow the work queue.
func (p *Projection) MarkDirty(ctx context.Context, objectType, objectID string) error {
	_, err := p.db.ExecContext(ctx, `INSERT OR IGNORE INTO projection_dirty(object_type,object_id) VALUES(?,?)`, objectType, objectID)
	return err
}

// AcknowledgeDirty removes only the keys supplied by a completed refresh.
// Callers must not acknowledge keys when the remote read was partial or failed.
func (p *Projection) AcknowledgeDirty(ctx context.Context, dirty map[string][]string) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for typ, ids := range dirty {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `DELETE FROM projection_dirty WHERE object_type=? AND object_id=?`, typ, id); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// ApplyIncremental merges a complete incremental result into the retained
// projection. Overlap records are naturally deduplicated by their IDs. An
// incomplete result is rejected before opening a write transaction, preserving
// both the prior snapshot and its watermark.
func (p *Projection) ApplyIncremental(ctx context.Context, snapshot ProjectionSnapshot) error {
	if !snapshot.Complete {
		return ErrIncompleteProjection
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, issue := range snapshot.Issues {
		payload, err := json.Marshal(issue)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO projection_issues(id,payload,seen_at) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload, seen_at=excluded.seen_at`, issue.ID, payload, now); err != nil {
			return err
		}
		comments, hasComments := snapshot.Comments[issue.ID]
		if hasComments {
			if _, err = tx.ExecContext(ctx, `DELETE FROM projection_comments WHERE issue_id=?`, issue.ID); err != nil {
				return err
			}
		}
		for _, comment := range comments {
			payload, err = json.Marshal(comment)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO projection_comments(id,issue_id,payload,seen_at) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET issue_id=excluded.issue_id, payload=excluded.payload, seen_at=excluded.seen_at`, comment.ID, issue.ID, payload, now); err != nil {
				return err
			}
		}
	}
	watermark := snapshot.Watermark
	var previous string
	if err = tx.QueryRowContext(ctx, `SELECT watermark FROM projection_meta WHERE id=1`).Scan(&previous); err != nil {
		return err
	}
	if watermark == "" || (previous != "" && watermark < previous) {
		watermark = previous
	}
	if _, err = tx.ExecContext(ctx, `UPDATE projection_meta SET complete=1, refreshed_at=?, watermark=? WHERE id=1`, now, watermark); err != nil {
		return err
	}
	return tx.Commit()
}

type ProjectionRead struct {
	Issues   []Issue
	Comments map[string][]Comment
	Meta     ProjectionMeta
}

func OpenProjection(path, scope string) (*Projection, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// SQLite projection transactions are serialized; a single pooled
	// connection prevents concurrent readers from waiting behind a writer.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	p := &Projection{db: db, scope: scope}
	if err := p.init(); err != nil {
		db.Close()
		return nil, err
	}
	return p, nil
}

func (p *Projection) init() error {
	ctx := context.Background()
	if _, err := p.db.ExecContext(ctx, `PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL;`); err != nil {
		return err
	}
	if _, err := p.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS projection_meta (id INTEGER PRIMARY KEY CHECK (id=1), scope TEXT NOT NULL, generation INTEGER NOT NULL, complete INTEGER NOT NULL, refreshed_at TEXT, watermark TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS projection_issues (id TEXT PRIMARY KEY, payload BLOB NOT NULL, seen_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS projection_comments (id TEXT PRIMARY KEY, issue_id TEXT NOT NULL, payload BLOB NOT NULL, seen_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS projection_dirty (object_type TEXT NOT NULL, object_id TEXT NOT NULL, PRIMARY KEY(object_type, object_id));
CREATE TABLE IF NOT EXISTS projection_delivery (dedupe_key TEXT PRIMARY KEY, delivered_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS projection_events (delivery_id TEXT PRIMARY KEY, event_type TEXT NOT NULL, action TEXT NOT NULL, payload BLOB NOT NULL, received_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS projection_intents (intent_key TEXT PRIMARY KEY, payload BLOB NOT NULL, created_at TEXT NOT NULL);`); err != nil {
		return err
	}
	var oldScope string
	err := p.db.QueryRowContext(ctx, `SELECT scope FROM projection_meta WHERE id=1`).Scan(&oldScope)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = p.db.ExecContext(ctx, `INSERT INTO projection_meta(id,scope,generation,complete,watermark) VALUES(1,?,1,0,'')`, p.scope)
		return err
	}
	if err != nil {
		return err
	}
	if oldScope != p.scope {
		_, err = p.db.ExecContext(ctx, `BEGIN IMMEDIATE; DELETE FROM projection_issues; DELETE FROM projection_comments; DELETE FROM projection_dirty; UPDATE projection_meta SET scope=?, generation=generation+1, complete=0, refreshed_at=NULL, watermark='' WHERE id=1; COMMIT`, p.scope)
	}
	return err
}

func (p *Projection) Close() error { return p.db.Close() }

// Meta returns refresh state without materializing the snapshot.
func (p *Projection) Meta(ctx context.Context) (ProjectionMeta, error) {
	var complete int
	var refreshed, watermark string
	var generation int64
	if err := p.db.QueryRowContext(ctx, `SELECT complete, refreshed_at, watermark, generation FROM projection_meta WHERE id=1`).Scan(&complete, &refreshed, &watermark, &generation); err != nil {
		return ProjectionMeta{}, err
	}
	var when time.Time
	var err error
	if refreshed != "" {
		when, err = time.Parse(time.RFC3339Nano, refreshed)
		if err != nil {
			return ProjectionMeta{}, fmt.Errorf("invalid projection freshness: %w", err)
		}
	}
	return ProjectionMeta{Fresh: refreshed != "", Complete: complete != 0, Source: ProjectionSource, Scope: p.scope, Generation: generation, Watermark: watermark, RefreshedAt: when}, nil
}

func (p *Projection) Publish(ctx context.Context, snapshot ProjectionSnapshot) error {
	if !snapshot.Complete {
		return ErrIncompleteProjection
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `DELETE FROM projection_issues; DELETE FROM projection_comments;`); err != nil {
		return err
	}
	for _, issue := range snapshot.Issues {
		payload, e := json.Marshal(issue)
		if e != nil {
			return e
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO projection_issues(id,payload,seen_at) VALUES(?,?,?)`, issue.ID, payload, now); err != nil {
			return err
		}
		for _, comment := range snapshot.Comments[issue.ID] {
			payload, e = json.Marshal(comment)
			if e != nil {
				return e
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO projection_comments(id,issue_id,payload,seen_at) VALUES(?,?,?,?)`, comment.ID, issue.ID, payload, now); err != nil {
				return err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE projection_meta SET complete=1, refreshed_at=?, watermark=? WHERE id=1`, now, snapshot.Watermark)
	if err != nil {
		return err
	}
	cutoff := time.Now().UTC().Add(-projectionRetention).Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `DELETE FROM projection_comments WHERE seen_at < ?; DELETE FROM projection_issues WHERE seen_at < ?`, cutoff, cutoff); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *Projection) Read(ctx context.Context, options ReadOptions) (ProjectionRead, error) {
	var complete int
	var refreshed, watermark string
	var generation int64
	err := p.db.QueryRowContext(ctx, `SELECT complete, refreshed_at, watermark, generation FROM projection_meta WHERE id=1`).Scan(&complete, &refreshed, &watermark, &generation)
	if err != nil {
		return ProjectionRead{}, err
	}
	if options.RequireComplete && complete == 0 {
		return ProjectionRead{}, ErrIncompleteProjection
	}
	when, err := time.Parse(time.RFC3339Nano, refreshed)
	if err != nil && refreshed != "" {
		return ProjectionRead{}, fmt.Errorf("invalid projection freshness: %w", err)
	}
	fresh := refreshed != "" && (options.MaxAge <= 0 || time.Since(when) <= options.MaxAge)
	result := ProjectionRead{Comments: make(map[string][]Comment), Meta: ProjectionMeta{Fresh: fresh, Complete: complete != 0, Source: ProjectionSource, Scope: p.scope, Generation: generation, Watermark: watermark}}
	if refreshed != "" {
		result.Meta.RefreshedAt = when
	}
	rows, err := p.db.QueryContext(ctx, `SELECT payload FROM projection_issues ORDER BY id`)
	if err != nil {
		return ProjectionRead{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return ProjectionRead{}, err
		}
		var issue Issue
		if err := json.Unmarshal(payload, &issue); err != nil {
			return ProjectionRead{}, err
		}
		result.Issues = append(result.Issues, issue)
	}
	if err := rows.Err(); err != nil {
		return ProjectionRead{}, err
	}
	if err := rows.Close(); err != nil {
		return ProjectionRead{}, err
	}
	commentRows, err := p.db.QueryContext(ctx, `SELECT issue_id, payload FROM projection_comments ORDER BY issue_id, id`)
	if err != nil {
		return ProjectionRead{}, err
	}
	defer commentRows.Close()
	for commentRows.Next() {
		var issueID string
		var payload []byte
		if err := commentRows.Scan(&issueID, &payload); err != nil {
			return ProjectionRead{}, err
		}
		var comment Comment
		if err := json.Unmarshal(payload, &comment); err != nil {
			return ProjectionRead{}, err
		}
		result.Comments[issueID] = append(result.Comments[issueID], comment)
	}
	if err := commentRows.Err(); err != nil {
		return ProjectionRead{}, err
	}
	return result, nil
}
