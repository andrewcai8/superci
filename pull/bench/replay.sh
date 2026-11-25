#!/bin/bash
# Replays an arrival pattern on real GitHub: one push every INTERVAL seconds to BRANCH of megpt-mono (each push a run
# of one wave, as workflow.py wrote it), WAVES times, and one burst push after BURST_AT waves. Each push is a commit on
# origin/main's tree with WORKFLOW as .github/workflows/superci-load.yml, made with git plumbing, so no checkout of
# megpt-mono is written. Run it inside any checkout of megpt-mono; the first push creates the branch. Prints each
# push's time.
#
#   replay.sh BRANCH WAVES INTERVAL BURST_AT WORKFLOW
set -euo pipefail
branch=$1 waves=$2 interval=$3 burst_at=$4 workflow=$5
idx=$(mktemp); trap 'rm -f "$idx"' EXIT
GIT_INDEX_FILE=$idx git read-tree origin/main
blob=$(git hash-object -w "$workflow")
GIT_INDEX_FILE=$idx git update-index --add --cacheinfo "100644,$blob,.github/workflows/superci-load.yml"
tree=$(GIT_INDEX_FILE=$idx git write-tree)
head=$(git rev-parse origin/main)
start=$(date +%s)
push() {
  head=$(git commit-tree "$tree" -p "$head" -m "$1")
  git push -q origin "$head:refs/heads/$branch" 2>&1 | grep -v '^remote:' || true
  echo "$(date -u +%T) pushed: $1 ($head)"
}
push "superci-load: wave 1"
for i in $(seq 2 "$waves"); do
  sleep $(( start + (i - 1) * interval - $(date +%s) > 0 ? start + (i - 1) * interval - $(date +%s) : 0 ))
  push "superci-load: wave $i"
  if [ "$i" = "$burst_at" ]; then push "superci-load: burst of the matrix"; fi
done
