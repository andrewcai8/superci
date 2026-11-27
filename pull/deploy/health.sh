#!/bin/bash
# The listener's health, every 15 seconds: the process answers /healthz (its reconciler finished a pass within two
# minutes), and the instance holds the Elastic IP machines reach. A listener that hangs is not crashed, so systemd
# never restarts it; this restarts it, and only a restart that did not cure it, or a failure a restart cannot cure,
# asks the Auto Scaling group to replace the instance.
#
#   /healthz failing 30 s on a listener that has run 60 s   -> systemctl restart (once per episode)
#   the same again after that restart, or a second hang within 10 minutes of the first -> mark the instance Unhealthy
#   any failure (Elastic IP lost, a listener that never settles) lasting 300 s -> mark the instance Unhealthy
#
# A listener that was just started (a release's restart, systemd's own, ours) is not judged for 60 s, and a state
# systemd reports as starting or stopping is never judged, so a restart is never mistaken for a hang. Marking waits
# until the instance is ten minutes old (a dud image is not replaced in a loop) and is made once; it asks the group to
# ignore its own 300 s grace period, which would otherwise drop the call. Time is the host's uptime, in seconds.
set -uo pipefail
D=${HEALTH_DIR:-/run/superci-pull-health}
UPTIME=${HEALTH_UPTIME:-/proc/uptime}
EIP_FILE=${HEALTH_EIP_FILE:-/etc/superci-pull/eip}
UNIT=superci-pull
RESTART_AFTER=30
SETTLE=60
REPLACE_AFTER=300
CALM=600
YOUNG=600
mkdir -p "$D"
now=$(cut -d. -f1 "$UPTIME")
get() { cat "$D/$1" 2> /dev/null || echo 0; }
T=$(curl -s -m 2 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token)
md() { curl -s -m 2 -H "X-aws-ec2-metadata-token: $T" "http://169.254.169.254/latest/meta-data/$1"; }

state=""
enter=0
while IFS== read -r k v; do
  case $k in
    ActiveState) state=$v ;;
    ActiveEnterTimestampMonotonic) enter=$((${v:-0} / 1000000)) ;;
  esac
done < <(systemctl show "$UNIT" -p ActiveState -p ActiveEnterTimestampMonotonic 2> /dev/null)
settled=0
case $state in
  active) [ $((now - enter)) -ge $SETTLE ] && settled=1 ;;
  failed) settled=1 ;;
esac

why=""
hz_bad=0
code=$(curl -s -m 5 -o "$D/healthz.body" -w '%{http_code}' localhost:8080/healthz 2> /dev/null)
if [ "$code" != 200 ]; then
  hz_bad=1
  why="healthz: ${code/000/no answer} $(head -c 200 "$D/healthz.body" 2> /dev/null)"
fi
eip_bad=0
eip=$(cat "$EIP_FILE" 2> /dev/null || true)
if [ -n "$eip" ] && [ "$(md public-ipv4)" != "$eip" ]; then
  eip_bad=1
  why="${why:+$why; }this instance does not hold the Elastic IP $eip"
fi

if [ $hz_bad = 0 ] && [ $eip_bad = 0 ]; then
  rm -f "$D/first" "$D/since"
  if [ "$(get restarts)" -gt 0 ] && [ $((now - $(get restarted))) -ge $CALM ]; then
    rm -f "$D/restarts" "$D/restarted"
  fi
  exit 0
fi

[ -f "$D/first" ] || echo "$now" > "$D/first"
first=$(get first)
echo "health check failing for $((now - first)) s: $why (service $state, active $((now - enter)) s)"

act=""
if [ $hz_bad = 1 ] && [ $settled = 1 ]; then
  [ -f "$D/since" ] || echo "$now" > "$D/since"
  if [ $((now - $(get since))) -ge $RESTART_AFTER ]; then
    if [ "$(get restarts)" -eq 0 ]; then act=restart; else act=replace; fi
  fi
else
  rm -f "$D/since"
fi
if [ -z "$act" ] && [ $((now - first)) -ge $REPLACE_AFTER ]; then act=replace; fi

case $act in
  restart)
    echo "restarting $UNIT"
    echo 1 > "$D/restarts"
    echo "$now" > "$D/restarted"
    rm -f "$D/since"
    systemctl restart "$UNIT"
    ;;
  replace)
    [ -f "$D/marked" ] && exit 0
    if [ "$now" -le $YOUNG ]; then
      echo "this instance is $now s old: not asking to be replaced yet"
      exit 0
    fi
    echo "asking the Auto Scaling group to replace this instance"
    aws autoscaling set-instance-health --region us-east-1 --instance-id "$(md instance-id)" --health-status Unhealthy \
      --no-should-respect-grace-period && touch "$D/marked"
    ;;
esac
exit 0
