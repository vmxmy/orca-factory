package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
	"github.com/vmxmy/orca-factory/driver/internal/flock"
)

func init() { Register("watch", runWatch) }

// watchFailRuns filters the short fail a deploy can cause between doctor runs.
const watchFailRuns = 2

// watchRemindEvery keeps a persistent fail visible: watch alerts instead of
// filing tickets, so one alert followed by silence would let it be forgotten.
const watchRemindEvery = 24 * time.Hour

// watchCheck is one doctor check's history across timer firings. Alerts are
// keyed by check name, not reason: reasons carry counts that change each run.
// AlertedAt is set only after a send succeeds, so a failed send stays due.
type watchCheck struct {
	Status       string    `json:"status"`
	FailRuns     int       `json:"fail_runs"`
	FailingSince time.Time `json:"failing_since,omitzero"`
	AlertedAt    time.Time `json:"alerted_at,omitzero"`
}

type watchState struct {
	Checks map[string]watchCheck `json:"checks"`
}

type driverWatch struct {
	stateRoot string
	now       func() time.Time
	doctor    func(context.Context) healthReport
	notify    func(context.Context, string) error
}

func runWatch(args []string) int {
	probe, err := newDoctorProbe()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	w := driverWatch{stateRoot: logStateRoot(), now: time.Now, doctor: probe.run}
	if !slices.Equal(args, []string{"--dry-run"}) {
		if w.notify, err = feishuWatchNotifier(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return w.main(ctx, args, os.Stdout)
}

// feishuWatchNotifier is resolved before doctor runs, so a missing channel
// fails the run before watch.json advances.
func feishuWatchNotifier() (func(context.Context, string) error, error) {
	settings, err := config.FeishuNotifySettings(nil)
	if err != nil {
		return nil, err
	}
	chat := strings.TrimSpace(settings["AGENT_FEISHU_NOTIFY_CHAT"])
	if chat == "" {
		return nil, errors.New("AGENT_FEISHU_NOTIFY_CHAT 未配置，watch 无处告警")
	}
	sender, err := feishu.New(feishu.Options{Endpoint: settings["AGENT_FEISHU_ENDPOINT"], AppID: settings["AGENT_FEISHU_APP_ID"], AppSecret: settings["AGENT_FEISHU_APP_SECRET"]})
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, text string) error { return sender.SendText(ctx, chat, text) }, nil
}

func advanceWatch(prev watchState, report healthReport, now time.Time) (watchState, []string) {
	next, due := watchState{Checks: map[string]watchCheck{}}, []string{}
	for _, name := range slices.Sorted(maps.Keys(report.Checks)) {
		entry := prev.Checks[name]
		entry.Status = report.Checks[name].Status
		switch entry.Status {
		case healthFail:
			if entry.FailRuns == 0 {
				entry.FailingSince = now
			}
			entry.FailRuns++
		case healthOK:
			entry = watchCheck{Status: healthOK}
		default:
			entry.FailRuns, entry.FailingSince = 0, time.Time{}
		}
		if entry.FailRuns >= watchFailRuns && (entry.AlertedAt.IsZero() || now.Sub(entry.AlertedAt) >= watchRemindEvery) {
			due = append(due, name)
		}
		next.Checks[name] = entry
	}
	return next, due
}

func (w driverWatch) main(ctx context.Context, args []string, out io.Writer) int {
	dryRun := slices.Equal(args, []string{"--dry-run"})
	if len(args) != 0 && !dryRun {
		fmt.Fprintln(os.Stderr, "Usage: orca-factory-driver watch [--dry-run]")
		return 2
	}
	// A manual run racing the timer would send the same alert twice.
	if !dryRun {
		lock, err := flock.TryLock(filepath.Join(w.stateRoot, "watch.lock"))
		if errors.Is(err, syscall.EWOULDBLOCK) {
			slog.Info("another watch run holds watch.lock; skipping this run")
			return 0
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() { _ = lock.Close() }() // Release only; process exit drops the lock too.
	}
	now := w.now()
	path := filepath.Join(w.stateRoot, "watch.json")
	prev, err := readWatchState(path, now, dryRun)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	report := w.doctor(ctx)
	next, due := advanceWatch(prev, report, now)
	if err := writeWatchSummary(out, next, due, dryRun); err != nil {
		fmt.Fprintln(os.Stderr, "cannot output watch summary")
		return 1
	}
	if dryRun {
		return 0
	}
	code := 0
	for _, name := range due {
		entry := next.Checks[name]
		if err := w.notify(ctx, watchAlertText(name, report.Checks[name], entry, now)); err != nil {
			fmt.Fprintf(os.Stderr, "%s 告警发送失败，下次运行重发: %v\n", name, err)
			code = 1
			continue
		}
		entry.AlertedAt = now
		next.Checks[name] = entry
	}
	if err := writeWatchState(path, next); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return code
}

// readWatchState moves an unparsable state file aside and starts empty, so a
// single bad write cannot stop every later run. Dry-run leaves the file alone.
func readWatchState(path string, now time.Time, dryRun bool) (watchState, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return watchState{}, nil
	}
	if err != nil {
		return watchState{}, fmt.Errorf("read watch state: %w", err)
	}
	var state watchState
	parseErr := json.Unmarshal(raw, &state)
	if parseErr == nil {
		return state, nil
	}
	if dryRun {
		slog.Warn("watch state is corrupt; dry-run uses empty state", "path", path, "error", parseErr)
		return watchState{}, nil
	}
	evidence := path + ".corrupt-" + now.Format("20060102T150405")
	if err := os.Rename(path, evidence); err != nil {
		return watchState{}, fmt.Errorf("move corrupt watch state aside: %w", err)
	}
	slog.Warn("watch state was corrupt; moved aside and restarted from empty state", "evidence", evidence, "error", parseErr)
	return watchState{}, nil
}

func writeWatchState(path string, state watchState) error {
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create watch state root: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".watch-*.json")
	if err != nil {
		return fmt.Errorf("write watch state: %w", err)
	}
	defer func() { _ = os.Remove(temp.Name()) }() // Cleanup only; rename consumes this path.
	_, writeErr := temp.Write(append(raw, '\n'))
	if err := errors.Join(writeErr, temp.Close()); err != nil {
		return fmt.Errorf("write watch state: %w", err)
	}
	return os.Rename(temp.Name(), path)
}

func writeWatchSummary(out io.Writer, state watchState, due []string, dryRun bool) error {
	action := "告警"
	if dryRun {
		action = "将告警（dry-run 不发送、不写状态）"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-13s %-8s %-9s %s\n", "CHECK", "STATUS", "FAIL_RUNS", "ALERT")
	for _, name := range slices.Sorted(maps.Keys(state.Checks)) {
		entry, note := state.Checks[name], "-"
		switch {
		case slices.Contains(due, name):
			note = action
		case !entry.AlertedAt.IsZero():
			note = "已告警，24 小时后再提醒"
		}
		fmt.Fprintf(&b, "%-13s %-8s %-9d %s\n", name, entry.Status, entry.FailRuns, note)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// watchAlertText uses the check reason only; doctor never puts command output
// or environment values there, while details may carry service settings.
func watchAlertText(name string, check healthCheck, entry watchCheck, now time.Time) string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown-host"
	}
	hours := int(now.Sub(entry.FailingSince).Hours())
	return fmt.Sprintf("orca-factory doctor 告警（%s）\n检查: %s\n状态: fail，连续 %d 次，持续 %d 小时\n原因: %s\n详情: orca-factory-driver doctor", host, name, entry.FailRuns, hours, check.Reason)
}
