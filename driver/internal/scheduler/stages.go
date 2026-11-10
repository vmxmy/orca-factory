package scheduler

import (
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/status"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

type stageStart struct {
	name string
	at   time.Time
}

// readStageTimeline reuses the chronologically sorted ownership comments.
func readStageTimeline(comments []task.Comment) []stageStart {
	var timeline []stageStart
	for _, comment := range comments {
		name := ""
		if strings.HasPrefix(comment.Body, "转人: ") {
			name = "转人"
		}
		for _, line := range strings.Split(comment.Body, "\n") {
			if strings.HasPrefix(line, "阶段: ") {
				name = strings.TrimPrefix(line, "阶段: ")
			}
		}
		if name == "" || comment.CreatedAt.IsZero() {
			continue
		}
		if len(timeline) == 0 || timeline[len(timeline)-1].name != name {
			timeline = append(timeline, stageStart{name: name, at: comment.CreatedAt})
		}
	}
	return timeline
}

func stageDurations(t Ticket, now time.Time) []status.Stage {
	timeline := t.stageTimeline
	if len(timeline) == 0 || timeline[len(timeline)-1].name != string(t.Phase) {
		return nil
	}
	if len(timeline) > 20 {
		timeline = timeline[len(timeline)-20:]
	}
	stages := make([]status.Stage, 0, len(timeline))
	for i, start := range timeline {
		end := now
		if i+1 < len(timeline) {
			end = timeline[i+1].at
		}
		if start.at.After(end) {
			return nil
		}
		stages = append(stages, status.Stage{Name: start.name, Seconds: int64(end.Sub(start.at) / time.Second)})
	}
	return stages
}
