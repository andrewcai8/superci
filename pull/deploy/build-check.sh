#!/bin/bash
# The checks build.sh runs on its build host before it builds: gofmt, go vet, the Go tests and agent.py's tests, with a
# python3 that is the machines' (the image's: the parity file records it as `tool python3 <version>`). The host is Amazon
# Linux 2023, whose python3 is 3.9 while the machines' is 3.12: the release of 0d60192 failed there on code 3.12 takes,
# and the fork's CI, on a newer Python still, had not said so. A python3 of another major.minor refuses, before any test.
#
#   pull/deploy/build-check.sh --want <parity file>           prints the machines' python3 as <major.minor>, or refuses
#   pull/deploy/build-check.sh <pull directory> <parity file> runs the checks there; python3, go and gofmt are the PATH's
set -euo pipefail
want() {
  local w
  w=$(awk '$1 == "tool" && $2 == "python3" { split($3, v, "."); print v[1] "." v[2]; exit }' "$1")
  [[ $w =~ ^[0-9]+\.[0-9]+$ ]] || { echo "refusing: $1 has no 'tool python3 <major.minor>' line" >&2; return 1; }
  echo "$w"
}
if [ "${1:-}" = --want ]; then want "${2:?parity file}"; exit; fi
dir=${1:?pull directory} parity=${2:?parity file}
w=$(want "$parity")
have=$(python3 -I -c 'import sys; print("%d.%d" % sys.version_info[:2])')
[ "$have" = "$w" ] || { echo "refusing: the tests' python3 is $have, the machines' is $w (tool python3 in the parity file)" >&2; exit 1; }
echo "build-check: python3 $have, the machines'"
cd "$dir"
# One check a line: under set -e a failure before the last && of a list stops nothing.
unformatted=$(gofmt -l .) || { echo "refusing: gofmt failed" >&2; exit 1; }
[ -z "$unformatted" ] || { echo "refusing: gofmt would change: $(echo $unformatted)" >&2; exit 1; }
go vet ./... || { echo "refusing: go vet failed" >&2; exit 1; }
go test -count=1 ./... || exit 1
# agent.py runs on the machines under that python3: its tests (the Go tests do not run them).
python3 -I -m unittest discover -s . -p test_agent.py
