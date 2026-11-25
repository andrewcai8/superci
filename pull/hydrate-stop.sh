#!/bin/bash
# The runner's job-started hook (boot.sh sets ACTIONS_RUNNER_HOOK_JOB_STARTED to it). The machine's one job has
# started, so the hydration still unpacking main's caches stops now and never competes with the job for CPU or disk.
# What it had finished is in place and what it had not stays out of reach (an entry appears whole by rename). The
# hydration is a process group of its own and its leader's id is in $SUPERCI_HYDRATED/.pid. This never fails the job.
pidfile=${SUPERCI_HYDRATED:-/home/runner/hydrated}/.pid
pgid=$(cat "$pidfile" 2> /dev/null)
case $pgid in '' | *[!0-9]*) exit 0 ;; esac
if kill -TERM -- "-$pgid" 2> /dev/null; then
  for _ in $(seq 20); do kill -0 -- "-$pgid" 2> /dev/null || break; sleep 0.1; done
  kill -KILL -- "-$pgid" 2> /dev/null
fi
rm -f "$pidfile"
exit 0
