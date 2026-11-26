#!/bin/bash
# Watches live canaries and sends a job back to Blacksmith the moment `gate job --canary` says it harms superci's jobs:
# it removes that job, and only that job, from megpt-mono's SUPERCI_JOBS. One check of every job each EVERY seconds (900;
# GitHub's read budget is shared, so never under 600), each check only while at least 1,500 reads remain. Stops at UNTIL.
#
#   pull/tools/gate/canary.sh SINCE UNTIL JOB...      # SINCE: when SUPERCI_JOBS listed the jobs; UNTIL: the end of the watch
#   EVERY=900 LIMITS=ci-limits.tsv LOG=dir pull/tools/gate/canary.sh ...
set -uo pipefail
R=Authentic-Intelligence/megpt-mono
here=$(cd "$(dirname "$0")" && pwd)
since=$1 until=$2; shift 2
every=${EVERY:-900}
log=${LOG:-.}
[ "$every" -ge 600 ] || { echo "refusing: EVERY under 600 s spends the shared GitHub read budget" >&2; exit 2; }
[ "$(aws --profile ci-runners sts get-caller-identity --query Account --output text)" = 243891825325 ] || { echo "refusing: not ci-runners" >&2; exit 2; }
remove() { # job: drop it from SUPERCI_JOBS, and delete the variable when it was the last
  local now left
  now=$(gh variable get SUPERCI_JOBS -R $R 2>/dev/null) || { echo "SUPERCI_JOBS already unset"; return; }
  left=$(jq -c --arg j "$1" 'map(select(. != $j))' <<<"$now")
  if [ "$left" = "[]" ]; then gh variable delete SUPERCI_JOBS -R $R; else gh variable set SUPERCI_JOBS -R $R --body "$left"; fi
  echo "SUPERCI_JOBS was $now, now $(gh variable get SUPERCI_JOBS -R $R 2>/dev/null || echo unset)"
}
while [ "$(date -u +%s)" -lt "$(date -u -d "$until" +%s)" ]; do
  sleep "$every"
  for job in "$@"; do
    listed=$(gh variable get SUPERCI_JOBS -R $R 2>/dev/null || echo '[]')
    jq -e --arg j "$job" 'index($j) != null' <<<"$listed" >/dev/null || continue
    left=$(gh api rate_limit --jq .resources.core.remaining)
    [ "$left" -ge 1500 ] || { echo "$(date -u +%TZ) $job: skipped, $left GitHub reads left"; continue; }
    out="$log/canary-$job-$(date -u +%H%M).txt"
    python3 -B "$here/gate.py" --profile ci-runners job "$job" --branch-glob '*' --since "$since" --canary ${LIMITS:+--limits "$LIMITS"} >"$out" 2>&1
    code=$?
    echo "$(date -u +%TZ) $job: $(grep -E '^canary ' "$out" || tail -1 "$out") (exit $code, $out)"
    [ $code -eq 3 ] && remove "$job"
  done
done
