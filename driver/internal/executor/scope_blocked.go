package executor

import (
	"os"
	"path"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/task"
)

const scopeBlockedPrefix = "SCOPE-BLOCKED:"

// scopeBlockedFiles returns the files a failed worker declared it needs outside
// the gate spec's SCOPE. A declaration the spec cannot confirm is void as a whole,
// and the failure keeps its ordinary correction path.
func scopeBlockedFiles(body, specPath string) []string {
	var files []string
	for _, line := range strings.Split(body, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), scopeBlockedPrefix); ok {
			files = append(files, strings.Fields(rest)...)
		}
	}
	if len(files) == 0 {
		return nil
	}
	spec, err := os.ReadFile(specPath)
	if err != nil {
		return nil
	}
	scope := specScope(string(spec))
	if len(scope) == 0 {
		return nil
	}
	for _, file := range files {
		if !plainRepoFile(file) {
			return nil
		}
		for _, pattern := range scope {
			if task.ScopeMatch(pattern, file) {
				return nil
			}
		}
	}
	return files
}

func specScope(spec string) []string {
	for _, line := range strings.Split(spec, "\n") {
		if value, ok := strings.CutPrefix(line, "SCOPE:"); ok {
			return strings.Fields(value)
		}
	}
	return nil
}

// These paths reach the ledger, so they are held to the FAIL summary's alphabet.
// A colon would admit file:line, which names no file.
func plainRepoFile(file string) bool {
	return failIdentifier(file) == file && !strings.Contains(file, ":") && !path.IsAbs(file) && path.Clean(file) == file && file != "." && file != ".." && !strings.HasPrefix(file, "../")
}
