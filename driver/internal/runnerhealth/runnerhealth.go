// Package runnerhealth turns a stalled isolated CI runner slot into one
// agent-claimable ticket. Slots, units and leases mirror
// scripts/local-runner/supervisor.py; the monitor only reads them.
package runnerhealth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// Repository is the only repository the isolated runners register for.
const Repository = "vmxmy/orca-factory"

// grace keeps a normal registration or a single crash from opening a ticket.
const grace = 10 * time.Minute

var slots = []int{1, 2, 3, 4}

type Tickets interface {
	Create(context.Context, issues.CreateInput) (issues.Task, error)
	Read(context.Context, string) (issues.Task, error)
	CreateComment(context.Context, string, string) (linear.Comment, error)
}

type Monitor struct {
	StateDir string // the supervisor's --state-dir
	Record   string // this monitor's own state file
	Tickets  Tickets
	Run      func(context.Context, runx.Spec) ([]byte, error)
}

type record struct {
	Since      map[string]time.Time `json:"since"`
	Restarts   map[string]int       `json:"restarts"`
	Ticket     string               `json:"ticket,omitempty"`
	Identifier string               `json:"identifier,omitempty"`
	Recovered  bool                 `json:"recovered"`
	// A failed create may still have opened an issue; never create again until
	// every slot recovers, so a flaky API cannot flood the tracker.
	Unconfirmed bool `json:"unconfirmed,omitempty"`
}

// Poll opens a ticket once any problem has persisted for the grace period, and
// comments once on that ticket when every slot is healthy again.
func (m *Monitor) Poll(ctx context.Context, now time.Time) error {
	rec, err := m.load()
	if err != nil {
		return err
	}
	problems, err := m.probe(ctx, now, rec)
	if err != nil {
		return err
	}
	since := map[string]time.Time{}
	due := false
	for key := range problems {
		first, ok := rec.Since[key]
		if !ok {
			first = now
		}
		since[key] = first
		due = due || now.Sub(first) >= grace
	}
	rec.Since = since
	// Reading the ticket costs a full issue listing, so only an action reads it.
	if due && rec.Ticket != "" {
		task, err := m.Tickets.Read(ctx, rec.Ticket)
		if err != nil {
			return err
		}
		if task.NativeState != "open" {
			rec.Ticket, rec.Identifier = "", ""
		}
	}
	switch {
	case due && rec.Ticket == "" && !rec.Unconfirmed:
		task, err := m.Tickets.Create(ctx, ticket(problems))
		if err != nil {
			rec.Unconfirmed = true
			return errors.Join(fmt.Errorf("runner alert ticket unconfirmed: %w", err), m.save(rec))
		}
		rec.Ticket, rec.Identifier, rec.Recovered = task.ID, task.Identifier, false
	case len(problems) > 0:
		rec.Recovered = false
	case rec.Unconfirmed:
		rec.Unconfirmed = false
	case rec.Ticket != "" && !rec.Recovered:
		body := fmt.Sprintf("runner 检测：%s 起 4 个槽位均已健康。", now.UTC().Format(time.RFC3339))
		if _, err := m.Tickets.CreateComment(ctx, rec.Ticket, body); err != nil {
			return err
		}
		rec.Recovered = true
	}
	return m.save(rec)
}

func (m *Monitor) probe(ctx context.Context, now time.Time, rec *record) (map[string]string, error) {
	problems := map[string]string{}
	for _, slot := range slots {
		unit := fmt.Sprintf("orca-go-isolated-runner@%d.service", slot)
		out, err := m.Run(ctx, runx.Spec{Path: "systemctl", Args: []string{"--user", "show", unit, "-p", "ActiveState", "-p", "NRestarts"}, Timeout: 10 * time.Second})
		if err != nil {
			return nil, fmt.Errorf("runner unit %d unreadable: %w", slot, err)
		}
		props := map[string]string{}
		for _, line := range strings.Split(string(out), "\n") {
			if key, value, ok := strings.Cut(line, "="); ok {
				props[key] = value
			}
		}
		restarts, err := strconv.Atoi(props["NRestarts"])
		if err != nil {
			return nil, fmt.Errorf("runner unit %d NRestarts unreadable", slot)
		}
		name := strconv.Itoa(slot)
		if previous, ok := rec.Restarts[name]; ok && restarts > previous {
			problems[fmt.Sprintf("slot-%d-restarting", slot)] = fmt.Sprintf("槽位 %d：`%s` 自上次检测重启 %d 次（NRestarts %d→%d）", slot, unit, restarts-previous, previous, restarts)
		}
		rec.Restarts[name] = restarts
		if props["ActiveState"] == "failed" {
			problems[fmt.Sprintf("slot-%d-failed", slot)] = fmt.Sprintf("槽位 %d：`%s` 处于 failed", slot, unit)
		}
		lease, modified, err := readLease(filepath.Join(m.StateDir, fmt.Sprintf("slot-%d.json", slot)))
		if err != nil {
			return nil, err
		}
		if lease.Active && lease.Phase == "registering" && now.Sub(modified) > grace {
			problems[fmt.Sprintf("slot-%d-registering", slot)] = fmt.Sprintf("槽位 %d：租约 `%s` 自 %s 停在 registering（unresolved registration intent）", slot, lease.Name, modified.UTC().Format(time.RFC3339))
		}
	}
	return problems, nil
}

type lease struct {
	Name   string `json:"name"`
	Active bool   `json:"active"`
	Phase  string `json:"phase"`
}

func readLease(path string) (lease, time.Time, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return lease{}, time.Time{}, nil
	}
	if err != nil {
		return lease{}, time.Time{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return lease{}, time.Time{}, err
	}
	var l lease
	if err := json.Unmarshal(b, &l); err != nil {
		return lease{}, time.Time{}, fmt.Errorf("runner lease %s unreadable", filepath.Base(path))
	}
	return l, info.ModTime(), nil
}

func ticket(problems map[string]string) issues.CreateInput {
	var evidence []string
	for _, detail := range problems {
		evidence = append(evidence, "- "+detail)
	}
	sort.Strings(evidence)
	description := `## 现状
driver 调度器检测到隔离 CI runner 槽位停摆超过 10 分钟，CI 只能靠其余槽位排队：
` + strings.Join(evidence, "\n") + `

## 要做
1. 按 ` + "`scripts/local-runner/README.md`" + ` 诊断：` + "`journalctl --user -u orca-go-isolated-runner@<槽位> -n 50`" + `，查看 ` + "`~/.local/state/orca-go-isolated-runner/slot-<槽位>.json`" + `。
2. 租约停在 registering 时，严格按 README「unresolved registration intent」一段做显式维护：停该槽位、持槽位锁、分页核对 GitHub 无同名 runner、确认无同名容器，归档租约与观察记录到 ` + "`maintenance/`" + `，再启动槽位。只动出问题的槽位，不删除任何文件。
3. 其他原因（镜像、代理、Docker）按 README 对应段处理；若根因在 ` + "`scripts/local-runner/`" + ` 代码，在 SCOPE 内修复并补测试。

## 验收
CHECK: for s in 1 2 3 4; do systemctl --user is-active --quiet orca-go-isolated-runner@$s || exit 1; done; ! grep -qs '"phase": "registering"' ~/.local/state/orca-go-isolated-runner/slot-*.json
断言: 4 个槽位服务均 active，且没有停在 registering 的租约；告警时为红。

## LANE 预判
2（依据：命中 ` + "`$AGENT_LANE2_GLOBS`" + ` 的 ` + "`scripts/*`" + `）

## SCOPE
scripts/local-runner

## 依赖
DEPENDENCIES: {"blockers":[],"related":[]}
无阻塞。

## 来源
由 driver 调度器 runner 检测自动开票；同一故障只开一张，恢复后在本票评论。`
	estimate := 1.0
	return issues.CreateInput{
		Title:       "ops(runner): 隔离 CI runner 槽位停摆",
		Description: description,
		State:       "Todo",
		Labels:      []string{"ready-for-agent"},
		Estimate:    &estimate,
		Blockers:    []string{},
		Related:     []string{},
	}
}

func (m *Monitor) load() (*record, error) {
	rec := &record{Since: map[string]time.Time{}, Restarts: map[string]int{}}
	b, err := os.ReadFile(m.Record)
	if errors.Is(err, os.ErrNotExist) {
		return rec, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, rec); err != nil {
		return nil, errors.New("runner health record unreadable")
	}
	if rec.Since == nil {
		rec.Since = map[string]time.Time{}
	}
	if rec.Restarts == nil {
		rec.Restarts = map[string]int{}
	}
	return rec, nil
}

func (m *Monitor) save(rec *record) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.Record + ".next"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.Record)
}
