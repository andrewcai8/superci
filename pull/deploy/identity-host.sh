#!/bin/bash
# superci-id: what runs on an identity host (deploy/identity.sh puts it in user data and pushes the current copy before
# each command). An identity is one classic runner under $ROOT/<name>, hard-linked to the image's runner tree, with the
# shim as its bin/Runner.Worker, run by the systemd unit superci-id@<name>. Its .runner, .credentials and
# .credentials_rsaparams are kept in s3://<bucket>/<deployment>/hosts/<az>/<name>/, so a replacement host in the same AZ
# restores its share and registers nothing. Nothing here prints a token. Boot, register and adopt run one at a time
# (flock on $LOCK): two of them at once would delete each other's identity tree.
#
#   superci-id boot                                      installs the shim and the unit, restores the share, starts it
#   superci-id register <label> <url> <group|-> <name>...   registers each name not held yet; token on stdin
#   superci-id adopt <name>...                           uploads and starts identities already registered here; no token
#   superci-id status                                    identities, listeners running, private memory per listener against its 64 MiB
#   superci-id harden                                    no path is left writable by others (boot runs it first)
#   superci-id health                                    run by superci-id-health.timer every 30 s: revives listeners that do not
#                                                        run, then marks this instance Unhealthy so its Auto Scaling group replaces it
#   superci-id install-health                            the timer, on a host that was not booted with it
#   superci-id check                                     read-only: exit 0 if the host is ready and runs every listener of its share
set -euo pipefail
umask 077
ROOT=${ID_ROOT:-/opt/id}
SRC=${RUNNER_SRC:-/home/runner}
SHIM=${SHIM_BIN:-/usr/local/bin/superci-shim}
SHIM_ENV=${SHIM_ENV:-/etc/superci-shim.env}
CONF=${ID_CONF:-/etc/superci-id.conf}
LOCK=${ID_LOCK:-/run/superci-id.lock}
UNIT_DIR=${UNIT_DIR:-/etc/systemd/system}
LOG=${ID_LOG:-/var/log/superci-id}
HEALTH_DIR=${HEALTH_DIR:-/run/superci-id-health}
HEALTH_UPTIME=${HEALTH_UPTIME:-/proc/uptime}
READY_FILE=${READY_FILE:-/run/superci-id-ready}
BOOTING_FILE=${BOOTING_FILE:-$(dirname "$READY_FILE")/superci-id-booting} # the uptime second a boot started at, while it runs
PROC=${PROC_ROOT:-/proc}
FSROOT=${FS_ROOT:-/}   # the filesystem harden walks: / on a host, a fake one in identity_test.sh
FILES=".runner .credentials .credentials_rsaparams"
BUDGET_KIB=65536 # private memory a listener may hold (64 MiB); its unit's MemoryMax restarts one at 192 MiB
export AWS_DEFAULT_REGION=us-east-1
# shellcheck disable=SC1090
. "$CONF"   # BUCKET, DEPLOYMENT, RELEASE (the release bucket), KMS_KEY (the identity files' key)
mkdir -p "$LOG" "$ROOT"

az() {
  if [ -n "${ID_AZ:-}" ]; then echo "$ID_AZ"; return; fi
  local t; t=$(curl -s -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token || true)
  curl -s -H "X-aws-ec2-metadata-token: $t" http://169.254.169.254/latest/meta-data/placement/availability-zone || true
}
AZ=$(az)
# valid_az: the share is this AZ's prefix in S3, and an empty or odd AZ names no share (or all of them): nothing runs on it.
valid_az() { [[ $AZ =~ ^us-east-1[a-f]$ ]]; }
no_az() { echo "no valid availability zone (got '$AZ')"; }
PREFIX="s3://$BUCKET/$DEPLOYMENT/hosts/$AZ"

# make_dir: a fresh identity tree. Its bin and externals are hard links to the image's, so the listeners share their
# pages; only the shim, which must know its own directory, is a file of its own.
make_dir() {
  local d=$ROOT/$1
  rm -rf "$d"; mkdir -p "$d"
  cp -al "$SRC/bin" "$SRC/externals" "$d/"
  cp -a "$SRC"/*.sh "$SRC/run-helper.sh.template" "$d/"
  rm "$d/bin/Runner.Worker"
  ln -f "$SHIM" "$d/bin/Runner.Worker" 2> /dev/null || install -m 755 "$SHIM" "$d/bin/Runner.Worker"
}

# locked: takes the host's one lock for the rest of this run, or fails if another run still holds it after LOCK_WAIT.
locked() {
  exec 9> "$LOCK"
  flock -w "${LOCK_WAIT:-900}" 9 || { echo "busy: another superci-id run holds $LOCK" >&2; exit 1; }
}

upload() {
  local name=$1 f t
  valid_az || { echo "superci-id: $(no_az): $name not uploaded" >&2; return 1; } # $PREFIX would name no share, or all of them
  for f in $FILES; do [ -s "$ROOT/$name/$f" ] || { echo "superci-id: $name has no $f" >&2; return 1; }; done
  t=$(mktemp -d)
  mkdir "$t/$name"
  for f in $FILES; do cp -p "$ROOT/$name/$f" "$t/$name/$f"; done
  aws s3 cp --recursive --only-show-errors --sse aws:kms --sse-kms-key-id "${KMS_KEY:?no KMS_KEY in $CONF}" "$t/$name" "$PREFIX/$name/" || { rm -rf "$t"; return 1; }
  rm -rf "$t"
}

# A unit that GitHub's removal of its identity made crash-loop stops at its start limit; a start here asks again.
start() {
  systemctl reset-failed "superci-id@$1" > /dev/null 2>&1 || true
  systemctl enable --now --no-block "superci-id@$1" > /dev/null 2>&1 || systemctl start --no-block "superci-id@$1"
}

register() {
  local label=$1 url=$2 group=$3 name d status args rc token=""
  shift 3
  IFS= read -r token || true
  [ -n "$token" ] || { echo "no token on stdin" >&2; exit 1; }
  [ -x "$SHIM" ] || { echo "no shim at $SHIM: boot did not finish" >&2; exit 1; }
  for name in "$@"; do
    d=$ROOT/$name
    if [ -e "$d/.runner" ]; then
      status=adopted
    else
      make_dir "$name"
      args=(--unattended --url "$url" --labels "$label" --no-default-labels --name "$name" --disableupdate --work _work)
      [ "$group" = - ] || args+=(--runnergroup "$group")
      # The token goes in the environment the runner reads its inputs from, not in its arguments. A config.sh that hangs
      # is killed: it may still have registered at GitHub, which the next run reports as an orphan.
      rc=0
      (cd "$d" && ACTIONS_RUNNER_INPUT_TOKEN=$token RUNNER_ALLOW_RUNASROOT=1 timeout "${CONFIG_TIMEOUT:-120}" ./config.sh "${args[@]}" > "$LOG/config-$name.log" 2>&1) || rc=$?
      if [ "$rc" != 0 ]; then
        rm -rf "$d"
        if grep -qiE '429|ConfigureTooManyRunners|rate limit' "$LOG/config-$name.log"; then
          echo "refused $name: config.sh exit $rc, GitHub refused it (429 or ConfigureTooManyRunners in its log) (log $LOG/config-$name.log)"
        else
          echo "failed $name: config.sh exit $rc$([ "$rc" = 124 ] && echo ' (timed out)') (log $LOG/config-$name.log)"
        fi
        continue
      fi
      status=registered
    fi
    # A registration made and not stored still spent the cap: it is told apart, and a run again stores it (adopt).
    upload "$name" || { echo "$([ $status = registered ] && echo unstored || echo failed) $name: upload"; continue; }
    start "$name"
    echo "$status $name"
  done
}

adopt() {
  local name
  for name; do
    if [ ! -e "$ROOT/$name/.runner" ]; then echo "orphan $name: registered at GitHub, files on no host"; continue; fi
    upload "$name" || { echo "failed $name: upload"; continue; }
    start "$name"
    echo "adopted $name"
  done
}

# fsroot: FSROOT resolved, "" for the real / (so "$r/proc" is /proc), whatever spelling FS_ROOT used ("//", "/.").
fsroot() {
  local r
  r=$(realpath -m "$FSROOT")
  [ "$r" = / ] && r=""
  echo "$r"
}

# writable_by_others: the paths under FSROOT, outside /proc, /sys, /dev and /run, that others may write: every file and
# every directory but a sticky one (/tmp's kind, where only an owner may remove or rename). NUL-separated; the first
# one alone with -q. A path that vanishes mid-walk is no error.
writable_by_others() {
  local r quit=()
  r=$(fsroot)
  [ "${1:-}" != -q ] || quit=(-quit)
  find "${r:-/}" -xdev \( -path "$r/proc" -o -path "$r/sys" -o -path "$r/dev" -o -path "$r/run" \) -prune -o \
    ! -type l -perm -0002 \( -type f -o \( -type d ! -perm -1000 \) \) -print0 "${quit[@]}" 2> /dev/null || :
}

# eic_chain: Instance Connect's key command and every directory above it, the paths sshd checks before it runs it.
eic_chain() {
  local r=$1 d
  for d in / /usr /usr/share /usr/share/ec2-instance-connect; do echo "${r}${d}"; done
  for d in "$r"/usr/share/ec2-instance-connect/*; do [ ! -e "$d" ] || echo "$d"; done
}

# harden: nothing others may write is left where root runs, since this host holds long-lived credentials. The
# machines' image leaves much of /usr/share, /usr/local and /opt (/opt/id's parent) mode 777 for jobs' tool installs,
# and its runner tree, which every identity hard-links and root runs, the runner user's. sshd refuses Instance
# Connect's key command when it or a directory above it is not root's or is writable by group or others ("Unsafe
# AuthorizedKeysCommand"), so before this no operator could log in either.
#
# Instance Connect's chain comes first, so a login works whatever the sweep meets. Then every path others may write
# loses group and other write, a path that cannot be changed is no stop, and the runner tree becomes root's. A path
# writable by group alone is left. It fails, and boot with it, while a path stays writable by others or the chain is
# not root's alone.
harden() {
  local r p owner uid bad=""
  r=$(fsroot)
  [ -n "$r" ] || [ "$(id -u)" = 0 ] || { echo "superci-id: harden walks / only as root" >&2; exit 1; }
  owner=${HARDEN_OWNER:-root:root}; uid=$(id -u "${owner%%:*}")
  while read -r p; do [ ! -e "$p" ] || { chown "$owner" "$p"; chmod go-w "$p"; }; done < <(eic_chain "$r")
  writable_by_others | xargs -0 -r chmod go-w 2> /dev/null || :
  chown "$owner" "$SRC"
  chown -R "$owner" "$SRC/bin" "$SRC/externals" "$SRC"/*.sh "$SRC/run-helper.sh.template"
  chmod -R go-w "$SRC/bin" "$SRC/externals" "$SRC"/*.sh "$SRC/run-helper.sh.template"
  chmod go-w "$SRC"
  while read -r p; do
    [ ! -e "$p" ] || { [ "$(stat -c %u "$p")" = "$uid" ] && [ $(( 0$(stat -c %a "$p") & 022 )) = 0 ]; } || bad="$bad $p"
  done < <(eic_chain "$r")
  [ -z "$bad" ] || { echo "superci-id: Instance Connect's key command is not root's alone:$bad; starting nothing" >&2; exit 1; }
  p=$(writable_by_others -q | tr -d '\0')
  [ -z "$p" ] || { echo "superci-id: $p is still writable by others; starting nothing" >&2; exit 1; }
  echo "hardened: nothing outside /proc, /sys, /dev, /run and sticky directories is writable by others"
}

# install_health: the timer that runs `superci-id health` every 30 s (its first 2 minutes after boot; the check itself
# judges nothing before the host is 15 minutes old). Written to $UNIT_DIR, enabled now.
install_health() {
  cat > "$UNIT_DIR/superci-id-health.service" <<'UNIT'
[Unit]
Description=superci identity host: revive listeners that do not run, then ask the Auto Scaling group to replace this host

[Service]
Type=oneshot
# A revive runs boot again inside this start: the restore of the largest share a host may hold (1,550 identities, when the
# other AZ's host is lost) is about 1,120 s (213 s for 295, extrapolated), so this is 2,700 s: twice that, and past the
# 40 minutes after which a host that never became ready is replaced.
TimeoutStartSec=2700
ExecStart=/usr/local/bin/superci-id health
UNIT
  cat > "$UNIT_DIR/superci-id-health.timer" <<'UNIT'
[Unit]
Description=superci identity host: health check every 30 seconds

[Timer]
OnBootSec=2min
OnUnitActiveSec=30s
AccuracySec=1s

[Install]
WantedBy=timers.target
UNIT
  systemctl daemon-reload
  systemctl enable --now superci-id-health.timer > /dev/null 2>&1 || systemctl start superci-id-health.timer
}

# running_listeners / held_identities: the units systemd runs, and the identities whose files are here.
running_listeners() { systemctl list-units --no-legend --state=running 'superci-id@*' 2> /dev/null | wc -l; }
held_identities() { find "$ROOT" -maxdepth 2 -name .runner 2> /dev/null | wc -l; }
count_lines() { grep -c . || true; }

# stored_names: the identities of this AZ whose three files are in S3, one a line. It fails when S3 cannot be listed.
stored_names() {
  local keys
  keys=$(aws s3api list-objects-v2 --bucket "$BUCKET" --prefix "$DEPLOYMENT/hosts/$AZ/" --query 'Contents[].Key' --output text) || return 1
  tr '\t' '\n' <<< "$keys" | awk -F/ 'NF == 5 && ($5 == ".runner" || $5 == ".credentials" || $5 == ".credentials_rsaparams") {c[$4]++} END {for (n in c) if (c[n] == 3) print n}' | sort
}

# judge: sets WHY and REVIVE, and fails, unless this host is healthy. The rule: the host is ready (boot finished), every
# identity of its share stored in S3 is restored here (its .runner is under $ROOT), and every listener runs, but for
# one in fifty of them (rounded down: none under 50 identities), which may be an identity GitHub removed and the unit
# stopped at its start limit. When S3 cannot be listed the identities held here stand for the share (for the timer; `check` fails).
#   REVIVE=boot   the host is not ready, or an identity is not restored: run boot again (it restores what is missing)
#   REVIVE=start  all are restored and too few run: start every unit
judge() {
  local stored total n missing=0 running
  WHY=""; REVIVE=""
  if ! valid_az; then WHY=$(no_az); REVIVE=boot; return 1; fi
  if [ ! -e "$READY_FILE" ]; then WHY="boot did not finish ($READY_FILE is missing)"; REVIVE=boot; return 1; fi
  if ! stored=$(stored_names); then
    # `check` (JUDGE_STRICT) cannot verify what it cannot list and fails; the timer does not revive a host over a listing it could not read.
    if [ "${JUDGE_STRICT:-0}" = 1 ]; then WHY="the identities stored in S3 for $AZ cannot be listed, so the host cannot be verified"; REVIVE=none; return 1; fi
    stored=$(find "$ROOT" -maxdepth 2 -name .runner 2> /dev/null | awk -F/ '{print $(NF-1)}')
  fi
  total=$(count_lines <<< "$stored")
  for n in $stored; do [ -e "$ROOT/$n/.runner" ] || missing=$((missing + 1)); done
  if [ "$missing" -gt 0 ]; then WHY="$missing of $total stored identities are not restored on this host"; REVIVE=boot; return 1; fi
  running=$(running_listeners)
  if [ $((running + total / 50)) -lt "$total" ]; then WHY="$running of $total listeners are running"; REVIVE=start; return 1; fi
  return 0
}

# stop_listeners: every listener stopped (a stop is not undone by Restart=always), so the host that goes holds no session
# at GitHub while its replacement boots: the group does not wait for the old instance's shutdown.
stop_listeners() {
  local try
  for try in 1 2; do
    systemctl stop 'superci-id@*' > /dev/null 2>&1 || true
    [ "$(running_listeners)" = 0 ] && return 0
  done
  echo "could not stop every listener ($(running_listeners) still run)"
}

# health: a host whose listeners do not run takes no jobs, and nothing else notices (systemd restarts each unit, but
# a host that is not ready, or whose units all stopped, stays so). Judged every 30 s from the host's uptime, from 15
# minutes on (the restore of a share takes minutes) by `judge`:
#   unhealthy for 60 s             -> revive, once per episode: boot again as a process of its own (so its errexit applies, and a
#                                     boot that fails leaves the host not ready), or start every unit
#   still so 120 s after the revive -> stop every listener, then mark this instance Unhealthy (once), which its group
#                                      replaces; the new host restores the share at boot, as up does. Never before the host
#                                      is 20 minutes old.
# A boot, register or adopt in progress holds the lock: that is a busy host, not a sick one, unless it is the boot of a
# boot that has run more than 40 minutes, which cannot be revived and is replaced after the same 120 s.
health() {
  local D=$HEALTH_DIR now first since="" why="" id t stuck=0 d booting group
  mkdir -p "$D"
  now=$(cut -d. -f1 "$HEALTH_UPTIME")
  [ "$now" -ge "${HEALTH_GRACE:-900}" ] || exit 0
  exec 9> "$LOCK"
  if ! flock -n 9; then
    # Stuck is a boot that has held the lock past the limit (its start time is in $BOOTING_FILE), not a host that has been up
    # that long: an operator's boot on a host up for days is busy, not sick. A register or adopt holding the lock is busy.
    booting=$(cat "$BOOTING_FILE" 2> /dev/null || true)
    { [[ $booting =~ ^[0-9]+$ ]] && [ $((now - booting)) -ge "${HEALTH_STUCK:-2400}" ]; } || exit 0
    stuck=1; why="boot has held the lock for $((now - booting)) s"
  else
    judge || why=$WHY
  fi
  if [ -z "$why" ]; then rm -f "$D/first" "$D/restarted" "$D/not-in-group"; exit 0; fi
  [ -f "$D/first" ] || echo "$now" > "$D/first"
  first=$(cat "$D/first")
  echo "health check failing for $((now - first)) s: $why"
  if [ -f "$D/restarted" ]; then since=$(cat "$D/restarted"); elif [ "$stuck" = 1 ]; then since=$first; fi
  if [ -n "$since" ]; then
    [ $((now - since)) -ge "${HEALTH_REPLACE_AFTER:-120}" ] || exit 0
    [ ! -f "$D/marked" ] || exit 0
    [ "$now" -ge "${HEALTH_YOUNG:-1200}" ] || { echo "this host is $now s old: not asking to be replaced yet"; exit 0; }
    id=${ID_INSTANCE:-}
    if [ -z "$id" ]; then
      t=$(curl -s -m 2 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token)
      id=$(curl -s -m 2 -H "X-aws-ec2-metadata-token: $t" http://169.254.169.254/latest/meta-data/instance-id)
    fi
    # Listeners are stopped only when a mark can follow: the instance is in this AZ's group (the role reads and marks its own
    # groups together). A plain host is not replaced by anything: stopping its listeners would only take its jobs.
    group=$(aws autoscaling describe-auto-scaling-instances --region us-east-1 --instance-ids "$id" --query 'AutoScalingInstances[0].AutoScalingGroupName' --output text 2> /dev/null) || group=""
    if [ "$group" != "superci-pull-identity-$AZ" ]; then
      [ -e "$D/not-in-group" ] || echo "this host is in no group of $AZ (${group:-none}): nothing would replace it, so its listeners are not stopped and it is not marked"
      touch "$D/not-in-group"; exit 0
    fi
    stop_listeners
    echo "asking the Auto Scaling group to replace $id"
    aws autoscaling set-instance-health --region us-east-1 --instance-id "$id" --health-status Unhealthy --no-should-respect-grace-period && touch "$D/marked"
    exit 0
  fi
  [ $((now - first)) -ge "${HEALTH_RESTART_AFTER:-60}" ] || exit 0
  echo "reviving: $why"
  echo "$now" > "$D/restarted"
  if [ "$stuck" = 0 ] && [ "$REVIVE" = start ]; then
    for d in "$ROOT"/*/; do [ ! -e "$d/.runner" ] || start "$(basename "$d")"; done
  else
    # Its own process, with the lock let go: in an `||` context bash ignores errexit, and a boot that failed halfway
    # would go on, touch the ready file with nothing restored, and read as healthy.
    exec 9>&-
    "$0" boot || echo "boot failed again"
  fi
  # The 120 s run from the end of the revive, which may have taken minutes.
  cut -d. -f1 "$HEALTH_UPTIME" > "$D/restarted"
  exit 0
}

# check: judge once, for `identity.sh up` to read before it adopts a running host (a host that is not healthy would be
# revived, then replaced, by the timer it installs).
check() {
  JUDGE_STRICT=1
  if judge; then echo "healthy: ready, every identity of the share restored and running"; else echo "not healthy: $WHY"; exit 1; fi
}

boot() {
  local stage release want got stored expected
  valid_az || { echo "superci-id: $(no_az): refusing" >&2; exit 1; }
  # While boot runs the host is not ready and the health check sees its lock: the marker says since when, so a long-lived host
  # whose operator runs boot is not taken for one whose boot never ends.
  trap 'rm -f "$BOOTING_FILE"' EXIT
  cut -d. -f1 "$HEALTH_UPTIME" > "$BOOTING_FILE"
  rm -f "$READY_FILE" # (a boot run again that fails leaves the host not ready)
  for _ in $(seq 1 60); do [ -x "$SRC/bin/Runner.Listener" ] && break; sleep 5; done
  harden
  command -v aws > /dev/null || { echo "superci-id: no aws cli on this image" >&2; exit 1; }
  # Only the shim the release names runs (release.sh writes bin/RELEASE.json).
  release=$(mktemp)
  aws s3 cp --only-show-errors "s3://$RELEASE/bin/superci-shim" "$release"
  want=$(aws s3 cp --only-show-errors "s3://$RELEASE/bin/RELEASE.json" - | sed -n 's/.*"bin\/superci-shim":"\([0-9a-f]\{64\}\)".*/\1/p') || want=""
  got=$(sha256sum < "$release" | cut -d' ' -f1)
  if [ -z "$want" ] || [ "$got" != "$want" ]; then
    echo "superci-id: refusing s3://$RELEASE/bin/superci-shim: sha256 $got, bin/RELEASE.json says ${want:-nothing}; starting nothing" >&2
    exit 1
  fi
  install -m 755 "$release" "$SHIM"; rm -f "$release"
  cat > "$UNIT_DIR/superci-id@.service" <<'UNIT'
[Unit]
Description=superci identity %i
After=network-online.target
Wants=network-online.target
# An identity GitHub has removed fails to start for good: it stops after 10 starts in 10 minutes, not crash-looping.
StartLimitIntervalSec=600
StartLimitBurst=10

[Service]
WorkingDirectory=/opt/id/%i
EnvironmentFile=/etc/superci-shim.env
Environment=RUNNER_ALLOW_RUNASROOT=1 SUPERCI_IDENTITY=%i
Environment=DOTNET_gcServer=0 DOTNET_GCgen0size=0x200000 DOTNET_GCHeapHardLimit=0x6000000 DOTNET_TieredPGO=0 DOTNET_TC_QuickJitForLoops=1 DOTNET_GCConserveMemory=7
MemoryMax=192M
ExecStart=/opt/id/%i/run.sh
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  install_health
  stage=$(mktemp -d)
  stored=$(stored_names)
  expected=$(count_lines <<< "$stored")
  aws s3 sync --only-show-errors "$PREFIX/" "$stage/"
  local name n=0
  for d in "$stage"/*/; do
    [ -d "$d" ] || continue
    name=$(basename "$d")
    for f in $FILES; do [ -s "$d/$f" ] || { echo "superci-id: $name lacks $f in S3; skipped" >&2; continue 2; }; done
    if [ ! -e "$ROOT/$name/.runner" ]; then
      make_dir "$name"
      for f in $FILES; do install -m 600 "$d/$f" "$ROOT/$name/$f"; done
    fi
    start "$name"
    n=$((n + 1))
    [ $((n % 25)) = 0 ] && sleep 1
  done
  rm -rf "$stage"
  echo "restored $n identities"
  # Ready is every identity stored for this AZ restored and started, not that the restore ended.
  [ "$n" -ge "$expected" ] || { echo "superci-id: restored $n of the $expected identities stored for $AZ; not ready" >&2; exit 1; }
  touch "$READY_FILE"
}

status() {
  local total running p name kib sum=0 max=0 seen=0 over=0
  total=$(find "$ROOT" -maxdepth 2 -name .runner 2> /dev/null | wc -l)
  running=$(systemctl list-units --no-legend --state=running 'superci-id@*' 2> /dev/null | wc -l)
  for p in $(pgrep -x Runner.Listener || true); do
    name=$(basename "$(readlink "$PROC/$p/cwd")" 2> /dev/null) || continue
    kib=$(awk '/^Private_Dirty:/ {print $2}' "$PROC/$p/smaps_rollup" 2> /dev/null || true)
    [ -n "$kib" ] || continue
    [ "${VERBOSE:-}" = 1 ] && echo "listener $name private_dirty_kib $kib"
    sum=$((sum + kib)); seen=$((seen + 1)); [ "$kib" -le "$max" ] || max=$kib
    [ "$kib" -le "$BUDGET_KIB" ] || over=$((over + 1))
  done
  echo "az $(az) identities $total listeners_running $running private_dirty_kib_avg $((seen ? sum / seen : 0)) max $max over_64mib $over listeners_measured $seen"
}

case "${1:-}" in
  boot) locked; boot ;;
  register) shift; [ $# -ge 4 ] || { echo "register <label> <url> <group|-> <name>..." >&2; exit 2; }
    valid_az || { echo "superci-id: $(no_az): refusing" >&2; exit 1; } # before any registration: an identity stored under no share is lost
    locked; register "$@" ;;
  adopt) shift; valid_az || { echo "superci-id: $(no_az): refusing" >&2; exit 1; }; locked; adopt "$@" ;;
  status) status ;;
  harden) locked; harden ;;
  health) health ;;
  check) check ;;
  install-health) locked; install_health ;;
  *) echo "usage: superci-id boot|register|adopt|status|harden|health|install-health|check" >&2; exit 2 ;;
esac
