package issues

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

var behaviorTracePath = regexp.MustCompile(`(?:[\pL\pN_.-]+/)+[\pL\pN_.-]+|[\pL\pN_.-]*\.[\pL\pN_-]+|\b(?:Makefile|Dockerfile)\b`)

func requireBehaviorTraceInScope(ctx context.Context, description string) error {
	var paths []string
	selfCheck := false
	for _, line := range strings.Split(strings.ReplaceAll(description, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if heading, ok := strings.CutPrefix(line, "## "); ok {
			selfCheck = strings.TrimSpace(heading) == "intake 自检"
			continue
		}
		trace, ok := strings.CutPrefix(line, "- 行为追踪")
		if !selfCheck || !ok {
			continue
		}
		for _, path := range behaviorTracePath.FindAllString(trace, -1) {
			if !slices.Contains(paths, path) {
				paths = append(paths, path)
			}
		}
	}
	if len(paths) == 0 {
		return nil
	}
	brief, _, err := parseFeishuDescription(description)
	if err != nil {
		return err
	}
	args := append([]string{"-c", `set -f
scope=$1
shift
for file do
  matched=0
  for pattern in $scope; do
    if [[ "$file" == $pattern ]]; then matched=1; break; fi
  done
  if [[ "$matched" == 0 ]]; then printf '%s\n' "$file"; fi
done`, "behavior-trace-scope", brief.Scope}, paths...)
	missing, err := exec.CommandContext(ctx, "bash", args...).Output()
	if err != nil {
		return fmt.Errorf("match behavior trace against SCOPE: %w", err)
	}
	if len(missing) > 0 {
		return fmt.Errorf("行为追踪 paths outside SCOPE: %s", strings.Join(strings.Fields(string(missing)), ", "))
	}
	return nil
}
