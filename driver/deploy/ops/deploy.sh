#!/bin/bash
# Deploys origin/main per driver/README.md "部署顺序"; one deploy at a time.
# ORCA_FACTORY_REPO is the orca-factory checkout to build from.
REPO_DIR=${ORCA_FACTORY_REPO:-$HOME/github/orca-factory}
STATE="$HOME/.local/state/orca-factory"
mkdir -p "$STATE"
exec 9>"$STATE/deploy.lock"
flock 9 || exit 1
set -e
cd "$REPO_DIR"
fetch_attempt=1
retry_delay=5
until git fetch -q origin; do
  if [ "$fetch_attempt" -ge 3 ]; then
    echo "git fetch 失败：尚未 drain，未改动任何状态" >&2
    exit 1
  fi
  echo "git fetch 失败，${retry_delay} 秒后重试" >&2
  sleep "$retry_delay"
  fetch_attempt=$((fetch_attempt + 1))
  retry_delay=15
done
git merge --ff-only -q origin/main
D="$HOME/.local/bin/orca-factory-driver"
SHA=$(git rev-parse --short HEAD)
"$D" maintenance drain --wait 2m | grep -o '"mode":"[a-z]*"'
replaced=false
restore_on_failure() {
  exit_code=$?
  trap - EXIT
  if [ "$exit_code" -ne 0 ]; then
    if [ "$replaced" = true ]; then
      cp -p "$B/orca-factory-driver" "$D.new"
      mv "$D.new" "$D"
      systemctl --user restart orca-factory-driver orca-factory-feishu-gateway
    fi
    "$D" maintenance resume --wait 2m
  fi
  exit "$exit_code"
}
trap restore_on_failure EXIT
idle='until "$0" maintenance status | grep -qF "\"writersActive\":false,\"sessionsActive\":false"; do sleep 15; done'
timeout 3600 bash -c "$idle" "$D" || { echo "sessions still active; resuming"; exit 1; }
B="$STATE/deploy-backups/$(date -u +%Y%m%dT%H%M%SZ)-$SHA"
mkdir -p "$B"
cp -p "$D" "$B/"
(cd driver && go build -trimpath -ldflags "-X main.version=$SHA" -o "$D.new" ./cmd/driver)
mv "$D.new" "$D"
replaced=true
systemctl --user restart orca-factory-driver orca-factory-feishu-gateway
sleep 5
systemctl --user is-active orca-factory-driver orca-factory-feishu-gateway
"$D" maintenance resume --wait 2m | grep -o '"mode":"[a-z]*"'
echo "backup=$B"
"$D" version
