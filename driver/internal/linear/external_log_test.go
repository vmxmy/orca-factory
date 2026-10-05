package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestExternalCallLog(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(fmt.Sprint(limited), func(t *testing.T) {
			c, _ := fixture(t, func(w http.ResponseWriter, r request) {
				if limited {
					fmt.Fprint(w, `{"errors":[{"extensions":{"code":"RATELIMITED"}}]}`)
				} else {
					fmt.Fprint(w, `{"data":{"issue":{"comments":{"nodes":[],"pageInfo":{}}}}}`)
				}
			})
			var b bytes.Buffer
			c.logger = slog.New(slog.NewJSONHandler(&b, nil))
			_, err := c.ListComments(context.Background(), "private-issue")
			if (err != nil) != limited {
				t.Fatalf("error: %v", err)
			}
			var record map[string]any
			if err := json.Unmarshal(b.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			level := "INFO"
			if limited {
				level = "WARN"
			}
			if record["operation"] != "ListComments" || record["http_status"] != float64(200) || record["complexity"] != "7" || record["requests_remaining"] != "2490" || record["level"] != level {
				t.Fatalf("record: %s", &b)
			}
			if record["duration_ms"].(float64) < 0 {
				t.Fatal("negative duration")
			}
			if strings.Contains(b.String(), "private") {
				t.Fatalf("private input logged: %s", &b)
			}
		})
	}
}
