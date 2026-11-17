package executor

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func TestChangedFilesDisablesRenames(t *testing.T) {
	var calls []runx.Spec
	c := &Commands{Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
		calls = append(calls, s)
		return []byte("other.txt\x00src/other.txt\x00"), nil
	}}
	files, err := c.ChangedFiles(context.Background(), "/repo/demo", "base-sha", "head-sha")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls=%+v, want one git diff", calls)
	}
	s := calls[0]
	want := []string{"diff", "--no-ext-diff", "--no-renames", "--name-only", "-z", "base-sha..head-sha", "--"}
	if s.Path != "git" || s.Dir != "/repo/demo" || !slices.Equal(s.Args, want) {
		t.Fatalf("diff spec=%+v, want git %v in /repo/demo", s, want)
	}
	if !slices.Equal(files, []string{"other.txt", "src/other.txt"}) {
		t.Fatalf("files=%v, want source and destination", files)
	}
}

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
