package orca

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestExternalCallLog(t *testing.T) {
	c, _ := fake(t)
	var b bytes.Buffer
	c.Logger = slog.New(slog.NewJSONHandler(&b, nil))
	if _, err := c.RunCreate(context.Background(), firstID, "build it"); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&b)
	found := false
	for dec.More() {
		var record map[string]any
		if err := dec.Decode(&record); err != nil {
			t.Fatal(err)
		}
		if record["component"] != "orca" {
			continue
		}
		found = true
		if record["request_id"] != firstID || record["success"] != true || record["command"] != "orchestration run-create" {
			t.Fatalf("record: %v", record)
		}
		if record["duration_ms"].(float64) < 0 {
			t.Fatal("negative duration")
		}
	}
	if !found {
		t.Fatal("missing orca record")
	}
}

func TestRecordedExternalCallRequestID(t *testing.T) {
	c := recordedClient(t, "orca-worktree-set.json")
	var b bytes.Buffer
	c.Logger = slog.New(slog.NewJSONHandler(&b, nil))
	if _, err := c.WorktreeSet(context.Background(), "/worktrees/ZII-1-impl", "ZII-1", "in-progress", "gate"); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&b)
	for dec.More() {
		var record map[string]any
		if err := dec.Decode(&record); err != nil {
			t.Fatal(err)
		}
		if record["component"] == "orca" {
			if record["request_id"] != "07a77606-cef4-4541-a451-76c90fe19226" {
				t.Fatalf("request id: %v", record)
			}
			return
		}
	}
	t.Fatal("missing orca record")
}
