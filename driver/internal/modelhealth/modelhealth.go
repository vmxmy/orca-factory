// Package modelhealth records model availability independently of dispatch policy.
package modelhealth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata"
)

const (
	OK              = "ok"
	Unavailable     = "unavailable"
	Unknown         = "unknown"
	DefaultCooldown = 30 * time.Minute
	RouteModelProxy = "model_proxy"
	ProbeTimeout    = 120 * time.Second
)

type Ref struct {
	Agent  string `json:"agent"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
	Route  string `json:"route,omitempty"`
}

func Parse(value string) (Ref, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 || parts[0] == "" {
		return Ref{}, errors.New("model reference requires agent:model:effort")
	}
	return Ref{Agent: parts[0], Model: parts[1], Effort: parts[2]}, nil
}
func (r Ref) String() string { return r.Agent + ":" + r.Model + ":" + r.Effort }
func (r Ref) Key() string {
	key := r.Agent + ":" + r.Model
	if r.Route != "" {
		key += "@" + r.Route
	}
	return key
}

type Status struct {
	Ref       Ref       `json:"ref"`
	State     string    `json:"state"`
	Reason    string    `json:"reason"`
	Until     time.Time `json:"until"`
	CheckedAt time.Time `json:"checkedAt"`
}

var resetDate = regexp.MustCompile(`resets ([A-Z][a-z]{2} [0-9]{1,2}), ([0-9]{1,2}(?::[0-9]{2})?[ap]m) \(([^)]+)\)`)

// Classify retains only reason codes, never raw provider output.
func Classify(ref Ref, output string, err error, now time.Time) Status {
	s := Status{Ref: ref, State: Unknown, Reason: "unexpected_output", CheckedAt: now}
	lower := strings.ToLower(output)
	switch {
	case strings.Contains(lower, "you've hit your") && strings.Contains(lower, "limit"):
		s.State, s.Reason = Unavailable, "usage_limit"
		if match := resetDate.FindStringSubmatch(output); match != nil {
			if loc, e := time.LoadLocation(match[3]); e == nil {
				layout := "2006 Jan 2 3pm"
				if strings.Contains(match[2], ":") {
					layout = "2006 Jan 2 3:04pm"
				}
				s.Until, _ = time.ParseInLocation(layout, fmt.Sprintf("%d %s %s", now.In(loc).Year(), match[1], match[2]), loc)
				if !s.Until.IsZero() && now.In(loc).Month() == time.December && s.Until.Month() == time.January {
					s.Until = s.Until.AddDate(1, 0, 0)
				}
			}
		}
	case strings.Contains(lower, "not supported when using codex with a chatgpt account"):
		s.State, s.Reason = Unavailable, "model_not_supported"
	case errors.Is(err, context.DeadlineExceeded):
		s.Reason = "timeout"
	case err != nil:
		s.Reason = "probe_failed"
	case strings.TrimSpace(output) == "ok":
		s.State, s.Reason = OK, "probe_ok"
	}
	if s.State == Unavailable && !s.Until.After(now) {
		s.Until = now.Add(DefaultCooldown)
	}
	return s
}
