package modelhealth

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/agentcli"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// Endpoint is how a role reaches its model. Binary replaces the agent name
// looked up on PATH, BaseURL overrides Claude's inherited endpoint, and an
// Isolated role sees HOME and PATH plus explicit Claude relay settings.
type Endpoint struct {
	Binary    string
	BaseURL   string
	AuthToken string
	Isolated  bool
}

// Probe sends one bounded, read-only request through endpoint.
func Probe(ctx context.Context, ref Ref, dir string, endpoint Endpoint) Status {
	profile, ok := agentcli.Lookup(ref.Agent)
	if !ok {
		return Status{Ref: ref, State: Unknown, Reason: "unsupported_agent", CheckedAt: time.Now()}
	}
	args := profile.ProbeArgv(ref.Model, "Reply with exactly: ok")
	env := []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")}
	if !endpoint.Isolated {
		env = env[:0]
		for _, value := range os.Environ() {
			key, _, _ := strings.Cut(value, "=")
			switch key {
			case "ORCA_PANE_KEY", "ORCA_AGENT_HOOK_PORT", "ORCA_AGENT_HOOK_TOKEN", "AGENT_MODEL_PROXY_TOKEN":
				continue
			}
			env = append(env, value)
		}
	}
	if profile.Relay && endpoint.BaseURL != "" {
		env = append(env, "ANTHROPIC_BASE_URL="+endpoint.BaseURL)
	}
	if profile.Relay && endpoint.AuthToken != "" {
		env = append(env, "ANTHROPIC_AUTH_TOKEN="+endpoint.AuthToken)
	}
	binary := endpoint.Binary
	if binary == "" {
		binary = ref.Agent
	}
	output, err := runx.Run(ctx, runx.Spec{Path: binary, Args: args, Dir: dir, Env: env, Timeout: ProbeTimeout})
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		output = append(append(output, '\n'), exit.Stderr...)
	}
	return Classify(ref, string(output), err, time.Now())
}
