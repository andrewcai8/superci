#!/bin/bash
# A superci-pull machine: it asks the listener for a just-in-time runner, runs it until it has taken one job, and
# asks again, until the listener says it is not needed (410); then it powers off, which terminates it. It holds no
# runner when it ends, so no job is lost with it.
exec > >(tee /var/log/superci-pull.log > /dev/console) 2>&1
( sleep @MAX_SECONDS@; poweroff ) > /dev/null 2>&1 &
printf '%s\n' kernel.apparmor_restrict_unprivileged_userns=0 kernel.apparmor_restrict_unprivileged_unconfined=0 net.ipv4.ip_local_reserved_ports=50000-59999 > /etc/sysctl.d/99-superci.conf
sysctl -eqp /etc/sysctl.d/99-superci.conf || :
usermod -aG docker runner 2> /dev/null || :
# First reads from a new volume are slow one at a time: the runner's own files are read 64 at a time first.
find /home/runner/bin -type f -print0 2> /dev/null | xargs -0 -r -P 64 -n 16 cat &> /dev/null
L='@LISTENER@'
PIN='@PIN@'
T=$(curl -s -m 2 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 21600' http://169.254.169.254/latest/api/token)
ID=$(curl -s -m 2 -H "X-aws-ec2-metadata-token: $T" http://169.254.169.254/latest/meta-data/instance-id)
# The cache (pull/cache): starts the cache server from the bucket's bin/up.sh and wraps the runner's Node; a machine without the role runs on GitHub's own cache.
CR=$(curl -s -m 2 -H "X-aws-ec2-metadata-token: $T" http://169.254.169.254/latest/meta-data/placement/region); PB=${PULLCACHE_BUCKET:-superci-pull-cache-243891825325-$CR}; aws s3 cp --only-show-errors --region "$CR" "s3://$PB/bin/up.sh" - 2> /dev/null | PULLCACHE_BUCKET=$PB bash || :
call() { curl -s -k --pinnedpubkey "$PIN" -m 60 -o "$2" -w '%{http_code}' "$L/$1?id=$ID"; }
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
      fails=0
      export ACTIONS_RUNNER_INPUT_JITCONFIG=$(cat /run/superci-jit)
      rm -f /run/superci-jit
      (cd /home/runner && sudo --preserve-env=ACTIONS_RUNNER_INPUT_JITCONFIG -u runner -H ./run.sh)
      unset ACTIONS_RUNNER_INPUT_JITCONFIG
      rm -rf /home/runner/_work/* 2> /dev/null ;;
    410) break ;;
    429) sleep 3 ;;
    *) fails=$((fails + 1)); [ $fails -ge 90 ] && break; sleep 2 ;;
  esac
done
poweroff
