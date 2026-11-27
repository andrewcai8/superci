#!/bin/bash
# Watches live canaries and sends a job back to Blacksmith the moment `gate job --canary` says it harms superci's jobs:
# it removes that job, and only that job, from megpt-mono's SUPERCI_JOBS. One check of every job each EVERY seconds (900;
# GitHub's read budget is shared, so never under 600), each check only while at least 1,500 reads remain. Stops at UNTIL.
#
#   pull/tools/gate/canary.sh SINCE UNTIL JOB...      # SINCE: when SUPERCI_JOBS listed the jobs; UNTIL: the end of the watch
#   EVERY=900 WORKFLOW=lint-test-backend.yml LIMITS=ci-limits.tsv LOG=dir pull/tools/gate/canary.sh ...
#
# Blacksmith's side is the 24 h before SINCE, when the jobs still ran there.
#
# Every read is on the superci GitHub App's installation budget (timeline/apptoken.py asks the listener for a token), and
# the floor reads that token's own budget. With no App token the watch refuses to start, and a check that cannot get one is
# skipped aloud; SUPERCI_PERSONAL_TOKEN=1 spends the shared personal token instead and says so. SUPERCI_JOBS stays on the
# personal token (`personal SUPERCI_JOBS`, logged): the App is not documented to hold Variables. pull/README.md lists both.
set -uo pipefail
R=Authentic-Intelligence/megpt-mono
here=$(cd "$(dirname "$0")" && pwd)
app=""
personal_ok=${SUPERCI_PERSONAL_TOKEN:-0}
pt=()
[ "$personal_ok" = 1 ] && pt=(--personal-token)
refresh_token() { # the App's token, in $app (this shell's memory, never printed); 1 when there is none and no override
  [ "$personal_ok" = 1 ] && return 0
  app=$(python3 -B "$here/../timeline/apptoken.py" 2> "$apperr") && [ -n "$app" ] && return 0
  app=""
  return 1
}
apperr=$(mktemp) || exit 2
trap 'rm -f "$apperr"' EXIT # on every way out: a refusal, the end of the watch, and (below) a signal, which bash does not run EXIT for by default
trap 'exit 130' INT
trap 'exit 143' TERM HUP
ghr() { if [ "$personal_ok" = 1 ]; then gh "$@"; else GH_TOKEN=$app gh "$@"; fi; } # a read or write on the budget the watch spends
exec 9>&2 # the log of an exception goes to the watch's own stderr, not into the captured output of the call
personal() { local name=$1; shift; echo "personal-token exception $name: the App lacks Variables: read and write; this call is on the shared personal budget" >&9; gh "$@"; }
since=$1 until=$2; shift 2
every=${EVERY:-900}
log=${LOG:-.}
[ "$every" -ge 600 ] || { echo "refusing: EVERY under 600 s spends the shared GitHub read budget" >&2; exit 2; }
[ "$(aws --profile ci-runners sts get-caller-identity --query Account --output text)" = 243891825325 ] || { echo "refusing: not ci-runners" >&2; exit 2; }
if [ "$personal_ok" = 1 ]; then echo "spending the shared personal GitHub token's budget (SUPERCI_PERSONAL_TOKEN=1), not the App's" >&2
else refresh_token || { cat "$apperr" >&2; echo "refusing: no GitHub App token. The canary reads GitHub on the superci App's budget, not the shared personal token; fix the listener's /github/token or set SUPERCI_PERSONAL_TOKEN=1 to spend the personal budget knowingly." >&2; exit 2; }; fi
jobs_now() { # prints SUPERCI_JOBS; returns 1 when it is unset, 2 when GitHub did not answer (gh 2.46 has no `variable get`)
  local out err
  err=$(mktemp)
  out=$(personal SUPERCI_JOBS api "repos/$R/actions/variables/SUPERCI_JOBS" --jq .value 2> "$err") && jq -e 'type == "array"' <<<"$out" > /dev/null && { rm -f "$err"; echo "$out"; return 0; }
  grep -q "Not Found" "$err" && { rm -f "$err"; return 1; }
  rm -f "$err"
  return 2
}
remove() { # job: drop it from SUPERCI_JOBS, and delete the variable when it was the last; retried while GitHub does not answer
  local now left code try
  for try in 1 2 3 4 5; do
    now=$(jobs_now) && break
    code=$?
    [ $code -eq 1 ] && { echo "SUPERCI_JOBS already unset"; return; }
    echo "GitHub did not answer reading SUPERCI_JOBS (try $try)"; sleep 10
  done
  [ -n "$now" ] || { echo "SUPERCI_JOBS unread after 5 tries: deleting it, which sends every job back to Blacksmith"; personal SUPERCI_JOBS variable delete SUPERCI_JOBS -R $R; return; }
  left=$(jq -c --arg j "$1" 'map(select(. != $j))' <<<"$now")
  if [ "$left" = "[]" ]; then personal SUPERCI_JOBS variable delete SUPERCI_JOBS -R $R; else personal SUPERCI_JOBS variable set SUPERCI_JOBS -R $R --body "$left"; fi
  echo "SUPERCI_JOBS was $now, now $(jobs_now || echo unset)"
}
while [ "$(date -u +%s)" -lt "$(date -u -d "$until" +%s)" ]; do
  sleep "$every"
  for job in "$@"; do
    listed=$(jobs_now) || { echo "$(date -u +%TZ) $job: SUPERCI_JOBS unset or unread, no check"; continue; }
    jq -e --arg j "$job" 'index($j) != null' <<<"$listed" >/dev/null || continue
    refresh_token || { echo "$(date -u +%TZ) $job: no GitHub App token, no check ($(head -c 300 "$apperr" | tr '\n' ' '))"; continue; }
    left=$(ghr api rate_limit --jq .resources.core.remaining) # the budget of the token the check reads with
    [ "$left" -ge 1500 ] || { echo "$(date -u +%TZ) $job: skipped, $left GitHub reads left"; continue; }
    out="$log/canary-${job//[^A-Za-z0-9._-]/_}-$(date -u +%H%M).txt"
    python3 -B "$here/gate.py" --profile ci-runners ${pt[@]+"${pt[@]}"} job "$job" --workflow "${WORKFLOW:-lint-test-backend.yml}" --branch-glob '*' --since "$since" \
      --baseline-until "$since" --canary ${LIMITS:+--limits "$LIMITS"} >"$out" 2>&1
    code=$?
    echo "$(date -u +%TZ) $job: $(grep -E '^canary ' "$out" || tail -1 "$out") (exit $code, $out)"
    [ $code -eq 3 ] && remove "$job"
  done
done
