package task

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	// GateSpecRefreshPrefix starts an owner comment asking the running execution
	// stage to rebuild gate.spec from the current ticket face.
	GateSpecRefreshPrefix = "gate-spec-refresh:"
	// GateSpecRefreshDonePrefix starts the receipt an executor writes before it
	// exits to honor a directive.
	GateSpecRefreshDonePrefix = "gate-spec-refresh 已执行 "
	// GateSpecRefreshFrozen tells the owner that a directive arrived too late:
	// the handoff freezes the spec the executor already gated.
	GateSpecRefreshFrozen = "gate-spec-refresh 未执行：已进入交接，按旧 spec 冻结"
)

// GateSpecRefreshReceipt is the executor's audit line for an honored directive.
func GateSpecRefreshReceipt(attempt int, terminal string) string {
	return fmt.Sprintf("%sattempt=%d term=%s", GateSpecRefreshDonePrefix, attempt, terminal)
}

// GateSpecRefresh is the refresh evidence of one claim cycle. The ledger cannot
// tell a driver comment from one an Agent wrote through the driver CLI, so the
// directive has no author check; one honored refresh per attempt bounds it.
type GateSpecRefresh struct {
	// Pending is a directive written after the latest ownership line.
	Pending bool
	// Exited is the first receipt of its attempt written after the latest
	// ownership line: the executor exited to honor a directive.
	Exited bool
	// Done holds the attempts that already wrote a receipt.
	Done map[int]bool
}

// ReadGateSpecRefresh scans the ledger in time order. Directives and receipts
// count only when the whole comment starts with their prefix: driver comments
// quote gate output on later lines, which must not forge either. An ownership
// line consumes both; an ownership line for no owner starts a new claim cycle.
func ReadGateSpecRefresh(comments []Comment) GateSpecRefresh {
	comments = append([]Comment(nil), comments...)
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].CreatedAt.Before(comments[j].CreatedAt) })
	refresh := GateSpecRefresh{Done: map[int]bool{}}
	for _, comment := range comments {
		if strings.HasPrefix(comment.Body, GateSpecRefreshPrefix) {
			refresh.Pending = true
		}
		if attempt, ok := receiptAttempt(comment.Body); ok {
			refresh.Exited = !refresh.Done[attempt]
			refresh.Done[attempt] = true
		}
		for _, line := range strings.Split(comment.Body, "\n") {
			owner, ok := strings.CutPrefix(line, "归属: ")
			if !ok {
				continue
			}
			refresh.Pending, refresh.Exited = false, false
			if fields := strings.Fields(owner); len(fields) > 0 && fields[0] == "无" {
				refresh.Done = map[int]bool{}
			}
		}
	}
	return refresh
}

func receiptAttempt(body string) (int, bool) {
	rest, ok := strings.CutPrefix(body, GateSpecRefreshDonePrefix+"attempt=")
	if !ok {
		return 0, false
	}
	value, _, _ := strings.Cut(rest, " ")
	attempt, err := strconv.Atoi(value)
	return attempt, err == nil && attempt > 0
}
