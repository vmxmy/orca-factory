package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/release"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

type releaseServeSource struct {
	now, last time.Time
	opened    int
}

func (b *releaseServeSource) Plugins(context.Context) ([]string, error) {
	return []string{"demo"}, nil
}
func (b *releaseServeSource) Env(context.Context, string) (string, error) { return "30", nil }
func (b *releaseServeSource) LatestTag(context.Context, string) (string, error) {
	return "demo-v1.0.0", nil
}
func (b *releaseServeSource) CommitsSince(context.Context, string, string) ([]release.Commit, error) {
	return []release.Commit{{Subject: "fix: bug"}}, nil
}
func (b *releaseServeSource) LastMerge(context.Context, string) (time.Time, error) {
	return b.last, nil
}
func (b *releaseServeSource) ReadFile(_ context.Context, p string) ([]byte, error) {
	if filepath.Ext(p) == ".json" {
		return []byte(`{"version":"1.0.0"}`), nil
	}
	return nil, nil
}
func (b *releaseServeSource) CreateReleasePR(context.Context, merge.Request, release.Plan) (int, error) {
	b.opened++
	return 7, nil
}

type releaseServeLinear struct{ serveMergeLinear }

func (l *releaseServeLinear) Snapshot(context.Context, []string, time.Time) ([]linear.Issue, error) {
	return nil, nil
}
func TestServeWiresRelease(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		t.Run(map[bool]string{true: "quiet_elapsed", false: "still_quiet"}[quiet], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			now := time.Now()
			b := &releaseServeSource{now: now, last: now.Add(-time.Minute)}
			if quiet {
				b.last = now.Add(-31 * time.Minute)
			}
			dir := t.TempDir()
			t.Setenv("DRIVER_STATUS_FILE", filepath.Join(dir, "status.json"))
			repo := config.Repository{Path: dir, MaxInFlight: 1, StaleHours: 8, Env: map[string]string{"AGENT_GH_REPO": "o/r", "AGENT_LINEAR_PROJECT": "p"}}
			cfg := config.Config{MaxInFlight: 1, Repositories: []config.Repository{repo}, TokenBucket: config.TokenBucket{Capacity: 1, RefillPerSecond: 1}}
			client := &releaseServeLinear{}
			manager := &release.Manager{File: filepath.Join(dir, "releases.json"), Request: merge.Request{Repository: "o/r", Path: dir}, Source: func(context.Context) (release.Source, error) { return b, nil }, Writer: b, Now: func() time.Time { return now }}
			runtime := &scheduler.Runtime{Reader: client, Clients: map[string]scheduler.LinearClient{dir: client}, Repositories: map[string]config.Repository{dir: repo}, Orca: &serveMergeOrca{}, Releases: map[string]*release.Manager{dir: manager}}
			err := serveWithStatus(ctx, filepath.Join(dir, "lock"), func(context.Context) (*scheduler.Scheduler, *scheduler.Runtime, error) {
				return &scheduler.Scheduler{Backend: runtime, Config: cfg}, runtime, nil
			}, func(context.Context) error { cancel(); return ctx.Err() }, func(e error) { t.Error(e) })
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if quiet {
				want = 1
			}
			head, err := manager.Head()
			if err != nil {
				t.Fatal(err)
			}
			if b.opened != want || head.IsZero() == quiet {
				t.Fatalf("release PRs=%d queued at=%v want=%d", b.opened, head, want)
			}
		})
	}
}

type releaseProbeOrca struct {
	serveMergeOrca
	calls    int
	rejected bool
}

func (o *releaseProbeOrca) AutomationsList(context.Context) (orca.Receipt, error) {
	o.calls++
	if o.calls == 1 {
		return orca.Receipt{"automations": json.RawMessage(`[]`)}, nil
	}
	if o.rejected {
		return orca.Receipt{"automations": json.RawMessage(`[{"enabled":true,"runContext":{"projectId":"github:o/r"}}]`)}, nil
	}
	return nil, errors.New("automation read unavailable")
}

func TestServeReleaseBarrierSurvivesSecondProbeFailure(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "unverifiable", true: "rejected"}[rejected], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dir := t.TempDir()
			t.Setenv("DRIVER_STATUS_FILE", filepath.Join(dir, "status.json"))
			head, base := strings.Repeat("b", 40), strings.Repeat("a", 40)
			h := merge.Handoff{Version: 1, TicketID: "id", Identifier: "ENG-1", Repository: "o/r", Unit: "executor", PR: 7, Head: head, Baseline: base,
				GateSpec: "UNIT: executor\nCHECK: true\nSCOPE: feature\nLANE: 1", GateOutput: "UNIT executor\nVERDICT green\nSTEP all\nREASON passed\nEVIDENCE\nSCOPE feature\nLANE 1", ReviewOutput: `{"verdict":"green","step":"review"}`, Lane: 1, Run: "run1", Task: "task1", Dispatch: "dispatch1", Worktree: dir, Attempt: 1, QueueID: "q1", EnqueuedAt: time.Now()}
			raw, err := json.Marshal(h)
			if err != nil {
				t.Fatal(err)
			}
			client := &serveMergeLinear{issue: linear.Issue{ID: "id", Identifier: "ENG-1", State: linear.NamedID{Name: "In Progress"}, Project: linear.NamedID{ID: "p"}}, comments: []linear.Comment{{ID: "handoff", Body: "归属: 合并通道\n" + merge.HandoffPrefix + string(raw)}}}
			repo := config.Repository{Path: dir, MaxInFlight: 2, StaleHours: 8, Env: map[string]string{"AGENT_LINEAR_PROJECT": "p", "AGENT_GH_REPO": "o/r", "AGENT_SCOPE_AVOID": "0"}}
			cfg := config.Config{MaxInFlight: 2, TokenBucket: config.TokenBucket{Capacity: 2, RefillPerSecond: 1}, Repositories: []config.Repository{repo}}
			file := filepath.Join(dir, "release.json")
			rows := []release.Entry{{EnqueuedAt: h.EnqueuedAt.Add(-time.Hour), Pending: release.Pending{Request: merge.Request{Repository: "o/r", Number: 8}, Plan: release.Plan{Plugin: "demo", Version: "1.0.1"}}}}
			raw, err = json.Marshal(rows)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			var reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reads.Add(1)
				if _, err := w.Write([]byte(`{}`)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			gh, err := github.New(github.Options{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "test", nil }})
			if err != nil {
				t.Fatal(err)
			}
			orcaClient := &releaseProbeOrca{rejected: rejected}
			runtime := &scheduler.Runtime{GitHub: gh, Reader: client, Clients: map[string]scheduler.LinearClient{dir: client}, Repositories: map[string]config.Repository{dir: repo}, Orca: orcaClient, Releases: map[string]*release.Manager{dir: {File: file}}}
			var tickErrors []error
			err = serveWithStatus(ctx, filepath.Join(dir, "lock"), func(context.Context) (*scheduler.Scheduler, *scheduler.Runtime, error) {
				return &scheduler.Scheduler{Backend: runtime, Config: cfg}, runtime, nil
			}, func(context.Context) error { cancel(); return ctx.Err() }, func(err error) { tickErrors = append(tickErrors, err) })
			if err != nil {
				t.Fatal(err)
			}
			if orcaClient.calls != 2 {
				t.Fatalf("automation probes=%d want 2", orcaClient.calls)
			}
			if reads.Load() != 0 || client.writes != 0 {
				t.Fatalf("feature advanced past release barrier: GitHub reads=%d Linear writes=%d", reads.Load(), client.writes)
			}
			if len(tickErrors) == 0 {
				t.Fatal("failed eligibility recheck was not reported")
			}
		})
	}
}
