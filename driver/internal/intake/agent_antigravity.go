package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/vmxmy/orca-factory/driver/internal/agentcli"
	"github.com/vmxmy/orca-factory/driver/internal/modelhealth"
)

const antigravity = "antigravity"

// antigravityState is agy's own directory under HOME: its sign-in token and
// conversations. The sandbox exposes it for the process, and the deny rules
// keep the agent's tools out of it.
const antigravityState = ".gemini/antigravity-cli"

var conversationID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// sessionAgent is the record of a session that started on an agent other
// than claude. A session without a record belongs to claude.
type sessionAgent struct {
	Agent        string `json:"agent"`
	Conversation string `json:"conversation"`
}

func (r *Runner) sessionPath(session string) string {
	return filepath.Join(r.StateRoot, "intake-agents", session+".json")
}

func (r *Runner) readSessionAgent(session string) (sessionAgent, error) {
	claude := sessionAgent{Agent: "claude"}
	if r.StateRoot == "" {
		return claude, nil
	}
	b, err := os.ReadFile(r.sessionPath(session))
	if errors.Is(err, os.ErrNotExist) {
		return claude, nil
	}
	var s sessionAgent
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	if err != nil {
		return sessionAgent{}, fmt.Errorf("intake session agent record unreadable: %w", err)
	}
	if s.Agent != antigravity || !conversationID.MatchString(s.Conversation) {
		return sessionAgent{}, errors.New("intake session agent record invalid")
	}
	return s, nil
}

// recordSessionAgent pins a session to the agent its first turn ran on.
func (r *Runner) recordSessionAgent(session string, s sessionAgent) error {
	path := r.sessionPath(session)
	if s.Agent == "claude" {
		if r.StateRoot == "" {
			return nil
		}
		if err := os.Remove(path); !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeJSON(path, s)
}

// antigravitySettings denies the agent every tool beyond reading the checkout.
// agy matches these rules before running a tool, also in --mode plan.
func antigravitySettings(home string) string {
	b, _ := json.Marshal(map[string]any{
		"allowNonWorkspaceAccess": false,
		"permissions": map[string][]string{"deny": {
			"read_file(" + filepath.Join(home, ".gemini") + ")", "read_file(/proc)",
			"write_file(*)", "command(*)", "mcp(*)", "read_url(*)", "execute_url(*)",
		}},
	})
	return string(b)
}

// antigravity runs one sandboxed agy --print turn. agy has no system prompt
// flag, so a first turn carries the intake role ahead of the message.
func (r *Runner) antigravity(ctx context.Context, t Turn, model modelhealth.Ref, conversation string) ([]byte, error) {
	profile, ok := agentcli.Lookup(antigravity)
	if !ok || len(profile.IntakeArgs) == 0 || len(profile.IntakeResumeArgs) == 0 {
		return nil, errors.New("intake antigravity requires intake_args and intake_resume_args in driver.toml")
	}
	if r.StateRoot == "" {
		return nil, errors.New("intake antigravity requires a state root for its sessions")
	}
	prompt := t.Prompt
	if conversation == "" {
		role, err := os.ReadFile(filepath.Join(r.Checkout, "scripts/agent-gates/roles/intake-chat.md"))
		if err != nil {
			return nil, fmt.Errorf("intake role unreadable: %w", err)
		}
		prompt = string(role) + "\n\n" + prompt
	}
	binary, err := exec.LookPath(profile.Executable())
	if err != nil {
		return nil, fmt.Errorf("intake sandbox cannot find agy: %w", err)
	}
	home := os.Getenv("HOME")
	sandbox, err := r.sandboxArgs(binary, antigravityState)
	if err != nil {
		return nil, err
	}
	args := append(profile.IntakeArgv(model.Model, model.Effort, prompt, conversation),
		"--output-format", "json", "--mode", "plan", "--settings", antigravitySettings(home))
	return r.sandboxed(ctx, append(sandbox, args...), []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}, "")
}

// agyPrintResult is agy --print --output-format json
// (testdata/recorded/agy-print-json-result.json).
type agyPrintResult struct {
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Response       string `json:"response"`
}

// antigravityAnswer returns the turn's reply and pins the session to the
// conversation agy reports.
func (r *Runner) antigravityAnswer(session string, b []byte) (string, error) {
	var result agyPrintResult
	if json.Unmarshal(b, &result) != nil || result.Status != "SUCCESS" || strings.TrimSpace(result.Response) == "" || !conversationID.MatchString(result.ConversationID) {
		return "", errors.New("intake agent returned no result")
	}
	if err := r.recordSessionAgent(session, sessionAgent{Agent: antigravity, Conversation: result.ConversationID}); err != nil {
		return "", fmt.Errorf("intake session agent not recorded: %w", err)
	}
	return strings.TrimSpace(result.Response), nil
}
