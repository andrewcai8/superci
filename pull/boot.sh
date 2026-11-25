#!/bin/bash
# A superci-pull machine runs one job. It asks the listener for its one just-in-time runner, proving who it is with
# the identity document EC2 signed for it, runs the runner until it has taken its job, and powers off, which
# terminates it; so no job runs on a machine another job has used. Told it gets none (410), it powers off holding no
# runner, so no job is lost with it. The listener ends the machine itself when its job completes, and bounds the job
# from when it started and an idle runner by the machine's age; the machine also powers off after @LIFE_SECONDS@ s,
# whatever it is doing.
exec > >(tee /var/log/superci-pull.log > /dev/console) 2>&1
# Marks for the job timeline (pull/events.go): the moments of this machine's own clock the listener cannot see. Each is a
# line of name=epoch=ms since the previous mark[=value] in a spool (the cache daemon appends its own), and a sender posts
# the lines the listener has not taken to /mark with the identity document, as the ask for the runner does, so the machine
# needs no credential for them. A line stays in the spool until the listener answers 204, so a listener restart loses none.
SP=/run/superci-marks
: > $SP
echo 0 > $SP.sent
mark() { local now=${EPOCHREALTIME/[.,]/}; printf '%s\n' "$1=${now:0:10}.${now:10}=$(( ( now - ${last:-$now} ) / 1000 ))${2:+=$2}" >> $SP; last=$now; }
send() {
  local took n m code
  took=$(cat $SP.sent); n=$(wc -l < $SP)
  [ "$n" -gt "$took" ] || return 0
  n=$(( n < took + 20 ? n : took + 20 ))
  m=$(sed -n "$((took + 1)),${n}p" $SP | paste -sd,)
  code=$(curl -s -k --pinnedpubkey "$PIN" -m 10 -H "X-Superci-Marks: $m" -H "X-Superci-Sent: $(date +%s.%N)" --data-binary "$DOC" -o /dev/null -w '%{http_code}' "$L/mark")
  [ "$code" = 204 ] && echo "$n" > $SP.sent
}
mark boot.start "$(cut -d' ' -f1 /proc/uptime)"
( sleep @LIFE_SECONDS@; poweroff ) > /dev/null 2>&1 &
# vm.dirty_ratio: a job's cache restore untars 1.5 to 2 GB (node_modules, Go's modules and build), which at the default
# 20% waited on the volume's 250 MB/s; nothing on a one-job machine needs its writes on disk soon.
printf '%s\n' kernel.apparmor_restrict_unprivileged_userns=0 kernel.apparmor_restrict_unprivileged_unconfined=0 net.ipv4.ip_local_reserved_ports=50000-59999 vm.dirty_ratio=50 > /etc/sysctl.d/99-superci.conf
sysctl -eqp /etc/sysctl.d/99-superci.conf || :
usermod -aG docker runner 2> /dev/null || :
# First reads from a new volume are slow one at a time: the runner's own files are read 64 at a time first.
find /home/runner/bin -type f -print0 2> /dev/null | xargs -0 -r -P 64 -n 16 cat &> /dev/null
# The image's files a job reads next, read while the machine waits: the Node every action runs on, git, and Chrome (Go's
# mongoose tests drive it; read cold it added 1.8 to 4.8 s to them). A cold read of the image goes at about 43 MB/s.
( find /home/runner/externals /usr/lib/git-core /opt/google/chrome -type f -print0 2> /dev/null | xargs -0 -r -P 32 -n 16 cat &> /dev/null ) &
L='@LISTENER@'
PIN='@PIN@'
T=$(curl -s -m 2 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 21600' http://169.254.169.254/latest/api/token)
# The actions cache (pull/cache): starts its daemon from the region's cache bucket and wraps the runner's Node; a machine without the role runs on GitHub's own cache.
mark cache.start
CR=$(curl -s -m 2 -H "X-aws-ec2-metadata-token: $T" http://169.254.169.254/latest/meta-data/placement/region); PB=superci-pull-cache-243891825325-$CR; aws s3 cp --only-show-errors --region "$CR" "s3://$PB/bin/up.sh" - 2> /dev/null | PULLCACHE_BUCKET=$PB bash || :
mark cache.up
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
mark doc.got
( while sleep 1; do send; done ) > /dev/null 2>&1 &
SENDER=$!
call() { curl -s -k --pinnedpubkey "$PIN" -m 60 --data-binary "$DOC" -o "$2" -w '%{http_code}' "$L/$1"; }
( while sleep 5; do
    [ "$(curl -s -m 2 -o /dev/null -w '%{http_code}' -H "X-aws-ec2-metadata-token: $T" http://169.254.169.254/latest/meta-data/spot/instance-action)" = 200 ] || continue
    call notice /dev/null; break
  done ) &
umask 077
fails=0
while :; do
  [ -n "${asked:-}" ] || mark jit.sent
  asked=1
  code=$(call jit /run/superci-jit)
  echo "superci-pull: jit $code at $(date -u +%T.%N)"
  case $code in
    200)
      mark jit.answered 200
      export ACTIONS_RUNNER_INPUT_JITCONFIG=$(cat /run/superci-jit)
      rm -f /run/superci-jit
      mark runner.start
      (cd /home/runner && sudo --preserve-env=ACTIONS_RUNNER_INPUT_JITCONFIG -u runner -H ./run.sh) 2>&1 | while IFS= read -r line; do
        printf '%s\n' "$line"
        case $line in
          *'Listening for Jobs'*) mark runner.online ;;
          *'Running job:'*) mark job.running ;;
          *' completed with result'*) mark job.done ;;
        esac
      done
      mark runner.exit "${PIPESTATUS[0]}"
      break ;;
    410) mark jit.answered 410; break ;;
    429) sleep 3 ;;
    # The listener unreachable (a restart, a replacement) for 15 minutes: no runner is held, so ending loses nothing.
    *) fails=$((fails + 1)); [ $fails -ge 300 ] && break; sleep 3 ;;
  esac
done
mark poweroff
kill "$SENDER" 2> /dev/null
for _ in 1 2 3; do send; [ "$(cat $SP.sent)" = "$(wc -l < $SP)" ] && break; done
poweroff
