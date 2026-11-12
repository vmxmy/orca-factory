package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
// stops this process, so deploy-intent is recorded before the build or install
// and the next serve resumes from it. A deploy-intent whose install
// may have started is never installed twice; one interrupted before the
// install returns to pending. Every other state converges in whichever serve
// reads it: passed, or paused with the drain this deployment started resumed.
const (
	maintenanceWait        = 2 * time.Minute
	selfDeployBuildTimeout = 10 * time.Minute
	selfDeploySmokeTimeout = 5 * time.Minute
	// selfDeployGuardWindow covers the new serve's startup, which waits for
	// Orca, plus its smoke.
	selfDeployGuardWindow = 10 * time.Minute
	// selfDeployGuardLockWait outlasts a serve smoking under deploy.lock; a
	// lock held longer belongs to a manual deploy.sh, which owns the binary.
	selfDeployGuardLockWait = selfDeploySmokeTimeout + 2*time.Minute
)

// Variables so tests can shorten the idle wait, retry and report intervals.
var selfDeployIdleWait, selfDeployIdlePoll, selfDeployIdleRetry, selfDeployIdleReport = time.Hour, 15 * time.Second, 15 * time.Minute, 10 * time.Minute

var errSelfDeployIdleTimeout = errors.New("等待会话空闲超时")

// One restart command queues both jobs; systemd does not order them.
// Restarting the driver unit stops this serve.
var selfDeployServices = []string{"orca-factory-feishu-gateway", "orca-factory-driver"}

// Markers under <state root>/self-deploy/<sha>.<kind>. drained means this
// deployment set maintenance draining and owes the resume; installing means the
// guard or the binary may have changed, so the install is not redone until a
// recovery returns the deployment to pending; the guard reads smoke-passed;
// rolled-back tells the restored serve to pause and resume, and holds the
// reason when serve itself rolled back. idle-timeout records the last timeout
// time and timeout count; no new drain starts until selfDeployIdleRetry has elapsed.
const (
	markerDrained     = "drained"
	markerInstalling  = "installing"
	markerSmokePassed = "smoke-passed"
	markerRolledBack  = "rolled-back"
	markerIdleTimeout = "idle-timeout"
)

// selfDeployGuardScript runs in its own transient unit, so it outlives the
// driver unit's restart and acts even when the new binary cannot start. It
// judges smoke-passed under deploy.lock: serve writes that marker under the
// same lock, so a smoke in progress finishes before the guard decides. It
// restores the backup only over the binary this deployment installed.
const selfDeployGuardScript = `lock=$1 wait=$2 sha=$3 passed=$4 backup=$5 driver=$6 rolled=$7
shift 7
exec 9>>"$lock" || exit 1
flock -w "$wait" 9 || { echo "deploy.lock still held after ${wait}s; no rollback" >&2; exit 0; }
[ -e "$passed" ] && exit 0
[ "$("$driver" version)" = "orca-factory-driver $sha" ] || { echo "installed driver is not $sha; no rollback" >&2; exit 0; }
cp -f "$backup" "$driver.guard" && mv -f "$driver.guard" "$driver" && : > "$rolled" && exec 9>&- && exec systemctl --user restart "$@"`

// errSelfDeployRestarting ends the tick after the restart is queued: systemd is
// about to stop this serve, and the next serve continues the deployment.
var errSelfDeployRestarting = errors.New("self deployment installed; serve restarting")

var errSelfDeployRestartPending = errors.New("self deployment restart pending: this serve is not the installed driver")

// errSelfDeployReinstall ends the tick after a deploy-intent interrupted before
// its install returned to pending; the next tick installs again.
var errSelfDeployReinstall = errors.New("self deployment interrupted before install; reinstalling")

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

// deploySelf waits in pending, then records deploy-intent before installing.
// Keeping the idle wait in pending also permits retries after retry-workflow,
// whose deploy-intent cannot return to pending without a new recovery decision.
func (r *Runtime) deploySelf(ctx context.Context, t Ticket, p *merge.Progress) error {
	d := p.Deployment
	sha := deploymentSHA(p)
	idleTimeout := r.selfMarker(sha, markerIdleTimeout)
	timeouts := 0
	if data, err := os.ReadFile(idleTimeout); err == nil {
		timestamp, count, _ := strings.Cut(string(data), " ")
		timedOut, err := time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return err
		}
		timeouts, err = strconv.Atoi(count)
		if err != nil {
			return err
		}
		now := time.Now()
		if r.Now != nil {
			now = r.Now()
		}
		if remaining := timedOut.Add(selfDeployIdleRetry).Sub(now); remaining > 0 {
			return fmt.Errorf("%w，%s 后重试", errSelfDeployIdleTimeout, remaining)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	round := timeouts + 1
	unlock, err := r.deployLock()
	if err != nil {
		return err
	}
	defer unlock()
	if err = os.Remove(r.selfMarker(sha, markerInstalling)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = touch(r.selfMarker(sha, markerDrained)); err == nil {
		err = maintenance.SetWithin("draining", maintenanceWait)
	}
	if err == nil {
		err = r.awaitIdle(ctx, func(elapsed time.Duration) {
			d.Reason = fmt.Sprintf("等待会话空闲，已等待 %s（第 %d 轮）", elapsed, round)
			if err := r.deploymentReceipt(ctx, t, p); err != nil {
				runx.LoggerFrom(ctx).Warn("self deployment idle progress receipt failed", "err", err)
			}
		})
	}
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		if errors.Is(err, errSelfDeployIdleTimeout) {
			if resumeErr := r.settleSelf(sha); resumeErr != nil {
				return errors.Join(err, resumeErr, r.pauseSelf(ctx, t, p, "idle wait timed out but maintenance resume failed"))
			}
			now := time.Now()
			if r.Now != nil {
				now = r.Now()
			}
			if markerErr := os.MkdirAll(filepath.Dir(idleTimeout), 0700); markerErr != nil {
				return errors.Join(err, markerErr)
			}
			if markerErr := os.WriteFile(idleTimeout, []byte(now.UTC().Format(time.RFC3339Nano)+" "+strconv.Itoa(round)), 0600); markerErr != nil {
				return errors.Join(err, markerErr)
			}
			d.Reason = fmt.Sprintf("%s（第 %d 轮）", err.Error(), round)
			return errors.Join(err, r.deploymentReceipt(ctx, t, p))
		}
		return errors.Join(err, r.pauseSelf(ctx, t, p, "self deployment failed before restart"))
	}
	if err = os.Remove(idleTimeout); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	d.State, d.Reason = "deploy-intent", ""
	if err = r.deploymentReceipt(ctx, t, p); err != nil {
		return err
	}
	replaced, err := r.installSelf(ctx, t.Repo, deploymentSHA(p))
	if err == nil {
		return errSelfDeployRestarting
	}
	if ctx.Err() != nil {
		// serve is stopping; the next serve converges from deploy-intent.
		return err
	}
	if replaced {
		return errors.Join(err, r.pauseBatchBeforeRollback(ctx, t, p, "self deployment failed before restart"), r.rollbackSelf(ctx, p, "self deployment failed before restart"))
	}
	return errors.Join(err, r.pauseSelf(ctx, t, p, "self deployment failed before restart"))
}

// installSelf arms the guard before the rename, so a serve killed between the
// rename and the restart still gets the rollback if the new binary fails.
func (r *Runtime) installSelf(ctx context.Context, repo, sha string) (replaced bool, err error) {
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
	if err = touch(r.selfMarker(sha, markerInstalling)); err != nil {
		return false, err
	}
	if err = r.startGuard(ctx, sha); err != nil {
		return false, err
	}
	if err = os.Rename(built, r.DriverPath); err != nil {
		return false, err
	}
	return true, r.restartServices(ctx)
}

// awaitIdle waits until no executor, resolver or admitted write remains, as
// deploy.sh does, for at most one hour. Before timing out, it reports elapsed
// time at selfDeployIdleReport intervals.
func (r *Runtime) awaitIdle(ctx context.Context, report func(elapsed time.Duration)) error {
	started := time.Now()
	deadline := started.Add(selfDeployIdleWait)
	reported := time.Duration(-1)
	for {
		state, err := maintenance.Status()
		if err != nil {
			return err
		}
		if !state.WritersActive && !state.SessionsActive {
			return nil
		}
		if step := time.Since(started).Truncate(selfDeployIdleReport); step > reported {
			report(step)
			reported = step
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w，已等待 %s", errSelfDeployIdleTimeout, time.Since(started).Round(time.Millisecond))
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
	_, err = r.runFor(ctx, selfDeployBuildTimeout, filepath.Join(src, "driver"), "go", "build", "-trimpath", "-ldflags", "-X main.version="+sha, "-o", out, "./cmd/driver")
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
		"/bin/sh", "-c", selfDeployGuardScript, "self-deploy-guard", filepath.Join(r.StateRoot, "deploy.lock"), strconv.Itoa(int(selfDeployGuardLockWait.Seconds())), sha,
		r.selfMarker(sha, markerSmokePassed), r.selfBackup(sha), r.DriverPath, r.selfMarker(sha, markerRolledBack)}
	_, err := r.runFor(ctx, time.Minute, "", "systemd-run", append(args, selfDeployServices...)...)
	return err
}

// stopGuard runs before every final receipt, so a guard cannot later swap in
// the backup over a binary this deployment no longer owns. Exit status 5 is
// systemctl's "unit not loaded": the guard was never armed or already ended.
func (r *Runtime) stopGuard(ctx context.Context, sha string) error {
	_, err := r.runFor(ctx, time.Minute, "", "systemctl", "--user", "stop", guardUnit(sha)+".timer", guardUnit(sha)+".service")
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 5 {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stop self deployment guard: %w", err)
	}
	return nil
}

// settleSelf resumes the drain this deployment started and clears its
// rollback marker, so a later retry-smoke is judged afresh. The installing
// marker stays through a pause: only passed or a new install from pending
// clears it.
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
	if err := r.stopGuard(ctx, deploymentSHA(p)); err != nil {
		return err
	}
	if batch, ok := ctx.Value(deployBatchKey{}).(*deployBatch); ok {
		if p.Deployment.State == "paused" && p.Deployment.Reason != "" {
			reason = p.Deployment.Reason
		}
		if err := r.batchReceipt(ctx, batch, p.Deployment, reason); err != nil {
			return errors.Join(errors.New(reason), err)
		}
		return errors.Join(errors.New(reason), r.settleSelf(deploymentSHA(p)))
	}
	return errors.Join(r.pauseDeployment(ctx, t, p, reason), r.settleSelf(deploymentSHA(p)))
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
	sha := deploymentSHA(p)
	reason, err := os.ReadFile(r.selfMarker(sha, markerRolledBack))
	if err == nil {
		if len(reason) == 0 {
			reason = []byte("guard restored the previous driver: the new serve did not pass smoke within the window")
		}
		return r.pauseSelf(ctx, t, p, string(reason))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ok, err := r.verifyInstalled(ctx, sha)
	if err != nil {
		return err
	}
	if !ok {
		return r.retryOrPauseSelf(ctx, t, p)
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
		return errors.Join(err, r.pauseBatchBeforeRollback(ctx, t, p, "self deployment smoke failed; previous driver restored"), r.rollbackSelf(ctx, p, "self deployment smoke failed; previous driver restored"))
	}
	if err = touch(r.selfMarker(sha, markerSmokePassed)); err != nil {
		return err
	}
	if err = r.stopGuard(ctx, sha); err != nil {
		return err
	}
	if err = r.settleSelf(sha); err != nil {
		return errors.Join(err, r.pauseDeployment(ctx, t, p, "smoke passed but maintenance resume failed"))
	}
	passed := *d
	passed.State, passed.SmokePassed = "passed", true
	next := *p
	next.Deployment = &passed
	if err = r.deploymentReceipt(ctx, t, &next); err != nil {
		return err
	}
	p.Deployment = &passed
	if err = os.Remove(r.selfMarker(sha, markerInstalling)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// retryOrPauseSelf handles a serve that is not running the squash SHA. Without
// the installing marker the install never began, so the deployment returns to
// pending and reinstalls; with it the binary is uncertain and the deployment
// pauses. A deployment with a recovery pauses too: retry-smoke authorizes only
// the smoke, also where an older serve installed without the marker, and a
// reinstall from retry-workflow is not repeated without a new decision. Reruns
// counts reinstalls, so the pending receipt differs from the first one and is
// recorded.
func (r *Runtime) retryOrPauseSelf(ctx context.Context, t Ticket, p *merge.Progress) error {
	installing, err := fileExists(r.selfMarker(deploymentSHA(p), markerInstalling))
	if err != nil {
		return err
	}
	d := p.Deployment
	if installing || d.Recovery != nil || d.State != "deploy-intent" {
		return r.pauseSelf(ctx, t, p, "installed driver is not the squash SHA; no rollback, the binary is not this deployment's")
	}
	d.State = "pending"
	d.Reruns++
	return errors.Join(errSelfDeployReinstall, r.deploymentReceipt(ctx, t, p))
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

// rollbackSelf restores the binary this deployment replaced and restarts,
// still drained, so the failed binary never claims work. The restored serve
// reads the rolled-back marker, pauses for the resolver and resumes. If the
// marker is lost, it finds a version that is not the squash SHA and pauses too.
func (r *Runtime) rollbackSelf(ctx context.Context, p *merge.Progress, reason string) error {
	sha := deploymentSHA(p)
	if err := r.stopGuard(ctx, sha); err != nil {
		return err
	}
	if err := copyAtomic(r.selfBackup(sha), r.DriverPath); err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	marked := os.WriteFile(r.selfMarker(sha, markerRolledBack), []byte(reason), 0600)
	return errors.Join(marked, r.restartServices(ctx))
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
