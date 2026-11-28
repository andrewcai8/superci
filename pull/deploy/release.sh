#!/bin/bash
# Releases the listener tree of a release directory (build.sh; see release-lib.sh): writes its bin/RELEASE.json
# (manifest.sh, which refuses a commit not on andrewcai8/superci main and any file that is not the commit's), boots the
# commit's machine and refuses unless its job environment is Blacksmith's (parity.sh; PARITY_SKIP=<reason> skips it and
# RELEASE.json records why), checks the override config with the new binary on the listener, uploads the tree to the
# release bucket from the directory (RELEASE.json last), installs the binary on the listener only if its sha256 is the
# manifest's, restarts it, and ends with verify-release.sh listener, failing if that fails. The instance fetches the
# same files at boot, so a replacement runs the same. A rollback is a release of an older commit of main: build.sh <dir> <commit>, then this.
#
#   pull/deploy/release.sh <release directory> pull/deploy/ssh.sh
set -euo pipefail
DIR=${1:?release directory built by build.sh}
SSH=${2:?ssh command for the listener, e.g. pull/deploy/ssh.sh}
A="aws --profile ci-runners"
[ "$($A sts get-caller-identity --query Account --output text)" = 243891825325 ] || { echo "refusing: not ci-runners" >&2; exit 1; }
here=$(cd "$(dirname "$0")" && pwd)
. "$here/release-lib.sh"
B=s3://$LISTENER_BUCKET
T=$DIR/listener
# The listener refuses to start without its claims bucket (claims.go), so a release before up.sh made it is an outage.
$A s3api get-bucket-policy --bucket superci-pull-claims-243891825325 --query Policy --output text | grep -q if-none-match || { echo "refusing: the claims bucket or its policy is missing; run pull/deploy/up.sh first" >&2; exit 1; }
"$here/manifest.sh" "$DIR" listener > "$DIR/listener.RELEASE.json"
# The listener carries boot.sh and agent.py and picks the image at its start: a release whose machines would give a job
# another environment than Blacksmith's stops here, before anything is uploaded (parity.sh). PARITY_SKIP=<reason>
# releases without the check, for an emergency rollback; the reason goes into the release's bin/RELEASE.json as its
# "parity", which is "passed" otherwise.
"$here/parity.sh" "$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["commit"])' "$DIR/REVISION")" || { echo "refusing: the job environment differs from Blacksmith's (pull/deploy/parity.sh)" >&2; exit 1; }
# A machine that is lost must be replaceable: the identity hosts' templates and the listener's group name images that
# exist (templates.sh; no PARITY_SKIP skips it). The dispatch templates are not checked here: the listener this release
# starts makes their next version on the pin, and verify-release.sh checks them after the restart.
"$here/templates.sh" check --pre-upload --commit "$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["commit"])' "$DIR/REVISION")" || { echo "refusing: a launch template names an image EC2 does not have, or not the pin (pull/deploy/templates.sh)" >&2; exit 1; }
if [ -n "${PARITY_SKIP:-}" ]; then
  parity="skipped: $PARITY_SKIP"
  echo "release: WARNING: the parity check was skipped (PARITY_SKIP: $PARITY_SKIP); bin/RELEASE.json records it" >&2
else
  parity=passed
fi
python3 -I -c 'import json,sys; p=sys.argv[1]; r=json.load(open(p)); r["parity"]=sys.argv[2]; open(p,"w").write(json.dumps(r, separators=(",", ":")) + "\n")' "$DIR/listener.RELEASE.json" "$parity"
mv "$DIR/listener.RELEASE.json" "$T/bin/RELEASE.json"
want=$(release_json_sha "$T/bin/RELEASE.json" bin/superci-pull)
$SSH "cat > /tmp/superci-pull.check && chmod 755 /tmp/superci-pull.check" < "$T/bin/superci-pull"
$SSH "/tmp/superci-pull.check -validate -config /dev/stdin" < "$T/config/config.json" || { echo "refusing: config/config.json is invalid" >&2; exit 1; }
(cd "$T" && find . -type f ! -path ./bin/RELEASE.json | sed 's|^\./||' | LC_ALL=C sort) | while read -r k; do $A s3 cp --quiet "$T/$k" "$B/$k"; done
$A s3 cp --quiet "$T/bin/RELEASE.json" "$B/bin/RELEASE.json"
echo "released $(cat "$T/bin/RELEASE.json") to $B"
$SSH "set -e; sudo aws s3 cp --quiet $B/bin/superci-pull /usr/local/bin/superci-pull.new
[ \"\$(sha256sum < /usr/local/bin/superci-pull.new | cut -d' ' -f1)\" = $want ] || { echo 'refusing: the bucket binary is not the one RELEASE.json names'; exit 1; }
sudo install -m 755 /usr/local/bin/superci-pull.new /usr/local/bin/superci-pull && sudo /usr/local/bin/superci-pull-sync && sudo systemctl restart superci-pull && systemctl is-active superci-pull"
SSH=$SSH "$here/verify-release.sh" listener
