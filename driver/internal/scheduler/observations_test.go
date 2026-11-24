package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
)

func TestAutomationDisabledRefusedPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	repo := config.Repository{Path: "/repo"}
	const wantError = "automation read failed: unavailable"

	t.Run("restart and expiry", func(t *testing.T) {
		root := t.TempDir()
		oc := &startupOrca{failRead: 1, fakeOrca: fakeOrca{automations: receipt(`{"automations":[]}`)}}
		first := &Runtime{StateRoot: root, Orca: oc}
		before := time.Now()
		disabled, err := first.AutomationDisabled(ctx, repo)
		if disabled || err == nil || err.Error() != wantError || oc.reads != 1 {
			t.Fatalf("first read: disabled=%v err=%v reads=%d", disabled, err, oc.reads)
		}
		if err := first.refused[repo.Path]; err == nil || err.Error() != wantError {
			t.Errorf("in-memory refusal = %v, want %q", err, wantError)
		}
		path := filepath.Join(root, "automations-refused", "816fc349d3faebf8")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var cached struct {
			Error     string
			ExpiresAt time.Time
		}
		if err := json.Unmarshal(data, &cached); err != nil {
			t.Fatal(err)
		}
		if cached.Error != wantError || cached.ExpiresAt.Before(before.Add(24*time.Hour)) || cached.ExpiresAt.After(time.Now().Add(24*time.Hour)) {
			t.Fatalf("persisted refusal = %+v", cached)
		}
		second := &Runtime{StateRoot: root, Orca: oc}
		disabled, err = second.AutomationDisabled(ctx, repo)
		if disabled || err == nil || err.Error() != wantError || oc.reads != 1 {
			t.Fatalf("restart: disabled=%v err=%v reads=%d", disabled, err, oc.reads)
		}
		if err := second.refused[repo.Path]; err == nil || err.Error() != wantError {
			t.Fatalf("restored in-memory refusal = %v", err)
		}

		old := time.Now().Add(-25 * time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		third := &Runtime{StateRoot: root, Orca: oc}
		disabled, err = third.AutomationDisabled(ctx, repo)
		if !disabled || err != nil || oc.reads != 2 {
			t.Fatalf("expired file: disabled=%v err=%v reads=%d", disabled, err, oc.reads)
		}
	})

	t.Run("no state root", func(t *testing.T) {
		cwd := t.TempDir()
		t.Chdir(cwd)
		oc := &startupOrca{failRead: 1, fakeOrca: fakeOrca{automations: receipt(`{"automations":[]}`)}}
		r := &Runtime{Orca: oc}
		disabled, err := r.AutomationDisabled(ctx, repo)
		if disabled || err == nil || err.Error() != wantError {
			t.Fatalf("read failure: disabled=%v err=%v", disabled, err)
		}
		disabled, err = r.AutomationDisabled(ctx, repo)
		if !disabled || err != nil || oc.reads != 2 {
			t.Fatalf("retry without persistence: disabled=%v err=%v reads=%d", disabled, err, oc.reads)
		}
		r.refused = map[string]error{repo.Path: errors.New("memory refusal")}
		disabled, err = r.AutomationDisabled(ctx, repo)
		if disabled || err == nil || err.Error() != "memory refusal" || oc.reads != 2 {
			t.Fatalf("memory refusal: disabled=%v err=%v reads=%d", disabled, err, oc.reads)
		}
		entries, err := os.ReadDir(cwd)
		if err != nil || len(entries) != 0 {
			t.Fatalf("no state root wrote files: entries=%v err=%v", entries, err)
		}
	})
}
