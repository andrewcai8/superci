#!/bin/bash
# Releases the cache of a build (pull/deploy/build.sh): uploads the daemons, the functions' code, the machine start
# script and their RELEASE.json (commit, build time, sha256; see pull/deploy/manifest.sh, which refuses a commit that
# is not a clean one of origin's main) to each region's bucket's bin/. Machines take the daemon at boot; run up.sh
# afterwards to put the code on the functions.
#
#   pull/cache/deploy/release.sh <directory built by pull/deploy/build.sh>
set -euo pipefail
DIR=${1:?directory built by pull/deploy/build.sh}
A="aws --profile ci-runners"
[ "$($A sts get-caller-identity --query Account --output text)" = 243891825325 ] || { echo "refusing: not ci-runners" >&2; exit 1; }
here=$(cd "$(dirname "$0")" && pwd)
FILES="pullcache-linux-amd64 pullcache-linux-arm64 pullcache-lambda-arm64.zip up.sh"
"$here/../../deploy/manifest.sh" "$DIR" $FILES > "$DIR/RELEASE.json"
for r in us-east-1 us-east-2; do
  for f in $FILES RELEASE.json; do $A s3 cp --quiet --region $r "$DIR/$f" s3://superci-pull-cache-243891825325-$r/bin/$f; done
done
echo "released $(cat "$DIR/RELEASE.json") to bin/ in us-east-1 and us-east-2"
