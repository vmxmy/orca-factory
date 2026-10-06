package linear

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func TestExternalCallLog(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "lin_api_SECRET")
	headers, err := os.ReadFile("../../testdata/recorded/linear-response-headers.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		status     int
		wantErr    bool
		level      string
	}{
		{"quota", `{"data":{"issue":{"comments":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}`, 200, false, "INFO"},
		{"rate limited", `{"errors":[{"extensions":{"code":"RATELIMITED"}}]}`, 200, true, "WARN"},
		{"unauthorized", `{}`, 401, true, "WARN"},
		{"forbidden", `{}`, 403, true, "WARN"},
		{"http quota", `{}`, 429, true, "WARN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for _, line := range strings.Split(string(headers), "\n")[1:] {
					if key, value, ok := strings.Cut(line, ":"); ok {
						w.Header().Set(key, strings.TrimSpace(value))
					}
				}
				w.WriteHeader(tc.status)
				if _, err := fmt.Fprint(w, tc.body); err != nil {
					t.Error(err)
				}
			}))
			defer srv.Close()
			c := &Client{key: os.Getenv("LINEAR_API_KEY"), endpoint: srv.URL, http: srv.Client(), now: time.Now}
			var buf bytes.Buffer
			ctx := runx.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
			_, err := c.ListComments(ctx, "private-issue-id")
			if (err != nil) != tc.wantErr {
				t.Fatalf("error %v", err)
			}
			for _, want := range []string{`"operation":"ListComments"`, fmt.Sprintf(`"status":%d`, tc.status), `"X-Complexity":"1"`, `"X-RateLimit-Requests-Remaining":"76"`, `"level":"` + tc.level + `"`, `"duration":`} {
				if !strings.Contains(buf.String(), want) {
					t.Fatalf("missing %s in %s", want, buf.String())
				}
			}
			if tc.name == "rate limited" && !strings.Contains(buf.String(), `"rate_limited":true`) {
				t.Fatalf("log %s", buf.String())
			}
			for _, secret := range []string{"query ListComments", "comments(first:", "private-issue-id", "lin_api_SECRET", "Authorization"} {
				if strings.Contains(buf.String(), secret) {
					t.Fatalf("leaked %s", buf.String())
				}
			}
		})
	}
	t.Run("transport failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/next")
			w.WriteHeader(http.StatusFound)
		}))
		defer srv.Close()
		hc := srv.Client()
		hc.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("lin_api_SECRET") }
		c := &Client{key: "lin_api_SECRET", endpoint: srv.URL, http: hc, now: time.Now}
		var buf bytes.Buffer
		ctx := runx.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
		_, err := c.ListComments(ctx, "private-issue-id")
		if err == nil || !strings.Contains(buf.String(), `"status":0`) || !strings.Contains(buf.String(), `"error":"transport failed"`) || strings.Contains(buf.String(), "lin_api_SECRET") {
			t.Fatalf("err=%v log=%s", err, buf.String())
		}
	})
}
