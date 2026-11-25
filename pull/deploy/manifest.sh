#!/bin/bash
# Prints the RELEASE.json of a release made by build.sh, for the files named: its commit, its build time and each
# file's sha256. Refuses, printing nothing on stdout, a commit with changes git does not hold, a commit not on
# origin's main, and a Go binary (or the function's bootstrap) that does not carry the commit.
#
#   pull/deploy/manifest.sh <directory built by build.sh> <file>...
set -euo pipefail
DIR=${1:?directory built by build.sh}; shift
[ $# -gt 0 ] || { echo "usage: manifest.sh <directory> <file>..." >&2; exit 2; }
here=$(cd "$(dirname "$0")" && pwd)
read -r COMMIT BUILT < <(python3 -I -c 'import json,sys; r=json.load(open(sys.argv[1])); print(r["commit"], r["built"])' "$DIR/REVISION")
case "$COMMIT" in *-dirty|unknown|"") echo "refusing: $DIR was built from $COMMIT, not a commit" >&2; exit 1 ;; esac
git -C "$here" fetch -q origin main
git -C "$here" merge-base --is-ancestor "$COMMIT" refs/remotes/origin/main || { echo "refusing: $COMMIT is not on origin's main" >&2; exit 1; }
for f in "$@"; do
  case "$f" in
    *.zip) python3 -I -c 'import sys,zipfile; sys.exit(0 if sys.argv[2].encode() in zipfile.ZipFile(sys.argv[1]).read("bootstrap") else 1)' "$DIR/$f" "$COMMIT" ;;
    *.sh) true ;;
    *) grep -aqF -- "$COMMIT" "$DIR/$f" ;;
  esac || { echo "refusing: $DIR/$f does not carry $COMMIT" >&2; exit 1; }
done
printf '{"commit":"%s","built":"%s","sha256":{' "$COMMIT" "$BUILT"
sep=""; for f in "$@"; do printf '%s"%s":"%s"' "$sep" "$f" "$(sha256sum "$DIR/$f" | cut -d' ' -f1)"; sep=","; done
printf '}}\n'
