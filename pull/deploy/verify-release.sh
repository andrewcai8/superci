#!/bin/bash
# Checks, reading only, that production runs a release: each bucket's bin/RELEASE.json names a commit on
# andrewcai8/superci main, every file it lists is in the bucket with its sha256, the bucket holds nothing else under
# the release's prefixes but operator files (OPERATOR_KEYS in release-lib.sh), the running listener's binary has the
# listener manifest's sha256 and prints its commit, and each cache function's code in each region is the cache
# manifest's zip. Prints a line per check and exits 1 when any fails.
#
#   pull/deploy/verify-release.sh [listener|cache|all]      SSH=<command> reaches the listener (pull/deploy/ssh.sh by default)
set -uo pipefail
PART=${1:-all}
A="aws --profile ci-runners"
[ "$($A sts get-caller-identity --query Account --output text)" = 243891825325 ] || { echo "refusing: not ci-runners" >&2; exit 1; }
here=$(cd "$(dirname "$0")" && pwd)
. "$here/release-lib.sh"
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
bad=0
ok() { echo "ok    $*"; }
fail() { echo "FAIL  $*"; bad=1; }

# bucket <bucket> <region> <manifest copy> <prefix>...: the bucket holds exactly its manifest's files.
bucket() {
  local b=$1 r=$2 m=$3; shift 3
  if ! $A s3 cp --quiet --region "$r" "s3://$b/bin/RELEASE.json" "$m" 2>/dev/null; then fail "s3://$b/bin/RELEASE.json: missing"; return 1; fi
  local commit; commit=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["commit"])' "$m") || { fail "s3://$b/bin/RELEASE.json: unreadable"; return 1; }
  if on_main "$here" "$commit" 2>/dev/null; then ok "s3://$b: release $commit is on main"; else fail "s3://$b: release $commit is not on andrewcai8/superci main"; fi
  local listed; listed=$(python3 -I -c 'import json,sys; [print(k) for k in json.load(open(sys.argv[1]))["sha256"]]' "$m")
  local k want got
  for k in $listed; do
    want=$(release_json_sha "$m" "$k")
    if got=$($A s3 cp --quiet --region "$r" "s3://$b/$k" - 2>/dev/null | sha256sum | cut -d' ' -f1) && [ "$got" = "$want" ]; then ok "s3://$b/$k"
    else fail "s3://$b/$k: sha256 $got, RELEASE.json says $want"; fi
  done
  local p keys
  for p in "$@"; do
    if ! keys=$($A s3api list-objects-v2 --region "$r" --bucket "$b" --prefix "$p" --query 'Contents[].Key' --output text); then
      fail "s3://$b/$p: could not be listed"; continue
    fi
    for k in $keys; do
      [ "$k" = None ] || [ "$k" = bin/RELEASE.json ] && continue
      grep -qxF -- "$k" <<< "$listed" && continue
      [[ "$k" =~ $OPERATOR_KEYS ]] && { echo "note  s3://$b/$k: an operator's file, not part of the release"; continue; }
      fail "s3://$b/$k: not in RELEASE.json"
    done
  done
}

if [ "$PART" = listener ] || [ "$PART" = all ]; then
  m=$tmp/listener.json
  if bucket "$LISTENER_BUCKET" us-east-1 "$m" bin/ deploy/ config/; then
    want=$(release_json_sha "$m" bin/superci-pull); commit=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["commit"])' "$m")
    if [ "$($A ec2 describe-instances --region us-east-1 --filters Name=tag:Name,Values=superci-pull-listener Name=instance-state-name,Values=running --query 'length(Reservations[].Instances[])' --output text)" = 0 ]; then
      echo "note  no listener instance is running"
    else
      read -r got rev < <(${SSH:-$here/ssh.sh} 'p=$(systemctl show -p MainPID --value superci-pull); echo "$(sudo sha256sum /proc/$p/exe | cut -d" " -f1 | grep . || echo none) $(sudo /proc/$p/exe -version 2>/dev/null || echo none)"')
      if [ "$got" = "$want" ] && [ "$rev" = "$commit" ]; then ok "listener runs $commit ($got)"; else fail "listener runs sha256 $got, -version ${rev:-none}; RELEASE.json says $want, $commit"; fi
    fi
  fi
fi
if [ "$PART" = cache ] || [ "$PART" = all ]; then
  for r in $CACHE_REGIONS; do
    m=$tmp/cache-$r.json
    bucket "$CACHE_BUCKET_PREFIX-$r" "$r" "$m" bin/ || continue
    want=$(python3 -I -c 'import base64,sys; print(base64.b64encode(bytes.fromhex(sys.argv[1])).decode())' "$(release_json_sha "$m" bin/pullcache-lambda-arm64.zip)")
    for fn in $FUNCTIONS; do
      got=$($A lambda get-function-configuration --region "$r" --function-name "$fn" --query CodeSha256 --output text 2>/dev/null) || got=missing
      if [ "$got" = "$want" ]; then ok "function $fn $r"; else fail "function $fn $r: CodeSha256 $got, RELEASE.json's zip is $want"; fi
    done
  done
fi
[ $bad = 0 ] && echo "release verified: $PART" || echo "release NOT verified: $PART"
exit $bad
