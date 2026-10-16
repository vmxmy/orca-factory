package executor

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

func isStatus(s runx.Spec) bool {
	return s.Path == "git" && len(s.Args) > 0 && s.Args[0] == "status"
}

func gateRecorder(installErr error, status string) (*Commands, *[]runx.Spec) {
	var calls []runx.Spec
	c := &Commands{InstallCommand: "npm ci", Run: func(_ context.Context, s runx.Spec) ([]byte, error) {
		calls = append(calls, s)
		if s.Path == "/bin/bash" || s.Path == "bash" {
			return []byte("npm ERR! secret-token"), installErr
		}
		if isStatus(s) {
			return []byte(status), nil
		}
		return []byte("VERDICT green\n"), nil
	}}
	return c, &calls
}

func TestGateRunsInstallCommandInVerifyCheckout(t *testing.T) {
	c, calls := gateRecorder(nil, "")
	spec := filepath.Join(t.TempDir(), "gate.spec")
	if _, err := c.Gate(context.Background(), "/repo/demo", "abc123", spec); err != nil {
		t.Fatal(err)
	}
	var add, install, status, gate = -1, -1, -1, -1
	for i, s := range *calls {
		switch {
		case s.Path == "git" && len(s.Args) > 1 && s.Args[0] == "worktree" && s.Args[1] == "add":
			add = i
		case strings.HasSuffix(s.Path, "bash"):
			install = i
		case isStatus(s):
			status = i
		case strings.HasSuffix(s.Path, "gate.sh"):
			gate = i
		}
	}
	if add < 0 || install < add || status < install || gate < status {
		t.Fatalf("order add=%d install=%d status=%d gate=%d calls=%+v", add, install, status, gate, *calls)
	}
	s, verify := (*calls)[install], (*calls)[add].Args[3]
	if strings.Join(s.Args, " ") != "-c npm ci" || s.Dir != verify || s.Timeout <= 0 {
		t.Fatalf("install spec=%+v verify=%q", s, verify)
	}
	if st := (*calls)[status]; st.Dir != verify || strings.Join(st.Args, " ") != "status --porcelain" {
		t.Fatalf("status spec=%+v verify=%q", st, verify)
	}
}

func TestGateInstallFailureIsTicketInfra(t *testing.T) {
	cases := []struct {
		name       string
		installErr error
		status     string
	}{
		{"install exits non-zero", errors.New("exit status 1"), ""},
		{"install modifies checkout", nil, " M package-lock.json\n?? secret-token.log\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, calls := gateRecorder(tc.installErr, tc.status)
			spec := filepath.Join(t.TempDir(), "gate.spec")
			out, err := c.Gate(context.Background(), "/repo/demo", "abc123", spec)
			var ticket *infra.Ticket
			if !errors.As(err, &ticket) {
				t.Fatalf("err=%v, want *infra.Ticket", err)
			}
			for _, leak := range []string{"secret-token", "package-lock.json"} {
				if strings.Contains(err.Error(), leak) || strings.Contains(out, leak) {
					t.Fatalf("output leaked %q: out=%q err=%v", leak, out, err)
				}
			}
			for _, s := range *calls {
				if strings.HasSuffix(s.Path, "gate.sh") {
					t.Fatalf("gate.sh ran after install problem: %+v", s)
				}
			}
		})
	}
}
