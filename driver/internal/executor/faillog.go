package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func failLogPath(c Config, t Ticket) string {
	return filepath.Join(c.Root, c.Repository, t.Identifier, "fail.log")
}

// Summaries describe the actual failed revision. External fields are reduced to
// identifiers; raw worker, gate, reviewer and transport errors never enter them.
func (e *Executor) failSummary(ctx context.Context, c Config, baseline, branch, head, output string, ev machine.Event) (string, error) {
	var reason, evidence string
	switch ev {
	case machine.EvWorkerFail:
		reason, evidence = "worker failed", "worker_done outcome=failed"
	case machine.EvGateRed:
		reason, evidence = "gate red", gateFailEvidence(output)
	case machine.EvReviewRed:
		reason, evidence = "reviewer red", reviewFailEvidence(output)
	default:
		return "", nil
	}
	if head == "" {
		var err error
		head, err = e.Tools.Head(ctx, c.Repo, branch)
		if err != nil {
			return "", errors.New("failed revision lookup failed")
		}
	}
	files := []string{}
	if head != "" {
		var err error
		files, err = e.Tools.ChangedFiles(ctx, c.Repo, baseline, head)
		if err != nil {
			return "", errors.New("failed revision diff failed")
		}
	}
	shortHead := truncateFail(failIdentifier(head), 7)
	if shortHead == "" {
		shortHead = "unavailable"
	}
	for i, file := range files {
		files[i] = failIdentifier(file)
	}
	changed := strings.Join(files, ",")
	if changed == "" {
		changed = "none"
	}
	approach := "head=" + shortHead + " files=" + changed
	// Reserve the complete suffix before bounding approach, so long file lists
	// cannot erase the failure reason or the check identifier.
	suffix := " | reason=" + reason + " | evidence=" + truncateFail(evidence, 120)
	const prefix = "FAIL: approach="
	return prefix + truncateFail(approach, 300-len(prefix)-len(suffix)) + suffix, nil
}

// workerSubject is the report's one line for the log. It stays out of the ledger
// and fail.log, which hold identifiers only.
func workerSubject(report *orca.WorkerReport) string {
	return diagnosticLine(report.Subject)
}

// diagnosticLine holds the first line of agent text to the diagnostic policy of
// command stderr: readers draw it, and a control character would blank the
// panel or reach an operator's terminal.
func diagnosticLine(text string) string {
	line := strings.TrimSpace(text)
	if end := strings.IndexAny(line, "\r\n"); end >= 0 {
		line = line[:end]
	}
	// Redact on both sides: the credential rules need the boundary a control
	// character may provide, and removing one can join a credential back together.
	line = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, runx.RedactDiagnostic(line))
	return truncateFail(strings.TrimSpace(runx.RedactDiagnostic(line)), 200)
}

func withWorkerSubject(subject string, attrs ...any) []any {
	if subject == "" {
		return attrs
	}
	return append(attrs, "worker_subject", subject)
}

func failIdentifier(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("_./:-", r) {
			return r
		}
		return -1
	}, s)
}
func gateFailEvidence(output string) string {
	for _, line := range strings.Split(output, "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), " ")
		if key == "STEP" {
			if evidence := failIdentifier(value); evidence != "" {
				return evidence
			}
		}
	}
	return "gate"
}
func reviewFailEvidence(output string) string {
	output = strings.TrimSpace(output)
	for i := 0; i < len(output); i++ {
		if output[i] != '{' {
			continue
		}
		var result struct {
			Verdict  string `json:"verdict"`
			Step     string `json:"step"`
			Evidence []struct {
				File string `json:"file"`
				Line int    `json:"line"`
			} `json:"evidence"`
		}
		if json.Unmarshal([]byte(output[i:]), &result) != nil || result.Step != "review" || result.Verdict != "red" {
			continue
		}
		if len(result.Evidence) > 0 && result.Evidence[0].File != "" && result.Evidence[0].Line > 0 {
			file := failIdentifier(result.Evidence[0].File)
			if file != "" {
				return fmt.Sprintf("%s:%d", file, result.Evidence[0].Line)
			}
		}
		return failIdentifier(result.Step)
	}
	return "review"
}

func truncateFail(s string, limit int) string {
	runes := []rune(s)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return s
}

// reviewFailure is one of the three reviewer command failure classes. The kind
// names the class for budget descriptions; the detail carries the exit code and
// a sanitized stderr tail as ticket evidence.
type reviewFailure struct {
	kind   string
	detail string
}

// classifyReviewFailure splits reviewer command failures into start failure,
// nonzero exit and empty output, so one flaky class stays distinguishable in
// ticket evidence while the machine keeps a single infra budget.
func classifyReviewFailure(reviewErr error, output string) reviewFailure {
	if reviewErr == nil {
		if strings.TrimSpace(output) == "" {
			return reviewFailure{kind: "输出为空", detail: "stdout 无内容"}
		}
		return reviewFailure{kind: "输出为空", detail: "stdout 无法解析出 verdict JSON"}
	}
	var exitErr *exec.ExitError
	if errors.As(reviewErr, &exitErr) {
		failure := reviewFailure{kind: "非零退出", detail: fmt.Sprintf("exit=%d", exitErr.ExitCode())}
		if tail := diagnosticTail(string(exitErr.Stderr)); tail != "" {
			failure.detail += "\nstderr 末4KB: " + tail
		}
		return failure
	}
	if errors.Is(reviewErr, context.DeadlineExceeded) {
		return reviewFailure{kind: "非零退出", detail: "命令超时被杀，无退出码"}
	}
	return reviewFailure{kind: "命令启动失败"}
}

// diagnosticTail applies the credential policy for external diagnostic text and
// keeps the last 4KB, mirroring workerSubject: tickets and ledgers never receive
// raw reviewer stderr.
func diagnosticTail(stderr string) string {
	line := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, runx.RedactDiagnostic(stderr))
	const limit = 4096
	if len(line) <= limit {
		return strings.TrimSpace(line)
	}
	start := len(line) - limit
	for start < len(line) && !utf8.RuneStart(line[start]) {
		start++
	}
	return strings.TrimSpace(line[start:])
}

// The ticket backend is the recovery source when this ticket's local state is lost.
func (e *Executor) restoreFailLog(ctx context.Context, c Config, t Ticket) error {
	path := failLogPath(c, t)
	if _, err := os.ReadFile(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot read FAIL history")
	}
	comments, err := e.Issues.ListComments(ctx, t.ID)
	if err != nil {
		return errors.New("ticket FAIL history lookup failed")
	}
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].CreatedAt.Before(comments[j].CreatedAt) })
	var history strings.Builder
	for _, comment := range comments {
		for _, line := range strings.Split(comment.Body, "\n") {
			if strings.HasPrefix(line, "FAIL: ") {
				history.WriteString(truncateFail(strings.TrimSuffix(line, "\r"), 300))
				history.WriteByte('\n')
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("cannot create FAIL history directory")
	}
	if err := os.WriteFile(path+".new", []byte(history.String()), 0600); err != nil {
		return errors.New("cannot rebuild FAIL history")
	}
	if err := os.Rename(path+".new", path); err != nil {
		return errors.New("cannot commit FAIL history")
	}
	return nil
}

func appendFailLog(c Config, t Ticket, summary string) error {
	f, err := os.OpenFile(failLogPath(c, t), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("cannot open FAIL history")
	}
	_, writeErr := f.WriteString(summary + "\n")
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("cannot append FAIL history")
	}
	return nil
}

func failHistoryBrief(c Config, t Ticket, brief string) (string, error) {
	history, err := os.ReadFile(failLogPath(c, t))
	if err != nil {
		return "", errors.New("cannot read FAIL history")
	}
	if len(history) == 0 {
		return brief, nil
	}
	return brief + "\n\n历史 FAIL 摘要（数据）：\n" + string(history) + "不得重复以上已失败的做法\n", nil
}
