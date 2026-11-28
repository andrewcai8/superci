#!/bin/bash
# The release's parity check: a job on superci runs in the environment a job on Blacksmith does. It boots one machine
# of superci-4cpu's shape from exactly the image the listener will boot, with the commit's boot.sh up to its job's
# branch, where it runs pull/parity/probe.sh as the agent runs the job (agent.py --probe: the runner user, its groups,
# the job's mask and environment), and holds what it prints against pull/parity/blacksmith-ubuntu-2404.txt with
# pull/parity/compare.py. Exits 1 on any difference, which fails release.sh before it uploads anything: a boot change or
# a new image that would give jobs another environment never reaches a machine that takes one. The machine never reaches
# the listener (its address is TEST-NET-3) and is terminated on exit; it is reached with a key made for the run (EC2
# Instance Connect).
#
# The image: a commit whose config.json has an "image" map boots the AMI id it pins for each region, a copy in our
# account (findImage in fleet.go uses that id and no other). Before booting, each region's pin is checked the way the
# listener checks it: the image exists, is available, is ours and carries the build tag (superci-pull-image) the other
# regions' copy carries, and no placeholder (ami-REPLACE-<region>) is left. A commit from before the pin (a rollback)
# boots what its listener would: findImage's old rule over the ImageConfig in its main.go (the pinned RunsOn build, or
# once RunsOn has deleted it the oldest newer build). A commit with neither boots this checkout's pins.
#
# What is the commit's and what is this checkout's: the commit's boot.sh, hydrate-stop.sh, image, regions
# (config.json) and machine type (deploy/override.json) are what its release would run. The check itself (probe.sh,
# compare.py, the manifest) is this checkout's, so a rollback to a commit from before the check is checked by today's
# check. agent.py is the commit's when it has --probe, else this checkout's; each line it prints says which.
#
#   pull/deploy/parity.sh <commit>          prints where each part came from, the image it boots, the probe's
#                                           differences and a last line "parity: PASS ..." or "parity: FAIL ..."
#   pull/deploy/parity.sh --pins            prints each region's pin (region, AMI id), read from this checkout's config.json
#   PARITY_SKIP=<reason> pull/deploy/parity.sh <commit>
#                                           an emergency release without the check: boots nothing, prints the reason
#                                           and exits 0; release.sh records it in the release's bin/RELEASE.json
set -euo pipefail
here=$(cd "$(dirname "$0")/.." && pwd)
A="aws --profile ci-runners"
R=us-east-1
OUR=243891825325
# The machine phase (boot to probe) is bounded as a whole, so no stalled wait or ssh holds a release.
BOUND=900
[ "${SUPERCI_TEST:-}" = 1 ] && BOUND=${PARITY_TIMEOUT:-$BOUND}
# pins <config.json>: "<region> <AMI id>" for each region the listener serves (an unpinned region has an empty id);
# prints nothing and fails when the config has no "image" map, as a commit from before the pin has none.
pins() {
  python3 -I -c 'import json,sys
c = json.load(open(sys.argv[1]))
if "image" not in c:
    sys.exit(1)
for r in sorted(c["regions"]):
    print(r, c["image"].get(r, ""))' "$1"
}
# image_config <main.go>: the owner, name pattern and pin of the ImageConfig a listener from before the pin boots (the
# literal NewFleet was called with, or the fleetImage var).
image_config() {
  sed -n 's/.*ImageConfig{Owner: "\([0-9]*\)", Name: "\([^"]*\)", Pin: "\([0-9]*\)"}.*/\1 \2 \3/p' "$1" | head -1 | grep .
}

# machine: run by the check under `timeout`, with the instance it launched: waits for it, puts agent.py and the probe
# in place over ssh and leaves the probe's output in $PARITY_TMP/probe.txt. The check terminates the instance.
machine() {
  local tmp=$PARITY_TMP ID=$PARITY_ID IP ZONE S
  step() { echo "$1" > "$tmp/step"; }
  step "waiting for $ID to run"
  $A ec2 wait instance-running --region $R --instance-ids "$ID"
  read -r IP ZONE < <($A ec2 describe-instances --region $R --instance-ids "$ID" --query 'Reservations[0].Instances[0].[PublicIpAddress,Placement.AvailabilityZone]' --output text)
  # The machine's own user data puts this run's key on a user of its own (parity), as the lab did: the image's sshd does
  # not take an EC2 Instance Connect key (Nov 28: the push answered true, ssh "Permission denied (publickey)").
  S="ssh -i $tmp/key -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -o ServerAliveInterval=5 -o ServerAliveCountMax=3 -o LogLevel=ERROR parity@$IP"
  if [ "${SUPERCI_TEST:-}" = 1 ] && [ -n "${PARITY_SSH:-}" ]; then S=$PARITY_SSH; fi
  step "waiting for $ID ($IP) to reach its probe"
  until $S test -d /run/superci-parity 2> /dev/null; do sleep 5; done
  step "putting the probe on $ID"
  tar -C "$tmp/put" -cf - agent.py probe.sh | $S 'sudo tar -C /run/superci-parity -xf - --no-same-owner && sudo touch /run/superci-parity/go'
  step "waiting for the probe on $ID to finish"
  until $S test -e /run/superci-parity/rc 2> /dev/null; do sleep 2; done
  step "reading the probe's output from $ID"
  $S 'cat /run/superci-parity/out' > "$tmp/probe.txt"
  [ "$($S 'cat /run/superci-parity/rc')" = 0 ] || { echo "parity: the probe failed: $($S 'tail -5 /run/superci-parity/err')"; exit 1; }
}
if [ "${1:-}" = --machine ]; then machine; exit 0; fi
if [ "${1:-}" = --pins ]; then pins "$here/config.json"; exit 0; fi
REV=${1:?commit}
if [ -n "${PARITY_SKIP:-}" ]; then
  echo "parity: SKIPPED for $REV by PARITY_SKIP: $PARITY_SKIP (no machine booted, nothing compared)"
  exit 0
fi
[ "$($A sts get-caller-identity --query Account --output text)" = $OUR ] || { echo "refusing: not ci-runners" >&2; exit 1; }
ID= tpid=
tmp=$(mktemp -d)
cleanup() {
  # The machine phase's timeout, which takes its ssh and waits with it, when the check is stopped while it runs.
  if [ -n "$tpid" ]; then kill "$tpid" 2> /dev/null || true; fi
  if [ -n "$ID" ]; then
    # Once more after a pause if the first call fails; either way the rest of the cleanup runs.
    if $A ec2 terminate-instances --region $R --instance-ids "$ID" > /dev/null || { sleep 2; $A ec2 terminate-instances --region $R --instance-ids "$ID" > /dev/null; }; then
      echo "parity: terminated $ID"
    else
      echo "parity: could not terminate $ID: terminate it by hand (tag purpose=superci-pull-parity)" >&2
    fi
  fi
  rm -rf "$tmp" || true
}
trap cleanup EXIT
trap 'exit 130' INT TERM
git -C "$here/.." archive "$REV" pull | tar -C "$tmp" -xf -
rev=$tmp/pull
co=$(git -C "$here/.." rev-parse --short HEAD 2> /dev/null || echo unknown)
# What is the commit's (theirs) and what is this checkout's (ours), for the line that says so.
theirs="boot.sh, hydrate-stop.sh" ours="probe.sh, compare.py, blacksmith-ubuntu-2404.txt"
for f in boot.sh hydrate-stop.sh; do
  [ -f "$rev/$f" ] || { echo "parity: FAIL: $REV has no pull/$f; a release without the check needs PARITY_SKIP=<reason>"; exit 1; }
done
# from <path> <what>: sets FROM to the commit's file, or this checkout's when the commit has none.
from() { if [ -e "$rev/$1" ]; then FROM=$rev/$1 theirs+=", $2"; else FROM=$here/$1 ours+=", $2 (the commit has none)"; fi; }
from config.json "the regions (config.json)"; CONFIG=$FROM
# The image: the commit's pin; or before the pin, its main.go's ImageConfig; or with neither, this checkout's pin.
if [ -e "$rev/config.json" ] && pins "$rev/config.json" > "$tmp/pins"; then MODE=pin theirs+=", the image (config.json's pin)"
elif read -r OWNER NAME PIN < <(image_config "$rev/main.go"); then MODE=name theirs+=", the image (main.go's ImageConfig, from before the pin)"
else
  pins "$here/config.json" > "$tmp/pins" || { echo "parity: FAIL: no image pin or ImageConfig in $REV, and no pin in this checkout's config.json"; exit 1; }
  MODE=pin ours+=", the image (config.json's pin; the commit names none)"
fi
from deploy/override.json "the machine type (deploy/override.json)"; OVERRIDE=$FROM
if grep -qF -- '--probe' "$rev/agent.py" 2> /dev/null; then AGENT=$rev/agent.py theirs+=", agent.py"; else
  AGENT=$here/agent.py ours+=", agent.py (the commit's has no --probe)"
fi
MANIFEST=$here/parity/blacksmith-ubuntu-2404.txt
echo "parity: checking $REV"
echo "parity:   from $REV: $theirs"
echo "parity:   from this checkout ($co): $ours"
if [ "$MODE" = pin ]; then
  # Every region's pin, checked as findImage checks it: no placeholder, listed, our own, available. A copy is tagged
  # with the RunsOn build it was made from, which says whether the regions' copies are one image.
  declare -A AMIS BUILDS
  while read -r r ami; do
    [[ $ami =~ ^ami-[0-9a-f]{8}([0-9a-f]{9})?$ ]] || { echo "parity: FAIL: $REV's config.json pins '${ami:-nothing}' for $r, not an AMI id (a placeholder ami-REPLACE-<region> ships nowhere): make the copy and pin it, as the README's \"Moving the image pin\" says"; exit 1; }
    row=$($A ec2 describe-images --region "$r" --image-ids "$ami" \
      --query "Images[0].[OwnerId,State,ImageId,Tags[?Key=='superci-pull-image']|[0].Value]" --output text 2> /dev/null < /dev/null) || row=None
    read -r owner state id build <<< "$row"
    [ "$owner" != None ] && [ "${id:-}" = "$ami" ] || { echo "parity: FAIL: the pinned image $ami in $r: EC2 does not list it"; exit 1; }
    [ "$owner" = $OUR ] || { echo "parity: FAIL: the pinned image $ami in $r is owned by $owner, not $OUR: only our own copy is pinned"; exit 1; }
    [ "$state" = available ] || { echo "parity: FAIL: the pinned image $ami in $r is $state, not available"; exit 1; }
    [ "${build:-None}" != None ] || { echo "parity: FAIL: the pinned image $ami in $r has no superci-pull-image tag (the RunsOn build it copies)"; exit 1; }
    AMIS[$r]=$ami BUILDS[$r]=$build
  done < "$tmp/pins"
  [ -n "${AMIS[$R]:-}" ] || { echo "parity: FAIL: $REV's config.json serves no region $R"; exit 1; }
  AMI=${AMIS[$R]} BUILD=${BUILDS[$R]}
  for r in "${!AMIS[@]}"; do
    [ "${BUILDS[$r]}" = "$BUILD" ] || { echo "parity: FAIL: $r pins a copy of build ${BUILDS[$r]}, $R of $BUILD: jobs would run on two images"; exit 1; }
  done
  WHAT="the pin of $REV's config.json for $R, our copy of build $BUILD, the build every region's pin copies"
else
  # pick <region>: the old findImage's choice there, as "<build> <image id>".
  pick() {
    $A ec2 describe-images --region "$1" --owners "$OWNER" self --filters "Name=name,Values=$NAME" Name=state,Values=available \
      --query 'Images[].[Name,ImageId]' --output text | awk '{ n = split($1, p, "-"); print p[n], $2 }' | LC_ALL=C sort |
      awk -v pin="$PIN" '{ last = $0 } !got && $1 >= pin { got = $0 } END { print (got != "" ? got : last) }'
  }
  read -r BUILD AMI < <(pick $R)
  [ -n "${AMI:-}" ] || { echo "parity: FAIL: no image $OWNER/$NAME in $R"; exit 1; }
  for r in $(python3 -I -c 'import json,sys; print(" ".join(sorted(json.load(open(sys.argv[1]))["regions"])))' "$CONFIG"); do
    read -r b _ < <(pick "$r")
    [ "$b" = "$BUILD" ] || { echo "parity: FAIL: $r boots build ${b:-none}, $R boots $BUILD: jobs would run on two images"; exit 1; }
  done
  WHAT="build $BUILD of $OWNER/$NAME, pin $PIN, the image every region picks"
fi
TYPE=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["sizes"]["superci-4cpu"]["types"][0])' "$OVERRIDE")
# The commit's boot.sh as the listener renders it for superci-4cpu, up to the job's branch, then the probe; gzipped as
# the listener's launch template holds it (encodeUserData in fleet.go): EC2 takes 16 KiB before its base64.
ssh-keygen -q -t ed25519 -N '' -f "$tmp/key"
python3 -I - "$rev" "$tmp/user-data" "$tmp/key.pub" << 'PY'
import gzip, re, sys
src, out, pub = sys.argv[1], sys.argv[2], open(sys.argv[3]).read().strip()
boot = open(f"{src}/boot.sh").read()
for k, v in {"@MODE@": "dispatch", "@LISTENER@": "https://203.0.113.1:8443", "@PIN@": "sha256//parity", "@LIFE_SECONDS@": "1800",
             "@LABEL@": "superci-4cpu", "@WARM_CAP_SECONDS@": "300", "@HYDRATE_STOP@": open(f"{src}/hydrate-stop.sh").read().strip()}.items():
    boot = boot.replace(k, v)
marker = "# Dispatch mode (pull/agent.py)"
if marker not in boot:
    sys.exit(f"parity: FAIL: boot.sh has no {marker!r} to stop at; a release without the check needs PARITY_SKIP=<reason>")
left = sorted(set(re.findall(r"@[A-Z_]+@", boot)))
if left:
    sys.exit(f"parity: FAIL: boot.sh has placeholders this check does not render: {', '.join(left)}")
text = boot[:boot.index(marker)] + """# pull/deploy/parity.sh: a user of the check's own, with this run's key, that the check reaches over ssh (the image's
# sshd does not take an EC2 Instance Connect key). The probe looks at the runner user, which this does not touch.
useradd -m -s /bin/bash parity
install -d -m 700 -o parity -g parity /home/parity/.ssh
printf '%s\\n' '""" + pub + """' > /home/parity/.ssh/authorized_keys
chown parity:parity /home/parity/.ssh/authorized_keys; chmod 600 /home/parity/.ssh/authorized_keys
printf 'parity ALL=(ALL) NOPASSWD:ALL\\n' > /etc/sudoers.d/90-parity; chmod 440 /etc/sudoers.d/90-parity
# boot's shell runs the probe as the agent runs a job, once the check has put them in place.
P=/run/superci-parity
install -d -m 755 $P
until [ -e $P/go ]; do sleep 1; done
python3 -I $P/agent.py --probe $P/probe.sh > $P/out 2> $P/err; echo $? > $P/rc
"""
zipped = gzip.compress(text.encode(), compresslevel=9, mtime=0)
if len(zipped) > 16384:
    sys.exit(f"parity: FAIL: the user data is {len(zipped)} bytes gzipped, over EC2's 16384")
open(out, "wb").write(zipped)
print(f"parity: user data {len(text)} bytes, {len(zipped)} gzipped")
PY
mkdir "$tmp/put"
install -m 755 "$AGENT" "$tmp/put/agent.py"
install -m 755 "$here/parity/probe.sh" "$tmp/put/probe.sh"
echo "parity: booting $AMI ($WHAT) as $TYPE in $R"
SG=$($A ec2 describe-security-groups --region $R --filters Name=group-name,Values=superci-pull-listener --query 'SecurityGroups[0].GroupId' --output text)
ID=$($A ec2 run-instances --region $R --image-id "$AMI" --instance-type "$TYPE" --security-group-ids "$SG" \
  --associate-public-ip-address --instance-initiated-shutdown-behavior terminate --iam-instance-profile Name=superci-pull-cache-machine \
  --metadata-options HttpTokens=required,HttpPutResponseHopLimit=1,HttpEndpoint=enabled \
  --block-device-mappings 'DeviceName=/dev/sda1,Ebs={VolumeSize=60,VolumeType=gp3,DeleteOnTermination=true}' \
  --tag-specifications "ResourceType=instance,Tags=[{Key=purpose,Value=superci-pull-parity},{Key=Name,Value=superci-pull-parity}]" "ResourceType=volume,Tags=[{Key=purpose,Value=superci-pull-parity}]" \
  --user-data "fileb://$tmp/user-data" --query 'Instances[0].InstanceId' --output text)
echo "parity: launched $ID"
rc=0
PARITY_TMP=$tmp PARITY_ID=$ID timeout -k 10 "$BOUND" "$0" --machine &
tpid=$!
wait "$tpid" || rc=$?
tpid=
case $rc in
  0) ;;
  124 | 137) echo "parity: FAIL: the machine phase did not finish in $BOUND s (at: $(cat "$tmp/step" 2> /dev/null || echo start))"; exit 1 ;;
  *) echo "parity: FAIL: the machine phase failed (exit $rc, at: $(cat "$tmp/step" 2> /dev/null || echo start))"; exit 1 ;;
esac
if python3 -I "$here/parity/compare.py" "$MANIFEST" "$tmp/probe.txt"; then
  echo "parity: PASS: a job on $AMI (build $BUILD) under $REV's boot.sh has Blacksmith's job environment"
else
  echo "parity: FAIL: a job on $AMI (build $BUILD) under $REV's boot.sh differs from Blacksmith's job environment, as listed above"
  exit 1
fi
