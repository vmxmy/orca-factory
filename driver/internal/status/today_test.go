package status

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestTodaySurvivesReadAndExpiresAtLocalMidnight(t *testing.T) {
	now := time.Date(2026, 11, 11, 23, 59, 0, 0, time.Local)
	for _, pushed := range []bool{false, true} {
		t.Run(map[bool]string{false: "snapshot", true: "push"}[pushed], func(t *testing.T) {
			path := Path()
			if pushed {
				t.Setenv("DRIVER_STATUS_FILE", "")
				path = PushPath()
				if err := os.MkdirAll(os.Getenv("XDG_RUNTIME_DIR")+"/orca-factory", 0700); err != nil {
					t.Fatal(err)
				}
			}
			raw := []byte(`{"state":"ok","generatedAt":"` + now.Format(time.RFC3339) + `","tickets":[],"today":{"merged":2,"escalated":0,"medianMinutes":4.5}}`)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			for _, at := range []time.Time{now, now.Add(2 * time.Minute)} {
				data, err := json.Marshal(Read(at, time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				var view map[string]json.RawMessage
				if err := json.Unmarshal(data, &view); err != nil {
					t.Fatal(err)
				}
				want := `{"merged":2,"escalated":0,"medianMinutes":4.5}`
				if at != now {
					want = ""
				}
				if string(view["today"]) != want {
					t.Fatalf("at=%s today=%s, want %s", at, view["today"], want)
				}
			}
		})
	}
}
