package executor

import "testing"

func TestReviewVerdictFencedJSON(t *testing.T) {
	fenced := "notes\n```json\n{\"verdict\":\"green\",\"step\":\"review\",\"evidence\":[]}\n```\n"
	if v, err := reviewVerdict(fenced); err != nil || v != "green" {
		t.Fatalf("fenced verdict=%q err=%v", v, err)
	}
	trailingProse := "{\"verdict\":\"green\",\"step\":\"review\"}\nactually there is a problem"
	if v, err := reviewVerdict(trailingProse); err == nil {
		t.Fatalf("verdict followed by prose must not count, got %q", v)
	}
}
