package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/release"
)

func TestReleaseSharesFeatureFIFO(t *testing.T) {
	f := newMergeFixture(t)
	// A durable older release must prevent the already-ready feature PR merging.
	file := filepath.Join(t.TempDir(), "pending.json")
	entries := []release.Entry{{EnqueuedAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Pending: release.Pending{Request: merge.Request{Repository: "o/r", Number: 8}, Plan: release.Plan{Plugin: "demo", Version: "1.0.1"}}}}
	raw, _ := json.Marshal(entries)
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	path := f.config.Repositories[0].Path
	f.runtime.Releases = map[string]*release.Manager{path: {File: file}}
	f.runtime.ReleaseEligible = map[string]bool{path: true}
	err := (&Scheduler{Config: f.config, Backend: f.runtime}).Tick(context.Background())
	if f.puts != 0 {
		t.Fatalf("feature overtook older release: merges=%d err=%v", f.puts, err)
	}
	if err == nil {
		t.Fatal("missing release dependencies should retain FIFO head and report failure")
	}

}
