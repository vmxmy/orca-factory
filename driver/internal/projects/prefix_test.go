package projects

import "testing"

// TestOnboardPrefix covers OF-50's 编号前缀 rule: the initials of the
// checkout's directory name (the first two letters for a one-word name),
// uppercased, led by a letter, with a numeric suffix only when the ledger
// project table already uses that prefix.
func TestOnboardPrefix(t *testing.T) {
	for _, tc := range []struct {
		name  string
		taken []string
		want  string
	}{
		{"ziikoo-control-plane", []string{"OF", "ZY"}, "ZCP"},
		{"orca-factory", []string{"OF"}, "OF2"},
		{"orca-factory", []string{"OF", "OF2"}, "OF3"},
		{"ansible", nil, "AN"},
		{"9lives_app", nil, "P9A"},
	} {
		if got := OnboardPrefix(tc.name, tc.taken); got != tc.want {
			t.Errorf("OnboardPrefix(%q, %v)=%q, want %q", tc.name, tc.taken, got, tc.want)
		}
	}
}
