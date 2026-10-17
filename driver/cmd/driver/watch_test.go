package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/flock"
)

type watchFakeNotifier struct {
	sent     []string
	failNext int
}

func (n *watchFakeNotifier) send(_ context.Context, text string) error {
	if n.failNext > 0 {
		n.failNext--
		return errors.New("feishu unavailable")
	}
	n.sent = append(n.sent, text)
	return nil
}

var watchEpoch = time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)

// watchAt runs one timer firing as a fresh process would: only the state
// root carries over between runs.
func watchAt(t *testing.T, root string, notifier *watchFakeNotifier, now time.Time, status string, args ...string) int {
	t.Helper()
	report := healthReport{Checks: map[string]healthCheck{
		"version":     {Status: healthOK, Reason: "运行版本与 factory origin/main 一致"},
		"maintenance": {Status: status, Reason: "维护状态为 deploying，不是 running", Details: map[string]string{"secret": "TEST-PLACEHOLDER-CRED"}},
	}, Errors: []string{}}
	w := driverWatch{stateRoot: root, now: func() time.Time { return now }, doctor: func(context.Context) healthReport { return report }, notify: notifier.send}
	return w.main(context.Background(), args, &bytes.Buffer{})
}

func watchOnce(t *testing.T, root string, notifier *watchFakeNotifier, status string, args ...string) {
	t.Helper()
	if code := watchAt(t, root, notifier, watchEpoch, status, args...); code != 0 {
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

func TestDriverWatchResendsAfterFailedSend(t *testing.T) {
	root, notifier := t.TempDir(), &watchFakeNotifier{failNext: 1}
	watchOnce(t, root, notifier, healthFail)
	if code := watchAt(t, root, notifier, watchEpoch, healthFail); code == 0 {
		t.Fatal("failed send exited 0")
	}
	watchOnce(t, root, notifier, healthFail)
	watchOnce(t, root, notifier, healthFail)
	if len(notifier.sent) != 1 {
		t.Fatalf("failed alert resent %d times, want 1: %q", len(notifier.sent), notifier.sent)
	}
}

func TestDriverWatchRemindsEvery24Hours(t *testing.T) {
	root, notifier := t.TempDir(), &watchFakeNotifier{}
	for i, step := range []struct {
		after  time.Duration
		alerts int
	}{
		{0, 0},
		{30 * time.Minute, 1},
		{23 * time.Hour, 1},
		{24*time.Hour + 30*time.Minute, 2},
		{25 * time.Hour, 2},
	} {
		if code := watchAt(t, root, notifier, watchEpoch.Add(step.after), healthFail); code != 0 {
			t.Fatalf("run %d exited %d", i+1, code)
		}
		if len(notifier.sent) != step.alerts {
			t.Fatalf("run %d (+%s): %d alerts, want %d", i+1, step.after, len(notifier.sent), step.alerts)
		}
	}
	if !strings.Contains(notifier.sent[1], "持续 24 小时") {
		t.Fatalf("reminder text: %q", notifier.sent[1])
	}
}

func TestDriverWatchRecoversCorruptState(t *testing.T) {
	root, notifier := t.TempDir(), &watchFakeNotifier{}
	path := filepath.Join(root, "watch.json")
	if err := os.WriteFile(path, []byte("{truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	watchOnce(t, root, notifier, healthFail)
	kept, err := filepath.Glob(path + ".corrupt-*")
	if err != nil || len(kept) != 1 {
		t.Fatalf("corrupt state not kept as evidence: %v %v", kept, err)
	}
	if evidence, err := os.ReadFile(kept[0]); err != nil || string(evidence) != "{truncated" {
		t.Fatalf("evidence: %q %v", evidence, err)
	}
	watchOnce(t, root, notifier, healthFail)
	if len(notifier.sent) != 1 {
		t.Fatalf("watch did not resume from empty state: %q", notifier.sent)
	}
}

func TestDriverWatchSkipsWhileLocked(t *testing.T) {
	root, notifier := t.TempDir(), &watchFakeNotifier{}
	watchOnce(t, root, notifier, healthFail)
	lock, err := flock.TryLock(filepath.Join(root, "watch.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	watchOnce(t, root, notifier, healthFail)
	if len(notifier.sent) != 0 {
		t.Fatalf("locked run alerted: %q", notifier.sent)
	}
}
