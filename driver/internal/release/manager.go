package release

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

// Entry retains the original plan before the first remote write. A crash between
// creation and enqueue reuses the same branch and plan, never a newer snapshot.
type Entry struct {
	Pending                   Pending
	EnqueuedAt                time.Time
	MergeSHA                  string
	Done                      bool
	SmokeStarted, SmokePassed bool
}
type Manager struct {
	File      string
	Request   merge.Request
	Source    func(context.Context) (Source, error)
	Writer    PRWriter
	Publisher Publisher
	Merger    merge.GitHub
	Git       merge.Git
	Gate      merge.Gate
	Deploy    merge.Deployment
	Now       func() time.Time
}

func (m *Manager) entries() ([]Entry, error) {
	raw, err := os.ReadFile(m.File)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rows []Entry
	err = json.Unmarshal(raw, &rows)
	if err != nil {
		return nil, err
	}
	for _, e := range rows {
		if e.EnqueuedAt.IsZero() || !pluginPattern.MatchString(e.Pending.Plan.Plugin) || !versionPattern.MatchString(e.Pending.Plan.Version) || e.Pending.Request.Number < 0 || (m.Request.Repository != "" && e.Pending.Request.Repository != m.Request.Repository) {
			return nil, errors.New("invalid durable release receipt")
		}
	}
	return rows, nil
}
func (m *Manager) save(rows []Entry) error {
	if err := os.MkdirAll(filepath.Dir(m.File), 0700); err != nil {
		return err
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(m.File), "release-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err = f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, m.File)
}
func (m *Manager) Scan(ctx context.Context) error {
	rows, err := m.entries()
	if err != nil {
		return err
	}
	// Recover pending writes before planning more work.
	for i := range rows {
		if !rows[i].Done && rows[i].Pending.Request.Number == 0 {
			if err = m.submit(ctx, &rows, i); err != nil {
				return err
			}
		}
	}
	src, err := m.Source(ctx)
	if err != nil {
		return err
	}
	now := m.Now
	if now == nil {
		now = time.Now
	}
	plans, err := Scan(ctx, src, now)
	if err != nil {
		return err
	}
	for _, p := range plans {
		skip := false
		for _, e := range rows {
			if e.Pending.Plan.Plugin == p.Plugin && (!e.Done || e.Pending.Plan.Version == p.Version) {
				skip = true
			}
		}
		if skip {
			continue
		}
		rows = append(rows, Entry{Pending: Pending{Request: m.Request, Plan: p}, EnqueuedAt: now()})
		if err = m.save(rows); err != nil {
			return err
		}
		if err = m.submit(ctx, &rows, len(rows)-1); err != nil {
			return err
		}
	}
	return nil
}

type receiptQueue struct{ save func(merge.Request) error }

func (q receiptQueue) Enqueue(r merge.Request) error { return q.save(r) }
func (m *Manager) submit(ctx context.Context, rows *[]Entry, i int) error {
	_, err := Submit(ctx, m.Writer, receiptQueue{func(r merge.Request) error { (*rows)[i].Pending.Request = r; return m.save(*rows) }}, (*rows)[i].Pending.Request, (*rows)[i].Pending.Plan)
	return err
}
func (m *Manager) Head() (time.Time, error) {
	rows, err := m.entries()
	if err != nil {
		return time.Time{}, err
	}
	for _, e := range rows {
		if !e.Done {
			return e.EnqueuedAt, nil
		}
	}
	return time.Time{}, nil
}
func (m *Manager) Advance(ctx context.Context) error {
	rows, err := m.entries()
	if err != nil {
		return err
	}
	for i := range rows {
		e := &rows[i]
		if e.Done {
			continue
		}
		if e.Pending.Request.Number == 0 {
			return m.submit(ctx, &rows, i)
		}
		if e.MergeSHA == "" {
			// Adapter recovery checks GitHub before any repeated squash.
			channel := merge.New(m.Merger, m.Git, m.Gate, nil)
			req := e.Pending.Request
			req.DeployWorkflow = ""
			req.SmokeCommand = ""
			if err = channel.Enqueue(req); err != nil {
				return err
			}
			result, nextErr := channel.Next(ctx, req.Repository)
			e.Pending.Request.CatchupRounds = result.Rounds
			if result.Status == merge.Merged {
				e.MergeSHA = result.MergeSHA
			}
			if err = m.save(rows); err != nil {
				return err
			}
			if nextErr != nil {
				return nextErr
			}
			if result.Status == merge.Human || result.Status == merge.Correction {
				return errors.New("release requires manual recovery: " + result.Reason)
			}
			if result.Status != merge.Merged {
				return nil
			}
		}
		if err = Publish(ctx, m.Publisher, e.Pending, merge.Result{Status: merge.Merged, Request: e.Pending.Request, MergeSHA: e.MergeSHA}); err != nil {
			return err
		}
		if e.Pending.Request.DeployWorkflow != "" || e.Pending.Request.SmokeCommand != "" {
			if m.Deploy == nil {
				return errors.New("release deployment checker required")
			}
			state, err := m.Deploy.Check(ctx, e.Pending.Request, e.MergeSHA)
			if err != nil {
				return err
			}
			if state != merge.DeploymentPassed {
				return errors.New("release deployment pending or requires recovery")
			}
		}
		if e.Pending.Request.SmokeCommand != "" && !e.SmokePassed {
			if e.SmokeStarted {
				return errors.New("release smoke outcome uncertain; manual recovery required")
			}
			smoke, ok := m.Deploy.(interface {
				Smoke(context.Context, merge.Request, string) error
			})
			if !ok {
				return errors.New("release smoke runner missing")
			}
			e.SmokeStarted = true
			if err = m.save(rows); err != nil {
				return err
			}
			if err = smoke.Smoke(ctx, e.Pending.Request, e.MergeSHA); err != nil {
				return err
			}
			e.SmokePassed = true
			if err = m.save(rows); err != nil {
				return err
			}
		}
		e.Done = true
		return m.save(rows)
	}
	return nil
}
