package intake

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Project is one project this gateway can route a thread to: a row of the
// Feishu ledger's 项目 table, joined with whether this machine has a local
// repository checkout for its remote. Repo is empty when no configured
// repository matches Remote. Checkout, Ledger and the space-separated lane
// globs serve draft validation and submission (OF-15); a nil Ledger leaves
// drafts as plain replies. Reason, when non-empty, is why the project
// discovery package (OF-42) would not let this project take over right now
// (e.g. "check 失败"); it overrides the generic unavailable-project reply.
type Project struct {
	Prefix, Name, Remote, Repo string
	Enabled                    bool
	Checkout                   string
	Ledger                     Ledger
	Lane2Globs, Lane3Globs     string
	Reason                     string
}

func (p Project) usable() bool { return p.Enabled && p.Repo != "" }

var (
	markerPrefix = regexp.MustCompile(`^\s*#([A-Z][A-Z0-9]*)(?:\s|$)`)
	markerName   = regexp.MustCompile(`^\s*项目:(\S+)(?:\s|$)`)
)

// parseProjectMarker extracts an explicit project selection from a thread's
// first message: a leading #<编号前缀> or 项目:<名称>, so a ticket reference
// such as #OF-17 inside the text never selects a project. rest is prompt with the marker
// removed; prefix and name are both "" when the message carries no marker.
func parseProjectMarker(prompt string) (prefix, name, rest string) {
	if m := markerPrefix.FindStringSubmatchIndex(prompt); m != nil {
		return prompt[m[2]:m[3]], "", strings.TrimSpace(prompt[:m[0]] + prompt[m[1]:])
	}
	if m := markerName.FindStringSubmatchIndex(prompt); m != nil {
		return "", prompt[m[2]:m[3]], strings.TrimSpace(prompt[:m[0]] + prompt[m[1]:])
	}
	return "", "", prompt
}

const (
	unknownProjectReply = "未识别该项目，请回复序号或编号前缀重新选择"
)

func unavailableProjectReply(p Project) string {
	if p.Reason != "" {
		return fmt.Sprintf("项目 %s 不可用：%s", p.Prefix, p.Reason)
	}
	if !p.Enabled {
		return fmt.Sprintf("项目 %s 未启用，无法使用", p.Prefix)
	}
	return fmt.Sprintf("项目 %s 本机未配置仓库，无法使用", p.Prefix)
}

// enabledProjects is every configured project usable right now, in
// declaration order: the stable order the picker's indices refer to.
func (g *Gateway) enabledProjects() []Project {
	var out []Project
	for _, p := range g.projectsSnapshot() {
		if p.usable() {
			out = append(out, p)
		}
	}
	return out
}

func (g *Gateway) projectListReply() string {
	var b strings.Builder
	b.WriteString("请回复序号或编号前缀选择项目：\n")
	for i, p := range g.enabledProjects() {
		fmt.Fprintf(&b, "%d. %s %s\n", i+1, p.Prefix, p.Name)
	}
	return strings.TrimRight(b.String(), "\n")
}

// findProject looks up a project by its marker-form identity (prefix or
// name), including disabled or locally unmatched ones, so the caller can
// give a specific rejection reason.
func (g *Gateway) findProject(prefix, name string) (Project, bool) {
	for _, p := range g.projectsSnapshot() {
		if (prefix != "" && p.Prefix == prefix) || (name != "" && p.Name == name) {
			return p, true
		}
	}
	return Project{}, false
}

// project resolves a session's stored prefix to its full Project, for the
// Turn passed to agent. It returns the zero Project when prefix is "" (a
// gateway with no projects configured) or unknown.
func (g *Gateway) project(prefix string) Project {
	p, _ := g.findProject(prefix, "")
	return p
}

func (g *Gateway) projectByRemote(remote string) (Project, bool) {
	for _, p := range g.projectsSnapshot() {
		if p.Remote == remote {
			return p, true
		}
	}
	return Project{}, false
}

// findProjectChoice resolves the requester's reply to the pending project
// list: a 1-based index into enabledProjects, or a prefix.
func (g *Gateway) findProjectChoice(input string) (Project, bool) {
	enabled := g.enabledProjects()
	if n, err := strconv.Atoi(input); err == nil {
		if n >= 1 && n <= len(enabled) {
			return enabled[n-1], true
		}
		return Project{}, false
	}
	for _, p := range enabled {
		if p.Prefix == input {
			return p, true
		}
	}
	return Project{}, false
}

// selectProject resolves a thread's project before any agent turn runs
// (OF-32): an explicit marker in the first message, the configured group
// default, or — failing both — the enabled project list, held pending until
// the requester picks one. The chosen project is written into the session
// and never changes again in this thread.
func (g *Gateway) selectProject(ctx context.Context, id, thread, path string, session Session, resume bool, sender, prompt string) error {
	if !resume {
		if prefix, name, rest := parseProjectMarker(prompt); prefix != "" || name != "" {
			p, ok := g.findProject(prefix, name)
			if !ok {
				return g.reply(ctx, id, unknownProjectReply)
			}
			if !p.usable() {
				return g.reply(ctx, id, unavailableProjectReply(p))
			}
			return g.beginProject(ctx, id, thread, path, session, sender, p, rest)
		}
		if g.defaultRemote != "" {
			if p, ok := g.projectByRemote(g.defaultRemote); ok && p.usable() {
				return g.beginProject(ctx, id, thread, path, session, sender, p, prompt)
			}
		}
		session.PendingPrompt = prompt
		session.State, session.LastMessage = Chatting, g.now()
		if err := writeJSON(path, session); err != nil {
			return err
		}
		return g.reply(ctx, id, g.projectListReply())
	}
	p, ok := g.findProjectChoice(strings.TrimSpace(prompt))
	if !ok {
		return g.reply(ctx, id, unknownProjectReply)
	}
	if !p.usable() {
		return g.reply(ctx, id, unavailableProjectReply(p))
	}
	return g.beginProject(ctx, id, thread, path, session, sender, p, session.PendingPrompt)
}

func (g *Gateway) beginProject(ctx context.Context, id, thread, path string, session Session, sender string, p Project, prompt string) error {
	session.Project = p.Prefix
	session.PendingPrompt = ""
	return g.respond(ctx, id, thread, path, session, false, sender, prompt)
}
