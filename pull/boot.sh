#!/bin/bash
# A superci-pull machine runs one job. It asks the listener for its one just-in-time runner, proving who it is with
# the identity document EC2 signed for it, runs the runner until it has taken its job, and powers off, which
# terminates it; so no job runs on a machine another job has used. Told it gets none (410), it powers off holding no
# runner, so no job is lost with it. The listener ends the machine itself when its job completes, and bounds the job
# from when it started and an idle runner by the machine's age; the machine also powers off after @LIFE_SECONDS@ s,
# whatever it is doing.
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
# The actions cache (pull/cache): starts its daemon from the region's cache bucket and wraps the runner's Node; a machine without the role runs on GitHub's own cache.
CR=$(curl -s -m 2 -H "X-aws-ec2-metadata-token: $T" http://169.254.169.254/latest/meta-data/placement/region); PB=superci-pull-cache-243891825325-$CR; aws s3 cp --only-show-errors --region "$CR" "s3://$PB/bin/up.sh" - 2> /dev/null | PULLCACHE_BUCKET=$PB bash || :
# Hydration (pull/README.md): with a manifest for this label, main's caches are unpacked in the background while the machine
# asks for its runner and waits for its job, which takes them by rename. The runner's job-started hook stops it when the job starts.
# The hydration is its own process group (setsid), whose leader's id is in $HR/.pid, so the hook ends all of it; it never blocks the machine.
# It drops to the runner user with setpriv, which execs in place (runuser forks and may start a session of its own, out of the hook's reach).
export HR=/home/runner/hydrated HM=/run/superci-hydrate.json PB CR
install -d -o runner -g runner "$HR"
cat > /home/runner/superci-hydrate-stop.sh << 'HOOK'
@HYDRATE_STOP@
HOOK
chmod 755 /home/runner/superci-hydrate-stop.sh
printf '\nSUPERCI_HYDRATED=%s\nACTIONS_RUNNER_HOOK_JOB_STARTED=%s\n' "$HR" /home/runner/superci-hydrate-stop.sh >> /home/runner/.env
hydrate() {
  local sp=/run/superci-marks t0 out val
  us() { local n=${EPOCHREALTIME/[.,]/}; echo "$n"; }
  # A mark for the job timeline (the spool of boot.sh's marks, when the listener keeps one): name=epoch=ms since the start[=value].
  mark_hydrate() { local n=$1; [ -w "$sp" ] && printf '%s=%s.%s=%s%s\n' "$2" "${n:0:10}" "${n:10}" "$(( (n - t0) / 1000 ))" "${3:+=$3}" >> "$sp"; return 0; }
  t0=$(us); mark_hydrate "$t0" hydrate.start
  echo $$ > "$HR/.pid"
  if [ -x /usr/local/bin/pullcache ] && aws s3 cp --only-show-errors --region "$CR" "s3://$PB/bin/hydrate/@LABEL@.json" "$HM" 2> /dev/null; then
    chown -R runner:runner "$HR"
    out=$(timeout --foreground -k 5 110 setpriv --reuid=runner --regid=runner --init-groups -- env HOME=/home/runner PULLCACHE_BUCKET="$PB" PULLCACHE_REGION="$CR" /usr/local/bin/pullcache hydrate -timeout 100s "$HM" "$HR" || :)
    echo "$out"
    val=$(printf '%s\n' "$out" | sed -n 's|^hydrated \([0-9]*\)/\([0-9]*\) entries \([0-9]*\) MB$|\1/\2:\3MB|p' | tail -1)
    mark_hydrate "$(us)" hydrate.done "$val"
  fi
  rm -f "$HR/.pid"
}
setsid bash -c "$(declare -f hydrate); hydrate" &
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
      (cd /home/runner && sudo --preserve-env=ACTIONS_RUNNER_INPUT_JITCONFIG -u runner -H ./run.sh)
      break ;;
    410) break ;;
    429) sleep 3 ;;
    # The listener unreachable (a restart, a replacement) for 15 minutes: no runner is held, so ending loses nothing.
    *) fails=$((fails + 1)); [ $fails -ge 300 ] && break; sleep 3 ;;
  esac
done
poweroff
