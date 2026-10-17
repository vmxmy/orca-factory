package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type watchFakeNotifier struct{ sent []string }

func (n *watchFakeNotifier) send(_ context.Context, text string) error {
	n.sent = append(n.sent, text)
	return nil
}

// watchOnce runs one timer firing as a fresh process would: only the state
// root carries over between runs.
func watchOnce(t *testing.T, root string, notifier *watchFakeNotifier, status string, args ...string) {
	t.Helper()
	report := healthReport{Checks: map[string]healthCheck{
		"version":     {Status: healthOK, Reason: "运行版本与 factory origin/main 一致"},
		"maintenance": {Status: status, Reason: "维护状态为 deploying，不是 running", Details: map[string]string{"secret": "TEST-PLACEHOLDER-CRED"}},
	}, Errors: []string{}}
	w := driverWatch{stateRoot: root, doctor: func(context.Context) healthReport { return report }, notify: notifier.send}
	if code := w.main(context.Background(), args, &bytes.Buffer{}); code != 0 {
		t.Fatalf("watch %v exited %d", args, code)
	}
}

func TestDriverWatchAlertsOncePerPersistentFail(t *testing.T) {
	root, notifier := t.TempDir(), &watchFakeNotifier{}
	for i, step := range []struct {
		status string
		alerts int
	}{
		{healthFail, 0},
		{healthFail, 1},
		{healthFail, 1},
		{healthFail, 1},
		{healthOK, 1},
		{healthFail, 1},
		{healthFail, 2},
	} {
		watchOnce(t, root, notifier, step.status)
		if len(notifier.sent) != step.alerts {
			t.Fatalf("run %d (%s): %d alerts, want %d: %q", i+1, step.status, len(notifier.sent), step.alerts, notifier.sent)
		}
	}
	for _, alert := range notifier.sent {
		if !strings.Contains(alert, "maintenance") || !strings.Contains(alert, "维护状态为 deploying") || strings.Contains(alert, "TEST-PLACEHOLDER-CRED") {
			t.Fatalf("alert text: %q", alert)
		}
	}
}

func TestDriverWatchIgnoresWarnUnknownAndDryRun(t *testing.T) {
	root, notifier := t.TempDir(), &watchFakeNotifier{}
	for _, status := range []string{healthFail, healthWarn, healthFail, healthUnknown, healthFail, healthWarn, healthUnknown, healthUnknown} {
		watchOnce(t, root, notifier, status)
	}
	if len(notifier.sent) != 0 {
		t.Fatalf("non-consecutive fails alerted: %q", notifier.sent)
	}
	watchOnce(t, root, notifier, healthFail)
	state, err := os.ReadFile(filepath.Join(root, "watch.json"))
	if err != nil {
		t.Fatal(err)
	}
	watchOnce(t, root, notifier, healthFail, "--dry-run")
	after, err := os.ReadFile(filepath.Join(root, "watch.json"))
	if err != nil || !bytes.Equal(state, after) || len(notifier.sent) != 0 {
		t.Fatalf("dry-run changed state or sent: %v %q", err, notifier.sent)
	}
	watchOnce(t, root, notifier, healthFail)
	if len(notifier.sent) != 1 {
		t.Fatalf("real run after dry-run: %q", notifier.sent)
	}
}
