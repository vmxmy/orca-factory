package task

import (
	"sort"
	"strings"
)

// GateSpecRefreshPrefix starts an owner comment asking the running execution
// stage to rebuild gate.spec from the current ticket face.
const GateSpecRefreshPrefix = "gate-spec-refresh:"

// GateSpecRefreshRequested reports a refresh directive written after the latest
// ownership line. Only a comment that starts with the prefix counts: driver
// comments quote gate output on later lines, which must not forge a directive.
// The next ownership line, written when the stage resumes, consumes it.
func GateSpecRefreshRequested(comments []Comment) bool {
	comments = append([]Comment(nil), comments...)
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].CreatedAt.Before(comments[j].CreatedAt) })
	requested := false
	for _, comment := range comments {
		if strings.HasPrefix(comment.Body, GateSpecRefreshPrefix) {
			requested = true
		}
		for _, line := range strings.Split(comment.Body, "\n") {
			if strings.HasPrefix(line, "归属: ") {
				requested = false
			}
		}
	}
	return requested
}
