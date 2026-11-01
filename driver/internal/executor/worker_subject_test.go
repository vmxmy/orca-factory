package executor

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/vmxmy/orca-factory/driver/internal/logx"
	"github.com/vmxmy/orca-factory/driver/internal/machine"
	"github.com/vmxmy/orca-factory/driver/internal/orca"
)

func TestWorkerReportSubjectReachesCorrectionLog(t *testing.T) {
	e, o, _, _, _, ticket := recoveryFixture(t)
	var logs bytes.Buffer
	e.logContext = logx.New("executor", &logs)
	o.failFirst = true
	o.failSubject = "ZY-333 blocked: same-origin guard outside SCOPE"
	if _, err := e.Execute(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range transitionLogRecords(t, &logs) {
		if r["msg"] == "correction" {
			found = true
			if r["worker_subject"] != o.failSubject || r["reason"] != "worker failed" {
				t.Fatalf("correction: %v", r)
			}
		}
	}
	if !found {
		t.Fatalf("no correction record: %s", logs.String())
	}
}

// The first correction carries the worker's subject. Corrections that follow come
// from gate reds, and the handoff that exhausts them must not inherit it.
func TestGateRedHandoffDoesNotInheritWorkerSubject(t *testing.T) {
	e, o, tools, _, _, ticket := recoveryFixture(t)
	var logs bytes.Buffer
	e.logContext = logx.New("executor", &logs)
	o.failFirst = true
	o.failSubject = "worker subject"
	tools.gateReds = 5
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerResolver {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var got []any
	for _, r := range transitionLogRecords(t, &logs) {
		if r["msg"] == "correction" || r["msg"] == "resolver handoff" {
			got = append(got, r["msg"], r["worker_subject"])
		}
	}
	want := []any{"correction", "worker subject", "correction", nil, "correction", nil, "resolver handoff", nil}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("records=%v want=%v", got, want)
	}
}

func TestScopeBlockedHandoffLogsSubjectAndFiles(t *testing.T) {
	e, o, _, _, _, ticket := recoveryFixture(t)
	var logs bytes.Buffer
	e.logContext = logx.New("executor", &logs)
	o.failFirst = true
	o.failSubject = "ZY-333 blocked"
	o.failBody = "SCOPE-BLOCKED: b.go\n"
	result, err := e.Execute(context.Background(), ticket)
	if err != nil || result.State.Owner != machine.OwnerResolver {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	found := false
	for _, r := range transitionLogRecords(t, &logs) {
		if r["msg"] == "resolver handoff" {
			found = true
			evidence, _ := r["evidence"].(string)
			if r["worker_subject"] != "ZY-333 blocked" || r["reason"] != result.State.HumanReason || !strings.Contains(evidence, "b.go") {
				t.Fatalf("handoff: %v", r)
			}
		}
	}
	if !found {
		t.Fatalf("no handoff record: %s", logs.String())
	}
}

func TestWorkerSubjectIsOneBoundedLine(t *testing.T) {
	if got := workerSubject(&orca.WorkerReport{Subject: "  first\nsecond"}); got != "first" {
		t.Fatalf("got %q", got)
	}
	if got := workerSubject(&orca.WorkerReport{Subject: strings.Repeat("长", 500)}); utf8.RuneCountInString(got) != 200 {
		t.Fatalf("runes=%d", utf8.RuneCountInString(got))
	}
	for _, subject := range []string{"leak ghp_" + strings.Repeat("a", 36), "push https://x:ghs_abcdef@github.com/a failed", "Bearer abc", "github_pat_abc"} {
		got := workerSubject(&orca.WorkerReport{Subject: subject})
		for _, secret := range []string{"ghp_", "ghs_abcdef", "Bearer abc", "github_pat_abc"} {
			if strings.Contains(got, secret) {
				t.Fatalf("credential in %q", got)
			}
		}
	}
}

func TestWorkerSubjectControlCharacterCannotHideCredential(t *testing.T) {
	for _, tc := range []struct{ subject, secret string }{
		{"gh\ap_abcdefgh", "ghp_abcdefgh"},
		{"s\x1bk-abcdefgh", "sk-abcdefgh"},
		{"Bea\arer abcdef", "abcdef"},
		{"https\a://x:pw@github.com/a", "x:pw"},
		{"see\tghp_abcdefgh", "ghp_abcdefgh"},
		{"see\x1bghp_abcdefgh", "ghp_abcdefgh"},
		{"use\tsk-abcdefgh", "sk-abcdefgh"},
		{"Bearer\tabcdef", "abcdef"},
		{"push --token\tsecret123", "secret123"},
	} {
		if got := workerSubject(&orca.WorkerReport{Subject: tc.subject}); strings.Contains(got, tc.secret) {
			t.Fatalf("subject %q exposed %q", tc.subject, got)
		}
	}
}

func TestWorkerSubjectDropsControlCharacters(t *testing.T) {
	for _, tc := range []struct{ subject, want string }{
		{"\x1b[31mred\x1b[0m done", "red done"},
		{"osc\x1b]52;c;aGk=\a end", "osc end"},
		{"a\vb\fc\u0085d\x7fe\tf", "abcdef"},
		{"first\rsecond", "first"},
		{"\x1b\n", ""},
	} {
		if got := workerSubject(&orca.WorkerReport{Subject: tc.subject}); got != tc.want {
			t.Fatalf("subject %q: got %q want %q", tc.subject, got, tc.want)
		}
	}
}

func TestWorkerSubjectRedactsColoredToken(t *testing.T) {
	for _, subject := range []string{
		"gh\x1b[31mp_private\x1b[0m tail",
		"gh\x1b]0;title\ap_private tail",
		"gh\x1b]8;;https://example.com\x1b\\p_private tail",
	} {
		if got := workerSubject(&orca.WorkerReport{Subject: subject}); got != "[REDACTED] tail" {
			t.Fatalf("subject %q: got %q", subject, got)
		}
	}
}
