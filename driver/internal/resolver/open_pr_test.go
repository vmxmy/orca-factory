package resolver

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResolverOpenPRResult(t *testing.T) {
	valid := map[string]string{"action": "open-pr", "branch": "agent/OF-5/executor", "head": strings.Repeat("a", 40), "title": "Recover PR", "body": "Verified fix", "reason": "gate passed"}
	raw, _ := json.Marshal(valid)
	p, err := ParseResult(string(raw))
	if err != nil || p.Action != "open-pr" {
		t.Fatalf("valid proposal rejected: %+v %v", p, err)
	}
	for _, field := range []string{"branch", "head", "title", "body", "reason"} {
		for _, bad := range []string{"", " ", "\n"} {
			copy := map[string]string{}
			for k, v := range valid {
				copy[k] = v
			}
			copy[field] = bad
			raw, _ := json.Marshal(copy)
			if _, err := ParseResult(string(raw)); err == nil {
				t.Fatalf("accepted %s=%q", field, bad)
			}
		}
	}
	for _, head := range []string{strings.Repeat("a", 39), strings.Repeat("a", 41), strings.Repeat("g", 40)} {
		valid["head"] = head
		raw, _ := json.Marshal(valid)
		if _, err := ParseResult(string(raw)); err == nil {
			t.Fatalf("accepted head %q", head)
		}
	}
}
