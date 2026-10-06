# Isolated local runner

Host requirements: Docker, Python 3, `gh` authenticated with repository runner administration permission, and a systemd user manager. Only the host invokes `gh`; its token is never passed to Docker. The image uses official Actions runner v2.337.0, pinned by linux/amd64 manifest digest in Dockerfile. The derived image is selected by immutable local image ID. Bash/git/make/gcc/libc headers and runner Node24 support setup-go v6 and CGO/race. The entire `_work` (checkout, tool cache, GOPATH, Go build cache, and XDG cache) is a 6 GiB tmpfs writable by UID 1001 and discarded with each container; setup-go may use GitHub remote cache.

```bash
PYTHONDONTWRITEBYTECODE=1 python3 scripts/local-runner/test_supervisor.py
scripts/local-runner/build.sh .omc/tmp/dev-tools/local-runner/image-id.txt
# Finite validation: do this before continuous services, never alongside them.
PYTHONDONTWRITEBYTECODE=1 python3 scripts/local-runner/supervisor.py \
  --image-file .omc/tmp/dev-tools/local-runner/image-id.txt \
  --state-dir "$HOME/.local/state/orca-go-isolated-runner" \
  --slot 1 --max-cycles 1 --idle-timeout 180
scripts/local-runner/install.sh .omc/tmp/dev-tools/local-runner/image-id.txt
systemctl --user enable --now orca-go-isolated-runner@1.service
# Verify the first instance online before enabling the other three:
systemctl --user enable --now orca-go-isolated-runner@{2,3,4}.service
```

Installed code/image ID live in `~/.local/lib/orca-go-isolated-runner`; services do not depend on a worktree. Durable nonsecret leases/slot locks live in `~/.local/state/orca-go-isolated-runner`. Use this same state directory for manual maintenance: different directories bypass slot locks. Only slots 1–4 exist, each with one 4 CPU/8 GiB/512 PID container. The fleet is bounded to four simultaneous jobs (16 CPU/32 GiB total); reserve additional CPU and memory for the host and production services. No host bind mounts, Docker socket, home, ports, or production credentials. Jobs run UID 1001, cap-drop ALL, no-new-privileges on bridge networking; sudo in the official base cannot elevate.

JIT configuration travels through host memory and stdin into the entrypoint, then `ACTIONS_RUNNER_INPUT_JITCONFIG`. Official [CommandSettings.cs](https://github.com/actions/runner/blob/v2.337.0/src/Runner.Listener/CommandSettings.cs) consumes and clears input environment variables before starting workers. [RunnerSettings](https://github.com/actions/runner/blob/v2.337.0/src/Runner.Common/ConfigurationStore.cs) defines `Ephemeral` and `DisableUpdate`; the host checks the former and sets the latter to keep the image version fixed. Runner-specific credentials exist inside the ephemeral container as required by GitHub; no host administration token is included. Never print JIT, complete environment, or runner credential files. Docker log storage is disabled; sanitized runner stdout is forwarded to journal.

## Observe and stop

```bash
systemctl --user status 'orca-go-isolated-runner@*.service'
journalctl --user -u orca-go-isolated-runner@1.service -n 30 --no-pager
gh api repos/vmxmy/orca-factory/actions/runners --jq '.runners[] | {id,name,status,busy}'
systemctl --user disable orca-go-isolated-runner@{1,2,3,4}.service
systemctl --user stop orca-go-isolated-runner@{1,2,3,4}.service
```

Stop requests prevent another registration and detach the supervisor, preserving any live container and lease. They **do not stop an idle listener or make it offline**: the listener may receive its one job and then exits naturally. A status check is not a dispatch barrier, and GitHub's DELETE endpoint is documented as forced removal, not a guaranteed busy rejection. Therefore neither normal stop nor recovery invokes Docker stop against a live container. The finite timeout likewise stops only supervision, not the container. To drain the fleet for an image replacement, leave supervision stopped while the existing ephemeral runners complete their one job, then restart; recovery waits for natural exit before registering the new image. An idle runner with no matching job can remain indefinitely. Do not start replacement slots with another state directory to bypass this bound.

`TimeoutStopSec=90s` bounds host supervisor shutdown; `KillMode=process` preserves the container/job. On restart, recovery observes only the exact owned lease and waits for container exit; STOP interrupts this waiting. Once the container is gone, GitHub registration cleanup runs. API/cleanup errors log `lease_retained` and retry on the next service start (30-second restart delay). Registration name intent is fsynced before POST. If the response is lost, recovery paginates runners and resolves the exact unique name before cleanup. If no matching registration is visible, it keeps the unresolved intent and fails closed rather than register more runners; investigate that name in GitHub before explicit maintenance. API subprocess timeouts are bounded at 60 seconds. Leases contain no JIT or credentials.

The 6 GiB `_work` and 1 GiB `/tmp` maxima share the container's **total 8 GiB memory budget** with processes (swap disabled). Setup-go SDKs, Go modules, caches, checkout, and compilation outputs use this space; there is no host writable cache. This suits the initial Go jobs but is a capacity limit, not a guarantee that any build fits: ENOSPC or cgroup OOM remains a real job failure to investigate. Validate the actual five jobs before calling the capacity proven. The image's remaining writable layer is not a global filesystem quota; runner diagnostics/configuration remain there and are discarded with the container.

## Manual image updates

Review a new official release and pin its exact manifest digest in Dockerfile. Build and validate the new image, drain all four units as above, then run install.sh with the new image-ID file and start the two units. No automatic runner updater is used. Keep the previous image ID for rollback; no automatic image pruning occurs. No production driver unit/configuration is involved.

Disabling automatic updates creates a maintenance obligation: subscribe to [actions/runner releases](https://github.com/actions/runner/releases) (Watch → Custom → Releases), review new releases, and update the pinned image within 30 days of a new runner version becoming available. GitHub stops dispatching jobs to outdated runners after that deadline; a critical security update can stop dispatch immediately until updated. See the [official update policy](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#runner-software-updates-on-self-hosted-runners).

## Login persistence and explicit maintenance cancellation

Check `loginctl show-user "$USER" -p Linger --value`. Without `yes`, enabling the units does not guarantee they survive logout or start at boot. `install.sh` warns but does not alter this setting. An authorized administrator can use `loginctl enable-linger "$USER"`; report a privilege failure rather than silently claiming persistence.

Stop means **停止补充，已有 runner 完成一项后退出** (stop replenishment; existing runners can still accept their one job). For explicit cancellation of idle maintenance runners: first move repository routing back to hosted runners, stop all four supervisors, then inspect *all queued and in-progress jobs* (including runs created before the routing change) and the exact lease runner. Only after confirming no queued job targets these labels and no owned runner is busy, use `docker stop --time 30 EXACT_OWNED_CONTAINER` for the container named by that lease and matching its owner label. Recheck immediately before the stop; arrange that no one submits matching jobs during the maintenance window. Restart the supervisor to recover/delete the old registration and replenish. Never use this procedure on a busy runner; cancel/wait for its workflow through GitHub first. A routing change alone does not retarget already queued jobs.

For an unresolved registration intent, keep automatic recovery fail-closed. Explicit maintenance may archive the exact nonsecret lease only after stopping that slot, taking its existing slot lock, checking all GitHub runner pages for its exact name, and confirming no matching Docker container exists. Verify no registration request remains in flight; a failed API read is not absence. Preserve the lease and the observation receipt before starting a replacement in the same slot/state directory. Never apply this procedure to a registered or running runner, and do not change other slots' leases.
