#!/bin/bash
# Releases a build: uploads the binary, the units and scripts, and (when given) the override config to the release
# bucket, then restarts the listener on its instance (which fetches them at boot too, so a replacement runs the same).
#
#   pull/deploy/release.sh <binary> [config.json] [ssh command for the listener, e.g. "ssh -i key ec2-user@1.2.3.4"]
set -euo pipefail
BIN=${1:?binary built by build.sh}
CONFIG=${2:-}
SSH=${3:-}
A="aws --profile ci-runners"
[ "$($A sts get-caller-identity --query Account --output text)" = 243891825325 ] || { echo "refusing: not ci-runners" >&2; exit 1; }
B=s3://superci-pull-243891825325
here=$(cd "$(dirname "$0")" && pwd)
$A s3 cp --quiet "$BIN" $B/bin/superci-pull
for f in superci-pull.service superci-pull-sync.service superci-pull-sync.timer superci-pull-health.service superci-pull-health.timer sync.sh health.sh; do
  $A s3 cp --quiet "$here/$f" $B/deploy/$f
done
[ -n "$CONFIG" ] && $A s3 cp --quiet "$CONFIG" $B/config/config.json
echo "released $(sha256sum "$BIN" | cut -c1-12) to $B"
if [ -n "$SSH" ]; then
  $SSH "set -e; sudo aws s3 cp --quiet $B/bin/superci-pull /usr/local/bin/superci-pull.new && sudo install -m 755 /usr/local/bin/superci-pull.new /usr/local/bin/superci-pull && sudo /usr/local/bin/superci-pull-sync && sudo systemctl restart superci-pull && systemctl is-active superci-pull"
fi
