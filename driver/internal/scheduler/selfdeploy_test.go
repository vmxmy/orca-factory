package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// selfDeployHost stands in for go, systemctl, git worktree, the installed
// driver binary and the smoke command; every other git call runs for real.
type selfDeployHost struct {
	driver           string
	builds, restarts int
	smokes           int
	smokeErr         error
}

func (h *selfDeployHost) run(ctx context.Context, s runx.Spec) ([]byte, error) {
	switch {
	case s.Path == "go":
		h.builds++
		out := s.Args[slices.Index(s.Args, "-o")+1]
		ldflags := s.Args[slices.Index(s.Args, "-ldflags")+1]
		return nil, os.WriteFile(out, []byte(strings.TrimPrefix(ldflags, "-X main.version=")), 0700)
	case s.Path == "systemctl":
		if !slices.Contains(s.Args, "--no-block") || !slices.Contains(s.Args, "orca-factory-driver") {
			return nil, errors.New("restart must not wait for its own stop")
		}
		h.restarts++
		return nil, nil
	case s.Path == h.driver:
		installed, err := os.ReadFile(h.driver)
		return []byte("orca-factory-driver " + string(installed) + "\n"), err
	case s.Path == "bash":
		h.smokes++
		return nil, h.smokeErr
	case s.Path == "git" && len(s.Args) > 1 && s.Args[0] == "worktree" && s.Args[1] == "add":
		return nil, os.MkdirAll(filepath.Join(s.Args[len(s.Args)-2], "driver"), 0700)
	}
	return runx.Run(ctx, s)
}

func newSelfDeployFixture(t *testing.T) (*mergeFixture, *selfDeployHost) {
	t.Helper()
	f := newMergeFixture(t)
	root := t.TempDir()
	t.Setenv("AGENT_DRIVER_STATE_ROOT", root)
	h := &selfDeployHost{driver: filepath.Join(t.TempDir(), "orca-factory-driver")}
	if err := os.WriteFile(h.driver, []byte("old"), 0700); err != nil {
		t.Fatal(err)
	}
	f.runtime.StateRoot = root
	f.runtime.DriverPath = h.driver
	f.runtime.Run = h.run
	f.repos[0].Env["AGENT_SELF_DEPLOY"] = "1"
	f.repos[0].Env["AGENT_SMOKE_CMD"] = "doctor"
	return f, h
}

func restartServe(f *mergeFixture) error {
	rebuilt := *f.runtime
	rebuilt.GitHub = nil
	return (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(context.Background())
}

func installed(t *testing.T, h *selfDeployHost) string {
	t.Helper()
	b, err := os.ReadFile(h.driver)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func maintenanceMode(t *testing.T) string {
	t.Helper()
	state, err := maintenance.Read()
	if err != nil {
		t.Fatal(err)
	}
	return state.Mode
}

func TestSelfDeployCompletesAcrossRestart(t *testing.T) {
	f, h := newSelfDeployFixture(t)
	sha := strings.Repeat("a", 40)
	if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background()); err == nil {
		t.Fatal("serve continued past its own restart")
	}
	receipts := strings.Join(f.tracker.bodies, "\n")
	if h.builds != 1 || h.restarts != 1 || h.smokes != 0 || installed(t, h) != sha || !strings.Contains(receipts, `"state":"deploy-intent"`) || f.tracker.issue.State.Name == "Done" || maintenanceMode(t) != "draining" {
		t.Fatalf("deployment before restart: builds=%d restarts=%d smokes=%d installed=%q mode=%s receipts=%v", h.builds, h.restarts, h.smokes, installed(t, h), maintenanceMode(t), f.tracker.bodies)
	}
	if err := restartServe(f); err != nil {
		t.Fatal(err)
	}
	if h.builds != 1 || h.restarts != 1 || h.smokes != 1 || f.tracker.issue.State.Name != "Done" || !strings.Contains(strings.Join(f.tracker.bodies, "\n"), `"state":"passed"`) || maintenanceMode(t) != "running" {
		t.Fatalf("deployment after restart: builds=%d restarts=%d smokes=%d state=%s mode=%s receipts=%v", h.builds, h.restarts, h.smokes, f.tracker.issue.State.Name, maintenanceMode(t), f.tracker.bodies)
	}
}

func TestSelfDeploySmokeFailureRollsBack(t *testing.T) {
	f, h := newSelfDeployFixture(t)
	if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(context.Background()); err == nil {
		t.Fatal("serve continued past its own restart")
	}
	h.smokeErr = errors.New("doctor reported fail")
	if err := restartServe(f); err == nil {
		t.Fatal("failed smoke completed the deployment")
	}
	receipts := strings.Join(f.tracker.bodies, "\n")
	if installed(t, h) != "old" || h.restarts != 2 || !strings.Contains(receipts, `"state":"paused"`) || !strings.Contains(receipts, "归属: 处置Agent") || f.tracker.issue.State.Name == "Done" || maintenanceMode(t) != "running" {
		t.Fatalf("rollback: installed=%q restarts=%d mode=%s state=%s receipts=%v", installed(t, h), h.restarts, maintenanceMode(t), f.tracker.issue.State.Name, f.tracker.bodies)
	}
	err := restartServe(f)
	if h.builds != 1 || h.smokes != 1 || h.restarts != 2 || f.tracker.issue.State.Name == "Done" {
		t.Fatalf("paused deployment acted again: err=%v builds=%d smokes=%d restarts=%d state=%s", err, h.builds, h.smokes, h.restarts, f.tracker.issue.State.Name)
	}
}
