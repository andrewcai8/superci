#!/bin/bash
# Builds the machines' hibernation-capable image from the image config.json pins, and says which ids to pin.
#
# The pinned image cannot hibernate as shipped (ec2-hibinit-agent is disabled; logind answers the sleep key with a suspend
# an EC2 machine cannot do, and the machine sits in `stopping`; and the agent refuses a root file system smaller than RAM, which
# boot.sh grows only after it). deploy/image-setup.sh is the change; this script applies it to a builder of the pin, makes an
# image of the builder, proves the image and only then tags it as a pin. The build id is
# the pin's own build id plus a hash of image-setup.sh ("<RunsOn build>-hib-<8 hex>"), so the same pin and the same setup are
# the same build: a second run finds the images by the tag and builds nothing (an edit to the setup is a new build, and no
# image that exists is changed). It never deregisters or modifies an image that exists and never writes config.json: the
# release moves the pin (README, "Moving the image pin"), and a rollback to a commit that pins the old copy still has it.
#
#   1. The pin of config.json for us-east-1 is read, and refused unless EC2 lists it, it is ours, available and tagged
#      superci-pull-image (the build it copies). Nothing is launched before that.
#   2. A builder of the pin (tag purpose=superci-pull-image-build) takes image-setup.sh over ssh, is sealed (its user, its key
#      and cloud-init's record of the boot removed, so the image boots as the pin does) and stopped, and create-image makes
#      the candidate, tagged superci-pull-image-candidate=<build> only.
#   3. The candidate is proved on three machines of the largest hibernating type the sets use (IMAGE_SETS, default
#      superci-4cpu and superci-8cpu: RAM plus the set's disk_gb gives the encrypted root's size), launched together:
#        - one with HibernationOptions.Configured, which is hibernated and resumed, and must come back with the boot id it
#          had and the memory it held (EC2 refuses the hibernate for a minute or two after a launch: retried);
#        - one launched as the fleet's are (no hibernation, the set's disk) that has no swap, no swap file, no resume= line,
#          no failed unit, the disk use of the pin's machine, exactly hibinit-agent more among its enabled units (which uses
#          at most IMAGE_AGENT_MAX_MS of CPU, default 500, and is not on the boot's critical chain), and a boot no slower
#          than the pin's machine's own by IMAGE_BOOT_TOL_MS (default 1500). Both are measured on a second boot, after one
#          read of the volume's image blocks: a snapshot's first boots read cold from EBS's backing store and take about 2.5 s
#          longer, whatever the image holds;
#        - one of the pin itself, launched the same way, which that comparison is made against.
#      Any failure leaves the candidate untagged for the pin (the line to delete it by hand is printed), copies nothing and
#      exits 1.
#   4. The candidate is tagged superci-pull-image=<build> and purpose=superci-pull (the two tags the pin's copies carry), and
#      copied to each other region of config.json, tagged the same.
#   Every instance made is terminated and confirmed terminated on any exit. IMAGE_TAGS="key=value ..." adds tags to the
#   instances and their volumes (a lab's), not to an image.
#
#   pull/deploy/image.sh          prints the build id, each region's image and the "image" map to put in config.json
set -euo pipefail
here=$(cd "$(dirname "$0")/.." && pwd)
A="aws --profile ci-runners"
R=us-east-1
OUR=243891825325
CONFIG=$here/config.json
SETUP=$here/deploy/image-setup.sh
BUILDER_TYPE=m8a.large
NAP=5 HIB_WAIT=900 STEP_WAIT=600
if [ "${SUPERCI_TEST:-}" = 1 ]; then
  CONFIG=${IMAGE_CONFIG:-$CONFIG} NAP=${IMAGE_NAP:-$NAP} HIB_WAIT=${IMAGE_HIB_WAIT:-$HIB_WAIT} STEP_WAIT=${IMAGE_STEP_WAIT:-$STEP_WAIT}
fi
SETS=${IMAGE_SETS:-"superci-4cpu superci-8cpu"}
BOOT_TOL_MS=${IMAGE_BOOT_TOL_MS:-1500} AGENT_MAX_MS=${IMAGE_AGENT_MAX_MS:-500}
say() { echo "image: $*"; }
refuse() { echo "image: refusing: $*" >&2; exit 1; }
fail() { echo "image: FAIL: $*" >&2; exit 1; }
nap() { sleep "$NAP"; }

# --- 1. the pin ---------------------------------------------------------------------------------------------------------
[ -f "$SETUP" ] || refuse "no $SETUP"
pin_of() {
  python3 -I -c 'import json,sys
c = json.load(open(sys.argv[1]))
print((c.get("image") or {}).get(sys.argv[2], ""))' "$CONFIG" "$1"
}
REGIONS=$(python3 -I -c 'import json,sys; print(" ".join(sorted(json.load(open(sys.argv[1]))["regions"])))' "$CONFIG")
[[ " $REGIONS " == *" $R "* ]] || refuse "config.json serves no region $R"
PIN=$(pin_of $R)
[[ $PIN =~ ^ami-[0-9a-f]{8}([0-9a-f]{9})?$ ]] || refuse "config.json pins '${PIN:-nothing}' for $R, not an AMI id: the build starts from the pin (a placeholder ami-REPLACE-<region> ships nowhere); pin the current copy first (README, \"Moving the image pin\")"
[ "$($A sts get-caller-identity --query Account --output text)" = $OUR ] || refuse "not ci-runners"
row=$($A ec2 describe-images --region $R --image-ids "$PIN" \
  --query "Images[0].[OwnerId,State,ImageId,Tags[?Key=='superci-pull-image']|[0].Value,BlockDeviceMappings[?DeviceName=='/dev/sda1']|[0].Ebs.VolumeSize]" --output text 2> /dev/null < /dev/null) || row=None
read -r owner state id PIN_BUILD ROOT_GB <<< "$row"
[ "$owner" != None ] && [ "${id:-}" = "$PIN" ] || refuse "the pinned image $PIN in $R: EC2 does not list it"
[ "$owner" = $OUR ] || refuse "the pinned image $PIN in $R is owned by $owner, not $OUR: only our own copy is built from"
[ "$state" = available ] || refuse "the pinned image $PIN in $R is $state, not available"
[ "${PIN_BUILD:-None}" != None ] || refuse "the pinned image $PIN in $R has no superci-pull-image tag (the build it copies)"
[[ ${ROOT_GB:-} =~ ^[0-9]+$ ]] || refuse "the pinned image $PIN in $R has no /dev/sda1 volume size"
BASE=$(sed 's/-hib-[0-9a-f]\{8\}$//' <<< "$PIN_BUILD")
BUILD=$BASE-hib-$(sha256sum < "$SETUP" | cut -c1-8)
say "build $BUILD (the pin $PIN is $PIN_BUILD; setup $(basename "$SETUP"))"

# --- lookups and the cleanup ---------------------------------------------------------------------------------------------
# image_in <region> <tag key>: the id of the image of the build the tag names, once available; empty when there is none.
image_in() {
  local r=$1 key=$2 row id st
  row=$($A ec2 describe-images --region "$r" --owners self --filters "Name=tag:$key,Values=$BUILD" --query 'Images[0].[ImageId,State]' --output text < /dev/null) || row=None
  read -r id st <<< "$row"
  [ -n "${id:-}" ] && [ "$id" != None ] || return 0
  wait_image "$r" "$id"
  echo "$id"
}
wait_image() { # region id: available, or fail after three waiters
  local i
  for i in 1 2 3; do $A ec2 wait image-available --region "$1" --image-ids "$2" < /dev/null 2> /dev/null && return 0; done
  fail "image $2 in $1 is not available"
}
IDS=() tmp=$(mktemp -d) UNPROVEN= SNAP=
cleanup() {
  local rc=$? id st i bad=0
  trap - EXIT
  if [ $rc != 0 ] && [ -n "$UNPROVEN" ]; then
    echo "image: the candidate $UNPROVEN stays untagged for the pin (superci-pull-image-candidate=$BUILD only); nothing was copied. Delete it by hand: $A ec2 deregister-image --region $R --image-id $UNPROVEN; $A ec2 delete-snapshot --region $R --snapshot-id ${SNAP:-<its snapshot>}" >&2
  fi
  if [ ${#IDS[@]} -gt 0 ]; then
    $A ec2 terminate-instances --region $R --instance-ids "${IDS[@]}" > /dev/null < /dev/null || { sleep 2; $A ec2 terminate-instances --region $R --instance-ids "${IDS[@]}" > /dev/null < /dev/null; } || bad=1
    for id in "${IDS[@]}"; do
      st=
      for i in $(seq 60); do
        st=$($A ec2 describe-instances --region $R --instance-ids "$id" --query 'Reservations[0].Instances[0].State.Name' --output text < /dev/null 2> /dev/null) || st=unknown
        [ "$st" = terminated ] && break
        sleep "$NAP"
      done
      if [ "$st" = terminated ]; then echo "image: terminated $id (terminated)"; else echo "image: $id is $st, not terminated: terminate it by hand (tag purpose=superci-pull-image-build)" >&2; bad=1; fi
    done
  fi
  rm -rf "$tmp" || true
  [ $bad = 0 ] || rc=1
  exit $rc
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# --- instances -----------------------------------------------------------------------------------------------------------
SG=$($A ec2 describe-security-groups --region $R --filters Name=group-name,Values=superci-pull-listener --query 'SecurityGroups[0].GroupId' --output text < /dev/null)
tags() { # the tags of an instance and its volume: purpose, Name and IMAGE_TAGS
  local t="{Key=purpose,Value=superci-pull-image-build},{Key=Name,Value=superci-pull-image-$1}" kv
  for kv in ${IMAGE_TAGS:-}; do t+=",{Key=${kv%%=*},Value=${kv#*=}}"; done
  echo "$t"
}
# launch <name> <ami> <type> <root GB> <mode: plain|hib|builder> <user data file>: sets LAUNCHED to the instance id and remembers
# it for the cleanup (not in a subshell: a $(...) would lose the id the cleanup terminates).
launch() {
  local name=$1 ami=$2 type=$3 gb=$4 mode=$5 ud=$6 ebs extra=() out
  ebs="VolumeSize=$gb,VolumeType=gp3,DeleteOnTermination=true"
  case $mode in
    hib) ebs+=",Iops=3000,Throughput=250,Encrypted=true"; extra=(--hibernation-options Configured=true) ;;
    plain) ebs+=",Iops=3000,Throughput=250"; extra=(--instance-initiated-shutdown-behavior terminate) ;;
    builder) extra=(--instance-initiated-shutdown-behavior stop) ;;
  esac
  out=$($A ec2 run-instances --region $R --image-id "$ami" --instance-type "$type" --security-group-ids "$SG" --associate-public-ip-address \
    --metadata-options HttpTokens=required,HttpPutResponseHopLimit=1,HttpEndpoint=enabled "${extra[@]}" \
    --block-device-mappings "DeviceName=/dev/sda1,Ebs={$ebs}" \
    --tag-specifications "ResourceType=instance,Tags=[$(tags "$name")]" "ResourceType=volume,Tags=[$(tags "$name")]" \
    --user-data "fileb://$ud" --query 'Instances[0].InstanceId' --output text < /dev/null) || fail "run-instances for $name ($type from $ami) failed"
  IDS+=("$out") LAUNCHED=$out
  say "launched $name $out ($type, $gb GB root, $mode, $ami)"
}
state_of() { $A ec2 describe-instances --region $R --instance-ids "$1" --query 'Reservations[0].Instances[0].State.Name' --output text < /dev/null; }
# until_state <id> <state> <seconds> <what>: polls the state, failing when it is not reached in time.
until_state() {
  local t=$SECONDS st
  while :; do
    st=$(state_of "$1") || st=unknown
    [ "$st" = "$2" ] && return 0
    [ $((SECONDS - t)) -lt "$3" ] || fail "$4: $1 is $st after $3 s, not $2"
    nap
  done
}
ip_of() { $A ec2 describe-instances --region $R --instance-ids "$1" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text < /dev/null; }
# keys: the builder's and the test machines' own, so a key left in the image is found by its text.
ssh-keygen -q -t ed25519 -N '' -f "$tmp/bkey"
ssh-keygen -q -t ed25519 -N '' -f "$tmp/tkey"
user_data() { # <user> <public key file> <out>: the machine's own user, as parity.sh makes (the image's sshd takes no Instance Connect key)
  cat > "$3" << UD
#!/bin/bash
useradd -m -s /bin/bash $1
install -d -m 700 -o $1 -g $1 /home/$1/.ssh
printf '%s\n' '$(cat "$2")' > /home/$1/.ssh/authorized_keys
chown $1:$1 /home/$1/.ssh/authorized_keys; chmod 600 /home/$1/.ssh/authorized_keys
printf '$1 ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/90-$1; chmod 440 /etc/sudoers.d/90-$1
( sleep 7200; poweroff ) > /dev/null 2>&1 &
UD
}
# rssh <ip> <user> <key> <command...>
rssh() {
  local ip=$1 user=$2 key=$3; shift 3
  timeout -k 5 "${RSSH_TIMEOUT:-300}" ssh -i "$key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 \
    -o ServerAliveInterval=5 -o ServerAliveCountMax=3 -o LogLevel=ERROR "$user@$ip" "$@"
}
# reach <id> <user> <key>: sets IP once the machine answers ssh.
reach() {
  local t=$SECONDS
  until_state "$1" running "$STEP_WAIT" "waiting for it to run"
  while :; do
    IP=$(ip_of "$1") || IP=
    [ -n "$IP" ] && [ "$IP" != None ] && rssh "$IP" "$2" "$3" true < /dev/null 2> /dev/null && return 0
    [ $((SECONDS - t)) -lt "$STEP_WAIT" ] || fail "$1 ($IP) did not answer ssh in $STEP_WAIT s"
    nap
  done
}

# --- 2. the candidate ----------------------------------------------------------------------------------------------------
SEAL='# superci-image: seal
set -eu
# The builder is made an image of: its user, its key and its sudo rule go, and cloud-init forgets the boot (its record holds the
# builder'"'"'s user data), so a machine of the image boots as one of the pin does.
rm -f /etc/sudoers.d/90-image
userdel -rf image 2> /dev/null || true
cloud-init clean --logs
journalctl --rotate > /dev/null 2>&1 || true
journalctl --vacuum-time=1s > /dev/null 2>&1 || true
sync
echo "seal: done"'
build_candidate() {
  local ud=$tmp/builder-ud id ip
  user_data image "$tmp/bkey.pub" "$ud"
  launch builder "$PIN" $BUILDER_TYPE "$ROOT_GB" builder "$ud"; id=$LAUNCHED
  reach "$id" image "$tmp/bkey"; ip=$IP
  say "builder $id answers at $ip: applying $(basename "$SETUP") ($(sha256sum < "$SETUP" | cut -c1-12))"
  rssh "$ip" image "$tmp/bkey" 'sudo bash -s' < "$SETUP" || fail "the setup failed on the builder $id"
  printf '%s\n' "$SEAL" | rssh "$ip" image "$tmp/bkey" 'sudo bash -s' || fail "sealing the builder $id failed"
  $A ec2 stop-instances --region $R --instance-ids "$id" > /dev/null < /dev/null || fail "stopping the builder $id failed"
  until_state "$id" stopped "$STEP_WAIT" "stopping the builder"
  CAND=$($A ec2 create-image --region $R --instance-id "$id" --name "superci-pull-$BUILD" --no-reboot \
    --description "superci-pull's hibernation-capable image of $BASE (pull/deploy/image.sh)" \
    --tag-specifications "ResourceType=image,Tags=[{Key=superci-pull-image-candidate,Value=$BUILD}]" \
    --query ImageId --output text < /dev/null) || fail "create-image of the builder $id failed"
  say "candidate $CAND: waiting for it to be available"
  wait_image $R "$CAND"
  # what the pin has: instances default to IMDSv2 (the templates ask for it too)
  $A ec2 modify-image-attribute --region $R --image-id "$CAND" --imds-support v2.0 < /dev/null > /dev/null || fail "setting the candidate's IMDS support failed"
  $A ec2 terminate-instances --region $R --instance-ids "$id" > /dev/null < /dev/null || true
}

# --- 3. the proof --------------------------------------------------------------------------------------------------------
# size: the largest hibernating type of the sets and the root it needs (RAM plus the set's disk_gb): "TYPE ROOT_GB SET RAM_GIB DISK_GB".
size() {
  local types
  types=$(python3 -I -c 'import json,sys
c = json.load(open(sys.argv[1]))
print(" ".join(sorted({t for s in sys.argv[2:] for t in c["sizes"][s]["types"]})))' "$CONFIG" $SETS) || fail "config.json has no size among: $SETS"
  # shellcheck disable=SC2086
  $A ec2 describe-instance-types --region $R --instance-types $types --output json < /dev/null > "$tmp/types.json" || fail "describe-instance-types failed"
  python3 -I - "$CONFIG" "$tmp/types.json" $SETS << 'PY'
import json, math, sys
c = json.load(open(sys.argv[1]))
info = {t["InstanceType"]: t for t in json.load(open(sys.argv[2]))["InstanceTypes"]}
best = None
for s in sys.argv[3:]:
    spec = c["sizes"][s]
    for t in spec["types"]:
        i = info.get(t)
        if i is None or not i.get("HibernationSupported"):
            print("image: note: %s of %s does not hibernate (EC2 lists no hibernation for it)" % (t, s), file=sys.stderr)
            continue
        ram = math.ceil(i["MemoryInfo"]["SizeInMiB"] / 1024)
        need = ram + spec["disk_gb"]
        print("image: %s %s: %d GiB RAM + %d GB disk = a %d GB root" % (s, t, ram, spec["disk_gb"], need), file=sys.stderr)
        if best is None or need > best[0]:
            best = (need, t, s, ram, spec["disk_gb"])
if best is None:
    sys.exit("image: FAIL: none of the types of these sets hibernates")
print(best[1], best[0], best[2], best[3], best[4])
PY
}
# warm_reboot <id> <ip>: reads the root volume's image blocks once and reboots, returning once the machine answers on a new boot.
warm_reboot() {
  local id=$1 ip=$2 old new t=$SECONDS
  old=$(rssh "$ip" verify "$tmp/tkey" 'cat /proc/sys/kernel/random/boot_id' < /dev/null) || fail "no boot id on $id"
  RSSH_TIMEOUT=1500 rssh "$ip" verify "$tmp/tkey" "sudo dd if=/dev/nvme0n1 of=/dev/null bs=4M count=$((ROOT_GB * 256)) status=none" < /dev/null || fail "reading the root volume of $id failed"
  rssh "$ip" verify "$tmp/tkey" 'sudo systemctl reboot' < /dev/null 2> /dev/null || true
  nap
  while :; do
    ip=$(ip_of "$id") || ip=
    new=$(rssh "$ip" verify "$tmp/tkey" 'cat /proc/sys/kernel/random/boot_id' < /dev/null 2> /dev/null) && [ "$new" != "$old" ] && return 0
    [ $((SECONDS - t)) -lt $((STEP_WAIT * 2)) ] || fail "$id did not come back from its reboot"
    nap
  done
}
# probe: what a machine of an image shows, as key=value lines; run as root.
PROBE='# superci-image: probe
cpu=$(systemctl show hibinit-agent.service -p CPUUsageNSec --value); case "$cpu" in "" | *[!0-9]* | ????????????????*) cpu=0 ;; esac
echo "swap_active=$(swapon --noheadings --show | wc -l)"
echo "swap_file=$([ -e /swap-hibinit ] && echo 1 || echo 0)"
echo "grub_resume=$([ -e /etc/default/grub.d/99-set-swap.cfg ] && echo 1 || echo 0)"
echo "cmdline_resume=$(grep -c "resume=" /proc/cmdline || true)"
echo "disk_used=$(df -B1 --output=used / | tail -1 | tr -d " ")"
echo "failed_units=$(systemctl --failed --no-legend | wc -l)"
echo "boot_ms=$(( $(systemctl show -p FinishTimestampMonotonic --value) / 1000 ))"
echo "agent_ms=$(( cpu / 1000000 ))"
echo "agent_chain=$(systemd-analyze critical-chain --no-pager | grep -c hibinit-agent || true)"
echo "enabled=$(systemctl list-unit-files --state=enabled --no-legend | cut -d" " -f1 | sort | tr "\n" ",")"
echo "builder_user=$(getent passwd image | wc -l)"
echo "builder_sudoers=$([ -e /etc/sudoers.d/90-image ] && echo 1 || echo 0)"
echo "builder_key=$(grep -rlF "$(cut -d" " -f2 /run/superci-image-bkey)" /etc /root /home /var/lib/cloud 2> /dev/null | wc -l)"'
BADCOUNT=0
bad() { echo "image: FAIL: $*" >&2; BADCOUNT=$((BADCOUNT + 1)); }
kv() { sed -n "s/^$2=//p" <<< "$1" | head -1; }
verify() {
  local ami=$1 spec TYPE ROOT SET RAM DISK ud=$tmp/test-ud hid nid cid hip nip cip pb pc
  spec=$(size) || exit 1
  read -r TYPE ROOT SET RAM DISK <<< "$spec"
  say "proving $ami on $TYPE: a $ROOT GB encrypted root with hibernation, and $DISK GB ($SET's disk_gb) without"
  user_data verify "$tmp/tkey.pub" "$ud"
  launch hibernating "$ami" "$TYPE" "$ROOT" hib "$ud"; hid=$LAUNCHED
  launch normal "$ami" "$TYPE" "$DISK" plain "$ud"; nid=$LAUNCHED
  launch reference "$PIN" "$TYPE" "$DISK" plain "$ud"; cid=$LAUNCHED
  # --- the machine launched as the fleet's are, against the pin's
  reach "$nid" verify "$tmp/tkey"; nip=$IP
  reach "$cid" verify "$tmp/tkey"; cip=$IP
  # Both are measured on a second boot, after one read of the volume's image blocks: the first boots of a snapshot read it cold
  # from EBS's backing store (a fresh image's kernel and cloud-init took 2.5 s longer, whatever it held), which is not the image's.
  warm_reboot "$nid" "$nip" & local w1=$!
  warm_reboot "$cid" "$cip" & local w2=$!
  wait $w1 || fail "the normal machine $nid did not come back from its warm reboot"
  wait $w2 || fail "the pin's machine $cid did not come back from its warm reboot"
  nip=$(ip_of "$nid"); cip=$(ip_of "$cid")
  printf '%s\n' "$(cut -d' ' -f2 "$tmp/bkey.pub")" | rssh "$nip" verify "$tmp/tkey" 'sudo tee /run/superci-image-bkey > /dev/null'
  printf '%s\n' "$(cut -d' ' -f2 "$tmp/bkey.pub")" | rssh "$cip" verify "$tmp/tkey" 'sudo tee /run/superci-image-bkey > /dev/null'
  pb=$(printf '%s\n' "$PROBE" | rssh "$nip" verify "$tmp/tkey" 'sudo bash -s') || fail "the probe failed on the normal machine $nid"
  pc=$(printf '%s\n' "$PROBE" | rssh "$cip" verify "$tmp/tkey" 'sudo bash -s') || fail "the probe failed on the pin's machine $cid"
  say "normal machine $nid ($ami): $(tr '\n' ' ' <<< "$pb" | sed 's/enabled=[^ ]* //')"
  say "pin's machine $cid ($PIN): $(tr '\n' ' ' <<< "$pc" | sed 's/enabled=[^ ]* //')"
  local k
  for k in swap_active swap_file grub_resume cmdline_resume builder_user builder_sudoers builder_key; do
    [ "$(kv "$pb" $k)" = 0 ] || bad "a machine launched without hibernation has $k=$(kv "$pb" $k), not 0 (hibinit-agent must do nothing there)"
  done
  [ "$(kv "$pb" failed_units)" -le "$(kv "$pc" failed_units)" ] || bad "the normal machine has $(kv "$pb" failed_units) failed units, the pin's $(kv "$pc" failed_units)"
  local du=$(( $(kv "$pb" disk_used) - $(kv "$pc" disk_used) ))
  [ "$du" -le 4194304 ] || bad "the normal machine uses $du bytes more disk than the pin's"
  local added
  added=$(LC_ALL=C comm -13 <(tr ',' '\n' <<< "$(kv "$pc" enabled)" | LC_ALL=C sort) <(tr ',' '\n' <<< "$(kv "$pb" enabled)" | LC_ALL=C sort) | grep . | tr '\n' ' ')
  case "$added" in "" | "hibinit-agent.service ") ;; *) bad "the normal machine enables '${added}' beyond the pin's, not just hibinit-agent.service" ;; esac
  [ "$(LC_ALL=C comm -23 <(tr ',' '\n' <<< "$(kv "$pc" enabled)" | LC_ALL=C sort) <(tr ',' '\n' <<< "$(kv "$pb" enabled)" | LC_ALL=C sort) | grep -c .)" = 0 ] || bad "the normal machine lacks a unit the pin's has enabled"
  local slower=$(( $(kv "$pb" boot_ms) - $(kv "$pc" boot_ms) ))
  say "normal boot: $(kv "$pb" boot_ms) ms against the pin's $(kv "$pc" boot_ms) ms ($slower ms; hibinit-agent used $(kv "$pb" agent_ms) ms of CPU and made no swap file)"
  [ "$slower" -le "$BOOT_TOL_MS" ] || bad "the normal machine boots $slower ms slower than the pin's, over $BOOT_TOL_MS"
  [ "$(kv "$pb" agent_ms)" -le "$AGENT_MAX_MS" ] || bad "hibinit-agent used $(kv "$pb" agent_ms) ms of CPU on a machine without hibernation, over $AGENT_MAX_MS"
  [ "$(kv "$pb" agent_chain)" = 0 ] || bad "hibinit-agent is on the normal machine's critical chain: it delays the boot"
  say "normal machine: ok"
  # --- the machine launched with hibernation: hibernate and resume
  reach "$hid" verify "$tmp/tkey"; hip=$IP
  local t=$SECONDS before after mark
  until rssh "$hip" verify "$tmp/tkey" 'sudo test -s /etc/default/grub.d/99-set-swap.cfg && sudo test -f /swap-hibinit && ! systemctl is-active --quiet hibinit-agent.service' < /dev/null 2> /dev/null; do
    [ $((SECONDS - t)) -lt "$STEP_WAIT" ] || fail "hibinit-agent made no swap file and no resume= line on $hid in $STEP_WAIT s (its log: $(rssh "$hip" verify "$tmp/tkey" 'sudo journalctl -u hibinit-agent --no-pager -q -n 4 | cut -c1-200' < /dev/null 2>&1 | tr '\n' ' '))"
    nap
  done
  say "hibernating machine $hid: the agent's swap file and GRUB line are in place $((SECONDS - t)) s after it answered"
  before=$(rssh "$hip" verify "$tmp/tkey" 'cat /proc/sys/kernel/random/boot_id' < /dev/null) || fail "no boot id"
  mark=superci-image-$RANDOM-$RANDOM
  rssh "$hip" verify "$tmp/tkey" "echo $mark > /dev/shm/superci-image-mark" < /dev/null || fail "no memory marker"
  t=$SECONDS
  local out
  until out=$($A ec2 stop-instances --region $R --instance-ids "$hid" --hibernate --query 'StoppingInstances[0].CurrentState.Name' --output text < /dev/null 2>&1); do
    case $out in *"not ready to hibernate"*) ;; *) fail "StopInstances Hibernate=true on $hid: $out" ;; esac
    [ $((SECONDS - t)) -lt "$HIB_WAIT" ] || fail "EC2 still refused the hibernate of $hid after $HIB_WAIT s: $out"
    nap
  done
  local waited=$((SECONDS - t)) reason st
  t=$SECONDS
  until_state "$hid" stopped "$STEP_WAIT" "hibernating (logind's HandleSuspendKey=hibernate, or the machine sits in stopping)"
  reason=$($A ec2 describe-instances --region $R --instance-ids "$hid" --query 'Reservations[0].Instances[0].StateReason.Code' --output text < /dev/null)
  [ "$reason" = Client.UserInitiatedHibernate ] || fail "$hid stopped with reason '$reason', not Client.UserInitiatedHibernate: it was stopped, not hibernated"
  say "hibernated $hid: EC2 accepted the call after $waited s, stopped $((SECONDS - t)) s later ($reason)"
  $A ec2 start-instances --region $R --instance-ids "$hid" > /dev/null < /dev/null || fail "start-instances on $hid failed"
  t=$SECONDS
  reach "$hid" verify "$tmp/tkey"; hip=$IP
  after=$(rssh "$hip" verify "$tmp/tkey" 'cat /proc/sys/kernel/random/boot_id' < /dev/null) || fail "no boot id after the resume"
  [ "$after" = "$before" ] || fail "$hid resumed with boot id $after, it had $before: it rebooted, it did not resume"
  [ "$(rssh "$hip" verify "$tmp/tkey" 'cat /dev/shm/superci-image-mark' < /dev/null)" = "$mark" ] || fail "$hid lost its memory across the hibernate"
  say "resumed $hid: ready to ssh $((SECONDS - t)) s after the start call, boot id $after before and after, memory kept"
  [ "$BADCOUNT" = 0 ] || fail "$BADCOUNT check(s) of the normal machine failed, as listed above"
  say "proved $ami"
}
# --- run -----------------------------------------------------------------------------------------------------------------
declare -A FINAL
for r in $REGIONS; do FINAL[$r]=$(image_in "$r" superci-pull-image); done
if [ -z "${FINAL[$R]}" ]; then
  CAND=$(image_in $R superci-pull-image-candidate)
  if [ -n "$CAND" ]; then say "candidate $CAND of this build exists: proving it again, building nothing"; else build_candidate; fi
  UNPROVEN=$CAND
  SNAP=$($A ec2 describe-images --region $R --image-ids "$CAND" --query 'Images[0].BlockDeviceMappings[0].Ebs.SnapshotId' --output text < /dev/null)
  verify "$CAND"
  $A ec2 create-tags --region $R --resources "$CAND" --tags "Key=superci-pull-image,Value=$BUILD" Key=purpose,Value=superci-pull < /dev/null
  $A ec2 delete-tags --region $R --resources "$CAND" --tags "Key=superci-pull-image-candidate" < /dev/null
  FINAL[$R]=$CAND UNPROVEN=
  say "$R $CAND: proved, tagged superci-pull-image=$BUILD and purpose=superci-pull"
else
  say "$R ${FINAL[$R]}: exists, reused (no builder, no copy)"
fi
for r in $REGIONS; do
  if [ -n "${FINAL[$r]}" ]; then [ "$r" = $R ] || say "$r ${FINAL[$r]}: exists, reused (no copy)"; continue; fi
  new=$($A ec2 copy-image --region "$r" --source-region $R --source-image-id "${FINAL[$R]}" --name "superci-pull-$BUILD" \
    --description "superci-pull's hibernation-capable image of $BASE (pull/deploy/image.sh)" --query ImageId --output text < /dev/null) || fail "copy-image to $r failed"
  wait_image "$r" "$new"
  $A ec2 create-tags --region "$r" --resources "$new" --tags "Key=superci-pull-image,Value=$BUILD" Key=purpose,Value=superci-pull < /dev/null
  FINAL[$r]=$new
  say "$r $new: copied from ${FINAL[$R]}, tagged superci-pull-image=$BUILD and purpose=superci-pull"
done
say "pin these in config.json (the release moves the pin; the old copies stay, a rollback to a commit that pins them needs them):"
python3 -I -c 'import json,sys; print("  \"image\": " + json.dumps(dict(zip(sys.argv[1::2], sys.argv[2::2])), indent=2).replace("\n", "\n  "))' $(for r in $REGIONS; do echo "$r ${FINAL[$r]}"; done)
