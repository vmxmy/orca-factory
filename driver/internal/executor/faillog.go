package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/linear"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
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

// Linear is the recovery source when this ticket's local state is lost.
func (e *Executor) restoreFailLog(ctx context.Context, c Config, t Ticket) error {
	path := failLogPath(c, t)
	if _, err := os.ReadFile(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot read FAIL history")
	}
	comments, err := linear.CriticalComments(ctx, e.Linear, t.ID)
	if err != nil {
		return errors.New("Linear FAIL history lookup failed")
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
