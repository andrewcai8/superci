#!/bin/bash
# Prints the RELEASE.json of one tree of a release (see release-lib.sh): its commit, its build time and each file's
# sha256. Refuses, printing nothing on stdout, a commit not on andrewcai8/superci main, a file the release does not
# know, a script or config that differs from the commit's, and a Go binary (or the function's bootstrap) that does
# not carry the commit.
#
#   pull/deploy/manifest.sh <release directory> listener|cache
set -euo pipefail
DIR=${1:?release directory}; TREE=${2:?listener or cache}
here=$(cd "$(dirname "$0")" && pwd)
. "$here/release-lib.sh"
read -r COMMIT BUILT < <(python3 -I -c 'import json,sys; r=json.load(open(sys.argv[1])); print(r["commit"], r["built"])' "$DIR/REVISION")
on_main "$here" "$COMMIT"
keys=$(cd "$DIR/$TREE" && find . -type f ! -path ./bin/RELEASE.json | sed 's|^\./||' | LC_ALL=C sort)
[ -n "$keys" ] || { echo "refusing: $DIR/$TREE is empty" >&2; exit 1; }
for k in $keys; do
  f=$DIR/$TREE/$k
  src=$(source_of "$k") || { echo "refusing: $TREE/$k is not part of a release" >&2; exit 1; }
  case "$src" in
    binary)
      if [ "${k##*.}" = zip ]; then
        python3 -I -c 'import sys,zipfile; sys.exit(0 if sys.argv[2].encode() in zipfile.ZipFile(sys.argv[1]).read("bootstrap") else 1)' "$f" "$COMMIT"
      else
        grep -aqF -- "$COMMIT" "$f"
      fi || { echo "refusing: $TREE/$k does not carry $COMMIT" >&2; exit 1; } ;;
    *) git -C "$here" show "$COMMIT:$src" | cmp -s - "$f" || { echo "refusing: $TREE/$k differs from $COMMIT:$src" >&2; exit 1; } ;;
  esac
done
printf '{"commit":"%s","built":"%s","sha256":{' "$COMMIT" "$BUILT"
sep=""; for k in $keys; do printf '%s"%s":"%s"' "$sep" "$k" "$(sha256sum "$DIR/$TREE/$k" | cut -d' ' -f1)"; sep=","; done
printf '}}\n'
