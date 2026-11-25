#!/bin/bash
# Replays an arrival pattern on real GitHub: pushes to BRANCH of megpt-mono, each push a run of one workflow, which
# workflow.py wrote. Each push is a commit on origin/main's tree with that workflow as .github/workflows/superci-load.yml,
# made with git plumbing, so no checkout of megpt-mono is written. Run it inside any checkout of megpt-mono; the first push
# creates the branch. Prints each push's time.
#
# A wave replay pushes one workflow every INTERVAL seconds, WAVES times, and one burst push after BURST_AT waves:
#   replay.sh BRANCH WAVES INTERVAL BURST_AT WORKFLOW
# A trace replay pushes, at each bucket's offset in the trace `gate replay-trace` wrote, the workflow of that bucket (the
# jobs that arrived in it, each sleeping its real run time). MAX_SLEEP caps one job's sleep (default 900 s); DRY_RUN=1 prints
# the schedule and each bucket's workflow size and pushes nothing:
#   replay.sh BRANCH TRACE.json
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
branch=$1
idx=$(mktemp); trap 'rm -f "$idx"' EXIT
head=$(git rev-parse origin/main)
commit() { # workflow-file message: a commit of origin/main's tree plus the workflow, on the previous commit of the branch
  GIT_INDEX_FILE=$idx git read-tree origin/main
  local blob; blob=$(git hash-object -w "$1")
  GIT_INDEX_FILE=$idx git update-index --add --cacheinfo "100644,$blob,.github/workflows/superci-load.yml"
  head=$(git commit-tree "$(GIT_INDEX_FILE=$idx git write-tree)" -p "$head" -m "$2")
}
push() { # message
  git push -q origin "$head:refs/heads/$branch" 2>&1 | grep -v '^remote:' || true
  echo "$(date -u +%T) pushed: $1 ($head)"
}
wait_until() { sleep $(( $1 - $(date +%s) > 0 ? $1 - $(date +%s) : 0 )); }
start=$(date +%s)

if [ $# -eq 2 ]; then
  trace=$2 max_sleep=${MAX_SLEEP:-900}
  wf=$(mktemp); trap 'rm -f "$idx" "$wf"' EXIT
  while read -r bucket offset legs; do
    python3 "$here/workflow.py" --trace "$trace" "$branch" "$bucket" "$max_sleep" > "$wf"
    if [ -n "${DRY_RUN:-}" ]; then echo "bucket $bucket at +${offset}s: $legs jobs, workflow $(wc -l < "$wf") lines"; continue; fi
    wait_until $(( start + offset ))
    commit "$wf" "superci-load: bucket $bucket"
    push "bucket $bucket, $legs jobs"
  done < <(python3 "$here/workflow.py" --schedule "$trace")
  exit 0
fi

waves=$2 interval=$3 burst_at=$4 workflow=$5
commit "$workflow" "superci-load: wave 1"
push "superci-load: wave 1"
for i in $(seq 2 "$waves"); do
  wait_until $(( start + (i - 1) * interval ))
  commit "$workflow" "superci-load: wave $i"
  push "superci-load: wave $i"
  if [ "$i" = "$burst_at" ]; then commit "$workflow" "superci-load: burst of the matrix"; push "superci-load: burst of the matrix"; fi
done
