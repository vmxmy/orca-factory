package projects

import (
	"slices"
	"strconv"
	"strings"
)

// OnboardPrefix proposes the ledger 编号前缀 for a repository being onboarded
// (OF-50) from its checkout directory name: the uppercased initials of its
// ASCII-alphanumeric words, or the first two letters of a one-word name, led
// by "P" when it would otherwise start with a digit (a prefix must match
// [A-Z][A-Z0-9]*). A prefix in taken gets the smallest numeric suffix from 2
// that is free.
func OnboardPrefix(name string, taken []string) string {
	words := strings.FieldsFunc(strings.ToUpper(name), func(r rune) bool {
		return (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	})
	var base string
	switch len(words) {
	case 0:
		base = "P"
	case 1:
		base = words[0][:min(2, len(words[0]))]
	default:
		for _, w := range words {
			base += w[:1]
		}
	}
	if base[0] < 'A' {
		base = "P" + base
	}
	prefix := base
	for n := 2; slices.Contains(taken, prefix); n++ {
		prefix = base + strconv.Itoa(n)
	}
	return prefix
}
