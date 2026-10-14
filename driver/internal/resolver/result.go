package resolver

import (
	"encoding/json"
	"errors"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"strings"
)

// Resolution is a proposal; the scheduler still verifies all merge/deploy gates.
type Resolution struct {
	Branch  string         `json:"branch,omitempty"`
	Head    string         `json:"head,omitempty"`
	Title   string         `json:"title,omitempty"`
	Body    string         `json:"body,omitempty"`
	Action  string         `json:"action"`
	Reason  string         `json:"reason"`
	Handoff *merge.Handoff `json:"handoff,omitempty"`
	Queue   string         `json:"queue,omitempty"`
	SHA     string         `json:"sha,omitempty"`
}

func ParseResult(body string) (Resolution, error) {
	var p Resolution
	if json.Unmarshal([]byte(body), &p) != nil || strings.TrimSpace(p.Reason) == "" || strings.ContainsAny(p.Reason, "\r\n") {
		return p, errors.New("invalid resolver result")
	}
	switch p.Action {
	case "open-pr":
		if strings.TrimSpace(p.Branch) == "" || strings.TrimSpace(p.Title) == "" || strings.TrimSpace(p.Body) == "" || len(p.Head) != 40 || strings.Trim(p.Head, "0123456789abcdef") != "" {
			return p, errors.New("resolver requires branch, full head SHA, title and body")
		}
	case "merge":
		if p.Handoff == nil || p.Handoff.Validated() != nil || p.Handoff.QueueID == "" {
			return p, errors.New("resolver requires validated merge handoff")
		}
	case "retry-workflow", "retry-smoke", "confirm-passed":
		if p.Queue == "" || len(p.SHA) != 40 || strings.Trim(p.SHA, "0123456789abcdef") != "" {
			return p, errors.New("resolver requires pinned deployment identity")
		}
	default:
		return p, errors.New("unknown resolver action")
	}
	return p, nil
}
