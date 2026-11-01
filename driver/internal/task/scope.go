package task

import (
	"path"
	"strings"
)

// ScopeMatch reports whether value matches one gate SCOPE pattern. Shell globs
// cross directory boundaries; path.Match otherwise uses the desired *, ?, bracket
// class and escape rules, so slash is hidden from its separator.
func ScopeMatch(pattern, value string) bool {
	matched, err := path.Match(strings.ReplaceAll(pattern, "/", "\x00"), strings.ReplaceAll(value, "/", "\x00"))
	return err == nil && matched
}
