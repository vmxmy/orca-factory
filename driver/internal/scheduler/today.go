package scheduler

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/ledger"
	"github.com/vmxmy/orca-factory/driver/internal/status"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

type todayRun struct {
	repo       string
	Ticket     string    `json:"ticket"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Outcome    string    `json:"outcome"`
	Minutes    *float64  `json:"minutes"`
}

// readTodayRuns uses the snapshot's existing comments, preferring ledger tickets
// over task comments just as ledger stats does for Feishu.
func readTodayRuns(rows []task.Issue, comments map[string][]task.Comment, repoOf map[string]string) []todayRun {
	books := false
	for _, issue := range rows {
		if _, ok := ledger.Number(issue.Title); ok {
			books = true
			break
		}
	}
	var runs []todayRun
	for _, issue := range rows {
		if _, ok := ledger.Number(issue.Title); books && !ok {
			continue
		}
		for _, comment := range comments[issue.ID] {
			for _, line := range strings.Split(comment.Body, "\n") {
				raw, ok := strings.CutPrefix(line, "ledger ")
				if !ok {
					continue
				}
				var run todayRun
				if json.Unmarshal([]byte(raw), &run) != nil {
					continue
				}
				run.repo = repoOf[issue.ID]
				runs = append(runs, run)
			}
		}
	}
	return runs
}

// TodayOutput is an observation of the latest snapshot; it never reads a backend.
func (r *Runtime) TodayOutput(now time.Time) *status.TodayOutput {
	return todayOutput(r.todayRuns, now)
}

func todayOutput(runs []todayRun, now time.Time) *status.TodayOutput {
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	seen := map[[3]string]bool{}
	var output status.TodayOutput
	var minutes []float64
	for _, run := range runs {
		if run.Ticket == "" || run.StartedAt.IsZero() || run.FinishedAt.Before(run.StartedAt) || run.FinishedAt.Before(start) || run.FinishedAt.After(now) {
			continue
		}
		if run.Outcome != "merged" && run.Outcome != "escalated" {
			continue
		}
		key := [3]string{run.repo, run.Ticket, run.StartedAt.UTC().Format(time.RFC3339Nano)}
		if seen[key] {
			continue
		}
		seen[key] = true
		if run.Outcome == "merged" {
			output.Merged++
		} else {
			output.Escalated++
		}
		if run.Minutes != nil && *run.Minutes >= 0 {
			minutes = append(minutes, *run.Minutes)
		}
	}
	if len(seen) == 0 {
		return nil
	}
	if n := len(minutes); n > 0 {
		sort.Float64s(minutes)
		median := minutes[n/2]
		if n%2 == 0 {
			median = minutes[n/2-1]/2 + median/2
		}
		output.MedianMinutes = &median
	}
	return &output
}
