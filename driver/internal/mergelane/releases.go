package mergelane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/release"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

type laneEntry struct {
	Request         merge.Request
	Ticket          *scheduler.Ticket
	Release         *release.Pending
	Blocked         string
	BlockedHead     string
	Escalated       bool
	MergeSHA        string
	DeploymentReady bool
}

func (l *Lane) load() error {
	if l.loaded {
		return nil
	}
	if l.Root != "" {
		b, err := os.ReadFile(filepath.Join(l.Root, "merge-queue.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			if err = json.Unmarshal(b, &l.entries); err != nil {
				return err
			}
		}
	}
	l.loaded = true
	return nil
}
func (l *Lane) save() error {
	return l.saveEntries(l.entries)
}
func (l *Lane) saveEntries(entries []laneEntry) error {
	if l.Root == "" {
		return nil
	}
	if err := os.MkdirAll(l.Root, 0700); err != nil {
		return err
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(l.Root, "merge-queue-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(l.Root, "merge-queue.json"))
}

// accept commits the durable queue before making its entries consumable.
func (l *Lane) accept(entries []laneEntry) error {
	if err := l.saveEntries(entries); err != nil {
		return err
	}
	l.entries = entries
	return nil
}

func (l *Lane) flushPending() error {
	if len(l.pending) == 0 {
		return nil
	}
	if err := l.accept(append(append([]laneEntry(nil), l.entries...), l.pending...)); err != nil {
		return err
	}
	l.pending = nil
	return nil
}

func (l *Lane) RepositoryPaused(path string) (bool, error) {
	if err := l.load(); err != nil {
		return false, err
	}
	for _, e := range l.entries {
		if e.Request.Path == path && e.MergeSHA != "" && e.Blocked != "" {
			return true, nil
		}
	}
	return false, nil
}
func (l *Lane) HasRelease(repo, plugin string) (bool, error) {
	if err := l.load(); err != nil {
		return false, err
	}
	for _, e := range l.entries {
		if e.Release != nil && e.Request.Repository == repo && e.Release.Plan.Plugin == plugin {
			return true, nil
		}
	}
	for _, e := range l.pending {
		if e.Release != nil && e.Request.Repository == repo && e.Release.Plan.Plugin == plugin {
			return true, nil
		}
	}
	return false, nil
}

// submissionQueue attaches the plan before the shared queue's durable accept.
// On failure Pending remains available for saving without another PR write.
type submissionQueue struct {
	lane *Lane
	plan release.Plan
	err  error
}

func (q *submissionQueue) Enqueue(r merge.Request) error {
	pending := release.Pending{Request: r, Plan: q.plan}
	q.lane.pending = append(q.lane.pending, laneEntry{Request: r, Release: &pending})
	q.err = q.lane.flushPending()
	return q.err
}

func (l *Lane) SubmitRelease(ctx context.Context, writer release.PRWriter, r merge.Request, p release.Plan) error {
	if err := l.load(); err != nil {
		return err
	}
	if err := l.flushPending(); err != nil {
		return err
	}
	if found, err := l.HasRelease(r.Repository, p.Plugin); err != nil {
		return err
	} else if found {
		return nil
	}
	env := l.Repositories[r.Path].Env
	r.DeployWorkflow, r.SmokeCommand = env["AGENT_DEPLOY_WORKFLOW"], env["AGENT_SMOKE_CMD"]
	queue := &submissionQueue{lane: l, plan: p}
	pending, err := release.Submit(ctx, writer, queue, r, p)
	if err != nil {
		if pending.Request.Number > 0 {
			// Preserve Submit's recovery identity even when persistence failed.
			for i := range l.pending {
				if l.pending[i].Request == pending.Request {
					l.pending[i].Release = &pending
				}
			}
		}
		return errors.Join(err, queue.err)
	}
	return nil
}
