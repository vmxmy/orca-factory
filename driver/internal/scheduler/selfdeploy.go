package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// Self deployment follows README "部署顺序" from inside serve. The restart
// stops this process, so the deploy-intent receipt is written before anything
// changes and the next serve resumes from it. A deploy-intent is never
// installed twice; every later state converges in whichever serve reads it:
// passed, or paused with the drain this deployment started resumed.
const (
	maintenanceWait        = 2 * time.Minute
	selfDeployBuildTimeout = 10 * time.Minute
	selfDeploySmokeTimeout = 5 * time.Minute
	// selfDeployGuardWindow covers the new serve's startup, which waits for
	// Orca, plus its smoke.
	selfDeployGuardWindow = 10 * time.Minute
)

// Variables so tests can shorten the one-hour idle wait.
var selfDeployIdleWait, selfDeployIdlePoll = time.Hour, 15 * time.Second

// One restart command queues both jobs; systemd does not order them.
// Restarting the driver unit stops this serve.
var selfDeployServices = []string{"orca-factory-feishu-gateway", "orca-factory-driver"}

// Markers under <state root>/self-deploy/<sha>.<kind>. drained means this
// deployment set maintenance draining and owes the resume; the guard reads
// smoke-passed and leaves rolled-back for the restored serve.
const (
	markerDrained     = "drained"
	markerSmokePassed = "smoke-passed"
	markerRolledBack  = "rolled-back"
)

// selfDeployGuardScript runs in its own transient unit, so it outlives the
// driver unit's restart and acts even when the new binary cannot start.
const selfDeployGuardScript = `[ -e "$1" ] && exit 0
cp -f "$2" "$3.guard" && mv -f "$3.guard" "$3" && : > "$4" && exec systemctl --user restart "$5" "$6"`

// errSelfDeployRestarting ends the tick after the restart is queued: systemd is
// about to stop this serve, and the next serve continues the deployment.
var errSelfDeployRestarting = errors.New("self deployment installed; serve restarting")

var errSelfDeployRestartPending = errors.New("self deployment restart pending: this serve is not the installed driver")

func selfDeployFlag(self bool) string {
	if self {
		return "1"
	}
	return ""
}

func (r *Runtime) runFor(ctx context.Context, timeout time.Duration, dir, path string, args ...string) ([]byte, error) {
	run := r.Run
	if run == nil {
		run = runx.Run
	}
	return run(ctx, runx.Spec{Path: path, Args: args, Dir: dir, Timeout: timeout})
}

// deployLock is the flock deploy.sh takes, so serve and a manual deployment
// never overlap. A busy lock retries on a later tick.
func (r *Runtime) deployLock() (func(), error) {
	if r.StateRoot == "" || r.DriverPath == "" {
		return nil, errors.New("self deployment requires state root and driver path")
	}
	if err := os.MkdirAll(r.StateRoot, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(r.StateRoot, "deploy.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(errors.New("deploy.lock held by another deployment"), file.Close())
	}
	return func() { _ = file.Close() }, nil // Closing the descriptor releases the flock.
}

func (r *Runtime) selfBackup(sha string) string {
	return filepath.Join(r.StateRoot, "deploy-backups", "self-"+sha, filepath.Base(r.DriverPath))
}

func (r *Runtime) selfMarker(sha, kind string) string {
	return filepath.Join(r.StateRoot, "self-deploy", sha+"."+kind)
}

func touch(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, nil, 0600)
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func guardUnit(sha string) string {
	return "orca-factory-self-deploy-guard-" + sha[:12]
}

// deploySelf records deploy-intent, then installs. Any failure pauses the
// deployment for the resolver; a failure after the binary was replaced
// restores the backup first.
func (r *Runtime) deploySelf(ctx context.Context, t Ticket, p *merge.Progress) error {
	unlock, err := r.deployLock()
	if err != nil {
		return err
	}
	defer unlock()
	p.Deployment.State = "deploy-intent"
	if err = r.deploymentReceipt(ctx, t, p); err != nil {
		return err
	}
	replaced, err := r.installSelf(ctx, t.Repo, p.MergeSHA)
	if err == nil {
		return errSelfDeployRestarting
	}
	if ctx.Err() != nil {
		// serve is stopping; the next serve converges from deploy-intent.
		return err
	}
	if replaced {
		return errors.Join(err, r.pauseDeployment(ctx, t, p, "self deployment failed before restart"), r.rollbackSelf(ctx, p.MergeSHA))
	}
	return errors.Join(err, r.pauseSelf(ctx, t, p, "self deployment failed before restart"))
}

// installSelf arms the guard before the rename, so a serve killed between the
// rename and the restart still gets the rollback if the new binary fails.
func (r *Runtime) installSelf(ctx context.Context, repo, sha string) (replaced bool, err error) {
	if err = touch(r.selfMarker(sha, markerDrained)); err != nil {
		return false, err
	}
	if err = maintenance.SetWithin("draining", maintenanceWait); err != nil {
		return false, err
	}
	if err = r.awaitIdle(ctx); err != nil {
		return false, err
	}
	backup := r.selfBackup(sha)
	exists, err := fileExists(backup)
	if err == nil && !exists {
		err = copyAtomic(r.DriverPath, backup)
	}
	if err != nil {
		return false, fmt.Errorf("backup driver: %w", err)
	}
	built := r.DriverPath + ".new"
	if err = r.buildSelf(ctx, repo, sha, built); err != nil {
		return false, err
	}
	if err = r.startGuard(ctx, sha); err != nil {
		return false, err
	}
	if err = os.Rename(built, r.DriverPath); err != nil {
		r.stopGuard(ctx, sha)
		return false, err
	}
	return true, r.restartServices(ctx)
}

// awaitIdle waits until no executor, resolver or admitted write remains, as
// deploy.sh does, for at most one hour.
func (r *Runtime) awaitIdle(ctx context.Context) error {
	deadline := time.Now().Add(selfDeployIdleWait)
	for {
		state, err := maintenance.Status()
		if err != nil {
			return err
		}
		if !state.WritersActive && !state.SessionsActive {
			return nil
		}
		if !time.Now().Before(deadline) {
			return errors.New("sessions still active after one hour; deployment abandoned")
		}
		timer := time.NewTimer(selfDeployIdlePoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// buildSelf builds the exact squash SHA in a throwaway worktree under the
// state root, never the shared checkout, and stamps the version with it.
func (r *Runtime) buildSelf(ctx context.Context, repo, sha, out string) (err error) {
	src := filepath.Join(r.StateRoot, "self-deploy", sha)
	if err = os.RemoveAll(src); err != nil {
		return err
	}
	if _, err = r.runFor(ctx, time.Minute, repo, "git", "worktree", "prune"); err != nil {
		return err
	}
	if _, err = r.runFor(ctx, 2*time.Minute, repo, "git", "fetch", "--quiet", "origin", "main"); err != nil {
		return err
	}
	if _, err = r.runFor(ctx, 2*time.Minute, repo, "git", "worktree", "add", "--detach", src, sha); err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, os.RemoveAll(src))
		_, pruneErr := r.runFor(context.WithoutCancel(ctx), time.Minute, repo, "git", "worktree", "prune")
		err = errors.Join(err, pruneErr)
	}()
	_, err = r.runFor(ctx, selfDeployBuildTimeout, filepath.Join(src, "driver"), "go", "build", "-ldflags", "-X main.version="+sha, "-o", out, "./cmd/driver")
	return err
}

func (r *Runtime) restartServices(ctx context.Context) error {
	// --no-block: the driver unit's stop kills this process and its children.
	args := append([]string{"--user", "--no-block", "restart"}, selfDeployServices...)
	_, err := r.runFor(ctx, time.Minute, "", "systemctl", args...)
	return err
}

// startGuard schedules, independently of serve, the rollback that runs unless
// the smoke-passed marker appears within the window.
func (r *Runtime) startGuard(ctx context.Context, sha string) error {
	args := []string{"--user", "--unit=" + guardUnit(sha), fmt.Sprintf("--on-active=%d", int(selfDeployGuardWindow.Seconds())), "--timer-property=AccuracySec=1s",
		"/bin/sh", "-c", selfDeployGuardScript, "self-deploy-guard", r.selfMarker(sha, markerSmokePassed), r.selfBackup(sha), r.DriverPath, r.selfMarker(sha, markerRolledBack)}
	_, err := r.runFor(ctx, time.Minute, "", "systemd-run", append(args, selfDeployServices...)...)
	return err
}

// stopGuard only tidies up: a guard left running exits on the smoke-passed
// marker, and otherwise restores the backup this deployment already restored.
func (r *Runtime) stopGuard(ctx context.Context, sha string) {
	if _, err := r.runFor(ctx, time.Minute, "", "systemctl", "--user", "stop", guardUnit(sha)+".timer"); err != nil {
		runx.LoggerFrom(ctx).Warn("self deployment guard not stopped", "unit", guardUnit(sha), "err", err)
	}
}

// settleSelf resumes the drain this deployment started and clears its
// rollback marker, so a later retry-smoke is judged afresh.
func (r *Runtime) settleSelf(sha string) error {
	drained, err := fileExists(r.selfMarker(sha, markerDrained))
	if err != nil {
		return err
	}
	if drained {
		if err = maintenance.SetWithin("running", maintenanceWait); err != nil {
			return err
		}
		if err = os.Remove(r.selfMarker(sha, markerDrained)); err != nil {
			return err
		}
	}
	if err = os.Remove(r.selfMarker(sha, markerRolledBack)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (r *Runtime) pauseSelf(ctx context.Context, t Ticket, p *merge.Progress, reason string) error {
	return errors.Join(r.pauseDeployment(ctx, t, p, reason), r.settleSelf(p.MergeSHA))
}

// verifyInstalled checks this process, not the file: right after the
// --no-block restart the old serve still runs while the file is already new,
// and that serve must wait rather than smoke or pause.
func (r *Runtime) verifyInstalled(ctx context.Context, sha string) (bool, error) {
	if r.Version == sha {
		return true, nil
	}
	out, err := r.runFor(ctx, 30*time.Second, "", r.DriverPath, "version")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(out)) == "orca-factory-driver "+sha {
		return false, errSelfDeployRestartPending
	}
	return false, nil
}

// finishSelf runs in the serve the restart started, and again after any
// crash before passed: it never installs, and smoking twice is harmless.
func (r *Runtime) finishSelf(ctx context.Context, t Ticket, p *merge.Progress) error {
	unlock, err := r.deployLock()
	if err != nil {
		return err
	}
	defer unlock()
	sha := p.MergeSHA
	rolled, err := fileExists(r.selfMarker(sha, markerRolledBack))
	if err != nil {
		return err
	}
	if rolled {
		return r.pauseSelf(ctx, t, p, "guard restored the previous driver: the new serve did not pass smoke within the window")
	}
	ok, err := r.verifyInstalled(ctx, sha)
	if err != nil {
		return err
	}
	if !ok {
		return r.pauseSelf(ctx, t, p, "installed driver is not the squash SHA; no rollback, the binary is not this deployment's")
	}
	d := p.Deployment
	d.State, d.SmokeStarted = "smoke-intent", true
	if err = r.deploymentReceipt(ctx, t, p); err != nil {
		return err
	}
	observed := t
	observed.Progress = p
	if err = r.selfSmoke(ctx, observed, p); err != nil {
		if ctx.Err() != nil {
			return errors.Join(err, ctx.Err())
		}
		return errors.Join(err, r.pauseDeployment(ctx, t, p, "self deployment smoke failed; previous driver restored"), r.rollbackSelf(ctx, sha))
	}
	d.SmokePassed = true
	if err = touch(r.selfMarker(sha, markerSmokePassed)); err != nil {
		return err
	}
	r.stopGuard(ctx, sha)
	if err = r.settleSelf(sha); err != nil {
		return errors.Join(err, r.pauseDeployment(ctx, t, p, "smoke passed but maintenance resume failed"))
	}
	d.State = "passed"
	return r.deploymentReceipt(ctx, t, p)
}

// selfSmoke judges only what this deployment can break: the version (checked
// before), the serve unit running this process, the config loading (doctor
// reports at all) and Orca answering. Other doctor fails, such as an old
// fetch or a long drain, are the environment's and only logged.
func (r *Runtime) selfSmoke(ctx context.Context, t Ticket, p *merge.Progress) error {
	out, runErr := r.runSmoke(ctx, t, p)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var report struct {
		Checks map[string]struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(out, &report); err != nil || len(report.Checks) == 0 {
		if runErr != nil {
			runErr = safeDeploymentFailure(runErr)
		}
		return errors.Join(errors.New("smoke printed no doctor report; driver config may not load"), runErr)
	}
	for name, check := range report.Checks {
		if check.Status == "fail" {
			runx.LoggerFrom(ctx).Warn("self deployment smoke ignores a doctor fail unrelated to the deployment", "check", name, "reason", check.Reason)
		}
	}
	if err := r.serveReady(ctx); err != nil {
		return err
	}
	if _, err := r.Orca.AutomationsList(ctx); err != nil {
		return fmt.Errorf("orca unreachable: %w", err)
	}
	return nil
}

// serveReady requires systemd to run this very process as the active serve.
func (r *Runtime) serveReady(ctx context.Context) error {
	out, err := r.runFor(ctx, 30*time.Second, "", "systemctl", "--user", "show", "--property=ActiveState,MainPID", "orca-factory-driver")
	if err != nil {
		return err
	}
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[key] = value
		}
	}
	if props["ActiveState"] != "active" || props["MainPID"] != strconv.Itoa(os.Getpid()) {
		return errors.New("orca-factory-driver unit is not active with this serve as its main process")
	}
	return nil
}

// rollbackSelf restores the binary this deployment replaced and restarts.
// The paused receipt is already written, so the restored serve hands the
// barrier to the resolver instead of deploying again.
func (r *Runtime) rollbackSelf(ctx context.Context, sha string) error {
	if err := copyAtomic(r.selfBackup(sha), r.DriverPath); err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	r.stopGuard(ctx, sha)
	if err := r.settleSelf(sha); err != nil {
		return err
	}
	return r.restartServices(ctx)
}

// copyAtomic writes to a temporary sibling first, so dst is either the old
// file or a complete copy.
func copyAtomic(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, in.Close()) }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+"-*")
	if err != nil {
		return err
	}
	defer func() {
		if removeErr := os.Remove(tmp.Name()); !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	_, err = io.Copy(tmp, in)
	if err == nil {
		err = tmp.Chmod(info.Mode().Perm())
	}
	if err == nil {
		err = tmp.Sync()
	}
	if err = errors.Join(err, tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
