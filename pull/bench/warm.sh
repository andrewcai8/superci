#!/bin/bash
# On the listener: the warm-pool load test's two steps around the burst.
#   warm.sh fill N   caps the set at N machines and keeps N idle runners; returns once N are idle.
#   warm.sh drain N  once the queue has N jobs assigned (the burst is all handed out), drops the pool to 0, so each
#                    machine whose job ends is told it is not needed.
set -euo pipefail
state() { curl -s localhost:8080/state; }
case $1 in
fill)
  curl -sf -X POST "localhost:8080/max?n=$2"
  curl -sf -X POST "localhost:8080/pool?n=$2"
  until [ "$(state | jq .idle)" -ge "$2" ]; do
    state | jq -c '{t: (now | todate), machines, booting, idle, total_regs, regs_window, refused}'
    sleep 30
  done
  echo "pool full at $(date -u +%T)" ;;
drain)
  until [ "$(state | jq .stats.totalAssignedJobs)" -ge "$2" ]; do sleep 0.5; done
  curl -sf -X POST "localhost:8080/pool?n=0"
  echo "pool dropped at $(date -u +%T.%N)" ;;
esac
