#!/bin/bash
# Replays an arrival pattern on real GitHub: pushes to BRANCH of megpt-mono, each push a run of one workflow, which
# workflow.py wrote. Each push is a commit on origin/main's tree with that workflow as .github/workflows/superci-load.yml,
# made with git plumbing, so no checkout of megpt-mono is written. Run it inside any checkout of megpt-mono; the first push
# creates the branch. Prints each push's time. The tree is origin/main's when the replay starts, for every push: other
# sessions fetch the same clone, and a push whose tree took main's newer commits carries their files, which megpt's pre-push
# hook then checks (F2, Nov 27: it refused 24 of 30 buckets). A push that is refused stops the replay: one with a bucket
# missing measures another load.
#
# A wave replay pushes one workflow every INTERVAL seconds, WAVES times, and one burst push after BURST_AT waves:
#   replay.sh BRANCH WAVES INTERVAL BURST_AT WORKFLOW
# A trace replay pushes, at each bucket's offset in the trace `gate replay-trace` wrote, the workflow of that bucket (the
# jobs that arrived in it, each sleeping its real run time). MAX_SLEEP caps one job's sleep (default 900 s); DRY_RUN=1 prints
# the schedule and each bucket's workflow size and pushes nothing:
#   replay.sh BRANCH TRACE.json
# Before its first push a trace replay reads, through the listener, how many idle identities each label has at GitHub,
# and refuses unless each of the trace's dispatch sizes (deploy/override.json) has the legs its peak holds at once
# (workflow.py --need).
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
branch=$1
idx=$(mktemp); trap 'rm -f "$idx"' EXIT
base=$(git rev-parse origin/main)
head=$base
commit() { # workflow-file message: a commit of the base's tree plus the workflow, on the previous commit of the branch
  GIT_INDEX_FILE=$idx git read-tree "$base"
  local blob; blob=$(git hash-object -w "$1")
  GIT_INDEX_FILE=$idx git update-index --add --cacheinfo "100644,$blob,.github/workflows/superci-load.yml"
  head=$(git commit-tree "$(GIT_INDEX_FILE=$idx git write-tree)" -p "$head" -m "$2")
}
push() { # message
  local out
  if ! out=$(git push -q origin "$head:refs/heads/$branch" 2>&1); then
    printf '%s\n' "$out" | grep -v '^remote:' >&2 || true
    echo "$(date -u +%T) refused: $1 ($head): the replay stops" >&2
    exit 1
  fi
  echo "$(date -u +%T) pushed: $1 ($head)"
}
# Identities labelled per size: each leg holds one of its size's identities from its arrival to its end, so a size with
# fewer than the trace's peak waits for an identity, and the replay measures that instead of the plane (F2, Nov 27:
# superci-8cpu had 60 of 470, its relabel having moved 50 and said it was done).
need_identities() { # trace max_sleep
  local have
  if [ "${SUPERCI_TEST:-}" = 1 ] && [ -n "${REPLAY_LABELS_CMD:-}" ]; then
    have=$($REPLAY_LABELS_CMD) || { echo "refusing: the test's labels command failed" >&2; exit 1; }
  else
    have=$(bash "$here/../deploy/identity.sh" labels) || { echo "refusing: no count of the identities' labels (identity.sh labels)" >&2; exit 1; }
  fi
  local dispatch ov=$here/../deploy/override.json
  if [ "${SUPERCI_TEST:-}" = 1 ] && [ -n "${REPLAY_OVERRIDE:-}" ]; then ov=$REPLAY_OVERRIDE; fi
  dispatch=$(python3 -I -c 'import json,sys
print(",".join(sorted(l for l, s in json.load(open(sys.argv[1]))["sizes"].items() if s.get("dispatch"))))' "$ov") || { echo "refusing: no dispatch sizes read from $ov" >&2; exit 1; }
  python3 "$here/workflow.py" --check-need "$1" "$2" "$dispatch" <<< "$have" || { echo "refusing: relabel identities first (identity.sh relabel <size> superci-park superci-<size>)" >&2; exit 1; }
}
wait_until() { sleep $(( $1 - $(date +%s) > 0 ? $1 - $(date +%s) : 0 )); }
start=$(date +%s)

if [ $# -eq 2 ]; then
  trace=$2 max_sleep=${MAX_SLEEP:-900}
  wf=$(mktemp); trap 'rm -f "$idx" "$wf"' EXIT
  [ -n "${DRY_RUN:-}" ] || need_identities "$trace" "$max_sleep"
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
