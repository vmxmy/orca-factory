package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/vmxmy/orca-factory/driver/internal/maintenance"
	"github.com/vmxmy/orca-factory/driver/internal/merge"
	"github.com/vmxmy/orca-factory/driver/internal/runx"
)

// Self deployment follows README "部署顺序" from inside serve. The restart
// stops this process, so the deploy-intent receipt is written before anything
// changes and the next serve resumes from it: it verifies the installed
// version, smokes, resumes maintenance and writes passed. A deploy-intent is
// never installed twice.
const (
	maintenanceWait        = 2 * time.Minute
	selfDeployIdleWait     = time.Hour
	selfDeployIdlePoll     = 15 * time.Second
	selfDeployBuildTimeout = 10 * time.Minute
	selfDeploySmokeTimeout = 5 * time.Minute
)

// The gateway restarts first; restarting the driver unit stops this serve.
var selfDeployServices = []string{"orca-factory-feishu-gateway", "orca-factory-driver"}

// errSelfDeployRestarting ends the tick after the restart is queued: systemd is
// about to stop this serve, and the next serve continues the deployment.
var errSelfDeployRestarting = errors.New("self deployment installed; serve restarting")

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
		// serve is stopping; the next serve finds deploy-intent and verifies.
		return err
	}
	paused := r.pauseDeployment(ctx, t, p, "self deployment failed before restart")
	if replaced {
		return errors.Join(err, paused, r.rollbackSelf(ctx, p.MergeSHA))
	}
	return errors.Join(err, paused, maintenance.SetWithin("running", maintenanceWait))
}

func (r *Runtime) installSelf(ctx context.Context, repo, sha string) (replaced bool, err error) {
	if err = maintenance.SetWithin("draining", maintenanceWait); err != nil {
		return false, err
	}
	if err = r.awaitIdle(ctx); err != nil {
		return false, err
	}
	if err = copyAtomic(r.DriverPath, r.selfBackup(sha)); err != nil {
		return false, fmt.Errorf("backup driver: %w", err)
	}
	built := r.DriverPath + ".new"
	if err = r.buildSelf(ctx, repo, sha, built); err != nil {
		return false, err
	}
	if err = os.Rename(built, r.DriverPath); err != nil {
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

func (r *Runtime) verifyInstalled(ctx context.Context, sha string) error {
	out, err := r.runFor(ctx, 30*time.Second, "", r.DriverPath, "version")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != "orca-factory-driver "+sha {
		return errors.New("installed driver version differs from the squash SHA")
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
	if err := maintenance.SetWithin("running", maintenanceWait); err != nil {
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
