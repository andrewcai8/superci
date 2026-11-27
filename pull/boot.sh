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
# needs no credential for them. A line stays in the spool until the listener answers 204, so a listener restart loses none;
# one the listener can never take (413) is dropped.
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
  # The listener answers 413 to a marks header over 32 KiB, and a batch it refuses is sent again for ever: the batch is cut
  # to 30,000 bytes, and one line alone over that is dropped. A 413 anyway (a limit lower than ours) drops the batch's first line.
  while [ "${#m}" -gt 30000 ] && [ "$n" -gt $((took + 1)) ]; do
    n=$(( n - 1 )); m=$(sed -n "$((took + 1)),${n}p" $SP | paste -sd,)
  done
  [ "${#m}" -le 30000 ] || { echo $((took + 1)) > $SP.sent; return 0; }
  code=$(curl -s -k --pinnedpubkey "$PIN" -m 10 -H "X-Superci-Marks: $m" -H "X-Superci-Sent: $(date +%s.%N)" --data-binary "$DOC" -o /dev/null -w '%{http_code}' "$L/mark")
  case $code in 204) echo "$n" > $SP.sent ;; 413) echo $((took + 1)) > $SP.sent ;; esac
}
mark boot.start "$(cut -d' ' -f1 /proc/uptime)"
( sleep @LIFE_SECONDS@; poweroff ) > /dev/null 2>&1 &
# vm.dirty_ratio: a job's cache restore untars 1.5 to 2 GB (node_modules, Go's modules and build), which at the default
# 20% waited on the volume's 250 MB/s; nothing on a one-job machine needs its writes on disk soon.
printf '%s\n' kernel.apparmor_restrict_unprivileged_userns=0 kernel.apparmor_restrict_unprivileged_unconfined=0 net.ipv4.ip_local_reserved_ports=50000-59999 vm.dirty_ratio=50 > /etc/sysctl.d/99-superci.conf
sysctl -eqp /etc/sysctl.d/99-superci.conf || :
# The image's cloud-init leaves out growpart, so its root stays the 29 GB it was built at (7 GB free) on any volume: a
# hydrated machine ran out of space restoring the Swift toolchain. It grows to the volume in the background (about 1 s).
# ROOT_BYTES is the root's size before it grows: the part of the volume its snapshot holds, which preread() reads.
# ext4 writes a file out at once when it is renamed over another (auto_da_alloc): the worker-boundary tests' facts cache
# does that 27,000 times, which took 36 s on the volume's 3,000 IOPS and 11 s without it. A one-job machine needs no file
# to survive a crash.
disk() {
  local root
  root=$(findmnt -no SOURCE /)
  ROOT_BYTES=$(lsblk -bndo SIZE "$root")
  export ROOT_BYTES
  mount -o remount,noauto_da_alloc /
  ( growpart "/dev/$(lsblk -no PKNAME "$root")" "${root##*[!0-9]}" && resize2fs "$root" ) > /dev/null 2>&1 &
}
disk
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
# The image's apt goes to archive.ubuntu.com and security.ubuntu.com over HTTPS: from EC2 that hung for 120 s or failed in
# 2 of 5 fresh updates (a Swift job's native packages, hermetic's Chromium), where the region's EC2 mirror took 4 s each time.
mirror() {
  [ -n "$1" ] || return 0
  sed -i "s|https://archive.ubuntu.com/ubuntu/|http://$1.ec2.archive.ubuntu.com/ubuntu/|; s|https://security.ubuntu.com/ubuntu/|http://$1.ec2.archive.ubuntu.com/ubuntu/|" "${2:-/etc/apt/sources.list.d/ubuntu.sources}"
}
mirror "$CR"
mark cache.up
# Hydration (pull/README.md): with a manifest for this label, main's caches are unpacked in the background while the machine
# asks for its runner and waits for its job, which takes them by rename. The runner's job-started hook stops it when the job starts.
# The hydration is its own process group (setsid), whose leader's id is in $HR/.pid, so the hook ends all of it; it never blocks the machine.
# It drops to the runner user with setpriv, which execs in place (runuser forks and may start a session of its own, out of the hook's reach).
export HR=/home/runner/hydrated HM=/run/superci-hydrate.json PB CR
install -d -o runner -g runner "$HR"
: > "$HR/.warming" # until hydrate() is done; a dispatch machine waits on it (below)
cat > /home/runner/superci-hydrate-stop.sh << 'HOOK'
@HYDRATE_STOP@
HOOK
chmod 755 /home/runner/superci-hydrate-stop.sh
printf '\nSUPERCI_HYDRATED=%s\nACTIONS_RUNNER_HOOK_JOB_STARTED=%s\n' "$HR" /home/runner/superci-hydrate-stop.sh >> /home/runner/.env
hydrate() {
  local sp=/run/superci-marks t0 out val rs
  us() { local n=${EPOCHREALTIME/[.,]/}; echo "$n"; }
  # A mark for the job timeline (the spool of boot.sh's marks, when the listener keeps one): name=epoch=ms since the start[=value].
  mark_hydrate() { local n=$1; [ -w "$sp" ] && printf '%s=%s.%s=%s%s\n' "$2" "${n:0:10}" "${n:10}" "$(( (n - t0) / 1000 ))" "${3:+=$3}" >> "$sp"; return 0; }
  # The volume is restored from the image's snapshot as its blocks are first read: a first read costs 5 to 16 ms where a
  # restored block costs 0.5 ms, and libLLVM's 123 MB took 0.3 s or 3.6 to 9.2 s. Reading the snapshot's part of the root
  # with 32 reads in flight takes 120 to 140 s (the volume's 250 MB/s) and costs nothing, and 85% of jobs start on a machine
  # older than that. The job-started hook removes the pid file, which ends the read within half a second, so a job that
  # starts first sees what it would have seen.
  # Runs "$@" until it ends (full) or the job-started hook removes the pid file (stopped).
  until_job() {
    local p
    "$@" &> /dev/null &
    p=$!
    while kill -0 "$p" 2> /dev/null; do
      [ -e "$HR/.pid" ] || { kill "$p" 2> /dev/null; break; }
      sleep 0.5
    done
    wait "$p" && echo full || echo stopped
  }
  # A size with a read set (pull/readsets/<label>.list, released to bin/warm/) reads only the image files its jobs were
  # recorded reading, 64 at a time and through the page cache, and nothing else: a unit-tests shard read 1.2 GB of the
  # snapshot's 25 GiB, in 2.2 to 6.8 s on fresh machines against 270 s for the whole root (Nov 27). It runs before the
  # hydration, and the job-started hook ends it as it ends the rest. A path the image lacks costs nothing.
  readset() {
    local list=/run/superci-readset.list
    # An empty list reads nothing, so that machine pre-reads as before. Paths the image lacks (xargs' 123) mark the read
    # full:missing, the sign of a list recorded on another image.
    aws s3 cp --only-show-errors --region "$CR" "s3://$PB/bin/warm/@LABEL@.list" "$list" 2> /dev/null && grep -q . "$list" || return 1
    readall() {
      tr '\n' '\0' < "$list" | xargs -0 -r -P 64 -n 8 cat -- > /dev/null 2>&1
      case $? in 0) ;; 123) : > "$list.missing" ;; *) return 1 ;; esac
    }
    mark_hydrate "$(us)" readset.start
    val=$(until_job readall)
    [ "$val" = full ] && [ -e "$list.missing" ] && val=full:missing
    mark_hydrate "$(us)" readset.done "$val"
    [ -e "$HR/.pid" ] && systemctl start docker.service > /dev/null 2>&1
    return 0
  }
  preread() {
    local val trees=() d
    [[ ${ROOT_BYTES:-} =~ ^[1-9][0-9]*$ ]] && command -v fio > /dev/null && [ -e "$HR/.pid" ] || return 0
    mark_hydrate "$(us)" preread.start
    val=$(until_job fio --name=preread --filename="$(findmnt -no SOURCE /)" --size="$ROOT_BYTES" --readonly --rw=read --bs=1M --iodepth=32 --ioengine=libaio --direct=1)
    mark_hydrate "$(us)" preread.done "$val"
    # A direct read drops the block device's cached pages in its range: ext4's inode tables and directories of every tree
    # hydration and the warm reads just wrote. A 117,600-file walk took 0.19 s warm, 8.91 s after the read and 0.18 s
    # again, and a job's first link of the hydrated node_modules 10.2 s against Blacksmith's 0.9 s (Nov 26). So walk them
    # again while the machine still waits; a job that came first walks them itself. %s stats every file (GNU find stats a
    # file only for a directive that needs it), so the files' inode tables are read, not only the directories.
    [ "$val" = full ] || return 0
    for d in "$HR" /home/runner/externals /usr/lib/git-core /opt/google/chrome; do [ -d "$d" ] && trees+=("$d"); done
    mark_hydrate "$(us)" rewarm.done "$(until_job find "${trees[@]}" -printf '%s')"
    [ -e "$HR/.pid" ] || return 0
    # The walk reads inodes, not files: an idle machine's first node took 274-403 ms and docker 1.7-3.8 s (Blacksmith 41
    # and 813). These binaries in the page cache and dockerd up took them to 11 and 10 ms (Nov 26). One file at a time,
    # so a job that starts stops it within a file.
    # The libraries are 1.2 GB more: only a machine of 12 GB or more reads them, so the warm never presses a smaller one's
    # hydrated trees out of memory.
    local libs=()
    [ "$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)" -ge 12000000 ] 2> /dev/null && libs=(/usr/lib/python3 /usr/lib/x86_64-linux-gnu)
    systemctl start docker.service > /dev/null 2>&1 || :
    hot() {
      local f
      find "$(readlink -f "$(command -v node)")" /usr/bin/docker /usr/bin/dockerd /usr/bin/containerd /usr/bin/containerd-shim-runc-v2 \
        /usr/bin/runc /usr/bin/git /usr/lib/git-core "$(readlink -f "$(command -v python3)")" /home/runner/externals \
        "${libs[@]}" -type f -print0 2> /dev/null | while IFS= read -r -d '' f; do
        [ -e "$HR/.pid" ] || return 1
        cat -- "$f" > /dev/null 2>&1
      done
    }
    mark_hydrate "$(us)" warm.done "$(until_job hot)"
  }
  t0=$(us); mark_hydrate "$t0" hydrate.start
  echo $$ > "$HR/.pid"
  readset && rs=1
  if [ -x /usr/local/bin/pullcache ] && aws s3 cp --only-show-errors --region "$CR" "s3://$PB/bin/hydrate/@LABEL@.json" "$HM" 2> /dev/null; then
    chown -R runner:runner "$HR"
    out=$(timeout --foreground -k 5 110 setpriv --reuid=runner --regid=runner --init-groups -- env HOME=/home/runner PULLCACHE_BUCKET="$PB" PULLCACHE_REGION="$CR" /usr/local/bin/pullcache hydrate -timeout 100s "$HM" "$HR" || :)
    echo "$out"
    val=$(printf '%s\n' "$out" | sed -n 's|^hydrated \([0-9]*\)/\([0-9]*\) entries \([0-9]*\) MB$|\1/\2:\3MB|p' | tail -1)
    mark_hydrate "$(us)" hydrate.done "$val"
  fi
  [ -n "${rs:-}" ] || preread
  rm -f "$HR/.pid" "$HR/.warming"
}
setsid bash -c "$(declare -f hydrate); hydrate" &
DOC=$(curl -s -m 2 -H "X-aws-ec2-metadata-token: $T" http://169.254.169.254/latest/dynamic/instance-identity/rsa2048)
mark doc.got
( while sleep 1; do send; done ) > /dev/null 2>&1 &
SENDER=$!
call() { curl -s -k --pinnedpubkey "$PIN" -m 60 --data-binary "$DOC" -o "$2" -w '%{http_code}' "${@:3}" "$L/$1"; }
# The notice carries IMDS's instance-action, sent until the listener answers.
( while sleep 5; do
    A=$(curl -s -f -m 2 -H "X-aws-ec2-metadata-token: $T" http://169.254.169.254/latest/meta-data/spot/instance-action | tr -d '\r\n')
    [ -n "$A" ] && case $(call notice /dev/null -H "X-Superci-Action: $A") in [24]??) break ;; esac
  done ) &
uptime_s() { cut -d. -f1 /proc/uptime; }
# Dispatch mode (pull/agent.py): no runner is registered. The machine fetches its agent with the identity document and
# the pin, which proves itself to the dispatcher and runs the one job it is paired with, and powers off as below.
if [ '@MODE@' = dispatch ]; then
  umask 077
  # Ask for a job once the warm is done (a cold machine's node took 2.5 s, Blacksmith's 39 ms), or at @WARM_CAP_SECONDS@ s of uptime.
  while [ -e "$HR/.warming" ] && [ "$(uptime_s)" -lt @WARM_CAP_SECONDS@ ]; do sleep 0.5; done
  mark warm.ready
  fails=0
  while :; do
    code=$(call agent /run/superci-agent.py)
    echo "superci-pull: agent $code at $(date -u +%T.%N)"
    case $code in
      200) mark runner.start; python3 -I /run/superci-agent.py "$L" "$PIN"; mark runner.exit "$?"; break ;;
      410) break ;;
      *) fails=$((fails + 1)); [ $fails -ge 300 ] && break; sleep 3 ;;
    esac
  done
  mark poweroff
  kill "$SENDER" 2> /dev/null
  for _ in 1 2 3; do send; [ "$(cat $SP.sent)" = "$(wc -l < $SP)" ] && break; done
  poweroff
  exit 0
fi
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
      # The job-started hook runs only after "Set up job", which a running pre-read slowed by 19 s (Nov 26), so the
      # hydration ends as soon as the runner takes its job, by the same hook as the runner's user.
      (cd /home/runner && sudo --preserve-env=ACTIONS_RUNNER_INPUT_JITCONFIG -u runner -H ./run.sh) 2>&1 | while IFS= read -r line; do
        printf '%s\n' "$line"
        case $line in
          *'Listening for Jobs'*) mark runner.online ;;
          *'Running job:'*) mark job.running; sudo -u runner /home/runner/superci-hydrate-stop.sh || : ;;
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
