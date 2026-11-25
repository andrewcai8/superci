#!/bin/bash
# The listener's health, every minute: the process answers /healthz, and the instance holds the Elastic IP machines
# reach. Five failures in a row (systemd's restarts did not help) and the instance tells its Auto Scaling group it is
# unhealthy, which replaces it. A fresh instance is given ten minutes first.
set -uo pipefail
f=/run/superci-pull-health.fails
T=$(curl -s -m 2 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token)
md() { curl -s -m 2 -H "X-aws-ec2-metadata-token: $T" "http://169.254.169.254/latest/meta-data/$1"; }
why=""
curl -sf -m 5 localhost:8080/healthz > /dev/null || why="healthz: $(curl -s -m 5 localhost:8080/healthz)"
eip=$(cat /etc/superci-pull/eip 2> /dev/null || true)
if [ -n "$eip" ] && [ "$(md public-ipv4)" != "$eip" ]; then
  why="${why:+$why; }this instance does not hold the Elastic IP $eip"
fi
if [ -z "$why" ]; then
  rm -f $f
  exit 0
fi
n=$(( $(cat $f 2> /dev/null || echo 0) + 1 ))
echo $n > $f
echo "health check failed ($n in a row): $why"
up=$(cut -d. -f1 /proc/uptime)
if [ "$n" -ge 5 ] && [ "$up" -gt 600 ]; then
  aws autoscaling set-instance-health --region us-east-1 --instance-id "$(md instance-id)" --health-status Unhealthy
fi
