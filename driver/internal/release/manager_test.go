package release

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
)

type managerRemote struct {
	created, merged, tags, releases int
	failPublish                     bool
	ready                           bool
}

func (f *managerRemote) CreateReleasePR(context.Context, merge.Request, Plan) (int, error) {
	f.created++
	return 7, nil
}
func (f *managerRemote) Snapshot(context.Context, merge.Request) (merge.Snapshot, error) {
	return merge.Snapshot{Head: "head", Main: "main", CIHead: "head", CIGreen: f.ready, MergeableState: "clean"}, nil
}
func (f *managerRemote) Squash(context.Context, merge.Request, string, string) (string, error) {
	f.merged++
	return "squash", nil
}
func (f *managerRemote) CreateTag(context.Context, string, string, string, string) error {
	f.tags++
	return nil
}
func (f *managerRemote) CreateRelease(context.Context, string, string, github.ReleaseRequest) (github.Release, error) {
	f.releases++
	if f.failPublish {
		return github.Release{}, errors.New("lost response")
	}
	return github.Release{ID: 1}, nil
}
func TestManagerRestartsRetainQueueAndPublication(t *testing.T) {
	now := time.Now()
	src := &fakeSource{now: now, age: time.Hour, commits: []Commit{{Subject: "fix: bug"}}}
	remote := &managerRemote{}
	m := Manager{File: filepath.Join(t.TempDir(), "state.json"), Request: merge.Request{Repository: "o/r"}, Source: func(context.Context) (Source, error) { return src, nil }, Writer: remote, Merger: remote, Publisher: remote, Now: func() time.Time { return now }}
	ctx := context.Background()
	if err := m.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	head, err := m.Head()
	if err != nil || !head.Equal(now) {
		t.Fatal(head, err)
	}
	copy := m
	if err := copy.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if remote.created != 1 {
		t.Fatal("duplicate PR", remote.created)
	}
	if err := copy.Advance(ctx); err != nil {
		t.Fatal(err)
	}
	if remote.merged != 0 || remote.tags != 0 {
		t.Fatal("published before CI")
	}
	remote.ready = true
	remote.failPublish = true
	if err := copy.Advance(ctx); err == nil {
		t.Fatal("lost publication not reported")
	}
	if remote.merged != 1 {
		t.Fatal(remote.merged)
	}
	restored := m
	remote.failPublish = false
	if err := restored.Advance(ctx); err != nil {
		t.Fatal(err)
	}
	if remote.merged != 1 || remote.releases != 2 {
		t.Fatalf("merged=%d releases=%d", remote.merged, remote.releases)
	}
	head, err = restored.Head()
	if err != nil || !head.IsZero() {
		t.Fatal(head, err)
	}
}

type smokeDeployment struct {
	calls int
	fail  bool
}

func (d *smokeDeployment) Check(context.Context, merge.Request, string) (merge.DeploymentState, error) {
	return merge.DeploymentPassed, nil
}
func (d *smokeDeployment) Smoke(context.Context, merge.Request, string) error {
	d.calls++
	if d.fail {
		return errors.New("uncertain smoke")
	}
	return nil
}
func TestReleaseSmokeReceiptSurvivesRestart(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "uncertain"}[fail], func(t *testing.T) {
			d := &smokeDeployment{fail: fail}
			remote := &managerRemote{ready: true}
			m := Manager{File: filepath.Join(t.TempDir(), "state"), Merger: remote, Publisher: remote, Deploy: d}
			rows := []Entry{{Pending: Pending{Request: merge.Request{Repository: "o/r", Number: 7, SmokeCommand: "smoke"}, Plan: Plan{Plugin: "demo", Version: "1.0.1"}}, EnqueuedAt: time.Now()}}
			if err := m.save(rows); err != nil {
				t.Fatal(err)
			}
			err := m.Advance(context.Background())
			if (err != nil) != fail || d.calls != 1 {
				t.Fatalf("smoke calls=%d err=%v", d.calls, err)
			}
			copy := m
			_ = copy.Advance(context.Background())
			if d.calls != 1 {
				t.Fatal("uncertain smoke replayed")
			}
		})
	}
}

func TestReleaseCorruptReceiptCannotDisappearFromFIFO(t *testing.T) {
	m := Manager{File: filepath.Join(t.TempDir(), "state")}
	if err := m.save([]Entry{{Pending: Pending{Request: merge.Request{Repository: "o/r", Number: 7}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Head(); err == nil {
		t.Fatal("invalid timestamp silently removes release barrier")
	}
}

func TestReleaseExhaustedCatchupIsReported(t *testing.T) {
	m := Manager{File: filepath.Join(t.TempDir(), "state"), Merger: &managerRemote{ready: true}}
	rows := []Entry{{Pending: Pending{Request: merge.Request{Repository: "o/r", Number: 7, CatchupRounds: 2}, Plan: Plan{Plugin: "demo", Version: "1.0.1"}}, EnqueuedAt: time.Now()}}
	if err := m.save(rows); err != nil {
		t.Fatal(err)
	}
	if err := m.Advance(context.Background()); err == nil {
		t.Fatal("manual recovery silently stalled release queue")
	}
}
