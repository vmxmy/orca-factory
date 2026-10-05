package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/machine"
)

func failLogPath(c Config, t Ticket) string {
	return filepath.Join(c.Root, c.Repository, t.Identifier, "fail.log")
}

// Bound individual fields so truncation cannot remove the reason or evidence.
// Only local brief metadata and fixed failure categories enter new summaries;
// raw worker, gate, reviewer and transport errors may contain credentials.
func failSummary(t Ticket, c Config, n int, ev machine.Event) string {
	var reason, evidence string
	switch ev {
	case machine.EvWorkerFail:
		reason, evidence = "worker failed", "worker outcome"
	case machine.EvGateRed:
		reason, evidence = "gate red", "gate"
	case machine.EvReviewRed:
		reason, evidence = "reviewer red", "review"
	default:
		return ""
	}
	approach := fmt.Sprintf("%s correction=%d: %s", c.Unit, n, t.Todo)
	approach = strings.ReplaceAll(strings.Join(strings.Fields(approach), " "), "|", "/")
	return truncateFail("FAIL: approach="+truncateFail(approach, 220)+" | reason="+reason+" | evidence="+evidence, 300)
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
	comments, err := e.Linear.ListComments(ctx, t.ID)
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
