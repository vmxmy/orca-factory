package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
	"github.com/vmxmy/orca-factory/driver/internal/task"
)

// ScopeLedger is the ledger access needed to approve a scope extension.
type ScopeLedger interface {
	Read(context.Context, string) (issues.Task, error)
	ListComments(context.Context, string) ([]task.Comment, error)
	Edit(context.Context, string, []byte) (issues.Task, error)
	CreateComment(context.Context, string, string) (task.Comment, error)
}

// AutoApproveScope rechecks the current human request before using the same
// scope write as intake. A nonempty refusal leaves the request for a human.
// An empty refusal also covers a stale snapshot whose request already settled.
func AutoApproveScope(ctx context.Context, ledger ScopeLedger, id string, attempt int, lane3Globs string, now time.Time) (refusal string, err error) {
	ticket, err := ledger.Read(ctx, id)
	if err != nil {
		return "", err
	}
	comments, err := ledger.ListComments(ctx, ticket.Metadata.ID)
	if err != nil {
		return "", err
	}
	latest := latestHumanEvidence(comments)
	var record resolver.Record
	if json.Unmarshal([]byte(latest[resolver.Prefix]), &record) != nil || record.Version != 1 || record.Attempt < 1 || record.Phase == "" {
		return "resolver-state 证据无效", nil
	}
	if record.Attempt != attempt || record.Phase != "human" {
		return "", nil
	}
	if ticket.Metadata.State == "Canceled" {
		return "票已取消", nil
	}
	if refusal := humanReleaseRefusal(id, ticket.Description, latest); refusal != "" {
		return refusal, nil
	}
	paths, refusal := scopeAddition(review{ticket: id}, record, lane3Globs)
	if refusal != "" {
		return refusal, nil
	}
	if len(paths) > 10 {
		return "申请超过 10 个路径", nil
	}
	for _, path := range paths {
		for _, glob := range []string{".github", ".github/*", "core/gate.sh", "scripts/agent-gates/gate*.sh", "scripts/agent-gates/agent-graph.env", "driver/deploy", "driver/deploy/*", "Makefile", ".env*", "*/.env*", "*secrets*"} {
			if task.ScopeMatch(glob, path) {
				return "路径 " + path + " 属于流水线控制文件", nil
			}
		}
	}
	approvals := 0
	for _, comment := range comments {
		if strings.HasPrefix(comment.Body, "扩SCOPE：auto ") {
			approvals++
		}
	}
	if approvals >= 2 {
		return "每张票自动批准已达 2 次", nil
	}
	conditions := fmt.Sprintf("AGENT_AUTO_REVIEW=scope；字面路径校验通过；%d 个路径不超过 10；不命中 lane 3 或控制文件；本票第 %d 次自动批准，不超过 2 次", len(paths), approvals+1)
	body := fmt.Sprintf("扩SCOPE：auto %s\n条件：%s", now.Format(time.RFC3339), conditions)
	return applyScopeReview(ctx, ledger, ticket, record, paths, body)
}

// applyScopeReview edits the face before writing the waiting receipt. If the
// comment write fails, retrying the request does not add duplicate paths.
func applyScopeReview(ctx context.Context, ledger ScopeLedger, ticket issues.Task, record resolver.Record, paths []string, body string) (string, error) {
	face, changed, found := withScope(ticket.Description, paths)
	if !found {
		return "票面缺少 SCOPE，不放行", nil
	}
	if changed {
		patch, err := json.Marshal(map[string]string{"description": face})
		if err != nil {
			return "", err
		}
		if _, err := ledger.Edit(ctx, ticket.Metadata.ID, patch); err != nil {
			return "", err
		}
	}
	body += "\n路径：" + strings.Join(paths, " ")
	body += fmt.Sprintf("\n%s{\"version\":1,\"attempt\":%d,\"phase\":\"waiting\"}", resolver.Prefix, record.Attempt)
	_, err := ledger.CreateComment(ctx, ticket.Metadata.ID, body)
	return "", err
}
