package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func TestExternalCallLog(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "SECRET123")
	for _, limited := range []bool{false, true} {
		name := "metadata"
		if limited {
			name = "RATELIMITED"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Complexity", "42")
				w.Header().Set("X-RateLimit-Requests-Remaining", "7")
				if limited {
					_, _ = io.WriteString(w, `{"errors":[{"extensions":{"code":"RATELIMITED"}}]}`)
					return
				}
				_, _ = io.WriteString(w, `{"data":{"team":{"states":{"nodes":[],"pageInfo":{"hasNextPage":false}},"labels":{"nodes":[],"pageInfo":{"hasNextPage":false}},"projects":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}`)
			}))
			defer server.Close()
			var buf bytes.Buffer
			ctx := runx.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
			_, err := New(ctx, Options{TeamID: "private-team", BaseURL: server.URL})
			var global *infra.Global
			if limited && (!errors.As(err, &global) || global.Dep != infra.Linear) {
				t.Fatalf("rate limit error: %v", err)
			}
			if !limited && err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
			wantCount := 3
			wantLevel := "INFO"
			if limited {
				wantCount = 1
				wantLevel = "WARN"
			}
			if len(lines) != wantCount {
				t.Fatalf("log count: %s", &buf)
			}
			for _, line := range lines {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if record["operation"] != "Metadata" || record["status"] != float64(200) || record["X-Complexity"] != "42" || record["X-RateLimit-Requests-Remaining"] != "7" || record["level"] != wantLevel {
					t.Fatalf("request fields: %s", line)
				}
				if d, ok := record["duration"].(float64); !ok || d < 0 {
					t.Fatalf("missing duration: %s", line)
				}
			}
			for _, secret := range []string{"SECRET123", "private-team", "variables", "query Metadata", "Authorization"} {
				if strings.Contains(buf.String(), secret) {
					t.Fatalf("private request logged: %s", &buf)
				}
			}
			buf.Reset()
			_, _ = New(context.Background(), Options{TeamID: "private-team", BaseURL: server.URL})
			if buf.Len() != 0 {
				t.Fatal("unrequested logging")
			}
		})
	}
	t.Run("transport error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		server.Close()
		var buf bytes.Buffer
		_, err := New(runx.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil))), Options{TeamID: "team", BaseURL: server.URL})
		if err == nil {
			t.Fatal("expected transport error")
		}
		var record map[string]any
		if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		if record["level"] != "ERROR" || strings.Contains(buf.String(), server.URL) {
			t.Fatalf("transport error log: %s", &buf)
		}
	})
}
