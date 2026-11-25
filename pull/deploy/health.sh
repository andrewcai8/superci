#!/bin/bash
# The listener's health, every minute: five failures in a row (systemd's restarts did not help) and the instance tells
# its Auto Scaling group it is unhealthy, which replaces it. A fresh instance is given ten minutes first.
set -uo pipefail
f=/run/superci-pull-health.fails
if curl -sf -m 5 localhost:8080/healthz > /dev/null; then
  rm -f $f
  exit 0
fi
n=$(( $(cat $f 2> /dev/null || echo 0) + 1 ))
echo $n > $f
echo "health check failed ($n in a row): $(curl -s -m 5 localhost:8080/healthz)"
up=$(cut -d. -f1 /proc/uptime)
if [ "$n" -ge 5 ] && [ "$up" -gt 600 ]; then
  T=$(curl -s -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token)
  ID=$(curl -s -H "X-aws-ec2-metadata-token: $T" http://169.254.169.254/latest/meta-data/instance-id)
  aws autoscaling set-instance-health --region us-east-1 --instance-id "$ID" --health-status Unhealthy
fi
