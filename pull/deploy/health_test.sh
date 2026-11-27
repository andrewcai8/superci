#!/bin/bash
# health.sh driven with fakes of curl, systemctl and aws on a clock the test sets (the host's uptime, in a file): a
# hung listener is restarted once and the instance marked Unhealthy only when that did not cure it, and a release's
# restart, a single failed check and a young instance are never acted on. Run by health_test.go; needs bash.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
F=$T/fake; mkdir -p "$F" "$T/bin"
export PATH=$T/bin:$PATH FAKE=$F HEALTH_DIR=$T/state HEALTH_UPTIME=$F/uptime HEALTH_EIP_FILE=$T/eip
HEALTH=${HEALTH:-$here/health.sh}

cat > "$T/bin/curl" <<'SH'
#!/bin/bash
url=""; out=""
while [ $# -gt 0 ]; do case $1 in -o) out=$2; shift ;; -m | -H | -X | -w) shift ;; -*) ;; *) url=$1 ;; esac; shift; done
case $url in
  *api/token) echo tok ;;
  *meta-data/public-ipv4) cat "$FAKE/ip" ;;
  *meta-data/instance-id) echo i-0test ;;
  localhost:8080/healthz)
    case $(cat "$FAKE/healthz") in
      ok) code=200; body=ok ;;
      503) code=503; body="reconciler idle for 2m5s" ;;
      *) code=000; body="" ;;
    esac
    [ -n "$out" ] && printf %s "$body" > "$out"
    printf %s "$code" ;;
esac
SH
cat > "$T/bin/systemctl" <<'SH'
#!/bin/bash
case $1 in
  show) echo "ActiveState=$(cat "$FAKE/state")"; echo "ActiveEnterTimestampMonotonic=$(( $(cat "$FAKE/enter") * 1000000 ))" ;;
  restart) echo "restart $2" >> "$FAKE/systemctl.log"; cut -d. -f1 "$FAKE/uptime" > "$FAKE/enter"; echo active > "$FAKE/state"
    [ -f "$FAKE/cure" ] && echo ok > "$FAKE/healthz" ;;
esac
exit 0
SH
cat > "$T/bin/aws" <<'SH'
#!/bin/bash
echo "$*" >> "$FAKE/aws.log"
exit "$(cat "$FAKE/aws-rc")"
SH
printf '#!/bin/sh\nexit 0\n' > "$T/bin/sleep"; printf '#!/bin/sh\ncat\n' > "$T/bin/tee"
chmod 755 "$T/bin/"*

failed=0
check() { if [ "$1" = 0 ]; then echo "ok    $2"; else echo "FAIL  $2"; failed=1; fi; }
new() {
  rm -rf "$T/state" "$F"/*; : > "$F/aws.log"; : > "$F/systemctl.log"
  echo ok > "$F/healthz"; echo active > "$F/state"; echo 0 > "$F/enter"; echo 0 > "$F/aws-rc"
  echo 203.0.113.9 > "$T/eip"; echo 203.0.113.9 > "$F/ip"; echo "0.00 0.00" > "$F/uptime"
}
tick() { echo "$1.00 0.00" > "$F/uptime"; bash "$HEALTH" >> "$T/health.out" 2>&1; }
ticks() { local t; for t in $(seq "$1" "${3:-15}" "$2"); do tick "$t"; done; } # first, last, step: a check every 15 s unless told
restarts() { grep -c '^restart superci-pull$' "$F/systemctl.log"; }
marks() { grep -c 'autoscaling set-instance-health' "$F/aws.log"; }
expect() { # name restarts marks
  local r m; r=$(restarts); m=$(marks)
  if [ "$r" = "$2" ] && [ "$m" = "$3" ]; then check 0 "$1"; else check 1 "$1 (restarts $r, want $2; marked $m, want $3)"; tail -3 "$T/health.out" | sed 's/^/      /'; fi
}

new; ticks 1000 1300 60; expect "a healthy listener is left alone" 0 0

new; echo hang > "$F/healthz"; tick 1000; echo ok > "$F/healthz"; ticks 1015 1100 85; echo 503 > "$F/healthz"; tick 1115; echo ok > "$F/healthz"; ticks 1130 1400 90
expect "a failed check or two, then healthy again, is left alone" 0 0

new; echo hang > "$F/healthz"; touch "$F/cure"; ticks 2000 2015; expect "a hung listener is not restarted before 30 s" 0 0
ticks 2030 2300 90; expect "a hung listener is restarted once, and the restart that cures it marks nothing" 1 0
echo hang > "$F/healthz"; rm "$F/cure"; ticks 2400 2415; expect "a second hang within 10 minutes is not restarted again" 1 0
ticks 2430 2700; expect "and marks the instance Unhealthy exactly once" 1 1
grep -q -- '--instance-id i-0test --health-status Unhealthy --no-should-respect-grace-period' "$F/aws.log"; check $? "through the call that ignores the group's grace period, for this instance"

new; echo hang > "$F/healthz"; ticks 2000 2030; expect "a hung listener is restarted" 1 0
ticks 2045 2115; expect "and the restarted listener is given 60 s, then 30 s more, before anything else" 1 0
ticks 2130 2145; expect "a restart that did not cure it marks the instance Unhealthy once" 1 1
ticks 2160 3000 60; expect "and nothing else is done while the group replaces it" 1 1

new; echo hang > "$F/healthz"; touch "$F/cure"; ticks 2000 2030; ticks 2045 2700 60
echo hang > "$F/healthz"; ticks 2730 2790; expect "10 minutes after a cured restart a hang is restarted again, not marked" 2 0

new; echo hang > "$F/healthz"; echo deactivating > "$F/state"; ticks 1000 1015; echo activating > "$F/state"; tick 1030
echo active > "$F/state"; echo 1040 > "$F/enter"; ticks 1045 1090; echo ok > "$F/healthz"; ticks 1105 1400 100
expect "a release's restart (stopping, starting, answering nothing for 50 s) is not a hang" 0 0

new; echo hang > "$F/healthz"; echo activating > "$F/state"; ticks 1000 1285; expect "a listener that crash-loops is not restarted by the check" 0 0
ticks 1300 1330; expect "and is replaced once it has failed 300 s" 0 1

new; echo hang > "$F/healthz"; echo failed > "$F/state"; ticks 1000 1030; expect "a listener systemd gave up on is restarted" 1 0

new; echo 198.51.100.1 > "$F/ip"; ticks 1000 1285; expect "an instance without the Elastic IP is not restarted, and waits 300 s" 0 0
ticks 1300 1500 100; expect "then marks itself Unhealthy once" 0 1

new; echo 198.51.100.1 > "$F/ip"; ticks 100 400; expect "an instance under ten minutes old is not replaced" 0 0

new; echo 198.51.100.1 > "$F/ip"; echo 1 > "$F/aws-rc"; ticks 1000 1330; echo 0 > "$F/aws-rc"; ticks 1345 1500
[ "$(marks)" -gt 1 ] && [ -f "$T/state/marked" ]; check $? "a refused call is made again, and once it succeeds it is not made again"
n=$(marks); ticks 1515 1700 90; [ "$(marks)" = "$n" ]; check $? "and no call follows it"

new; : > "$T/boot.out"
printf '%s\n' '#!/bin/bash' 'echo "$@" >> "$FAKE/aws.log"; case "$*" in *associate-address*) exit 1 ;; esac' > "$T/bin/aws"
bash "$here/listener-boot.sh" > "$T/boot.out" 2>&1
grep -q -- 'set-instance-health.*--health-status Unhealthy --no-should-respect-grace-period' "$F/aws.log"; check $? "the boot of an instance that cannot take the Elastic IP asks to be replaced past the group's grace period"

grep -q '^OnUnitActiveSec=15s$' "$here/superci-pull-health.timer" && grep -q '^AccuracySec=1s$' "$here/superci-pull-health.timer"; check $? "the timer fires every 15 s, accurate to a second (systemd coalesces to a minute otherwise)"
grep -q '^TimeoutStopSec=15$' "$here/superci-pull.service"; check $? "a restart of a hung listener waits 15 s for its stop, not 90"
grep -q '^OnBootSec=1min$' "$here/superci-pull-health.timer"; check $? "the first check comes a minute after boot, not five: a new host's hung listener is seen at once (health.sh judges nothing in a listener's first 60 s and marks nothing under 600 s of uptime)"
exit $failed
