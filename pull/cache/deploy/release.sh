#!/bin/bash
# Releases the cache tree of a release directory (pull/deploy/build.sh; see pull/deploy/release-lib.sh): writes its
# bin/RELEASE.json (pull/deploy/manifest.sh, which refuses a commit not on andrewcai8/superci main and any file that is
# not the commit's), uploads the tree to each region's bucket from the directory (RELEASE.json last), runs up.sh, which
# puts the manifest's zip on the functions, and ends with pull/deploy/verify-release.sh cache, failing if that fails.
# Machines take the daemon at boot, only with the manifest's sha256 (machine-up.sh). A rollback is a release of an
# older commit of main.
#
#   pull/cache/deploy/release.sh <release directory>
set -euo pipefail
DIR=${1:?release directory built by pull/deploy/build.sh}
A="aws --profile ci-runners"
[ "$($A sts get-caller-identity --query Account --output text)" = 243891825325 ] || { echo "refusing: not ci-runners" >&2; exit 1; }
here=$(cd "$(dirname "$0")" && pwd)
. "$here/../../deploy/release-lib.sh"
T=$DIR/cache
"$here/../../deploy/manifest.sh" "$DIR" cache > "$DIR/cache.RELEASE.json"
mv "$DIR/cache.RELEASE.json" "$T/bin/RELEASE.json"
for r in $CACHE_REGIONS; do
  (cd "$T" && find . -type f ! -path ./bin/RELEASE.json | sed 's|^\./||' | LC_ALL=C sort) | while read -r k; do
    $A s3 cp --quiet --region "$r" "$T/$k" "s3://$CACHE_BUCKET_PREFIX-$r/$k"
  done
  $A s3 cp --quiet --region "$r" "$T/bin/RELEASE.json" "s3://$CACHE_BUCKET_PREFIX-$r/bin/RELEASE.json"
done
echo "released $(cat "$T/bin/RELEASE.json") to bin/ in $CACHE_REGIONS"
"$here/up.sh"
"$here/../../deploy/verify-release.sh" cache
