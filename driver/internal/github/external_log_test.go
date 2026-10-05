package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"testing"
)

func TestExternalCallLog(t *testing.T) {
	for _, status := range []int{200, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var b bytes.Buffer
			c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-GitHub-Request-Id", "request-123")
				w.WriteHeader(status)
				fmt.Fprint(w, `{"number":17}`)
			}, Options{Logger: slog.New(slog.NewJSONHandler(&b, nil))})
			_, err := c.GetPR(context.Background(), "acme", "app", 17)
			if (err == nil) != (status == 200) {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(b.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record["request_id"] != "request-123" || record["http_status"] != float64(status) || record["success"] != (status == 200) || record["method"] != "GET" || record["endpoint"] != "/repos/acme/app/pulls/17" {
				t.Fatalf("record: %v", record)
			}
			if record["duration_ms"].(float64) < 0 {
				t.Fatal("negative duration")
			}
		})
	}
}
