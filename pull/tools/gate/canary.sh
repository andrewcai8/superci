#!/bin/bash
# Watches live canaries and sends a job back to Blacksmith the moment `gate job --canary` says it harms superci's jobs:
# it removes that job, and only that job, from megpt-mono's SUPERCI_JOBS. The first check of every job comes at once,
# right after the start probe, then one each EVERY seconds (900; GitHub's read budget is shared, so never under 600), each check only
# while at least 1,500 reads remain. Each tick checks and then sleeps; a tick that would start at or after UNTIL does not, and the watch ends.
#
#   pull/tools/gate/canary.sh SINCE UNTIL JOB...      # SINCE: when SUPERCI_JOBS listed the jobs; UNTIL: the end of the watch
#   EVERY=900 WORKFLOW=lint-test-backend.yml LIMITS=ci-limits.tsv LOG=dir pull/tools/gate/canary.sh ...
#
# Blacksmith's side is the 24 h before SINCE, when the jobs still ran there.
#
# Every read is on the superci GitHub App's installation budget (timeline/apptoken.py asks the listener for a token), and
# the floor reads that token's own budget. With no App token the watch refuses to start, and a check that cannot get one is
# skipped aloud; SUPERCI_PERSONAL_TOKEN=1 spends the shared personal token instead and says so.
#
# Every call on an App token also sets GH_SHIM_KEEP_TOKEN=1: a host `gh` wrapper that routes reads through another App keeps this token
# instead of replacing it. The personal-token paths do not set it; the wrapper's routing is the operator's policy there.
#
# SUPERCI_JOBS, the fleet switch, is read and written with the App's `variables` token (actions_variables: write on megpt-mono
# alone). The listener hands out a token it holds while it has over 10 minutes left, so the watch keeps one at most 300 s and drops
# it the moment GitHub refuses a call made with it (401/403), asks once more and tries again. The watch does not start when it
# cannot read SUPERCI_JOBS (a probe before the loop). The rollback never waits on the endpoint for long: the token is asked for 3
# times (30 s each), and when it is refused the rollback fails loudly (a ROLLBACK FAILED line on stdout and stderr naming the
# cause: the listener, an expired token, GitHub; the job still listed; the next tick tries again) unless SUPERCI_PERSONAL_TOKEN=1,
# in which case the App is asked once (10 s) and the write goes out on the personal token, announced (`personal-token exception
# SUPERCI_JOBS write`). A gate that exits 2 (a refusal) is logged as `NO CHECK`, never as a check. pull/README.md lists the exception.
#
# The operator hears of it within minutes (Nov 28 00:57:56Z: test-go was rolled back on its queue wait p90 and nobody knew for 59 minutes,
# because the only record was a line in a log):
#   ON_ROLLBACK=<command>   run once after every rollback this watch makes, with one more argument: a one-line summary (UTC time, the job,
#                           the gate's reason line, SUPERCI_JOBS before and after). Also run when the rollback's write fails (the job is
#                           still on superci, and the line says so), on each failed tick. And once when a job has had no check for 3 ticks
#                           running (no App token, SUPERCI_JOBS unreadable, under the budget floor, a gate that refused or crashed), with why.
#                           The command line is run by `bash -c` with the summary appended as its last argument, under `env -i` with only PATH,
#                           HOME, LANG and the NOTIFY_* variables (never GH_TOKEN, GITHUB_TOKEN or AWS credentials), for NOTIFY_TIMEOUT s (60): TERM
#                           then, 5 s later, KILL to its whole process group, its output a file read for 250 bytes at most. Its failure is
#                           logged and never stops the watch or undoes the rollback.
#   STOP_ON_ROLLBACK=1      exit 3 at the end of the tick that made a rollback, once ON_ROLLBACK has run, so a wrapper that runs the watch as a
#                           background task is woken. The tick's other jobs are still checked first (and may be rolled back), so the exit
#                           leaves no job unchecked in that tick; but the watch is over, and a wrapper must restart it for the jobs
#                           SUPERCI_JOBS still lists. A rollback that could not be written does not stop the watch: the next tick tries again.
#                           A job already gone from SUPERCI_JOBS when the rollback read it (another watch, a `set`) is not a rollback of this watch.
# A re-route by hand uses the same token and the same fallback, with no watch, no aws and no actions token:
#   pull/tools/gate/canary.sh set '["test-go","verify-tools"]'     # SUPERCI_JOBS := that JSON array of job names
#   pull/tools/gate/canary.sh unset                                # delete SUPERCI_JOBS: every job back to Blacksmith
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
ghr() { if [ "$personal_ok" = 1 ]; then gh "$@"; else GH_TOKEN=$app GH_SHIM_KEEP_TOKEN=1 gh "$@"; fi; } # a read or write on the budget the watch spends
exec 9>&2 # the log of an exception goes to the watch's own stderr, not into the captured output of the call
# The variables token: SUPERCI_JOBS is read and written with it, never with the App's read token.
vt=""        # this shell's copy of it, never printed
vt_at=0      # when it was minted
vt_failed=0  # when the last ask was refused
vt_err=""    # and why, with no token in it (apptoken.py scrubs its messages)
drop_vt() { vt=""; vt_at=0; }
# The listener hands out the token it holds while it has more than 10 minutes left, so a token may expire 10 minutes after the ask:
# it is kept VT_TTL seconds, which is never over 300, and dropped as soon as GitHub refuses a call made with it.
get_vt() { # [tries]: the variables token in $vt; 1 when there is none. Bounded: tries x (TOKEN_TIMEOUT 30 s + 5 s). Under SUPERCI_PERSONAL_TOKEN=1 one ask of 10 s, so the personal fallback stays immediate
  local tries=${1:-3} try now rc ttl=${VT_TTL:-300} timeout=${TOKEN_TIMEOUT:-30}
  [ "$ttl" -gt 300 ] && ttl=300
  [ "$personal_ok" = 1 ] && { tries=1; timeout=${TOKEN_TIMEOUT:-10}; }
  now=$(date -u +%s)
  [ -n "$vt" ] && [ $((now - vt_at)) -lt "$ttl" ] && return 0
  vt=""
  [ "$vt_failed" -gt 0 ] && [ $((now - vt_failed)) -lt 60 ] && return 1 # asked and refused a moment ago: not again for each call of one rollback
  for try in $(seq 1 "$tries"); do
    vt=$(SUPERCI_TOKEN_TIMEOUT=$timeout python3 -B "$here/../timeline/apptoken.py" variables 2> "$apperr"); rc=$?
    [ $rc -eq 0 ] && [ -n "$vt" ] && { vt_at=$(date -u +%s); vt_failed=0; return 0; }
    vt=""
    vt_err=$(head -c 300 "$apperr" | tr '\n' ' ')
    [ $rc -eq 3 ] && break # the installation lacks the permission: asking again changes nothing
    [ "$try" -lt "$tries" ] && sleep 5
  done
  vt_failed=$(date -u +%s)
  return 1
}
vt_why() { echo "$vt_err"; }
vgh() { # a gh read of SUPERCI_JOBS: on the variables token ($vt, got by get_vt); with none, on the personal token only under SUPERCI_PERSONAL_TOKEN=1; else 98
  if [ -n "$vt" ]; then GH_TOKEN=$vt GH_SHIM_KEEP_TOKEN=1 gh "$@"; elif [ "$personal_ok" = 1 ]; then gh "$@"; else return 98; fi
}
personal_fallback() { local name=$1; shift; echo "personal-token exception $name: the variables token was refused and SUPERCI_PERSONAL_TOKEN=1; this call is on the shared personal budget" >&9; gh "$@"; }
vwrite() { # a gh write of SUPERCI_JOBS (gh args): on the variables token; GitHub refusing it (the token expired or was revoked) drops it, asks once more and writes again; the personal token only under SUPERCI_PERSONAL_TOKEN=1 (the one named exception), said aloud. 1 with $fail_cause when nothing works
  local err fresh=0
  fail_cause=""
  while [ -n "$vt" ]; do
    err=$(mktemp)
    GH_TOKEN=$vt GH_SHIM_KEEP_TOKEN=1 gh "$@" 2> "$err" && { rm -f "$err"; return 0; }
    wr_err=$(head -c 2000 "$err" | tr '\n' ' ' | scrub | head -c 200 | sed -E 's/ +$//'); rm -f "$err"
    echo "writing SUPERCI_JOBS with the variables token failed: $wr_err" >&9
    if [ "$fresh" = 1 ]; then fail_cause="GitHub refused the write with a fresh variables token ($wr_err)"; break; fi
    if [ "$personal_ok" = 1 ] || [[ $wr_err == *"Not Found"* ]]; then fail_cause="GitHub refused the write with the variables token ($wr_err)"; break; fi
    fresh=1; drop_vt
    get_vt 1 && continue
    fail_cause="GitHub refused the write with the variables token ($wr_err), which may have expired, and the listener would not give a new one ($(vt_why))"
  done
  [ -n "$fail_cause" ] || fail_cause="the listener would not give a variables token ($(vt_why))"
  [ "$personal_ok" = 1 ] && { personal_fallback "SUPERCI_JOBS write" "$@" && return 0; fail_cause="$fail_cause, and the write on the personal token failed too"; }
  return 1
}
jobs_now() { # prints SUPERCI_JOBS; 1 when it is unset, 2 when GitHub did not answer, 3 when there is no token to read it with, 4 when GitHub refused the token (gh 2.46 has no `variable get`)
  local out err
  [ -n "$vt" ] || [ "$personal_ok" = 1 ] || return 3
  err=$(mktemp)
  out=$(vgh api "repos/$R/actions/variables/SUPERCI_JOBS" --jq .value 2> "$err") && jq -e 'type == "array"' <<<"$out" > /dev/null && { rm -f "$err"; echo "$out"; return 0; }
  grep -q "Not Found" "$err" && { rm -f "$err"; return 1; }
  grep -qE "HTTP 40[13]|Bad credentials|not accessible by integration" "$err" && { rm -f "$err"; return 4; }
  rm -f "$err"
  return 2
}
read_jobs() { # [tries]: $jobs_val := SUPERCI_JOBS, with jobs_now's status; a token GitHub refused is dropped, asked for again once and the read made again (never 4)
  local rc
  jobs_val=$(jobs_now); rc=$?
  if [ $rc -eq 4 ]; then
    drop_vt
    [ "$personal_ok" = 1 ] || get_vt "${1:-1}"
    jobs_val=$(jobs_now); rc=$?
    [ $rc -eq 4 ] && rc=2
  fi
  return $rc
}
rb_fail=""
rollback_failed() { # why: the line the operator reads when SUPERCI_JOBS could not be written
  local line="ROLLBACK FAILED: $1. SUPERCI_JOBS is unchanged. Fix the listener's /github/token (scope variables), or run SUPERCI_PERSONAL_TOKEN=1 $0 unset"
  echo "$line"
  echo "$line" >&2
  rb_fail=$line
  return 1
}
scrub() { sed -E 's/gh[pousr]_[A-Za-z0-9_]+|github_pat_[A-Za-z0-9_]+|eyJ[A-Za-z0-9_.-]{20,}|AKIA[0-9A-Z]{16}|ASIA[0-9A-Z]{16}/<token>/g'; } # a GitHub token, a JWT, an AWS access key id
notify() { # line: tell the operator (ON_ROLLBACK); a failure is logged and changes nothing else
  local out rc f pid v nv=()
  [ -n "${ON_ROLLBACK:-}" ] || return 0
  # The notifier gets PATH, HOME, LANG and NOTIFY_* and nothing else: the watch's GH_TOKEN, GITHUB_TOKEN and AWS credentials stay out of it.
  for v in "${!NOTIFY_@}"; do nv+=("$v=${!v}"); done
  f=$(mktemp) || { echo "$(date -u +%TZ) ON_ROLLBACK not run: no temp file" >&9; return 0; }
  # A hard bound: TERM at NOTIFY_TIMEOUT, KILL 5 s later, to the whole group (timeout leads its own), whatever the notifier traps or leaves behind.
  # Its output goes to a file capped at 256 KiB (a notifier that floods is killed by the limit), never a pipe a background child could hold or bash could buffer.
  ( ulimit -f 256 2> /dev/null; exec timeout -k 5 "${NOTIFY_TIMEOUT:-60}" env -i PATH="$PATH" HOME="${HOME:-/}" LANG="${LANG:-C}" ${nv[@]+"${nv[@]}"} \
      bash -c "$ON_ROLLBACK"' "$@"' canary-notify "$(tr '\n' ' ' <<<"$1" | scrub)" ) < /dev/null > "$f" 2>&1 &
  pid=$!
  wait "$pid"; rc=$?
  kill -KILL -- "-$pid" 2> /dev/null # what the notifier left running in its group
  if [ $rc -eq 0 ]; then rm -f "$f"; return 0; fi
  if [ $rc -eq 124 ] || [ $rc -eq 137 ]; then out="timed out after ${NOTIFY_TIMEOUT:-60} s"; else out=$(head -c 2000 "$f" | tr '\n' ' ' | scrub | head -c 250); fi # scrubbed before it is cut, so no token is cut in two
  rm -f "$f"
  out="$(date -u +%TZ) ON_ROLLBACK failed (exit $rc): $out"
  echo "$out"
  echo "$out" >&9
  return 0
}
checked() { printf -v "streak_${1//[^A-Za-z0-9_]/_}" %s 0; } # job: a verdict was had, so the run of ticks with no check starts over
nocheck() { # job why: the tick's log line for a job that was not checked; the 3rd tick running tells the operator, once
  local var="streak_${1//[^A-Za-z0-9_]/_}" n
  echo "$(date -u +%TZ) $1: $2"
  n=$(( ${!var:-0} + 1 )); printf -v "$var" %s "$n"
  [ "$n" -eq 3 ] && notify "$(date -u +%Y-%m-%dT%H:%M:%SZ) canary: no check of $1 for 3 ticks running (EVERY ${every}s), so a harm to it would not roll it back: $2"
}
jobs_write() { # [delete | set JSON]: one write of SUPERCI_JOBS, as the rollback and the re-route make it; 1 (loudly) when it could not be made
  get_vt 3 || [ "$personal_ok" = 1 ] || { rollback_failed "the listener would not give a variables token ($(vt_why))"; return; }
  if [ "$1" = delete ]; then vwrite variable delete SUPERCI_JOBS -R $R; else vwrite variable set SUPERCI_JOBS -R $R --body "$2"; fi && return 0
  rollback_failed "$fail_cause"
}
remove() { # job: drop it from SUPERCI_JOBS, and delete the variable when it was the last; retried while GitHub does not answer
  local now="" left code try after # now stays empty when no read answers, so the delete below sends every job back
  rb_state=""; rb_before=""; rb_after="" # for the notice: unset (nothing to roll back), or done with SUPERCI_JOBS before and after
  get_vt 3 || [ "$personal_ok" = 1 ] || { rollback_failed "the listener would not give a variables token to remove $1 with ($(vt_why))"; return 1; }
  for try in 1 2 3 4 5; do
    read_jobs 3 && { now=$jobs_val; break; }
    code=$?
    [ $code -eq 1 ] && { echo "SUPERCI_JOBS already unset"; rb_state=unset; return; }
    [ $code -eq 3 ] && { rollback_failed "no variables token to read SUPERCI_JOBS with ($(vt_why))"; return 1; }
    echo "GitHub did not answer reading SUPERCI_JOBS (try $try)"; sleep 10
  done
  if [ -z "$now" ]; then
    echo "SUPERCI_JOBS unread after 5 tries: deleting it, which sends every job back to Blacksmith"
    jobs_write delete || return 1
    rb_state=done; rb_before="$listed (the rollback could not re-read it)"; rb_after=unset
    return
  fi
  if ! jq -e --arg j "$1" 'index($j) != null' <<<"$now" > /dev/null; then # another watch or a `set` took it out between the tick's read and this one: nothing was rolled back here
    echo "$1 is no longer in SUPERCI_JOBS ($now): removed by another watch or by hand, nothing to write"; rb_state=gone; return 0
  fi
  left=$(jq -c --arg j "$1" 'map(select(. != $j))' <<<"$now")
  if [ "$left" = "[]" ]; then jobs_write delete; else jobs_write set "$left"; fi || return 1
  read_jobs 1 && after=$jobs_val || after=unset
  echo "SUPERCI_JOBS was $now, now $after"
  rb_state=done; rb_before=$now; rb_after=$after
}
jobs_helper() { # set JSON | unset: the operator's re-route, on the same token and fallback as the rollback
  local usage="usage: $0 set '[\"job\",...]' | $0 unset" now after rc
  case "${1:-}" in
    set) [ $# -eq 2 ] && jq -e 'type == "array" and all(type == "string")' <<<"$2" > /dev/null 2>&1 || { echo "$usage" >&2; return 2; } ;;
    unset) [ $# -eq 1 ] || { echo "$usage" >&2; return 2; } ;;
  esac
  get_vt 3 || [ "$personal_ok" = 1 ] || { rollback_failed "the listener would not give a variables token ($(vt_why))"; return 1; }
  read_jobs 3; rc=$?; now=$jobs_val
  case $rc in 0) ;; 1) now=unset ;; *) now=unread ;; esac
  [ "$1" = unset ] && [ "$now" = unset ] && { echo "SUPERCI_JOBS already unset"; return 0; }
  if [ "$1" = set ]; then jobs_write set "$2"; else jobs_write delete; fi || return 1
  read_jobs 1 && after=$jobs_val || after=unset
  echo "SUPERCI_JOBS was $now, now $after"
}
case "${1:-}" in set | unset) jobs_helper "$@"; exit $? ;; esac
since=$1 until=$2; shift 2
every=${EVERY:-900}
log=${LOG:-.}
[ "$every" -ge 600 ] || { echo "refusing: EVERY under 600 s spends the shared GitHub read budget" >&2; exit 2; }
[ "$(aws --profile ci-runners sts get-caller-identity --query Account --output text)" = 243891825325 ] || { echo "refusing: not ci-runners" >&2; exit 2; }
if [ "$personal_ok" = 1 ]; then echo "spending the shared personal GitHub token's budget (SUPERCI_PERSONAL_TOKEN=1), not the App's" >&2
else refresh_token || { cat "$apperr" >&2; echo "refusing: no GitHub App token. The canary reads GitHub on the superci App's budget, not the shared personal token; fix the listener's /github/token or set SUPERCI_PERSONAL_TOKEN=1 to spend the personal budget knowingly." >&2; exit 2; }; fi
# A watch that cannot read SUPERCI_JOBS cannot roll a job back (an old listener answers the read token for scope=variables, and every
# variable read is then refused), so it does not start. No token at all is not that: every tick then skips its check aloud.
if get_vt 1 || [ "$personal_ok" = 1 ]; then
  read_jobs 1; rc=$?
  [ $rc -eq 2 ] && { echo "refusing: SUPERCI_JOBS cannot be read with the variables token (GitHub refused it or did not answer), so this watch could not roll a job back. Is the listener built with the variables scope (an old one answers an actions token for scope=variables)? Check: deploy/ssh.sh \"curl -s -X POST 'localhost:8080/github/token?scope=variables'\"" >&2; exit 2; }
else echo "no variables token at start ($(vt_why)): each tick skips its check until SUPERCI_JOBS can be read" >&2; fi
until_s=$(date -u -d "$until" +%s) || { echo "refusing: UNTIL is not a date: $until" >&2; exit 2; }
# Each tick checks and then sleeps, so the first check comes right after the start probe, not EVERY seconds later (Nov 28: the unit-tests
# canary started 03:08:42Z and a shard failed at 03:13:37Z; the first check was due at 03:18:53Z). The watch never sleeps past UNTIL: when
# the next tick would start at or after it, the watch ends.
while [ "$(date -u +%s)" -lt "$until_s" ]; do
  stop=0
  for job in "$@"; do
    get_vt 1 || [ "$personal_ok" = 1 ] || { nocheck "$job" "no GitHub App token, no check (SUPERCI_JOBS, variables: $(vt_why))"; continue; }
    read_jobs 1; jrc=$?
    case $jrc in
      0) ;;
      1) checked "$job"; echo "$(date -u +%TZ) $job: SUPERCI_JOBS unset, nothing to check"; continue ;; # unset is not a failure to check: someone sent every job back (the watch's own rollback, `canary.sh unset`)
      *) nocheck "$job" "SUPERCI_JOBS unread, no check"; continue ;; # 2: GitHub did not answer, 3: no token
    esac
    listed=$jobs_val
    jq -e --arg j "$job" 'index($j) != null' <<<"$listed" >/dev/null || { checked "$job"; continue; } # off the list: nothing to check, the run of no-checks ends
    refresh_token || { nocheck "$job" "no GitHub App token, no check ($(head -c 300 "$apperr" | tr '\n' ' '))"; continue; }
    left=$(ghr api rate_limit --jq .resources.core.remaining) # the budget of the token the check reads with
    [ "$left" -ge 1500 ] || { nocheck "$job" "skipped, $left GitHub reads left"; continue; }
    out="$log/canary-${job//[^A-Za-z0-9._-]/_}-$(date -u +%H%M).txt"
    python3 -B "$here/gate.py" --profile ci-runners ${pt[@]+"${pt[@]}"} job "$job" --workflow "${WORKFLOW:-lint-test-backend.yml}" --branch-glob '*' --since "$since" \
      --baseline-until "$since" --canary ${LIMITS:+--limits "$LIMITS"} >"$out" 2>&1
    code=$?
    if [ $code -eq 2 ]; then # a refusal (a token the gate was refused, say), not a verdict: it keeps no job and says so
      nocheck "$job" "NO CHECK (the gate refused: $( (grep -m1 '^refusing' "$out" || tail -1 "$out") | head -c 250 | scrub)) (exit 2, $out)"
    elif [ $code -ne 0 ] && [ $code -ne 3 ]; then # a crash, a stop or a kill (1, 137, 143): no verdict, so no check, and it says so
      nocheck "$job" "NO CHECK (the gate failed: $(tail -1 "$out" | head -c 250 | scrub)) (exit $code, $out)"
    else
      checked "$job"
      verdict=$( (grep -E '^canary ' "$out" || tail -1 "$out") | head -c 2000 | scrub) # the log line and the notice carry the gate's words, scrubbed
      echo "$(date -u +%TZ) $job: $verdict (exit $code, $out)"
    fi
    if [ $code -eq 3 ]; then
      verdict=$(head -c 300 <<<"$verdict" | scrub)
      if remove "$job"; then
        if [ "$rb_state" = done ]; then # an unset variable means someone else already sent every job back: nothing was rolled back here
          notify "$(date -u +%Y-%m-%dT%H:%M:%SZ) canary rolled $job back to Blacksmith: $verdict. SUPERCI_JOBS was $rb_before, now $rb_after"
          [ "${STOP_ON_ROLLBACK:-0}" = 1 ] && stop=1 # after the rest of this tick's jobs have been checked
        fi
      else
        notify "$(date -u +%Y-%m-%dT%H:%M:%SZ) canary could NOT roll $job back, it is still on superci: $verdict. ${rb_fail:-ROLLBACK FAILED} SUPERCI_JOBS is $listed"
      fi
    fi
  done
  [ "$stop" = 1 ] && exit 3
  [ $(( $(date -u +%s) + every )) -lt "$until_s" ] || break
  sleep "$every"
done
exit 0 # the end of the watch is a clean exit, whatever the last tick's last test returned: 3 means a rollback (STOP_ON_ROLLBACK)
