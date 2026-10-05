package linear

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

type logRecorder struct {
	level  string
	fields map[string]any
}

func (l *logRecorder) Debug(_ string, args ...any) { l.save("debug", args) }
func (l *logRecorder) Info(_ string, args ...any)  { l.save("info", args) }
func (l *logRecorder) Warn(_ string, args ...any)  { l.save("warn", args) }
func (l *logRecorder) Error(_ string, args ...any) { l.save("error", args) }
func (l *logRecorder) save(level string, args []any) {
	l.level, l.fields = level, map[string]any{}
	for i := 0; i+1 < len(args); i += 2 {
		l.fields[args[i].(string)] = args[i+1]
	}
}

func TestExternalCallLogContext(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(fmt.Sprint(limited), func(t *testing.T) {
			c, _ := fixture(t, func(w http.ResponseWriter, r request) {
				w.Header().Set("X-Complexity", "42")
				w.Header().Set("X-RateLimit-Requests-Remaining", "999")
				if limited {
					fmt.Fprint(w, `{"errors":[{"extensions":{"code":"RATELIMITED"}}]}`)
					return
				}
				fmt.Fprint(w, `{"data":{"commentCreate":{"success":true,"comment":{"id":"c1","body":"private-body"}}}}`)
			})
			log := new(logRecorder)
			c, err := New(runx.WithLogger(context.Background(), log), Options{TeamID: "team-id", BaseURL: c.endpoint, HTTPClient: c.http})
			if err != nil {
				t.Fatal(err)
			}
			if log.fields["operation"] != "query Metadata" {
				t.Fatalf("startup context logger: %v", log.fields)
			}
			_, err = c.CreateComment(context.Background(), "private-issue", "private-body")
			if (err != nil) != limited || (log.level == "warn") != limited {
				t.Fatalf("rate limit: err=%v level=%s", err, log.level)
			}
			if log.fields["operation"] != "mutation CreateComment" || log.fields["http_status"] != 200 || log.fields["X-Complexity"] != "42" || log.fields["X-RateLimit-Requests-Remaining"] != "999" {
				t.Fatalf("request fields: %v", log.fields)
			}
			if usage := c.Usage(); usage.Requests != 4 || usage.Complexity != 63 || usage.RequestLimit.Remaining != 999 {
				t.Fatalf("accounting changed: %+v", usage)
			}
		})
	}
}
