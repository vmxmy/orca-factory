package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

func TestResolverOpenPRResult(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/recorded/orca-task-list.json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result struct {
			Tasks []orca.Task `json:"tasks"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	task := envelope.Result.Tasks[0]
	result := func(task orca.Task) string {
		t.Helper()
		c, s, o := setupController()
		s.record = &Record{Version: 1, Attempt: 1, Phase: "running", Run: task.RunID, Task: task.ID, Dispatch: "known-from-start", Spec: c.Spec, Agent: c.Agent}
		o.tasks = []orca.Task{task}
		if _, err := c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if s.record.Phase != "result" {
			t.Fatalf("real report ignored: %+v", s.record)
		}
		return s.record.Result
	}
	if _, err := ParseResult(result(task)); err == nil {
		t.Fatal("accepted recorded free-text worker report as a resolver proposal")
	}

	raw, err = os.ReadFile("../../testdata/recorded/github-pr-open-clean.json")
	if err != nil {
		t.Fatal(err)
	}
	var pr struct {
		Head struct{ SHA string }
	}
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatal(err)
	}
	var encoded string
	if err := json.Unmarshal(task.Result, &encoded); err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(encoded), &report); err != nil {
		t.Fatal(err)
	}
	branch := "agent/OF-5/executor"
	report["body"] = fmt.Sprintf(`{"action":"open-pr","branch":%q,"head":%q,"title":"Recover PR","body":"Verified fix","reason":"gate passed"}`, branch, pr.Head.SHA)
	raw, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	task.Result, err = json.Marshal(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	body := result(task)
	p, err := ParseResult(body)
	if err != nil || p.Action != "open-pr" || p.Branch != branch || p.Head != pr.Head.SHA {
		t.Fatalf("valid proposal rejected: %+v %v", p, err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"branch", "head", "title", "body", "reason"} {
		for _, bad := range []any{nil, "", " ", "\n"} {
			invalid := maps.Clone(payload)
			if bad == nil {
				delete(invalid, field)
			} else {
				invalid[field] = bad
			}
			raw, err := json.Marshal(invalid)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseResult(string(raw)); err == nil {
				t.Fatalf("accepted %s=%v", field, bad)
			}
		}
	}
	for _, head := range []string{strings.Repeat("a", 39), strings.Repeat("a", 41), strings.Repeat("g", 40)} {
		invalid := maps.Clone(payload)
		invalid["head"] = head
		raw, err := json.Marshal(invalid)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseResult(string(raw)); err == nil {
			t.Fatalf("accepted head %q", head)
		}
	}
}
