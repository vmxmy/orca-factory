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

// AutoReleaseHuman rechecks a human transfer and writes the same release
// receipt as an approver when the release review is safe and has not already
// happened for this ticket. A stale resolver state is left for the next scan.
func AutoReleaseHuman(ctx context.Context, ledger ScopeLedger, id string, attempt int, reason string, now time.Time) (released bool, refusal string, err error) {
	ticket, err := ledger.Read(ctx, id)
	if err != nil {
		return false, "", err
	}
	comments, err := ledger.ListComments(ctx, ticket.Metadata.ID)
	if err != nil {
		return false, "", err
	}
	latest := latestHumanEvidence(comments)
	raw, ok := latest[resolver.Prefix]
	var record resolver.Record
	if !ok || json.Unmarshal([]byte(raw), &record) != nil || record.Version != 1 || record.Attempt < 1 || record.Phase == "" {
		return false, "resolver-state 证据无效", nil
	}
	if record.Attempt != attempt || record.Phase != "human" {
		return false, "", nil
	}
	if ticket.Metadata.State == "Canceled" {
		return false, "票已取消", nil
	}
	if refusal := humanReleaseRefusal(id, ticket.Description, latest); refusal != "" {
		return false, refusal, nil
	}
	// The notice reason is the ledger line, cut at 300 runes. Only an escalation
	// keeps the full reason, in the resolver result.
	reasons := []string{reason}
	decision, parseErr := resolver.ParseResult(record.Result)
	switch {
	case parseErr == nil && decision.Action == "escalate":
		reasons = append(reasons, decision.Reason)
	case strings.TrimSpace(reason) == "":
		return false, "转人原因证据无效", nil
	case strings.HasSuffix(reason, "…"):
		return false, "转人原因已截断，无法核对完整原因", nil
	}
	for _, r := range reasons {
		if refusal := autoReleaseReasonRefusal(r); refusal != "" {
			return false, refusal, nil
		}
	}
	for _, comment := range comments {
		if strings.HasPrefix(comment.Body, "放行：") {
			return false, "本票此前已有放行记录", nil
		}
	}
	if err := applyHumanRelease(ctx, ledger, ticket, record, "auto", now); err != nil {
		return false, "", err
	}
	return true, "", nil
}

// AutoApproveScope rechecks the current human request before using the same
// scope write as intake. A nonempty refusal leaves the request for a human.
// An empty refusal also covers a stale snapshot whose request already settled.
func AutoApproveScope(ctx context.Context, ledger ScopeLedger, id string, attempt int, lane3Globs string, now time.Time) (refusal string, err error) {
	return autoApproveScope(ctx, ledger, id, attempt, lane3Globs, now, false)
}

func autoApproveScope(ctx context.Context, ledger ScopeLedger, id string, attempt int, lane3Globs string, now time.Time, top bool) (refusal string, err error) {
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
	phase, sender, flag := "human", "auto", "scope"
	if top {
		phase, sender, flag = "result", "top-resolver", "top"
	}
	if top && !record.Top {
		return "不是终审提案", nil
	}
	if record.Attempt != attempt || record.Phase != phase {
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
		if strings.HasPrefix(comment.Body, "扩SCOPE：auto ") || strings.HasPrefix(comment.Body, "扩SCOPE：top-resolver ") {
			approvals++
		}
	}
	if approvals >= 2 {
		return "每张票自动批准已达 2 次", nil
	}
	conditions := fmt.Sprintf("AGENT_AUTO_REVIEW=%s；字面路径校验通过；%d 个路径不超过 10；不命中 lane 3 或控制文件；本票第 %d 次自动批准，不超过 2 次", flag, len(paths), approvals+1)
	body := fmt.Sprintf("扩SCOPE：%s %s\n条件：%s", sender, now.Format(time.RFC3339), conditions)
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
