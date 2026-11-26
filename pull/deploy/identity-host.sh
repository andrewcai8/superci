#!/bin/bash
# superci-id: what runs on an identity host (deploy/identity.sh puts it in user data and pushes the current copy before
# each command). An identity is one classic runner under $ROOT/<name>, hard-linked to the image's runner tree, with the
# shim as its bin/Runner.Worker, run by the systemd unit superci-id@<name>. Its .runner, .credentials and
# .credentials_rsaparams are kept in s3://<bucket>/<deployment>/hosts/<az>/<name>/, so a replacement host in the same AZ
# restores its share and registers nothing. Nothing here prints a token.
#
#   superci-id boot                                      installs the shim and the unit, restores the share, starts it
#   superci-id register <label> <url> <group|-> <name>...   registers each name not held yet; token on stdin
#   superci-id adopt <name>...                           uploads and starts identities already registered here; no token
#   superci-id status                                    identities, listeners running, private memory per listener against its 64 MiB
set -euo pipefail
umask 077
ROOT=${ID_ROOT:-/opt/id}
SRC=${RUNNER_SRC:-/home/runner}
SHIM=${SHIM_BIN:-/usr/local/bin/superci-shim}
SHIM_ENV=${SHIM_ENV:-/etc/superci-shim.env}
CONF=${ID_CONF:-/etc/superci-id.conf}
UNIT_DIR=${UNIT_DIR:-/etc/systemd/system}
LOG=${ID_LOG:-/var/log/superci-id}
PROC=${PROC_ROOT:-/proc}
FILES=".runner .credentials .credentials_rsaparams"
BUDGET_KIB=65536 # private memory a listener may hold (64 MiB); its unit's MemoryMax restarts one at 192 MiB
export AWS_DEFAULT_REGION=us-east-1
# shellcheck disable=SC1090
. "$CONF"   # BUCKET, DEPLOYMENT, RELEASE (the release bucket)
mkdir -p "$LOG" "$ROOT"

az() {
  if [ -n "${ID_AZ:-}" ]; then echo "$ID_AZ"; return; fi
  local t; t=$(curl -s -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token)
  curl -s -H "X-aws-ec2-metadata-token: $t" http://169.254.169.254/latest/meta-data/placement/availability-zone
}
PREFIX="s3://$BUCKET/$DEPLOYMENT/hosts/$(az)"

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

upload() {
  local name=$1 f t
  for f in $FILES; do [ -s "$ROOT/$name/$f" ] || { echo "superci-id: $name has no $f" >&2; return 1; }; done
  t=$(mktemp -d)
  mkdir "$t/$name"
  for f in $FILES; do cp -p "$ROOT/$name/$f" "$t/$name/$f"; done
  aws s3 cp --recursive --only-show-errors --sse aws:kms "$t/$name" "$PREFIX/$name/" || { rm -rf "$t"; return 1; }
  rm -rf "$t"
}

start() { systemctl enable --now --no-block "superci-id@$1" > /dev/null 2>&1 || systemctl start --no-block "superci-id@$1"; }

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
      # The token goes in the environment the runner reads its inputs from, not in its arguments.
      rc=0
      (cd "$d" && ACTIONS_RUNNER_INPUT_TOKEN=$token RUNNER_ALLOW_RUNASROOT=1 ./config.sh "${args[@]}" > "$LOG/config-$name.log" 2>&1) || rc=$?
      if [ "$rc" != 0 ]; then
        rm -rf "$d"
        echo "failed $name: config.sh exit $rc$(grep -qiE '429|ConfigureTooManyRunners|rate limit' "$LOG/config-$name.log" && echo ', GitHub refused it (429 or ConfigureTooManyRunners in its log)') (log $LOG/config-$name.log)"
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

boot() {
  local stage release want got
  for _ in $(seq 1 60); do [ -x "$SRC/bin/Runner.Listener" ] && break; sleep 5; done
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

[Service]
WorkingDirectory=/opt/id/%i
EnvironmentFile=/etc/superci-shim.env
Environment=RUNNER_ALLOW_RUNASROOT=1 SUPERCI_IDENTITY=%i
Environment=DOTNET_gcServer=0 DOTNET_GCgen0size=0x200000 DOTNET_GCHeapHardLimit=0x6000000 DOTNET_TieredPGO=0 DOTNET_TC_QuickJitForLoops=1 DOTNET_GCConserveMemory=7
MemoryMax=192M
ExecStart=/opt/id/%i/run.sh
Restart=always
RestartSec=2
# A listener that dies or restarts leaves its running shim (bin/Runner.Worker) alive: the shim's job goes on, and its
# machine renews the job itself (measured Nov 26: with the shim killed with its listener, the job ran on and failed only
# when the machine was ended).
KillMode=process

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  stage=$(mktemp -d)
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
  touch "${READY_FILE:-/run/superci-id-ready}"
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
  boot) boot ;;
  register) shift; [ $# -ge 4 ] || { echo "register <label> <url> <group|-> <name>..." >&2; exit 2; }; register "$@" ;;
  adopt) shift; adopt "$@" ;;
  status) status ;;
  *) echo "usage: superci-id boot|register|adopt|status" >&2; exit 2 ;;
esac
