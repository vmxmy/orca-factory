package modelhealth

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// Probe sends one bounded, read-only request. For Claude, a non-empty baseURL
// overrides the endpoint inherited from the process.
func Probe(ctx context.Context, ref Ref, dir, baseURL string) Status {
	var args []string
	switch ref.Agent {
	case "codex":
		args = []string{"exec"}
		if ref.Model != "" {
			args = append(args, "-m", ref.Model)
		}
		args = append(args, "-c", "model_reasoning_effort=low", "--sandbox", "read-only")
	case "claude":
		args = []string{"-p"}
		if ref.Model != "" {
			args = append(args, "--model", ref.Model)
		}
		args = append(args, "--tools", "", "--permission-mode", "dontAsk", "--restricted", "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence")
	default:
		return Status{Ref: ref, State: Unknown, Reason: "unsupported_agent", CheckedAt: time.Now()}
	}
	args = append(args, "Reply with exactly: ok")
	env := []string{}
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		switch key {
		case "ORCA_PANE_KEY", "ORCA_AGENT_HOOK_PORT", "ORCA_AGENT_HOOK_TOKEN":
			continue
		}
		env = append(env, value)
	}
	if ref.Agent == "claude" && baseURL != "" {
		env = append(env, "ANTHROPIC_BASE_URL="+baseURL)
	}
	output, err := runx.Run(ctx, runx.Spec{Path: ref.Agent, Args: args, Dir: dir, Env: env, Timeout: ProbeTimeout})
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		output = append(append(output, '\n'), exit.Stderr...)
	}
	return Classify(ref, string(output), err, time.Now())
}
