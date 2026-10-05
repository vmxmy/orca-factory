package linear

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestExternalCallLog(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		warn       bool
	}{
		{"success", `{"data":{"commentCreate":{"success":true,"comment":{"id":"c1","body":"private-body"}}}}`, 200, false},
		{"ratelimited", `{"errors":[{"extensions":{"code":"RATELIMITED"}}]}`, 200, true},
		{"HTTP failure", `{"errors":[]}`, 500, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := fixture(t, func(w http.ResponseWriter, r request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) })
			var logs []string
			// Reuse fixture transport without changing the shared test helper.
			c, err := New(context.Background(), Options{TeamID: "team-id", BaseURL: c.endpoint, HTTPClient: c.http, Logger: func(line string) { logs = append(logs, line) }})
			if err != nil {
				t.Fatal(err)
			}
			if len(logs) != 3 || !strings.Contains(logs[0], "operation=Metadata") {
				t.Fatalf("metadata logging: %v", logs)
			}
			logs = nil
			_, err = c.CreateComment(context.Background(), "private-issue", "private-body")
			if (err != nil) != tc.warn {
				t.Fatalf("error: %v", err)
			}
			if len(logs) != 1 {
				t.Fatalf("log count: %d", len(logs))
			}
			line := logs[0]
			for _, want := range []string{"operation=CreateComment", fmt.Sprintf("status=%d", tc.status), "X-Complexity=7", "X-RateLimit-Requests-Remaining=2490"} {
				if !strings.Contains(line, want) {
					t.Fatalf("missing %s: %s", want, line)
				}
			}
			if strings.HasPrefix(line, "[WARN]") != tc.warn {
				t.Fatalf("level: %s", line)
			}
			match := regexp.MustCompile(`duration_ms=([0-9]+)`).FindStringSubmatch(line)
			if len(match) != 2 {
				t.Fatalf("missing milliseconds: %s", line)
			}
			milliseconds, err := strconv.Atoi(match[1])
			if err != nil || milliseconds < 0 {
				t.Fatalf("duration: %s", line)
			}
			if strings.Contains(line, "private") {
				t.Fatalf("private input logged: %s", line)
			}
		})
	}
}
