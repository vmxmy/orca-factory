package scheduler_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/scheduler"
)

func TestProjectionClaimRevalidatesRemoteEvidence(t *testing.T) {
	for _, mode := range []string{"done", "blocker", "missing-issue", "owner", "barrier", "valid"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := liveBatchSetup(t)
			r, repo := liveBatchRuntime(t, f)
			issues, err := f.client.Snapshot(ctx, []string{"Alpha"}, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			p, err := linear.OpenProjection(filepath.Join(t.TempDir(), "linear.db"), "team/Alpha")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = p.Close() }()
			if err := p.Publish(ctx, linear.ProjectionSnapshot{Issues: issues, Complete: true, Watermark: time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
				t.Fatal(err)
			}
			r.Clients[repo.Path] = linear.NewProjectionClient(f.client, p, time.Hour)
			beforeSnapshots, beforeComments := f.snapshots.Load(), f.comments.Load()
			tickets, err := r.Snapshot(ctx, []config.Repository{repo})
			if err != nil || len(tickets) == 0 {
				t.Fatalf("discovery: tickets=%d err=%v", len(tickets), err)
			}
			if f.snapshots.Load() != beforeSnapshots || f.comments.Load() != beforeComments {
				t.Fatal("discovery bypassed fresh projection")
			}
			f.mode.Store(mode)
			f.conflict.Store(mode == "owner")
			err = r.Apply(ctx, tickets[0], scheduler.Action{Kind: scheduler.Claim, Attempt: 1})
			if mode == "valid" {
				if err != nil || f.writes.Load() != 2 {
					t.Fatalf("valid claim: writes=%d err=%v", f.writes.Load(), err)
				}
			} else if err == nil || f.writes.Load() != 0 {
				t.Fatalf("stale projection authorized claim: writes=%d err=%v", f.writes.Load(), err)
			}
			if f.freshReads.Load() != 1 {
				t.Fatalf("remote issue reads=%d, want 1", f.freshReads.Load())
			}
			if (mode == "owner" || mode == "barrier" || mode == "valid") && f.comments.Load() != beforeComments+1 {
				t.Fatal("claim did not fetch remote ownership")
			}
		})
	}
}
