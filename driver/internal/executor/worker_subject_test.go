package executor

import (
	"bytes"
	"context"
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

func TestGateRedCorrectionLogHasNoSubject(t *testing.T) {
	e, _, tools, _, _, ticket := recoveryFixture(t)
	var logs bytes.Buffer
	e.logContext = logx.New("executor", &logs)
	tools.gateReds = 1
	if _, err := e.Execute(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	for _, r := range transitionLogRecords(t, &logs) {
		if _, ok := r["worker_subject"]; ok && r["msg"] == "correction" {
			t.Fatalf("gate correction carries subject: %v", r)
		}
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
	var logs bytes.Buffer
	logx.New("executor", &logs).Info("correction", "worker_subject", workerSubject(&orca.WorkerReport{Subject: "leak ghp_" + strings.Repeat("a", 36)}))
	if strings.Contains(logs.String(), "ghp_") {
		t.Fatalf("credential reached the log: %s", logs.String())
	}
}
