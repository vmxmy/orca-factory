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
git fetch -q origin
git merge --ff-only -q origin/main
D="$HOME/.local/bin/orca-factory-driver"
SHA=$(git rev-parse --short HEAD)
"$D" maintenance drain --wait 2m | grep -o '"mode":"[a-z]*"'
idle='until "$0" maintenance status | grep -qF "\"writersActive\":false,\"sessionsActive\":false"; do sleep 15; done'
timeout 3600 bash -c "$idle" "$D" || { echo "sessions still active; resuming"; "$D" maintenance resume --wait 2m; exit 1; }
B="$STATE/deploy-backups/$(date -u +%Y%m%dT%H%M%SZ)-$SHA"
mkdir -p "$B"
cp -p "$D" "$B/"
(cd driver && go build -trimpath -ldflags "-X main.version=$SHA" -o "$D.new" ./cmd/driver)
mv "$D.new" "$D"
systemctl --user restart orca-factory-driver orca-factory-feishu-gateway
sleep 5
systemctl --user is-active orca-factory-driver orca-factory-feishu-gateway
"$D" maintenance resume --wait 2m | grep -o '"mode":"[a-z]*"'
echo "backup=$B"
"$D" version
