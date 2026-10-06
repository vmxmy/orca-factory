package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestStatusSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	input := Snapshot{GeneratedAt: now, Tickets: []Ticket{{Repo: "/repo", Identifier: "ZII-883", Owner: "executor", Phase: "gate"}}, Pauses: []Pause{{Dependency: "Linear", Repo: "/repo", Kind: "quota", Start: now.Add(-time.Minute), Duration: time.Minute}}, Linear: Usage{Hour: now, Requests: 12, Complexity: 34, Limits: []ClientLimits{{Repo: "/repo", Requests: RateLimit{Limit: 100, Remaining: 88, Reset: now.Add(time.Hour)}, Complexity: RateLimit{Limit: 1000, Remaining: 966, Reset: now.Add(time.Hour)}}}}}
	t.Run("fields and atomic replacement", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "status.json")
		t.Setenv("DRIVER_STATUS_FILE", path)
		if err := os.WriteFile(path, []byte("broken old cache"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Write(input); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var got Snapshot
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, input) {
			t.Fatalf("snapshot = %#v, want %#v", got, input)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"generatedAt", "tickets", "pauses", "linear"} {
			if _, ok := fields[field]; !ok {
				t.Errorf("missing %s", field)
			}
		}
		var wire map[string]any
		if err := json.Unmarshal(data, &wire); err != nil {
			t.Fatal(err)
		}
		ticket := wire["tickets"].([]any)[0].(map[string]any)
		if !reflect.DeepEqual(ticket, map[string]any{"repo": "/repo", "identifier": "ZII-883", "owner": "executor", "phase": "gate"}) {
			t.Fatalf("ticket fields: %v", ticket)
		}
		paused := wire["pauses"].([]any)[0].(map[string]any)
		if !reflect.DeepEqual(paused, map[string]any{"dependency": "Linear", "repo": "/repo", "kind": "quota", "start": "2026-10-05T07:59:00Z", "duration": float64(time.Minute)}) {
			t.Fatalf("pause fields: %v", paused)
		}
		usage := wire["linear"].(map[string]any)
		if usage["hour"] != "2026-10-05T08:00:00Z" || usage["requests"] != float64(12) || usage["complexity"] != float64(34) {
			t.Fatalf("usage fields: %v", usage)
		}
		limits := usage["limits"].([]any)[0].(map[string]any)
		if limits["repo"] != "/repo" || !reflect.DeepEqual(limits["requests"], map[string]any{"limit": float64(100), "remaining": float64(88), "reset": "2026-10-05T09:00:00Z"}) || !reflect.DeepEqual(limits["complexity"], map[string]any{"limit": float64(1000), "remaining": float64(966), "reset": "2026-10-05T09:00:00Z"}) {
			t.Fatalf("quota fields: %v", limits)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "status.json" {
			t.Fatalf("temporary files remain: %v", entries)
		}
	})
	t.Run("failed create and rename leave no partial files", func(t *testing.T) {
		dir := t.TempDir()
		blocked := filepath.Join(dir, "blocked")
		if err := os.Mkdir(blocked, 0700); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{filepath.Join(dir, "missing", "status.json"), blocked} {
			t.Setenv("DRIVER_STATUS_FILE", path)
			if err := Write(input); err == nil {
				t.Fatal("expected write failure")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "blocked" {
				t.Fatalf("partial files: %v", entries)
			}
		}
	})
	t.Run("default path and empty pauses", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("TMPDIR", dir)
		t.Setenv("DRIVER_STATUS_FILE", "")
		if Path() != filepath.Join(dir, "orca-factory-driver-status.json") {
			t.Fatal(Path())
		}
		if err := Write(Snapshot{GeneratedAt: now}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(Path())
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["pauses"]) != "[]" && len(fields["pauses"]) != 0 {
			t.Fatalf("empty pauses = %s", fields["pauses"])
		}
	})
}

func TestReadViewJSON(t *testing.T) {
	now := time.Date(2026, 10, 5, 8, 2, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "status.json")
	t.Setenv("DRIVER_STATUS_FILE", path)
	input := `{"generatedAt":"2026-10-05T08:01:00Z","tickets":[{"repo":"/repo","identifier":"ZII-867","owner":"executor","phase":"gate"}],"pauses":[{"repo":"","dependency":"Linear","kind":"credentials","start":"2026-10-05T07:59:00Z","duration":60000000000},{"repo":"/repo","start":"2026-10-05T07:00:00Z"}],"linear":{"hour":"2026-10-05T08:00:00Z","requests":12,"complexity":34,"limits":[{"repo":"/repo","requests":{"limit":100,"remaining":88,"reset":"2026-10-05T09:00:00Z"},"complexity":{"limit":1000,"remaining":966,"reset":"2026-10-05T09:00:00Z"}}]}}`
	for _, tc := range []struct {
		name, data, state string
		missing           bool
	}{
		{"missing", "", "unknown", true},
		{"corrupt", "{broken", "unknown", false},
		{"normal", input, "ok", false},
		{"stale", `{"generatedAt":"2026-10-05T07:56:59Z"}`, "stale", false},
		{"five minute boundary", `{"generatedAt":"2026-10-05T07:57:00Z"}`, "ok", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.missing {
				if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			data, err := json.Marshal(Read(now, 0))
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got["state"] != tc.state {
				t.Fatalf("view = %s", data)
			}
			if tc.name == "normal" {
				want := `{"state":"ok","generatedAt":"2026-10-05T08:01:00Z","tickets":[{"repo":"/repo","identifier":"ZII-867","owner":"executor","phase":"gate"}],"globalPause":{"active":true,"durationSeconds":180},"linear":{"hour":"2026-10-05T08:00:00Z","requests":12,"complexity":34,"limits":[{"repo":"/repo","requests":{"limit":100,"remaining":88,"reset":"2026-10-05T09:00:00Z"},"complexity":{"limit":1000,"remaining":966,"reset":"2026-10-05T09:00:00Z"}}]}}`
				var expected map[string]any
				if err := json.Unmarshal([]byte(want), &expected); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, expected) {
					t.Fatalf("got %s; want %s", data, want)
				}
			}
		})
	}
}

func TestReadCurrentHourAndRepoPause(t *testing.T) {
	t.Setenv("DRIVER_STATUS_FILE", filepath.Join(t.TempDir(), "status.json"))
	err := os.WriteFile(Path(), []byte(`{"generatedAt":"2026-10-05T08:59:30Z","pauses":[{"repo":"/repo","start":"2026-10-05T08:00:00Z"}],"linear":{"hour":"2026-10-05T08:00:00Z","requests":12,"complexity":34,"limits":[{"repo":"/repo","requests":{"remaining":88}}]}}`), 0600)
	if err != nil {
		t.Fatal(err)
	}
	view := Read(time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC), 0)
	if view.GlobalPause.Active || view.GlobalPause.DurationSeconds != 0 || view.Linear.Requests != 0 || view.Linear.Complexity != 0 || view.Linear.Limits[0].Requests.Remaining != 88 {
		t.Fatalf("view = %+v", view)
	}
}
