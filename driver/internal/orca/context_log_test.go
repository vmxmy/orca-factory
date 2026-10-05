package orca

import (
	"context"
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
	for _, tc := range []struct {
		name, requestID string
		ok              bool
	}{
		{"orca-worktree-set.json", "07a77606-cef4-4541-a451-76c90fe19226", true},
		{"orca-worktree-set-not-found.json", "4108c3f2-1a6d-4753-bc08-201cf2acedc8", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := recordedClient(t, tc.name)
			args := []string{"worktree", "set", "--worktree", "path:/private-worktree", "--comment", "private-body"}
			plain, plainErr := c.call(context.Background(), args)
			log := new(logRecorder)
			got, err := c.call(runx.WithLogger(context.Background(), log), args)
			if !reflect.DeepEqual(got, plain) || (err == nil) != (plainErr == nil) {
				t.Fatalf("receipt/error changed: %v / %v", err, plainErr)
			}
			if log.fields["command"] != "worktree" || log.fields["subcommand"] != "set" || log.fields["ok"] != tc.ok {
				t.Fatalf("command fields: %v", log.fields)
			}
			if log.fields["request_id"] != tc.requestID {
				t.Fatalf("recorded request id: %v", log.fields)
			}
			if _, ok := log.fields["elapsed_ms"].(int64); !ok {
				t.Fatalf("missing duration: %v", log.fields)
			}
		})
	}
}
