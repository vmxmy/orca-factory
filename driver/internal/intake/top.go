package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/issues"
	"github.com/vmxmy/orca-factory/driver/internal/resolver"
)

// TopMustEscalate is the final resolver's non-delegable decision boundary.
const TopMustEscalate = "lane 3；凭证、环境或授权类阻塞；不可逆操作；生产 DDL 与数据；部署 paused；证据冲突；终审自己的第二次失败"

const topForbiddenPhrases = "lane 3|lane=3|凭证|环境|授权|不可逆|生产 ddl|数据|paused|证据冲突|第二次失败|credential|environment|authorization|irreversible|production ddl|production data|evidence conflict"

// ApplyTopReview uses the human ruling and scope writers after re-reading the
// exact final-review result. A stale result never grants authority.
func ApplyTopReview(ctx context.Context, ledger ScopeLedger, id string, attempt int, lane3Globs string, now time.Time) (string, error) {
	ticket, err := ledger.Read(ctx, id)
	if err != nil {
		return "", err
	}
	comments, err := ledger.ListComments(ctx, id)
	if err != nil {
		return "", err
	}
	latest := latestHumanEvidence(comments)
	var record resolver.Record
	if json.Unmarshal([]byte(latest[resolver.Prefix]), &record) != nil || record.Version != 1 {
		return "resolver-state 证据冲突", nil
	}
	if record.Attempt != attempt || record.Phase != "result" {
		return "", nil
	}
	if !record.Top {
		return "不是终审提案", nil
	}
	if ticket.Metadata.State == "Canceled" {
		return "票已取消", nil
	}
	if refusal := humanReleaseRefusal(id, ticket.Description, latest); refusal != "" {
		return refusal, nil
	}
	decision, err := resolver.ParseResult(record.Result)
	if err != nil {
		return "终审提案无效", nil
	}
	text := strings.ToLower(ticket.Description + "\n" + record.TopReason + "\n" + decision.Reason + "\n" + decision.Body)
	for _, phrase := range strings.Split(topForbiddenPhrases, "|") {
		if strings.Contains(text, phrase) {
			return "必须转人：" + phrase, nil
		}
	}
	if decision.Action == "escalate" && len(decision.ScopeRequest()) > 0 {
		return autoApproveScope(ctx, ledger, id, attempt, lane3Globs, now, true)
	}
	if decision.Action != "ruling" || record.Outcome != "succeeded" {
		return "不是成功的终审 ruling", nil
	}
	return "", applyRuling(ctx, ledger, ticket, record, "top-resolver", decision.Reason, now)
}

func applyRuling(ctx context.Context, ledger ScopeLedger, ticket issues.Task, record resolver.Record, sender, reason string, now time.Time) error {
	body := fmt.Sprintf("裁定：%s %s\n理由：%s\n%s{\"version\":1,\"attempt\":%d,\"phase\":\"waiting\"}", sender, now.Format(time.RFC3339), strings.Join(strings.Fields(reason), " "), resolver.Prefix, record.Attempt)
	_, err := ledger.CreateComment(ctx, ticket.Metadata.ID, body)
	return err
}
