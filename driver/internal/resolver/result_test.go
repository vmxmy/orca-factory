package resolver

import "testing"

func TestResolutionRejectsUnstructuredOrUnknownDecision(t *testing.T) {
	for _, body := range []string{`done`, `{"action":"done","reason":"looks good"}`, `{"action":"merge","reason":"checked"}`, `{"action":"retry-smoke","queue":"q","sha":"a","reason":"checked"}`} {
		if _, err := ParseResult(body); err == nil {
			t.Fatalf("accepted invalid proposal: %s", body)
		}
	}
}
func TestResolutionAcceptsPinnedDeploymentProposal(t *testing.T) {
	p, err := ParseResult(`{"action":"retry-smoke","queue":"q","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"verified previous side effects"}`)
	if err != nil || p.Queue != "q" {
		t.Fatalf("pinned decision rejected: %+v %v", p, err)
	}
}
