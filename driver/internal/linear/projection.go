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
	Fresh      bool
	Complete   bool
	Source     string
	Scope      string
	Generation int64
	Watermark  string
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
