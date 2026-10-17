package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// unrelatedDoctorFails is what doctor reports right after a long drain: these
// fails are not caused by the deployment and must not roll it back.
const unrelatedDoctorFails = `{"checks":{"version":{"status":"warn","reason":"running differs from origin/main"},"maintenance":{"status":"fail","reason":"draining over 60 minutes"},"sync_prs":{"status":"fail","reason":"sync PR open over 24 hours"},"credentials":{"status":"fail","reason":"gh token unreadable"}},"errors":[]}`

// selfDeployHost stands in for go, systemctl, systemd-run, git worktree, the
// installed driver binary and the smoke command; every other git call runs
// for real. kill names the command during which serve is killed.
type selfDeployHost struct {
	driver                   string
	builds, restarts, smokes int
	guardStops               []string
	guard                    []string
	guardErr                 error
	smokeOut                 []byte
	smokeErr                 error
	kill                     string
	cancel                   context.CancelFunc
}

func (h *selfDeployHost) killed(step string) bool {
	if h.kill != step {
		return false
	}
	h.kill = ""
	h.cancel()
	return true
}

func (h *selfDeployHost) run(ctx context.Context, s runx.Spec) ([]byte, error) {
	switch {
	case s.Path == "go":
		if h.killed("go") {
			return nil, context.Canceled
		}
		h.builds++
		out := s.Args[slices.Index(s.Args, "-o")+1]
		ldflags := s.Args[slices.Index(s.Args, "-ldflags")+1]
		return nil, os.WriteFile(out, []byte(strings.TrimPrefix(ldflags, "-X main.version=")), 0700)
	case s.Path == "systemd-run":
		h.guard = s.Args
		return nil, h.guardErr
	case s.Path == "systemctl" && slices.Contains(s.Args, "show"):
		return fmt.Appendf(nil, "MainPID=%d\nActiveState=active\n", os.Getpid()), nil
	case s.Path == "systemctl" && slices.Contains(s.Args, "stop"):
		h.guardStops = append(h.guardStops, s.Args[slices.Index(s.Args, "stop")+1:]...)
		return nil, nil
	case s.Path == "systemctl":
		if !slices.Contains(s.Args, "--no-block") || !slices.Contains(s.Args, "orca-factory-driver") {
			return nil, errors.New("restart must not wait for its own stop")
		}
		if h.killed("restart") {
			return nil, context.Canceled
		}
		h.restarts++
		return nil, nil
	case s.Path == h.driver:
		installed, err := os.ReadFile(h.driver)
		return []byte("orca-factory-driver " + string(installed) + "\n"), err
	case s.Path == "bash":
		if h.killed("smoke") {
			return nil, context.Canceled
		}
		h.smokes++
		return h.smokeOut, h.smokeErr
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
	h := &selfDeployHost{driver: filepath.Join(t.TempDir(), "orca-factory-driver"), smokeOut: []byte(unrelatedDoctorFails), smokeErr: errors.New("exit status 1"), cancel: func() {}}
	if err := os.WriteFile(h.driver, []byte("old"), 0700); err != nil {
		t.Fatal(err)
	}
	f.runtime.StateRoot = root
	f.runtime.DriverPath = h.driver
	f.runtime.Run = h.run
	f.runtime.Version = "old"
	f.repos[0].Env["AGENT_SELF_DEPLOY"] = "1"
	f.repos[0].Env["AGENT_SMOKE_CMD"] = "doctor --json"
	return f, h
}

// serveAs ticks a serve process running the given driver version; a kill
// requested on h cancels the tick as systemd stopping serve would.
func serveAs(f *mergeFixture, h *selfDeployHost, version string) error {
	rebuilt := *f.runtime
	rebuilt.GitHub = nil
	rebuilt.Version = version
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.cancel = cancel
	return (&Scheduler{Config: f.config, Repositories: f.repos, Backend: &rebuilt}).Tick(ctx)
}

func firstServe(t *testing.T, f *mergeFixture, h *selfDeployHost) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.cancel = cancel
	if err := (&Scheduler{Config: f.config, Repositories: f.repos, Backend: f.runtime}).Tick(ctx); err == nil {
		t.Fatal("serve continued past its own restart")
	}
}

func installed(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
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

func lastState(f *mergeFixture) string {
	for i := len(f.tracker.bodies) - 1; i >= 0; i-- {
		if _, after, ok := strings.Cut(f.tracker.bodies[i], `"state":"`); ok {
			state, _, _ := strings.Cut(after, `"`)
			return state
		}
	}
	return ""
}

func markerExists(f *mergeFixture, sha, kind string) bool {
	_, err := os.Stat(filepath.Join(f.runtime.StateRoot, "self-deploy", sha+"."+kind))
	return err == nil
}

var selfSHA = strings.Repeat("a", 40)

func TestSelfDeployCompletesAcrossRestart(t *testing.T) {
	f, h := newSelfDeployFixture(t)
	firstServe(t, f, h)
	if h.builds != 1 || h.restarts != 1 || h.smokes != 0 || installed(t, h.driver) != selfSHA || lastState(f) != "deploy-intent" || f.tracker.issue.State.Name == "Done" || maintenanceMode(t) != "draining" {
		t.Fatalf("deployment before restart: builds=%d restarts=%d smokes=%d installed=%q mode=%s state=%s", h.builds, h.restarts, h.smokes, installed(t, h.driver), maintenanceMode(t), lastState(f))
	}
	if !slices.Contains(h.guard, "--user") || !slices.Contains(h.guard, "--on-active=600") || !slices.Contains(h.guard, "--unit=orca-factory-self-deploy-guard-"+selfSHA[:12]) {
		t.Fatalf("guard unit not armed before restart: %v", h.guard)
	}
	// The --no-block restart returns before systemd stops the old serve.
	if err := serveAs(f, h, "old"); err == nil || h.smokes != 0 || lastState(f) != "deploy-intent" {
		t.Fatalf("old serve acted on the new binary: err=%v smokes=%d state=%s", err, h.smokes, lastState(f))
	}
	if err := serveAs(f, h, selfSHA); err != nil {
		t.Fatal(err)
	}
	if h.builds != 1 || h.restarts != 1 || h.smokes != 1 || !guardStopped(h) || !markerExists(f, selfSHA, "smoke-passed") || f.tracker.issue.State.Name != "Done" || lastState(f) != "passed" || maintenanceMode(t) != "running" {
		t.Fatalf("deployment after restart: builds=%d restarts=%d smokes=%d guardStops=%v state=%s ledger=%s mode=%s", h.builds, h.restarts, h.smokes, h.guardStops, f.tracker.issue.State.Name, lastState(f), maintenanceMode(t))
	}
}

func TestSelfDeploySmokeFailureRollsBack(t *testing.T) {
	f, h := newSelfDeployFixture(t)
	firstServe(t, f, h)
	h.smokeOut, h.smokeErr = nil, errors.New("driver.toml does not load")
	if err := serveAs(f, h, selfSHA); err == nil {
		t.Fatal("failed smoke completed the deployment")
	}
	receipts := strings.Join(f.tracker.bodies, "\n")
	if installed(t, h.driver) != "old" || h.restarts != 2 || lastState(f) != "paused" || !strings.Contains(receipts, "归属: 处置Agent") || f.tracker.issue.State.Name == "Done" || maintenanceMode(t) != "running" {
		t.Fatalf("rollback: installed=%q restarts=%d mode=%s state=%s ledger=%s", installed(t, h.driver), h.restarts, maintenanceMode(t), f.tracker.issue.State.Name, lastState(f))
	}
	err := serveAs(f, h, "old")
	if h.builds != 1 || h.smokes != 1 || h.restarts != 2 || f.tracker.issue.State.Name == "Done" || maintenanceMode(t) != "running" {
		t.Fatalf("paused deployment acted again: err=%v builds=%d smokes=%d restarts=%d state=%s", err, h.builds, h.smokes, h.restarts, f.tracker.issue.State.Name)
	}
}

func guardStopped(h *selfDeployHost) bool {
	unit := "orca-factory-self-deploy-guard-" + selfSHA[:12]
	return slices.Contains(h.guardStops, unit+".timer") && slices.Contains(h.guardStops, unit+".service")
}

// guardCommand builds the guard unit's command as systemd runs it after the window.
func guardCommand(t *testing.T, h *selfDeployHost, systemctlLog string) *exec.Cmd {
	t.Helper()
	i := slices.Index(h.guard, "/bin/sh")
	if i < 0 {
		t.Fatalf("guard has no command: %v", h.guard)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\necho \"$@\" >> "+systemctlLog+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(h.guard[i], h.guard[i+1:]...)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	return cmd
}

func runGuard(t *testing.T, h *selfDeployHost, systemctlLog string) {
	t.Helper()
	if out, err := guardCommand(t, h, systemctlLog).CombinedOutput(); err != nil {
		t.Fatalf("guard: %v %s", err, out)
	}
}

func TestSelfDeployGuardRestoresCrashingBinary(t *testing.T) {
	f, h := newSelfDeployFixture(t)
	firstServe(t, f, h)
	log := filepath.Join(t.TempDir(), "systemctl.log")
	passed := filepath.Join(f.runtime.StateRoot, "self-deploy", selfSHA+".smoke-passed")
	if err := os.WriteFile(passed, nil, 0600); err != nil {
		t.Fatal(err)
	}
	runGuard(t, h, log)
	if installed(t, h.driver) != selfSHA || markerExists(f, selfSHA, "rolled-back") {
		t.Fatal("guard rolled back a smoked deployment")
	}
	if err := os.Remove(passed); err != nil {
		t.Fatal(err)
	}
	// The new serve crashed on start: no smoke-passed marker within the window.
	runGuard(t, h, log)
	restarted, err := os.ReadFile(log)
	if err != nil || installed(t, h.driver) != "old" || !markerExists(f, selfSHA, "rolled-back") || !strings.Contains(string(restarted), "--user restart orca-factory-feishu-gateway orca-factory-driver") {
		t.Fatalf("guard: installed=%q systemctl=%q err=%v", installed(t, h.driver), restarted, err)
	}
	if err := serveAs(f, h, "old"); err == nil {
		t.Fatal("restored serve ignored the rollback")
	}
	if lastState(f) != "paused" || maintenanceMode(t) != "running" || h.smokes != 0 || markerExists(f, selfSHA, "rolled-back") || !guardStopped(h) {
		t.Fatalf("after guard rollback: state=%s mode=%s smokes=%d guardStops=%v", lastState(f), maintenanceMode(t), h.smokes, h.guardStops)
	}
}

// The guard judges smoke-passed under deploy.lock, so a serve still smoking
// cannot write passed while the guard swaps the binary back.
func TestSelfDeployGuardWaitsForDeployLock(t *testing.T) {
	f, h := newSelfDeployFixture(t)
	firstServe(t, f, h)
	held, err := os.OpenFile(filepath.Join(f.runtime.StateRoot, "deploy.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	guard := guardCommand(t, h, filepath.Join(t.TempDir(), "systemctl.log"))
	if err = guard.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if installed(t, h.driver) != selfSHA || markerExists(f, selfSHA, "rolled-back") {
		t.Fatal("guard rolled back while serve held deploy.lock")
	}
	if err = os.WriteFile(filepath.Join(f.runtime.StateRoot, "self-deploy", selfSHA+".smoke-passed"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = held.Close(); err != nil {
		t.Fatal(err)
	}
	if err = guard.Wait(); err != nil {
		t.Fatal(err)
	}
	if installed(t, h.driver) != selfSHA || markerExists(f, selfSHA, "rolled-back") {
		t.Fatal("guard rolled back a deployment that passed under the lock")
	}
}

func TestSelfDeployGuardFailureAbortsBeforeRestart(t *testing.T) {
	f, h := newSelfDeployFixture(t)
	h.guardErr = errors.New("systemd-run failed")
	firstServe(t, f, h)
	if h.restarts != 0 || installed(t, h.driver) != "old" || lastState(f) != "paused" || maintenanceMode(t) != "running" {
		t.Fatalf("guard failure: restarts=%d installed=%q state=%s mode=%s", h.restarts, installed(t, h.driver), lastState(f), maintenanceMode(t))
	}
}

// Every intermediate state converges in the next serve: done, rolled back, or
// paused with maintenance running again.
func TestSelfDeployConvergesAfterCrash(t *testing.T) {
	t.Run("drain or build", func(t *testing.T) {
		f, h := newSelfDeployFixture(t)
		h.kill = "go"
		firstServe(t, f, h)
		if lastState(f) != "deploy-intent" || maintenanceMode(t) != "draining" || installed(t, h.driver) != "old" {
			t.Fatalf("crash: state=%s mode=%s", lastState(f), maintenanceMode(t))
		}
		_ = serveAs(f, h, "old")
		if lastState(f) != "paused" || maintenanceMode(t) != "running" || h.builds != 0 || h.restarts != 0 || installed(t, h.driver) != "old" {
			t.Fatalf("after crash: state=%s mode=%s builds=%d restarts=%d", lastState(f), maintenanceMode(t), h.builds, h.restarts)
		}
	})
	t.Run("replaced before restart", func(t *testing.T) {
		f, h := newSelfDeployFixture(t)
		h.kill = "restart"
		firstServe(t, f, h)
		if installed(t, h.driver) != selfSHA || h.restarts != 0 || h.guard == nil {
			t.Fatalf("crash: installed=%q restarts=%d guard=%v", installed(t, h.driver), h.restarts, h.guard)
		}
		if err := serveAs(f, h, selfSHA); err != nil || lastState(f) != "passed" || maintenanceMode(t) != "running" || h.builds != 1 {
			t.Fatalf("after crash: err=%v state=%s mode=%s builds=%d", err, lastState(f), maintenanceMode(t), h.builds)
		}
	})
	t.Run("smoke", func(t *testing.T) {
		f, h := newSelfDeployFixture(t)
		firstServe(t, f, h)
		h.kill = "smoke"
		_ = serveAs(f, h, selfSHA)
		if lastState(f) != "smoke-intent" || maintenanceMode(t) != "draining" {
			t.Fatalf("crash: state=%s mode=%s", lastState(f), maintenanceMode(t))
		}
		if err := serveAs(f, h, selfSHA); err != nil || lastState(f) != "passed" || maintenanceMode(t) != "running" || h.smokes != 1 || h.builds != 1 {
			t.Fatalf("after crash: err=%v state=%s mode=%s smokes=%d builds=%d", err, lastState(f), maintenanceMode(t), h.smokes, h.builds)
		}
	})
	t.Run("version differs", func(t *testing.T) {
		f, h := newSelfDeployFixture(t)
		firstServe(t, f, h)
		// A manual deploy.sh installed another version in the restart gap.
		if err := os.WriteFile(h.driver, []byte("other"), 0700); err != nil {
			t.Fatal(err)
		}
		_ = serveAs(f, h, "other")
		if lastState(f) != "paused" || maintenanceMode(t) != "running" || installed(t, h.driver) != "other" || h.smokes != 0 || !guardStopped(h) {
			t.Fatalf("version differs: state=%s mode=%s installed=%q smokes=%d guardStops=%v", lastState(f), maintenanceMode(t), installed(t, h.driver), h.smokes, h.guardStops)
		}
	})
}

func TestSelfDeployRetrySmokeKeepsBackup(t *testing.T) {
	f, h := newSelfDeployFixture(t)
	firstServe(t, f, h)
	h.kill = "smoke"
	_ = serveAs(f, h, selfSHA)
	recovered := *f.runtime
	recovered.GitHub = nil
	if err := recovered.ResumeDeployment(context.Background(), "ENG-1", "q1", selfSHA, "retry-smoke", "operator", "smoke interrupted", false); err != nil {
		t.Fatal(err)
	}
	if err := serveAs(f, h, selfSHA); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(f.runtime.StateRoot, "deploy-backups", "self-"+selfSHA, "orca-factory-driver")
	if h.builds != 1 || h.restarts != 1 || installed(t, backup) != "old" || lastState(f) != "passed" {
		t.Fatalf("retry-smoke reinstalled: builds=%d restarts=%d backup=%q state=%s", h.builds, h.restarts, installed(t, backup), lastState(f))
	}
}

func TestSelfDeployWaitsForLockAndIdle(t *testing.T) {
	t.Run("deploy lock", func(t *testing.T) {
		f, h := newSelfDeployFixture(t)
		held, err := os.OpenFile(filepath.Join(f.runtime.StateRoot, "deploy.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := held.Close(); err != nil {
				t.Error(err)
			}
		})
		if err = syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		_ = serveAs(f, h, "old")
		if h.builds != 0 || slices.ContainsFunc(f.tracker.bodies, func(b string) bool { return strings.Contains(b, "deploy-intent") }) {
			t.Fatalf("deployed under a held deploy.lock: builds=%d state=%s", h.builds, lastState(f))
		}
	})
	t.Run("sessions active", func(t *testing.T) {
		f, h := newSelfDeployFixture(t)
		wait, poll := selfDeployIdleWait, selfDeployIdlePoll
		selfDeployIdleWait, selfDeployIdlePoll = 50*time.Millisecond, 10*time.Millisecond
		t.Cleanup(func() { selfDeployIdleWait, selfDeployIdlePoll = wait, poll })
		release, err := maintenance.Session()
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		_ = serveAs(f, h, "old")
		if h.builds != 0 || installed(t, h.driver) != "old" || lastState(f) != "paused" || maintenanceMode(t) != "running" {
			t.Fatalf("deployed with an active session: builds=%d state=%s mode=%s", h.builds, lastState(f), maintenanceMode(t))
		}
	})
}
