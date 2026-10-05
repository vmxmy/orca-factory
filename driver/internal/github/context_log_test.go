package github

import (
	"context"
	"net/http"
	"os"
	"reflect"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

type logRecorder struct{ fields map[string]any }

func (l *logRecorder) Debug(_ string, args ...any) { l.save(args) }
func (l *logRecorder) Info(_ string, args ...any)  { l.save(args) }
func (l *logRecorder) Warn(_ string, args ...any)  { l.save(args) }
func (l *logRecorder) Error(_ string, args ...any) { l.save(args) }
func (l *logRecorder) save(args []any) {
	l.fields = map[string]any{}
	for i := 0; i+1 < len(args); i += 2 {
		l.fields[args[i].(string)] = args[i+1]
	}
}

func TestRecordedExternalCallLogContext(t *testing.T) {
	body, err := os.ReadFile("../../testdata/recorded/github-pr-open-clean.json")
	if err != nil {
		t.Fatal(err)
	}
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-GitHub-Request-Id", "request-123")
		w.Write(body)
	}, Options{})
	log := new(logRecorder)
	var got, plain PullRequest
	path := "/repos/vmxmy/orca-factory/pulls/138?token=private-query"
	if err := c.request(runx.WithLogger(context.Background(), log), http.MethodGet, path, nil, &got); err != nil {
		t.Fatal(err)
	}
	if err := c.request(context.Background(), http.MethodGet, path, nil, &plain); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, plain) || got.Number != 138 {
		t.Fatal("logger changed recorded PR response")
	}
	if log.fields["method"] != "GET" || log.fields["path"] != "/repos/vmxmy/orca-factory/pulls/138" || log.fields["status"] != 200 || log.fields["request_id"] != "request-123" || log.fields["result"] != "success" {
		t.Fatalf("request fields: %v", log.fields)
	}
	if _, ok := log.fields["elapsed_ms"].(int64); !ok {
		t.Fatalf("missing duration: %v", log.fields)
	}
}
