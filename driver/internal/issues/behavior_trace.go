package issues

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

var behaviorTracePath = regexp.MustCompile(`(?:[\pL\pN_@.-]+/)+[\pL\pN_@.-]+|[\pL\pN_@.-]*\.[\pL\pN_@-]+|\b(?:Makefile|Dockerfile)\b`)

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
	missing, err := pathsOutsideScope(ctx, brief.Scope, paths)
	if err != nil {
		return fmt.Errorf("match behavior trace against SCOPE: %w", err)
	}
	if len(missing) > 0 {
		return fmt.Errorf("行为追踪 paths outside SCOPE: %s", strings.Join(missing, ", "))
	}
	return nil
}

const recordedSampleDir = "driver/testdata/recorded/"

var recordedSampleReference = regexp.MustCompile(`testdata/recorded|recorded/`)

func requireRecordedSampleScope(ctx context.Context, description string) error {
	if selfCheckLine(description, "- 外部契约样本") == "已含 `*_test.go` 与 `testdata/` 路径" {
		return nil
	}
	brief, _, err := parseFeishuDescription(description)
	if err != nil {
		return err
	}
	if !recordedSampleReference.MatchString(brief.Check) && !recordedSampleReference.MatchString(brief.Todo) {
		return nil
	}
	for _, pattern := range strings.Fields(brief.Scope) {
		if strings.HasPrefix(pattern, recordedSampleDir) {
			return nil
		}
	}
	missing, err := pathsOutsideScope(ctx, brief.Scope, []string{recordedSampleDir + "sample"})
	if err != nil {
		return fmt.Errorf("match recorded sample directory against SCOPE: %w", err)
	}
	if len(missing) > 0 {
		return fmt.Errorf("CHECK 或「要做」读取录制样本，SCOPE 缺 %s 下的路径；把 `%s*` 或具体样本文件加进 SCOPE", recordedSampleDir, recordedSampleDir)
	}
	return nil
}

func selfCheckLine(description, prefix string) string {
	selfCheck := false
	for _, line := range strings.Split(strings.ReplaceAll(description, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if heading, ok := strings.CutPrefix(line, "## "); ok {
			selfCheck = strings.TrimSpace(heading) == "intake 自检"
			continue
		}
		if rest, ok := strings.CutPrefix(line, prefix); selfCheck && ok {
			return strings.TrimSpace(strings.TrimLeft(rest, "：: "))
		}
	}
	return ""
}

func pathsOutsideScope(ctx context.Context, scope string, paths []string) ([]string, error) {
	args := append([]string{"-c", `set -f
scope=$1
shift
for file do
  matched=0
  for pattern in $scope; do
    if [[ "$file" == $pattern ]]; then matched=1; break; fi
  done
  if [[ "$matched" == 0 ]]; then printf '%s\n' "$file"; fi
done`, "scope-match", scope}, paths...)
	missing, err := exec.CommandContext(ctx, "bash", args...).Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(missing)), nil
}
