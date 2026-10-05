package executor

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FollowupLinear is optional. ListFollowupIssues must exhaust pagination across
// projects, including completed/canceled issues, for restart-safe quota counts.
// Scope comes from the issue's SCOPE section; Machine comes from the persisted
// followup-machine marker. StateType is Linear's type, not its display name.
// Writes must not be retried automatically after an ambiguous response.
type FollowupLinear interface {
	FollowupProject(context.Context, string) (string, error)
	ListFollowupIssues(context.Context) ([]FollowupIssue, error)
	CreateFollowup(context.Context, FollowupCreate) (string, error)
	RelateFollowup(context.Context, string, string) error
}
type FollowupIssue struct {
	ID, Project, Body, Scope, StateType, Machine string
	CreatedAt                                    time.Time
}
type FollowupCreate struct {
	Title, Body, Scope, Project, State, Machine string
	Labels                                      []string
	Estimate                                    int
}

// FollowupTools separates main-tree inspection and CHECK execution from policy.
// ValidateFollowupScope returns a fixed rejection reason or an infrastructure error.
type FollowupTools interface {
	ValidateFollowupScope(context.Context, string, string, []string) (string, error)
	FollowupCheckFails(context.Context, string, string) (bool, error)
}
type followupDraft struct{ Title, Evidence, Todo, Check, Scope, Estimate string }

func parseFollowupDrafts(body string) []followupDraft {
	var drafts []followupDraft
	active := false
	fenced := false
	for _, line := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if trim == "## 后续工作" {
			active = true
			continue
		}
		if strings.HasPrefix(trim, "## ") || strings.HasPrefix(trim, "# ") {
			active = false
		}
		if !active {
			continue
		}
		if strings.HasPrefix(line, "- 标题：") {
			drafts = append(drafts, followupDraft{Title: strings.TrimSpace(strings.TrimPrefix(line, "- 标题："))})
			continue
		}
		if len(drafts) == 0 || line == strings.TrimLeft(line, " \t") {
			continue
		}
		key, value, ok := strings.Cut(trim, "：")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		d := &drafts[len(drafts)-1]
		switch key {
		case "证据":
			d.Evidence = value
		case "要做":
			d.Todo = value
		case "CHECK":
			d.Check = value
		case "SCOPE":
			d.Scope = value
		case "estimate":
			d.Estimate = value
		}
	}
	return drafts
}

var followupEvidence = regexp.MustCompile(`(?:[A-Za-z0-9_./-]+:[0-9]+\b|\b(?:Test|Benchmark|Example)[A-Za-z0-9_]+\b|\b[a-zA-Z][a-zA-Z0-9_]*(?:[-_][a-zA-Z0-9_]+)+\b)`)

func (d followupDraft) invalid() string {
	if d.Title == "" || d.Evidence == "" || d.Todo == "" || d.Check == "" || d.Scope == "" || d.Estimate == "" {
		return "字段不齐全"
	}
	if !followupEvidence.MatchString(d.Evidence) {
		return "证据缺少文件:行或检查名"
	}
	if d.Estimate != "1" && d.Estimate != "2" && d.Estimate != "3" {
		return "estimate 必须为 1–3 整数"
	}
	return ""
}
func followupScope(scope string) string {
	fields := strings.Fields(scope)
	sort.Strings(fields)
	return strings.Join(fields, " ")
}
func (e *Executor) processFollowups(ctx context.Context, t Ticket, body string) {
	if e.Followups == nil || e.FollowupTools == nil {
		return
	}
	drafts := parseFollowupDrafts(body)
	if len(drafts) == 0 {
		return
	}
	comment := func(reason string) {
		if e.Linear != nil {
			_, _ = e.Linear.CreateComment(ctx, t.ID, "后续工作丢弃: "+reason)
		}
	}
	project, err := e.Followups.FollowupProject(ctx, t.ID)
	if err != nil || project == "" {
		comment("来源项目查询失败")
		return
	}
	host := e.Config.Machine
	if host == "" {
		host, err = os.Hostname()
		if err != nil || host == "" {
			comment("机器标识不可用")
			return
		}
	}
	if strings.ContainsAny(host, "\r\n<>") {
		comment("机器标识无效")
		return
	}
	now := time.Now()
	if e.Now != nil {
		now = e.Now()
	}
	// UTC days make counts stable across executor restarts and timezone changes.
	now = now.UTC()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	source := "来源: " + t.Identifier + " 的后续工作"
	for _, d := range drafts {
		if reason := d.invalid(); reason != "" {
			comment(reason)
			continue
		}
		issues, err := e.Followups.ListFollowupIssues(ctx)
		if err != nil {
			comment("Linear 票据查询失败")
			return
		}
		sourceCount, daily := 0, 0
		duplicate := false
		for _, issue := range issues {
			first, _, _ := strings.Cut(issue.Body, "\n")
			if first == source {
				sourceCount++
			}
			if issue.Machine == host && strings.Contains(issue.Body, "来源:") && !issue.CreatedAt.Before(start) && issue.CreatedAt.Before(start.Add(24*time.Hour)) {
				daily++
			}
			if issue.Project == project && issue.StateType != "completed" && issue.StateType != "canceled" && followupScope(issue.Scope) == followupScope(d.Scope) {
				duplicate = true
			}
		}
		if sourceCount >= 2 {
			comment("来源票限额 2 张")
			continue
		}
		if daily >= 10 {
			comment("机器每日限额 10 张")
			continue
		}
		if duplicate {
			comment("同项目存在相同 SCOPE 的未完成票")
			continue
		}
		reason, err := e.FollowupTools.ValidateFollowupScope(ctx, e.Config.Repo, d.Scope, e.Config.Lane3Globs)
		if err != nil {
			comment("SCOPE 校验失败")
			continue
		}
		if reason != "" {
			comment(reason)
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		failed, err := e.FollowupTools.FollowupCheckFails(checkCtx, e.Config.Repo, d.Check)
		expired := checkCtx.Err() != nil
		cancel()
		if err != nil || expired || !failed {
			comment("CHECK 未在 5 分钟内非零退出")
			continue
		}
		estimate, _ := strconv.Atoi(d.Estimate)
		description := fmt.Sprintf("%s\n\n## 现状/证据\n%s\n\n## 要做\n%s\n\n## 验收\nCHECK: %s\n\n## SCOPE\n%s\n\n<!-- followup-machine: %s -->", source, d.Evidence, d.Todo, d.Check, d.Scope, host)
		id, err := e.Followups.CreateFollowup(ctx, FollowupCreate{Title: d.Title, Body: description, Scope: d.Scope, Project: project, State: "Todo", Labels: []string{"ready-for-agent"}, Estimate: estimate, Machine: host})
		if err != nil || id == "" {
			comment("Linear 建票失败，需核对后重试")
			return
		}
		if err = e.Followups.RelateFollowup(ctx, t.ID, id); err != nil {
			comment("Linear related 关系创建失败，票已创建")
			return
		}
	}
}
