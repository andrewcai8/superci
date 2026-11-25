#!/bin/bash
# A superci-pull machine runs one job. It asks the listener for a just-in-time runner, proving who it is with the
# identity document EC2 signed for it, runs the runner until it has taken its job, and powers off, which terminates
# it; so no job runs on a machine another job has used. Told it is not needed (410), it powers off holding no runner,
# so no job is lost with it. A job may run @JOB_SECONDS@ s (the longest job's timeout and a margin); the machine
# powers off after @LIFE_SECONDS@ s whatever it is doing.
exec > >(tee /var/log/superci-pull.log > /dev/console) 2>&1
( sleep @LIFE_SECONDS@; poweroff ) > /dev/null 2>&1 &
printf '%s\n' kernel.apparmor_restrict_unprivileged_userns=0 kernel.apparmor_restrict_unprivileged_unconfined=0 net.ipv4.ip_local_reserved_ports=50000-59999 > /etc/sysctl.d/99-superci.conf
sysctl -eqp /etc/sysctl.d/99-superci.conf || :
usermod -aG docker runner 2> /dev/null || :
# First reads from a new volume are slow one at a time: the runner's own files are read 64 at a time first.
find /home/runner/bin -type f -print0 2> /dev/null | xargs -0 -r -P 64 -n 16 cat &> /dev/null
L='@LISTENER@'
PIN='@PIN@'
T=$(curl -s -m 2 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 21600' http://169.254.169.254/latest/api/token)
DOC=$(curl -s -m 2 -H "X-aws-ec2-metadata-token: $T" http://169.254.169.254/latest/dynamic/instance-identity/rsa2048)
call() { curl -s -k --pinnedpubkey "$PIN" -m 60 --data-binary "$DOC" -o "$2" -w '%{http_code}' "$L/$1"; }
( while sleep 5; do
    [ "$(curl -s -m 2 -o /dev/null -w '%{http_code}' -H "X-aws-ec2-metadata-token: $T" http://169.254.169.254/latest/meta-data/spot/instance-action)" = 200 ] || continue
    call notice /dev/null; break
  done ) &
umask 077
fails=0
while :; do
  code=$(call jit /run/superci-jit)
  echo "superci-pull: jit $code at $(date -u +%T.%N)"
  case $code in
    200)
      export ACTIONS_RUNNER_INPUT_JITCONFIG=$(cat /run/superci-jit)
      rm -f /run/superci-jit
      (cd /home/runner && timeout -s KILL @JOB_SECONDS@ sudo --preserve-env=ACTIONS_RUNNER_INPUT_JITCONFIG -u runner -H ./run.sh)
      break ;;
    410) break ;;
    429) sleep 3 ;;
    # The listener unreachable (a restart, a replacement) for 15 minutes: no runner is held, so ending loses nothing.
    *) fails=$((fails + 1)); [ $fails -ge 300 ] && break; sleep 3 ;;
  esac
done
poweroff
