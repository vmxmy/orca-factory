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
	Request merge.Request
	Ticket  *scheduler.Ticket
	Release *release.Pending
	Blocked string
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
	if l.Root == "" {
		return nil
	}
	if err := os.MkdirAll(l.Root, 0700); err != nil {
		return err
	}
	b, err := json.Marshal(l.entries)
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
func (l *Lane) Enqueue(r merge.Request) error {
	if err := l.load(); err != nil {
		return err
	}
	env := l.Repositories[r.Path].Env
	if env["AGENT_DEPLOY_WORKFLOW"] != "" || env["AGENT_SMOKE_CMD"] != "" {
		return errors.New("deployment-gated merges are not implemented; repository lane refused")
	}
	if r.Repository == "" || r.Number <= 0 {
		return errors.New("invalid merge request")
	}
	for _, e := range l.entries {
		if e.Request.Repository == r.Repository && e.Request.Number == r.Number {
			return nil
		}
	}
	l.entries = append(l.entries, laneEntry{Request: r})
	return nil
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
	return false, nil
}
func (l *Lane) SubmitRelease(ctx context.Context, writer release.PRWriter, r merge.Request, p release.Plan) error {
	if found, err := l.HasRelease(r.Repository, p.Plugin); err != nil {
		return err
	} else if found {
		return nil
	}
	pending, err := release.Submit(ctx, writer, l, r, p)
	if err != nil {
		return err
	}
	for i := range l.entries {
		if l.entries[i].Request.Repository == pending.Request.Repository && l.entries[i].Request.Number == pending.Request.Number {
			l.entries[i].Release = &pending
		}
	}
	return l.save()
}
