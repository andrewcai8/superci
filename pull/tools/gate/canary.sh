#!/bin/bash
# Watches live canaries and sends a job back to Blacksmith the moment `gate job --canary` says it harms superci's jobs:
# it removes that job, and only that job, from megpt-mono's SUPERCI_JOBS. One check of every job each EVERY seconds (900;
# GitHub's read budget is shared, so never under 600), each check only while at least 1,500 reads remain. Stops at UNTIL.
#
#   pull/tools/gate/canary.sh SINCE UNTIL JOB...      # SINCE: when SUPERCI_JOBS listed the jobs; UNTIL: the end of the watch
#   EVERY=900 WORKFLOW=lint-test-backend.yml LIMITS=ci-limits.tsv LOG=dir pull/tools/gate/canary.sh ...
#
# Blacksmith's side is the 24 h before SINCE, when the jobs still ran there.
set -uo pipefail
R=Authentic-Intelligence/megpt-mono
here=$(cd "$(dirname "$0")" && pwd)
since=$1 until=$2; shift 2
every=${EVERY:-900}
log=${LOG:-.}
[ "$every" -ge 600 ] || { echo "refusing: EVERY under 600 s spends the shared GitHub read budget" >&2; exit 2; }
[ "$(aws --profile ci-runners sts get-caller-identity --query Account --output text)" = 243891825325 ] || { echo "refusing: not ci-runners" >&2; exit 2; }
jobs_now() { # prints SUPERCI_JOBS; returns 1 when it is unset, 2 when GitHub did not answer (gh 2.46 has no `variable get`)
  local out
  out=$(gh api "repos/$R/actions/variables/SUPERCI_JOBS" --jq .value 2>&1) && { echo "$out"; return 0; }
  [[ $out == *"Not Found"* ]] && return 1 || return 2
}
remove() { # job: drop it from SUPERCI_JOBS, and delete the variable when it was the last; retried while GitHub does not answer
  local now left code try
  for try in 1 2 3 4 5; do
    now=$(jobs_now) && break
    code=$?
    [ $code -eq 1 ] && { echo "SUPERCI_JOBS already unset"; return; }
    echo "GitHub did not answer reading SUPERCI_JOBS (try $try)"; sleep 10
  done
  [ -n "$now" ] || { echo "SUPERCI_JOBS unread after 5 tries: deleting it, which sends every job back to Blacksmith"; gh variable delete SUPERCI_JOBS -R $R; return; }
  left=$(jq -c --arg j "$1" 'map(select(. != $j))' <<<"$now")
  if [ "$left" = "[]" ]; then gh variable delete SUPERCI_JOBS -R $R; else gh variable set SUPERCI_JOBS -R $R --body "$left"; fi
  echo "SUPERCI_JOBS was $now, now $(jobs_now || echo unset)"
}
while [ "$(date -u +%s)" -lt "$(date -u -d "$until" +%s)" ]; do
  sleep "$every"
  for job in "$@"; do
    listed=$(jobs_now) || { echo "$(date -u +%TZ) $job: SUPERCI_JOBS unset or unread, no check"; continue; }
    jq -e --arg j "$job" 'index($j) != null' <<<"$listed" >/dev/null || continue
    left=$(gh api rate_limit --jq .resources.core.remaining)
    [ "$left" -ge 1500 ] || { echo "$(date -u +%TZ) $job: skipped, $left GitHub reads left"; continue; }
    out="$log/canary-${job//[^A-Za-z0-9._-]/_}-$(date -u +%H%M).txt"
    python3 -B "$here/gate.py" --profile ci-runners job "$job" --workflow "${WORKFLOW:-lint-test-backend.yml}" --branch-glob '*' --since "$since" \
      --baseline-until "$since" --canary ${LIMITS:+--limits "$LIMITS"} >"$out" 2>&1
    code=$?
    echo "$(date -u +%TZ) $job: $(grep -E '^canary ' "$out" || tail -1 "$out") (exit $code, $out)"
    [ $code -eq 3 ] && remove "$job"
  done
done
