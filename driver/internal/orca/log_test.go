package orca

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/github"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func TestOrcaCallLogOmitsFreeText(t *testing.T) {
	c, _ := fake(t)
	var buf bytes.Buffer
	ctx := runx.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	if _, err := c.RunCreate(ctx, "5e1dde31-cd0d-59a8-8358-5caf59d9e87f", "PRIVATE objective"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"command":"orchestration run-create"`, `"request_id":"5e1dde31-cd0d-59a8-8358-5caf59d9e87f"`, `"result":"ok"`, `"duration":`} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("missing %s in %s", want, buf.String())
		}
	}
	if strings.Contains(buf.String(), "PRIVATE objective") || strings.Count(buf.String(), "\n") != 1 {
		t.Fatalf("log %s", buf.String())
	}
}

func TestGitHubRequestLog(t *testing.T) {
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-GitHub-Request-Id", "GH-REQUEST-42")
		w.Header().Set("ETag", `"cache"`)
		w.WriteHeader(status)
		if status == 200 {
			if _, err := fmt.Fprint(w, `{"number":17}`); err != nil {
				t.Error(err)
			}
		}
	}))
	defer srv.Close()
	c, err := github.New(github.Options{BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "ghp_SECRET", nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []int{200, 304, 403} {
		status = code
		var buf bytes.Buffer
		ctx := runx.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
		_, err := c.GetPR(ctx, "acme", "app", 17)
		if (err != nil) != (code == 403) {
			t.Fatalf("status=%d err=%v", code, err)
		}
		for _, want := range []string{fmt.Sprintf(`"status":%d`, code), `"X-GitHub-Request-Id":"GH-REQUEST-42"`, `"method":"GET"`, `"path":"/repos/acme/app/pulls/17"`, `"duration":`} {
			if !strings.Contains(buf.String(), want) {
				t.Fatalf("missing %s in %s", want, buf.String())
			}
		}
		if strings.Contains(buf.String(), "ghp_SECRET") {
			t.Fatalf("log %s", buf.String())
		}
	}
}
