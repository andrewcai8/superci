#!/bin/bash
# The runner's job-started hook (boot.sh sets ACTIONS_RUNNER_HOOK_JOB_STARTED to it). The machine's one job has
# started, so the hydration still unpacking main's caches stops now and never competes with the job for CPU or disk.
# What it had finished is in place and what it had not stays out of reach (an entry appears whole by rename). The
# hydration is a process group of its own and its leader's id is in $SUPERCI_HYDRATED/.pid. This never fails the job:
# the runner runs it with `bash -e -o pipefail`, so every command that may fail is guarded.
# The job's start block, printed first (the hook's output is in the job's "Set up job" log): where the job runs and what the
# machine did before it, from IMDS and the marks boot.sh spools (name=epoch=ms since the previous mark[=value]). Read before
# the stop below, so a pre-read still running is seen as running. It prints what it can and never delays the job by over 3 s.
block() {
  local ud m=${SUPERCI_MARKS:-/run/superci-marks} im=${SUPERCI_IMDS:-http://169.254.169.254/latest} t name ro hd pd pv now pre idle
  mk() { awk -F= -v n="$1" -v k="$2" '$1 == n { print $k; exit }' "$m" 2> /dev/null || :; }
  sec() { case $1 in '' | *[!0-9]*) echo - ;; *) echo $(($1 / 1000)) ;; esac; }
  t=$(curl -s -m 0.5 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' "$im/api/token" 2> /dev/null || :)
  md() { [ -z "$t" ] || curl -s -m 0.5 -H "X-aws-ec2-metadata-token: $t" "$im/$1" 2> /dev/null || :; }
  name=$(sed -n 's/.*"agentName": *"\([^"]*\)".*/\1/p' "${SUPERCI_RUNNER:-/home/runner/.runner}" 2> /dev/null | head -1 || :)
  now=${EPOCHREALTIME/[.,]/}; now=${now:0:10}
  ro=$(mk runner.online 2); ro=${ro%%.*}
  case $ro in '' | *[!0-9]*) idle=- ;; *) idle=$((now - ro)) ;; esac
  pd=$(mk preread.done 3); pv=$(mk preread.done 4)
  if [ -n "$pv" ]; then pre="$pv in $(sec "$pd") s"; elif [ -n "$(mk preread.start 2)" ]; then pre="running at job start, stopped now"; else pre=none; fi
  hd=$(mk hydrate.done 3)
  ud=$(md user-data)
  echo "superci: $(md meta-data/instance-id) $(md meta-data/instance-type) $(md meta-data/instance-life-cycle) $(md meta-data/placement/availability-zone) $(md meta-data/ami-id) boot ${ud:+$(printf %s "$ud" | sha256sum | cut -c1-8)}"
  echo "superci: set ${name%-i-*}  up $(cut -d' ' -f1 /proc/uptime | cut -d. -f1) s  runner online $idle s before the job  hydrate $(sec "$hd") s $(mk hydrate.done 4)  pre-read $pre"
}
block || :
pidfile=${SUPERCI_HYDRATED:-/home/runner/hydrated}/.pid
pgid=$(cat "$pidfile" 2> /dev/null || :)
case $pgid in '' | *[!0-9]*) exit 0 ;; esac
if kill -TERM -- "-$pgid" 2> /dev/null; then
  for _ in $(seq 20); do kill -0 -- "-$pgid" 2> /dev/null || break; sleep 0.1; done
  kill -KILL -- "-$pgid" 2> /dev/null || :
fi
rm -f "$pidfile" || :
exit 0
