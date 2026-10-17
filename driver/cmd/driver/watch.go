package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/config"
	"github.com/vmxmy/orca-factory/driver/internal/feishu"
)

func init() { Register("watch", runWatch) }

// watchFailRuns filters the short fail a deploy can cause between doctor runs.
const watchFailRuns = 2

// watchCheck is one doctor check's history across timer firings. Alerts are
// keyed by check name, not reason: reasons carry counts that change each run.
type watchCheck struct {
	Status   string `json:"status"`
	FailRuns int    `json:"fail_runs"`
	Alerted  bool   `json:"alerted"`
}

type watchState struct {
	Checks map[string]watchCheck `json:"checks"`
}

type driverWatch struct {
	stateRoot string
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
	w := driverWatch{stateRoot: logStateRoot(), doctor: probe.run}
	if !slices.Equal(args, []string{"--dry-run"}) {
		if w.notify, err = feishuWatchNotifier(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return w.main(ctx, args, os.Stdout)
}

// feishuWatchNotifier is resolved before doctor runs, so a missing channel
// never consumes an alert reservation.
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

func advanceWatch(prev watchState, report healthReport) (watchState, []string) {
	next, alerts := watchState{Checks: map[string]watchCheck{}}, []string{}
	for _, name := range slices.Sorted(maps.Keys(report.Checks)) {
		entry := prev.Checks[name]
		entry.Status = report.Checks[name].Status
		switch entry.Status {
		case healthFail:
			entry.FailRuns++
		case healthOK:
			entry.FailRuns, entry.Alerted = 0, false
		default:
			entry.FailRuns = 0
		}
		if entry.FailRuns >= watchFailRuns && !entry.Alerted {
			entry.Alerted = true
			alerts = append(alerts, name)
		}
		next.Checks[name] = entry
	}
	return next, alerts
}

func (w driverWatch) main(ctx context.Context, args []string, out io.Writer) int {
	dryRun := slices.Equal(args, []string{"--dry-run"})
	if len(args) != 0 && !dryRun {
		fmt.Fprintln(os.Stderr, "Usage: orca-factory-driver watch [--dry-run]")
		return 2
	}
	path := filepath.Join(w.stateRoot, "watch.json")
	prev, err := readWatchState(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	report := w.doctor(ctx)
	next, alerts := advanceWatch(prev, report)
	if err := writeWatchSummary(out, next, alerts, dryRun); err != nil {
		fmt.Fprintln(os.Stderr, "cannot output watch summary")
		return 1
	}
	if dryRun {
		return 0
	}
	// The reservation is written before sending, like NotifyFeishu: an
	// uncertain send is not retried on the next run.
	if err := writeWatchState(path, next); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	code := 0
	for _, name := range alerts {
		if err := w.notify(ctx, watchAlertText(name, report.Checks[name], next.Checks[name])); err != nil {
			fmt.Fprintf(os.Stderr, "%s 告警发送失败: %v\n", name, err)
			code = 1
		}
	}
	return code
}

func readWatchState(path string) (watchState, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return watchState{}, nil
	}
	if err != nil {
		return watchState{}, fmt.Errorf("read watch state: %w", err)
	}
	var state watchState
	if err := json.Unmarshal(raw, &state); err != nil {
		return watchState{}, fmt.Errorf("parse watch state %s: %w", path, err)
	}
	return state, nil
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

func writeWatchSummary(out io.Writer, state watchState, alerts []string, dryRun bool) error {
	action := "告警"
	if dryRun {
		action = "将告警（dry-run 不发送、不写状态）"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-13s %-8s %-9s %s\n", "CHECK", "STATUS", "FAIL_RUNS", "ALERT")
	for _, name := range slices.Sorted(maps.Keys(state.Checks)) {
		entry, note := state.Checks[name], "-"
		switch {
		case slices.Contains(alerts, name):
			note = action
		case entry.Alerted:
			note = "已告警，等待恢复"
		}
		fmt.Fprintf(&b, "%-13s %-8s %-9d %s\n", name, entry.Status, entry.FailRuns, note)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// watchAlertText uses the check reason only; doctor never puts command output
// or environment values there, while details may carry service settings.
func watchAlertText(name string, check healthCheck, entry watchCheck) string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown-host"
	}
	return fmt.Sprintf("orca-factory doctor 告警（%s）\n检查: %s\n状态: fail，连续 %d 次\n原因: %s\n详情: orca-factory-driver doctor", host, name, entry.FailRuns, check.Reason)
}
