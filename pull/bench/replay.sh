#!/bin/bash
# Replays an arrival pattern on real GitHub: one push every INTERVAL seconds to BRANCH of megpt-mono (each push a run
# of one wave, as workflow.py wrote it), WAVES times, and one burst push after BURST_AT waves. Run it in a checkout
# whose HEAD already holds the workflow; the first push creates the branch. Prints each push's time.
#
#   replay.sh BRANCH WAVES INTERVAL BURST_AT
set -euo pipefail
branch=$1 waves=$2 interval=$3 burst_at=$4
start=$(date +%s)
push() {
  git commit -q --allow-empty -m "$1"
  git push -q origin "HEAD:refs/heads/$branch" 2>&1 | grep -v '^remote:' || true
  echo "$(date -u +%T) pushed: $1"
}
git push -q origin "HEAD:refs/heads/$branch" 2>&1 | grep -v '^remote:' || true
echo "$(date -u +%T) pushed: wave 1 (the workflow's own commit)"
for i in $(seq 2 "$waves"); do
  sleep $(( start + (i - 1) * interval - $(date +%s) > 0 ? start + (i - 1) * interval - $(date +%s) : 0 ))
  push "superci-load: wave $i"
  if [ "$i" = "$burst_at" ]; then push "superci-load: burst of the matrix"; fi
done
