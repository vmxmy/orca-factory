package executor

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func gateRecorder(installErr error) (*Commands, *[]runx.Spec) {
	var calls []runx.Spec
	c := &Commands{InstallCommand: "npm ci", Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
		calls = append(calls, s)
		if s.Path == "/bin/bash" || s.Path == "bash" {
			return []byte("npm ERR! secret-token"), installErr
		}
		return []byte("VERDICT green\n"), nil
	}}
	return c, &calls
}

func TestGateTimeLimitCoversLockWait(t *testing.T) {
	t.Setenv("GATE_LOCK_WAIT", "7200")
	c, calls := gateRecorder(nil)
	spec := filepath.Join(t.TempDir(), "gate.spec")
	if _, err := c.Gate(context.Background(), "/repo/demo", "abc123", spec); err != nil {
		t.Fatal(err)
	}
	var gate *runx.Spec
	for i, s := range *calls {
		if strings.HasSuffix(s.Path, "gate.sh") {
			gate = &(*calls)[i]
		} else if s.Timeout != 10*time.Minute {
			t.Errorf("%s timeout = %v, want 10m", s.Path, s.Timeout)
		}
	}
	if gate == nil {
		t.Fatal("gate.sh was not called")
	}
	if !slices.Contains(gate.Env, "GATE_LOCK_WAIT=1800") {
		t.Errorf("gate must explicitly set GATE_LOCK_WAIT=1800")
	}
	var lockWait string
	for _, env := range gate.Env {
		if value, ok := strings.CutPrefix(env, "GATE_LOCK_WAIT="); ok {
			lockWait = value
		}
	}
	seconds, err := strconv.Atoi(lockWait)
	if err != nil || seconds <= 0 {
		t.Fatalf("invalid gate lock wait %q", lockWait)
	}
	if gate.Timeout <= time.Duration(seconds)*time.Second {
		t.Errorf("gate timeout %v must exceed lock wait %ds", gate.Timeout, seconds)
	}
	if gate.Timeout != 60*time.Minute {
		t.Errorf("gate timeout = %v, want 60m", gate.Timeout)
	}
}

func TestGateRunsInstallCommandInVerifyCheckout(t *testing.T) {
	c, calls := gateRecorder(nil)
	spec := filepath.Join(t.TempDir(), "gate.spec")
	if _, err := c.Gate(context.Background(), "/repo/demo", "abc123", spec); err != nil {
		t.Fatal(err)
	}
	var add, install, gate = -1, -1, -1
	for i, s := range *calls {
		switch {
		case s.Path == "git" && len(s.Args) > 1 && s.Args[0] == "worktree" && s.Args[1] == "add":
			add = i
		case strings.HasSuffix(s.Path, "bash"):
			install = i
		case strings.HasSuffix(s.Path, "gate.sh"):
			gate = i
		}
	}
	if add < 0 || install <= add || gate <= install {
		t.Fatalf("order add=%d install=%d gate=%d calls=%+v", add, install, gate, *calls)
	}
	s, verify := (*calls)[install], (*calls)[add].Args[3]
	if strings.Join(s.Args, " ") != "-c npm ci" || s.Dir != verify || s.Timeout <= 0 {
		t.Fatalf("install spec=%+v verify=%q", s, verify)
	}
}

func TestGateInstallFailureIsTicketInfra(t *testing.T) {
	c, calls := gateRecorder(errors.New("exit status 1"))
	spec := filepath.Join(t.TempDir(), "gate.spec")
	out, err := c.Gate(context.Background(), "/repo/demo", "abc123", spec)
	var ticket *infra.Ticket
	if !errors.As(err, &ticket) {
		t.Fatalf("err=%v, want *infra.Ticket", err)
	}
	if strings.Contains(err.Error(), "secret-token") || strings.Contains(out, "secret-token") {
		t.Fatalf("output leaked install output: out=%q err=%v", out, err)
	}
	for _, s := range *calls {
		if strings.HasSuffix(s.Path, "gate.sh") {
			t.Fatalf("gate.sh ran after install failure: %+v", s)
		}
	}
}
