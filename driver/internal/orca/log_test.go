package orca

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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
	if strings.Contains(buf.String(), "PRIVATE objective") || strings.Count(buf.String(), "\n") != 2 {
		t.Fatalf("log %s", buf.String())
	}
}

func TestGitHubRequestLog(t *testing.T) {
	headers, err := os.ReadFile("../../testdata/recorded/github-response-headers.txt")
	if err != nil {
		t.Fatal(err)
	}
	requestID := ""
	for _, line := range strings.Split(string(headers), "\n") {
		if key, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(key, "X-GitHub-Request-Id") {
			requestID = strings.TrimSpace(value)
		}
	}
	if requestID == "" {
		t.Fatal("recorded request ID missing")
	}
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, line := range strings.Split(string(headers), "\n") {
			if key, value, ok := strings.Cut(line, ":"); ok {
				w.Header().Set(key, strings.TrimSpace(value))
			}
		}
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
		for _, want := range []string{fmt.Sprintf(`"status":%d`, code), `"X-GitHub-Request-Id":"` + requestID + `"`, `"method":"GET"`, `"path":"/repos/acme/app/pulls/17"`, `"duration":`} {
			if !strings.Contains(buf.String(), want) {
				t.Fatalf("missing %s in %s", want, buf.String())
			}
		}
		if strings.Contains(buf.String(), "ghp_SECRET") {
			t.Fatalf("log %s", buf.String())
		}
	}
}

func TestOrcaReadRequestID(t *testing.T) {
	var buf bytes.Buffer
	ctx := runx.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	c := recordedClient(t, "orca-terminal-list.json")
	if _, err := c.TerminalList(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"request_id":"f19b4c60-9885-4790-874f-42d3aeca5f7e"`) {
		t.Fatalf("missing receipt ID: %s", &buf)
	}
}

func TestOrcaFailureProcessLog(t *testing.T) {
	c, _ := fake(t)
	t.Setenv("ORCA_TEST_SCENARIO", "failure")
	var buf bytes.Buffer
	ctx := runx.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	if _, err := c.RunCreate(ctx, firstID, "PRIVATE objective"); err == nil {
		t.Fatal("expected failure")
	}
	for _, want := range []string{`"exit_code":1`, `"stderr":`, `"result":"invalid_argument"`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("missing %s in %s", want, &buf)
		}
	}
	if strings.Contains(buf.String(), "PRIVATE objective") {
		t.Fatal("leaked objective")
	}
}
