package resolver

import (
	"os"
	"strings"
	"testing"
)

func TestRecordedTopRuling(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/recorded/resolver-top-ruling.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseResult(string(raw))
	if err != nil || p.Action != "ruling" || strings.TrimSpace(p.Reason) == "" || strings.ContainsAny(p.Reason, "\r\n") {
		t.Fatalf("recorded ruling rejected: %+v %v", p, err)
	}
}

func TestRecordedEscalateBlockClass(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/recorded/resolver-escalate-credentials.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseResult(string(raw))
	if err != nil || p.Action != "escalate" || p.Class != "credentials" {
		t.Fatalf("recorded block class rejected: %+v %v", p, err)
	}
}

func TestResolutionRejectsUnstructuredOrUnknownDecision(t *testing.T) {
	for _, body := range []string{`done`, `{"action":"done","reason":"looks good"}`, `{"action":"merge","reason":"checked"}`, `{"action":"retry-smoke","queue":"q","sha":"a","reason":"checked"}`} {
		if _, err := ParseResult(body); err == nil {
			t.Fatalf("accepted invalid proposal: %s", body)
		}
	}
}
func TestResolutionEscalateRequiresReasonAndEvidence(t *testing.T) {
	p, err := ParseResult(`{"action":"escalate","reason":"PATH 缺少 PostgreSQL","body":"make test 需要 pg_ctl"}`)
	if err != nil || p.Action != "escalate" || p.Body != "make test 需要 pg_ctl" {
		t.Fatalf("escalation rejected: %+v %v", p, err)
	}
	if _, err := ParseResult(`{"action":"escalate","reason":"blocked"}`); err == nil {
		t.Fatal("accepted escalation without evidence")
	}
}
func TestResolutionAcceptsPinnedDeploymentProposal(t *testing.T) {
	p, err := ParseResult(`{"action":"retry-smoke","queue":"q","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"verified previous side effects"}`)
	if err != nil || p.Queue != "q" {
		t.Fatalf("pinned decision rejected: %+v %v", p, err)
	}
}

// resolver-escalate-scope.json is stdout of the real resolver model given
// core/roles/resolver.md and a written scenario.
func TestHumanTicketEscalateCarriesScope(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/recorded/resolver-escalate-scope.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseResult(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.ScopeRequest(); len(got) != 1 || got[0] != "scripts/module-size-metric-report.test.mjs" {
		t.Fatalf("scope=%v class=%s", got, p.Class)
	}
	p.Class = "brief-interpretation"
	if got := p.ScopeRequest(); got != nil {
		t.Fatalf("other class exposed scope %v", got)
	}
}

func TestResolutionAcceptsLandedDoneProposal(t *testing.T) {
	p, err := ParseResult(`{"action":"landed-done","pr":133,"sha":"0ab0047178cd32132001eef0682d50a77a6f2622","reason":"merged and verified"}`)
	if err != nil || p.Action != "landed-done" || p.PR != 133 || p.SHA != "0ab0047178cd32132001eef0682d50a77a6f2622" {
		t.Fatalf("landed-done rejected: %+v %v", p, err)
	}
}

func TestResolutionRejectsMalformedLandedDoneProposal(t *testing.T) {
	for _, body := range []string{
		`{"action":"landed-done","pr":0,"sha":"0ab0047178cd32132001eef0682d50a77a6f2622","reason":"checked"}`,
		`{"action":"landed-done","pr":133,"sha":"bad","reason":"checked"}`,
	} {
		if _, err := ParseResult(body); err == nil {
			t.Fatalf("accepted malformed landed-done: %s", body)
		}
	}
}
