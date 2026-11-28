#!/bin/bash
# image.sh end to end with EC2 and ssh stubbed (testdata/image/aws, testdata/image/ssh): the hibernation-capable image is built from
# the pin once, proved by hibernating and resuming a machine, tagged as the pin's copies are and copied; every refusal and
# every failed proof leaves nothing tagged, copied or deregistered, and every machine terminated. Run by image_script_test.go;
# needs bash and python3.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
# The fake aws and ssh are put on PATH from a copy made executable here, so the test needs no mode bits of the files it is given.
mkdir -p "$T/bin"; install -m 755 "$here/testdata/image/aws" "$here/testdata/image/ssh" "$T/bin"
export FAKE_AWS=$T/aws PATH=$T/bin:$PATH SUPERCI_TEST=1 IMAGE_NAP=0 IMAGE_STEP_WAIT=5 IMAGE_HIB_WAIT=5
failed=0
check() { if [ "$1" = 0 ]; then echo "ok    $2"; else echo "FAIL  $2"; failed=1; fi; }
refuses() { local name=$1 want=$2; shift 2; out=$("$@" 2>&1); local rc=$?
  if [ $rc != 0 ] && grep -qF -- "$want" <<< "$out"; then check 0 "$name"; else check 1 "$name (rc $rc)"; echo "$out" | tail -5 | sed 's/^/      /'; fi; }
D=$here/image.sh
CALLS=$FAKE_AWS/calls.log
P1=ami-0aaaa1111bbbb2222 P2=ami-0cccc3333dddd4444 BASE=runs-on-v2.2-ubuntu24-full-x64-20261120075252
BUILD=$BASE-hib-$(sha256sum "$here/image-setup.sh" | cut -c1-8)
# The config the script reads: config.json with the test pins.
sed -E "s/(\"us-east-1\": )\"ami-[^\"]*\"/\1\"$P1\"/; s/(\"us-east-2\": )\"ami-[^\"]*\"/\1\"$P2\"/" "$here/../config.json" > "$T/config.json"
export IMAGE_CONFIG=$T/config.json
CONFIG_SUM=$(sha256sum "$T/config.json")
world() { rm -rf "$FAKE_AWS"; mkdir -p "$FAKE_AWS"; : > "$CALLS"; python3 -I "$T/bin/aws" __seed__ "$BASE" "$P1" "$P2"; }
fault() { echo "${2:-1}" > "$FAKE_AWS/$1"; }
run() { OUT=$(bash "$D" 2>&1); RC=$?; }
called() { grep -cE -- "$1" "$CALLS" || true; }
state() { python3 -I -c 'import json,os,sys; s=json.load(open(os.environ["FAKE_AWS"]+"/state.json")); exec(sys.argv[1])' "$1"; }
# Every machine the run made is terminated (at least one was made), and the run said so for each.
all_terminated() { local n; n=$(state 'print(len(s["instances"]))'); [ "$n" -gt 0 ] &&
  [ "$(state 'print(sum(1 for i in s["instances"].values() if i["state"] != "terminated"))')" = 0 ] &&
  [ "$(grep -c '^image: terminated i-.* (terminated)$' <<< "$OUT")" = "$n" ]; }

# --- the setup is only what the image needs
S=$here/image-setup.sh
grep -q '^systemctl enable hibinit-agent.service$' "$S" && grep -q 'HandleSuspendKey=hibernate' "$S"; check $? "image-setup.sh enables hibinit-agent and makes the sleep key hibernate"
[ -f "$S" ] && ! grep -vE '^\s*#' "$S" | grep -qE 'swapon|fallocate|mkswap|fstab|--now|systemctl (start|restart)|reboot|dd '; check $? "and makes no swap, no fstab line and starts nothing: a machine without hibernation gets no swap file"

[ -f "$S" ] && grep -q 'ExecStartPre=/usr/local/sbin/superci-hibinit-prepare' "$S" && grep -q 'meta-data/hibernation/configured' "$S" && grep -q 'resize2fs' "$S"; check $? "and grows the root first, on the agent's own unit and only when the metadata service says hibernation is configured: the agent refuses a root smaller than RAM"
grep -qF "Insufficient disk space" <<< "$(world; fault no-swap; run; echo "$OUT")"; check $? "(and a run whose agent made no swap file prints the agent's own log)"

# --- refusals before anything is launched
world
python3 -I -c 'import json,sys; c=json.load(open(sys.argv[1])); del c["image"]; json.dump(c, open(sys.argv[2], "w"))' "$T/config.json" "$T/nopin.json"
refuses_quietly() { local name=$1 want=$2; shift 2; : > "$CALLS"; refuses "$name" "$want" "$@"; [ ! -s "$CALLS" ] && grep -qF -- "$want" <<< "$out"; check $? "and asked EC2 nothing"; }
refuses_quietly "image.sh refuses a config.json with no image pin" "pins 'nothing' for us-east-1" env IMAGE_CONFIG="$T/nopin.json" bash "$D"
sed -E "s/\"$P1\"/\"ami-REPLACE-us-east-1\"/" "$T/config.json" > "$T/placeholder.json"
refuses_quietly "image.sh refuses a placeholder pin" "pins 'ami-REPLACE-us-east-1' for us-east-1, not an AMI id" env IMAGE_CONFIG="$T/placeholder.json" bash "$D"
world; python3 -I -c 'import json,os; p=os.environ["FAKE_AWS"]+"/state.json"; s=json.load(open(p)); s["images"]["us-east-1"].pop("'$P1'"); json.dump(s,open(p,"w"))'
refuses "image.sh refuses a pin EC2 does not list" "the pinned image $P1 in us-east-1: EC2 does not list it" bash "$D"
[ "$(called 'run-instances')" = 0 ] && grep -qF "EC2 does not list it" <<< "$out"; check $? "and launched nothing"
world; python3 -I -c 'import json,os; p=os.environ["FAKE_AWS"]+"/state.json"; s=json.load(open(p)); s["images"]["us-east-1"]["'$P1'"]["owner"]="135269210855"; json.dump(s,open(p,"w"))'
refuses "image.sh refuses a pin that is another account's" "owned by 135269210855, not 243891825325" bash "$D"
world; python3 -I -c 'import json,os; p=os.environ["FAKE_AWS"]+"/state.json"; s=json.load(open(p)); s["images"]["us-east-1"]["'$P1'"]["state"]="pending"; json.dump(s,open(p,"w"))'
refuses "image.sh refuses a pin that is not available" "is pending, not available" bash "$D"
world; python3 -I -c 'import json,os; p=os.environ["FAKE_AWS"]+"/state.json"; s=json.load(open(p)); s["images"]["us-east-1"]["'$P1'"]["tags"]={}; json.dump(s,open(p,"w"))'
refuses "image.sh refuses a pin with no build tag" "has no superci-pull-image tag" bash "$D"
world; fault account 060795898020
refuses "image.sh refuses another account" "not ci-runners" bash "$D"
[ "$(called 'run-instances|describe-images')" = 0 ] && grep -qF "not ci-runners" <<< "$out"; check $? "and launched and read no image"

# --- a build from the pin, proved, tagged and copied
world; fault hib-notready 2
run
[ $RC = 0 ] && grep -qF "image: build $BUILD (the pin $P1 is $BASE" <<< "$OUT"; check $? "image.sh builds the hibernation image of the pin: the build is the pin's build plus the setup's hash"
[ "$(called 'ec2 run-instances')" = 4 ] && [ "$(called 'ec2 create-image')" = 1 ] && [ "$(called 'ec2 copy-image')" = 1 ]; check $? "from one builder, three proving machines, one image and one copy"
grep -q "ec2 run-instances --region us-east-1 --image-id $P1 --instance-type m8a.large" "$CALLS"; check $? "the builder is a machine of the pin"
B=$(state 'print(next(k for k,v in s["instances"].items() if v["type"]=="m8a.large"))')
cmp -s "$here/image-setup.sh" "$FAKE_AWS/setup-stdin.$B" && grep -q 'userdel -rf image' "$FAKE_AWS/seal-stdin.$B" && grep -q 'cloud-init clean --logs' "$FAKE_AWS/seal-stdin.$B" && grep -q 'rm -f /etc/sudoers.d/90-image' "$FAKE_AWS/seal-stdin.$B"
check $? "the builder is given exactly image-setup.sh, then sealed: its user, sudo rule and cloud-init's record gone"
[ "$(state 'print(s["instances"]["'$B'"]["state"])')" = terminated ] && grep -q 'ec2 stop-instances --region us-east-1 --instance-ids '"$B"'$' "$CALLS"; check $? "and stopped before the image was made, then terminated"
HIB=$(grep 'ec2 run-instances' "$CALLS" | grep -- '--hibernation-options Configured=true')
[ "$(wc -l <<< "$HIB")" = 1 ] && grep -q -- '--instance-type m8azn.3xlarge' <<< "$HIB" && grep -q 'VolumeSize=128,' <<< "$HIB" && grep -q 'Encrypted=true' <<< "$HIB"
check $? "the machine proved for hibernation is the largest hibernating type (m8azn.3xlarge: 48 GiB + superci-8cpu's 80 GB = a 128 GB encrypted root), with HibernationOptions.Configured"
PLAIN=$(grep 'ec2 run-instances' "$CALLS" | grep 'superci-pull-image-normal')
grep -q -- '--instance-type m8azn.3xlarge' <<< "$PLAIN" && grep -q 'VolumeSize=80,' <<< "$PLAIN" && ! grep -q 'Encrypted\|hibernation' <<< "$PLAIN"
check $? "the machine proved for a normal boot is launched as the fleet's are: the set's 80 GB, unencrypted, no hibernation"
REF=$(grep 'ec2 run-instances' "$CALLS" | grep 'superci-pull-image-reference')
grep -q -- "--image-id $P1 " <<< "$REF" && ! grep -q 'Encrypted\|hibernation' <<< "$REF"; check $? "and beside it a machine of the pin, launched the same way, to compare with"
[ "$(called 'stop-instances .*--hibernate')" = 3 ]; check $? "EC2 refused the hibernate twice as not ready: it was retried until accepted"
grep -qF "hibernated i-" <<< "$OUT" && grep -qF "(Client.UserInitiatedHibernate)" <<< "$OUT" && grep -qE "resumed i-.*boot id boot-i-[0-9a-f]+-1 before and after, memory kept" <<< "$OUT"; check $? "the hibernating machine came back with the boot id and the memory it had"
grep -qE "normal boot: 4500 ms against the pin's 4400 ms \(100 ms; hibinit-agent used 120 ms of CPU and made no swap file\)" <<< "$OUT"; check $? "the normal machine's boot is measured against the pin's, and hibinit-agent's own time said"
CAND=$(state 'print(next(k for k,v in s["images"]["us-east-1"].items() if "from" in v))')
N=$(state 'print(next(k for k,v in s["instances"].items() if v["ami"]=="'$CAND'" and not v["hib"]))'); RF=$(state 'print(next(k for k,v in s["instances"].items() if v["ami"]=="'$P1'" and v["type"]!="m8a.large"))')
warm=1; { [ -n "$N" ] && [ -n "$RF" ]; } || warm=0
for m in $N $RF; do grep -q "^$m verify@[0-9.]* sudo dd if=/dev/nvme0n1 of=/dev/null bs=4M count=7680" "$FAKE_AWS/ssh.log" && grep -q "^$m verify@[0-9.]* sudo systemctl reboot" "$FAKE_AWS/ssh.log" && [ "$(grep -n "^$m .* sudo systemctl reboot" "$FAKE_AWS/ssh.log" | head -1 | cut -d: -f1)" -lt "$(grep -n "^$m .* sudo bash -s" "$FAKE_AWS/ssh.log" | head -1 | cut -d: -f1)" ] || warm=0; done
[ $warm = 1 ]
check $? "both are measured on a second boot, after one read of the root volume (a fresh snapshot's first boots are cold)"
CAND=$(state 'print(next(k for k,v in s["images"]["us-east-1"].items() if "from" in v))')
[ "$(state 'print(sorted(s["images"]["us-east-1"]["'$CAND'"]["tags"].items()))')" = "[('purpose', 'superci-pull'), ('superci-pull-image', '$BUILD')]" ]; check $? "the image is tagged superci-pull-image=<build> and purpose=superci-pull, and no candidate tag is left"
E2=$(state 'print(next(k for k,v in s["images"]["us-east-2"].items() if "copy-of" in v))')
[ "$(state 'print(sorted(s["images"]["us-east-2"]["'$E2'"]["tags"].items()))')" = "[('purpose', 'superci-pull'), ('superci-pull-image', '$BUILD')]" ] &&
  grep -q "ec2 copy-image --region us-east-2 --source-region us-east-1 --source-image-id $CAND --name superci-pull-$BUILD" "$CALLS"; check $? "the copy in us-east-2 is tagged the same, and made from the proved image"
grep -qF "\"us-east-1\": \"$CAND\"" <<< "$OUT" && grep -qF "\"us-east-2\": \"$E2\"" <<< "$OUT"; check $? "and the ids to pin are printed"
[ "$(state 'print(s["images"]["us-east-1"]["'$P1'"]["tags"]["superci-pull-image"], len(s["images"]["us-east-1"]), len(s["images"]["us-east-2"]))')" = "$BASE 2 2" ]; check $? "the pin's own copies are untouched, and one image was added to each region"
[ "$(called 'ec2 create-image')" = 1 ] && [ "$(called 'ec2 copy-image')" = 1 ] && [ "$(called 'deregister-image|delete-snapshot')" = 0 ]; check $? "image.sh never deregisters an image or deletes a snapshot, in a run that made one image and copied it"
[ "$(called 'ec2 create-image')" = 1 ] && [ "$(sha256sum "$T/config.json")" = "$CONFIG_SUM" ]; check $? "and never writes config.json"
all_terminated; check $? "every machine it made was terminated, and confirmed terminated"
grep -q "superci-pull-image-build" "$CALLS" && ! grep 'ec2 run-instances' "$CALLS" | grep -q 'ResourceType=instance,Tags=\[[^]]*superci-lab'; check $? "(the machines are tagged purpose=superci-pull-image-build)"

# --- the same build is reused
: > "$CALLS"; run
[ $RC = 0 ] && grep -qF "image: us-east-1 $CAND: exists, reused (no builder, no copy)" <<< "$OUT" && grep -qF "image: us-east-2 $E2: exists, reused (no copy)" <<< "$OUT"; check $? "a second run finds the build by its tag in both regions and reuses it"
[ $RC = 0 ] && grep -qF ": exists, reused" <<< "$OUT" && [ "$(called 'run-instances|create-image|copy-image|create-tags|delete-tags|stop-instances|terminate-instances|modify-image-attribute')" = 0 ]; check $? "and launches, makes, copies and tags nothing"
grep -qF "\"us-east-1\": \"$CAND\"" <<< "$OUT" && grep -qF "\"us-east-2\": \"$E2\"" <<< "$OUT"; check $? "and prints the same ids"
[ $RC = 0 ] && grep -qF ": exists, reused" <<< "$OUT" && [ "$(called 'deregister-image|delete-snapshot')" = 0 ]; check $? "(and deregisters nothing)"
# Only the copy is missing (a run that ended after the proof): it is copied, nothing is built or proved again.
python3 -I -c 'import json,os; p=os.environ["FAKE_AWS"]+"/state.json"; s=json.load(open(p)); s["images"]["us-east-2"].pop("'$E2'"); json.dump(s,open(p,"w"))'; : > "$CALLS"; run
[ $RC = 0 ] && [ "$(called 'run-instances|create-image')" = 0 ] && [ "$(called 'ec2 copy-image')" = 1 ] && [ "$(called 'create-tags .*us-east-2|create-tags --region us-east-2')" = 1 ]; check $? "a run that finds the proved image in us-east-1 and none in us-east-2 only copies it, and tags the copy"
# The pin has moved to the build itself: it is its own build.
world; python3 -I -c 'import json,os; p=os.environ["FAKE_AWS"]+"/state.json"; s=json.load(open(p)); [i["tags"].update({"superci-pull-image": "'$BUILD'"}) for r in s["images"].values() for i in r.values()]; json.dump(s,open(p,"w"))'; run
[ $RC = 0 ] && [ "$(called 'run-instances|create-image|copy-image')" = 0 ] && grep -qF "image: build $BUILD (the pin $P1 is $BUILD" <<< "$OUT"; check $? "run against a pin that is already this build, it is reused (the build id is the same)"
# An edit to the setup is another build, built beside the first: the base is the pin's build without its -hib- suffix.
world; python3 -I -c 'import json,os; p=os.environ["FAKE_AWS"]+"/state.json"; s=json.load(open(p)); [i["tags"].update({"superci-pull-image": "'$BASE'-hib-00000000"}) for r in s["images"].values() for i in r.values()]; json.dump(s,open(p,"w"))'; run
[ $RC = 0 ] && grep -qF "image: build $BUILD (the pin $P1 is $BASE-hib-00000000" <<< "$OUT" && [ "$(called 'ec2 create-image')" = 1 ]; check $? "a pin that is an earlier build of this image builds the new one, named from the same RunsOn build"
# A candidate left by a run that ended before its proof is proved again, not built again.
world; fault hib-refuse; run
[ $RC != 0 ] && [ "$(called 'ec2 create-image')" = 1 ]; check $? "(a refused hibernate leaves a candidate)"
rm "$FAKE_AWS/hib-refuse"; : > "$CALLS"; run
[ $RC = 0 ] && [ "$(called 'ec2 create-image')" = 0 ] && [ "$(called 'ec2 run-instances')" = 3 ] && grep -qF "of this build exists: proving it again, building nothing" <<< "$OUT"; check $? "the next run proves that candidate again without a builder, and tags it"

# --- fail closed: a proof that fails leaves nothing tagged as the pin, copied or deregistered, and no machine running
failing() { # name, fault file, what the run says, and "build" when it fails before there is a candidate
  local name=$1 f=$2 want=$3 kind=${4:-proof}
  world; fault "$f"; run
  [ $RC != 0 ] && grep -qF -- "$want" <<< "$OUT"; check $? "image.sh fails when $name"
  local ran=0; [ "$(called 'ec2 run-instances')" -gt 0 ] && ran=1
  if [ "$kind" = proof ]; then ran=0; [ "$(called 'ec2 create-image')" = 1 ] && [ "$(called 'ec2 run-instances')" = 4 ] && ran=1; fi
  [ $ran = 1 ] && [ "$(called 'ec2 create-tags|ec2 copy-image|ec2 delete-tags')" = 0 ]; check $? "and, having got that far, tags no image as the pin, copies none and deletes no tag"
  [ $ran = 1 ] && [ "$(called 'deregister-image|delete-snapshot')" = 0 ]; check $? "and deregisters and deletes nothing"
  if [ "$kind" = proof ]; then
    grep -qF "stays untagged for the pin (superci-pull-image-candidate=$BUILD only); nothing was copied. Delete it by hand: aws --profile ci-runners ec2 deregister-image --region us-east-1 --image-id ami-" <<< "$OUT"; check $? "and leaves the candidate untagged for the pin, with the line that deletes it printed"
    all_terminated; check $? "and terminated every machine"
  else
    [ $ran = 1 ] && ! grep -qF "stays untagged" <<< "$OUT" && [ $RC != 0 ] && [ "$(called 'ec2 create-image')" = 0 ]; check $? "and made no image"
    [ $ran = 1 ] && [ $RC != 0 ] && [ "$(state 'print(sum(1 for i in s["instances"].values() if i["state"] != "terminated"))')" = 0 ]; check $? "and left no machine running"
  fi
}
failing "the hibernated machine comes back with another boot id (it rebooted)" resume-reboots "it rebooted, it did not resume"
failing "the hibernated machine loses its memory" memory-lost "lost its memory across the hibernate"
failing "the machine is stopped, not hibernated" stop-not-hibernate "it was stopped, not hibernated"
failing "the machine sits in stopping (the sleep key does not hibernate)" stuck-stopping "hibernating (logind's HandleSuspendKey=hibernate, or the machine sits in stopping)"
failing "EC2 keeps refusing the hibernate" hib-refuse "EC2 still refused the hibernate of"
failing "a machine launched without hibernation has swap on" normal-swap "has swap_active=1, not 0 (hibinit-agent must do nothing there)"
failing "a machine launched without hibernation boots over a second slower than the pin's" normal-slow "boots 5000 ms slower than the pin's, over 1500"
failing "the agent makes no swap file on the hibernating machine (it refused for want of disk, say)" no-swap "hibinit-agent made no swap file and no resume= line on i-" 
failing "hibinit-agent uses 900 ms of CPU on a machine without hibernation" agent-slow "hibinit-agent used 900 ms of CPU on a machine without hibernation, over 500"
failing "hibinit-agent is on the normal machine's critical chain" agent-critical "hibinit-agent is on the normal machine's critical chain: it delays the boot"
failing "the setup fails on the builder" setup-fails "the setup failed on the builder" build
failing "a machine cannot be launched" run-fails "run-instances for builder" build

exit $failed
