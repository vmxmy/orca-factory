package executor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func writeRole(t *testing.T, repo, name, text string) {
	t.Helper()
	dir := filepath.Join(repo, "scripts", "agent-gates", "roles")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// withReviewerRole makes a faked `git worktree add` produce a checkout that
// carries reviewer.md, as the real head checkout does.
func withReviewerRole(t *testing.T, run func(context.Context, runx.Spec) ([]byte, error)) func(context.Context, runx.Spec) ([]byte, error) {
	return func(ctx context.Context, s runx.Spec) ([]byte, error) {
		if s.Path == "git" && len(s.Args) >= 4 && s.Args[0] == "worktree" && s.Args[1] == "add" {
			writeRole(t, s.Args[3], "reviewer.md", "REVIEWER ROLE\n")
		}
		return run(ctx, s)
	}
}

// Role rules live in roles/*.md so editing them takes effect without a driver
// release; Go only appends ticket data.
func TestRolePromptWorkerSpecPrefixesRolesBeforeBrief(t *testing.T) {
	repo := t.TempDir()
	if _, err := workerTaskSpec(repo, "BRIEF", nil); err == nil {
		t.Fatal("missing driver-worker.md must fail instead of dispatching an unbriefed worker")
	}
	writeRole(t, repo, "driver-worker.md", "WORKER ROLE\n")
	spec, err := workerTaskSpec(repo, "BRIEF", nil)
	if err != nil || spec != "WORKER ROLE\n\nBRIEF" {
		t.Fatalf("spec=%q err=%v", spec, err)
	}
	writeRole(t, repo, "local-worker.md", "LOCAL RULES\n")
	spec, err = workerTaskSpec(repo, "BRIEF", nil)
	if err != nil || spec != "WORKER ROLE\n\nLOCAL RULES\n\nBRIEF" {
		t.Fatalf("spec=%q err=%v", spec, err)
	}
	if strings.Contains(briefTemplate, "Constraints:") || strings.Contains(briefTemplate, "Delivery:") {
		t.Fatal("brief template must carry ticket data only; rules belong in driver-worker.md")
	}
}

func TestRolePromptCorrectionStartsWithRole(t *testing.T) {
	repo := t.TempDir()
	var prompt string
	c := &Commands{Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
		prompt = s.Args[1]
		return nil, nil
	}}
	req := CorrectionRequest{Repo: repo, Path: "/state/correction.md", Cause: "gate red", Brief: "BRIEF", Number: 1}
	if err := c.Correction(context.Background(), req); err == nil || prompt != "" {
		t.Fatalf("missing correction.md must fail before starting a session: err=%v prompt=%q", err, prompt)
	}
	writeRole(t, repo, "correction.md", "CORRECTION ROLE\n")
	if err := c.Correction(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(prompt, "CORRECTION ROLE\n") {
		t.Fatalf("prompt must start with correction.md: %q", prompt)
	}
	for _, want := range []string{`"/state/correction.md"`, "1/3", "gate red", "BRIEF"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q: %q", want, prompt)
		}
	}
}

func TestRolePromptReviewerRequired(t *testing.T) {
	c := &Commands{ReviewerCommand: "review", Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
		if s.Path != "git" {
			t.Fatal("reviewer must not start without reviewer.md")
		}
		return []byte("diff --git a/x b/x\n+added\n"), nil
	}}
	if _, _, err := c.Review(context.Background(), t.TempDir(), "base", "head", nil); err == nil {
		t.Fatal("missing reviewer.md must fail the review")
	}
}

func TestWorkerSkillsRejectInvalidNames(t *testing.T) {
	for _, value := range []string{"/tdd", "pkg.skill", "skill name", "skill;exit", "技能", "tdd,", ",tdd", "tdd,,review"} {
		t.Run(value, func(t *testing.T) {
			repo := workerSkillsRepository(t, "AGENT_WORKER_SKILLS='"+value+"'\n")
			_, _, err := repositoryExecutor(context.Background(), repo)
			if err == nil || !strings.Contains(err.Error(), "AGENT_WORKER_SKILLS") || !strings.Contains(err.Error(), value) {
				t.Fatalf("invalid value %q must fail startup with key and value: %v", value, err)
			}
		})
	}
}

func workerSkillsRepository(t *testing.T, extra string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENT_WORKER_SKILLS", "")
	repo := t.TempDir()
	writeRole(t, repo, "driver-worker.md", "WORKER ROLE\n")
	writeRole(t, repo, "local-worker.md", "LOCAL RULES\n")
	body := "AGENT_GH_REPO=owner/repo\nAGENT_ROLE_REVIEWER_CMD=review\nAGENT_TIER_DEFAULT=codex:model:high\nAGENT_TIER_ESCALATE=0\n" + extra
	if err := os.WriteFile(filepath.Join(repo, "scripts/agent-gates/agent-graph.env"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestWorkerSkillsPrompt(t *testing.T) {
	for _, tc := range []struct {
		name, env, want string
		skills          []string
	}{
		{name: "unset", want: "WORKER ROLE\n\nLOCAL RULES\n\nBRIEF"},
		{name: "empty", env: "AGENT_WORKER_SKILLS=''\n", want: "WORKER ROLE\n\nLOCAL RULES\n\nBRIEF"},
		{name: "single", env: "AGENT_WORKER_SKILLS=tdd\n", skills: []string{"tdd"}, want: "WORKER ROLE\n\nLOCAL RULES\n\n开工前用 Skill 工具依次加载：tdd。这些 skill 与本角色的限制冲突时，以本角色为准。\n\nBRIEF"},
		{name: "multiple", env: "AGENT_WORKER_SKILLS=tdd,pkg:review_2-A\n", skills: []string{"tdd", "pkg:review_2-A"}, want: "WORKER ROLE\n\nLOCAL RULES\n\n开工前用 Skill 工具依次加载：tdd,pkg:review_2-A。这些 skill 与本角色的限制冲突时，以本角色为准。\n\nBRIEF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := workerSkillsRepository(t, tc.env)
			e, _, err := repositoryExecutor(context.Background(), repo)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(e.Config.WorkerSkills, tc.skills) {
				t.Fatalf("skills=%q want=%q", e.Config.WorkerSkills, tc.skills)
			}
			spec, err := workerTaskSpec(repo, "BRIEF", e.Config.WorkerSkills)
			if err != nil || spec != tc.want {
				t.Fatalf("spec=%q want=%q err=%v", spec, tc.want, err)
			}
		})
	}
}

func TestWorkerSkillsInheritedAndRepositoryOverride(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want []string
	}{
		{want: []string{"inherited"}},
		{env: "AGENT_WORKER_SKILLS=repository\n", want: []string{"repository"}},
		{env: "AGENT_WORKER_SKILLS=''\n"},
	} {
		t.Run(tc.env, func(t *testing.T) {
			repo := workerSkillsRepository(t, tc.env)
			t.Setenv("AGENT_WORKER_SKILLS", "inherited")
			e, _, err := repositoryExecutor(context.Background(), repo)
			if err != nil || !slices.Equal(e.Config.WorkerSkills, tc.want) {
				t.Fatalf("skills=%q want=%q err=%v", e.Config.WorkerSkills, tc.want, err)
			}
		})
	}
}

type workerSkillsRecovery struct {
	*recoveryService
	spec string
}

func (r *workerSkillsRecovery) ChildTask(ctx context.Context, run, parent, spec, request string) (string, error) {
	r.spec = spec
	return r.recoveryService.ChildTask(ctx, run, parent, spec, request)
}

func TestWorkerSkillsInitialAndCorrectionDispatch(t *testing.T) {
	e, o, tools, service, _, ticket := recoveryFixture(t)
	e.Config.WorkerSkills = []string{"tdd", "pkg:review_2-A"}
	writeRole(t, e.Config.Repo, "local-worker.md", "LOCAL RULES\n")
	r := &workerSkillsRecovery{recoveryService: service}
	e.Recovery = r
	var logs bytes.Buffer
	e.Stderr = &logs
	tools.gateReds = 1
	if _, err := e.Execute(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if len(o.starts) != 2 || r.children != 1 {
		t.Fatalf("starts=%d children=%d", len(o.starts), r.children)
	}
	const instruction = "LOCAL RULES\n\n开工前用 Skill 工具依次加载：tdd,pkg:review_2-A。这些 skill 与本角色的限制冲突时，以本角色为准。\n\n"
	if !strings.Contains(o.starts[0].Spec, instruction+"# "+ticket.Identifier) {
		t.Fatalf("initial spec=%q", o.starts[0].Spec)
	}
	if !strings.Contains(r.spec, instruction+"correction 1/3\nRepair the failing assertion\n# "+ticket.Identifier) {
		t.Fatalf("correction spec=%q", r.spec)
	}
	if count := strings.Count(logs.String(), `"worker_skills":"tdd,pkg:review_2-A"`); count != 2 {
		t.Fatalf("want skills logged for both dispatches, count=%d logs=%s", count, logs.String())
	}
}
