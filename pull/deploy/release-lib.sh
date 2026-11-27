# Sourced by manifest.sh, release.sh, verify-release.sh and cache/deploy/release.sh.
# A release is a snapshot directory build.sh writes: REVISION (commit, build time), and one tree per destination
# whose paths are the S3 keys they are uploaded to: listener/ (s3://superci-pull-<account>/) and cache/ (each region's
# s3://superci-pull-cache-<account>-<region>/). bin/RELEASE.json in each tree names the commit, the build time and
# the sha256 of every other file in it.
LISTENER_BUCKET=superci-pull-243891825325
CACHE_BUCKET_PREFIX=superci-pull-cache-243891825325
CACHE_REGIONS="us-east-1 us-east-2"
FUNCTIONS="superci-pull-cache-broker superci-pull-cache-janitor superci-pull-cache-mirror"
# Objects under a release's prefixes that are not part of a release: written by an operator (the issuer, the
# hydration manifests) or by a lane's proof (dev-*).
OPERATOR_KEYS='^bin/(issuer|hydrate/[^/]+\.json|dev-[^/]+/.*)$'

# main_ref: fetches andrewcai8/superci's main exactly into refs/remotes/origin/main, refusing any other origin.
main_ref() {
  local git_dir=$1 url
  url=$(git -C "$git_dir" config remote.origin.url)
  case "$url" in
    https://github.com/andrewcai8/superci | https://github.com/andrewcai8/superci.git | git@github.com:andrewcai8/superci.git) ;;
    *) echo "refusing: remote origin is $url, not andrewcai8/superci" >&2; return 1 ;;
  esac
  git -C "$git_dir" fetch -q origin +refs/heads/main:refs/remotes/origin/main
}

# on_main <git dir> <commit>: the commit is andrewcai8/superci main or behind it.
on_main() {
  main_ref "$1" || return 1
  git -C "$1" merge-base --is-ancestor "$2" refs/remotes/origin/main || { echo "refusing: $2 is not on andrewcai8/superci main" >&2; return 1; }
}

# source_of <key>: the path at the commit a key's file is, or "binary" for a Go program built from it.
source_of() {
  case "$1" in
    bin/superci-pull | bin/superci-shim | bin/pullcache-linux-amd64 | bin/pullcache-linux-arm64 | bin/pullcache-lambda-arm64.zip) echo binary ;;
    bin/up.sh) echo pull/cache/deploy/machine-up.sh ;;
    deploy/*) echo "pull/$1" ;;
    config/config.json) echo pull/deploy/override.json ;;
    *) return 1 ;;
  esac
}

# release_json_sha <RELEASE.json file> <key>: the sha256 the manifest gives a key, empty when it names none.
release_json_sha() {
  python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["sha256"].get(sys.argv[2], ""))' "$1" "$2"
}

# released_object <bucket> <region> <key> <out file>: fetches the object to the file only if its sha256 is the one
# the bucket's bin/RELEASE.json names; otherwise prints why and fails.
released_object() {
  local b=$1 r=$2 k=$3 out=$4 m want got
  m=$(mktemp)
  aws --profile ci-runners s3 cp --quiet --region "$r" "s3://$b/bin/RELEASE.json" "$m" 2>/dev/null || { rm -f "$m"; echo "refusing: no bin/RELEASE.json in $b; release first" >&2; return 1; }
  want=$(release_json_sha "$m" "$k"); rm -f "$m"
  aws --profile ci-runners s3 cp --quiet --region "$r" "s3://$b/$k" "$out" 2>/dev/null || { echo "refusing: no $k in $b" >&2; return 1; }
  got=$(sha256sum "$out" | cut -d' ' -f1)
  [ -n "$want" ] && [ "$got" = "$want" ] || { echo "refusing: $k in $b has sha256 $got, its bin/RELEASE.json says ${want:-nothing}" >&2; return 1; }
}
