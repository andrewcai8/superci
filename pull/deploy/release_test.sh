#!/bin/bash
# The release end to end with AWS and the listener's SSH stubbed (testdata/aws, testdata/ssh): from a commit of a
# main, release.sh and cache/deploy/release.sh (with the real cache/deploy/up.sh) succeed and leave verify-release.sh
# passing, and every refusal the release relies on refuses. Run by release_test.go; needs bash, git and python3.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
export FAKE_AWS=$T/aws FAKE_HOST=$T/host PATH=$here/testdata:$PATH
# parity.sh's machine answers with a superci job's real probe (testdata/parity-probe.txt) unless a case changes it.
export SUPERCI_TEST=1 PARITY_SSH=$here/testdata/parity-ssh PARITY_PROBE=$T/probe.txt
cp "$here/testdata/parity-probe.txt" "$T/probe.txt"
mkdir -p "$FAKE_AWS" "$FAKE_HOST/usr/local/bin" "$FAKE_HOST/tmp"
printf '#!/bin/sh\nexit 0\n' > "$FAKE_HOST/usr/local/bin/superci-pull-sync"; chmod 755 "$FAKE_HOST/usr/local/bin/superci-pull-sync"
failed=0
check() { if [ "$1" = 0 ]; then echo "ok    $2"; else echo "FAIL  $2"; failed=1; fi; }
ok() { local name=$1; shift; out=$("$@" 2>&1); local rc=$?; check $rc "$name"; [ $rc = 0 ] || echo "$out" | tail -5 | sed 's/^/      /'; }
refuses() { local name=$1 want=$2; shift 2; out=$("$@" 2>&1); local rc=$?
  if [ $rc != 0 ] && grep -qF -- "$want" <<< "$out"; then check 0 "$name"; else check 1 "$name (rc $rc)"; echo "$out" | tail -5 | sed 's/^/      /'; fi; }

# A main of andrewcai8/superci holding this tree, reached through a local remote.
W=$T/work; mkdir -p "$W"; cp -r "$here/.." "$W/pull"
# config.json pins our copies of the machine image by AMI id: this main pins two test copies, one per region, in place
# of the checked-in ones.
P1=ami-0aaaa1111bbbb2222 P2=ami-0cccc3333dddd4444
sed -i -E "s/(\"us-east-1\": )\"ami-[^\"]*\"/\1\"$P1\"/; s/(\"us-east-2\": )\"ami-[^\"]*\"/\1\"$P2\"/" "$W/pull/config.json"
[ "$(grep -c "\"$P1\"\|\"$P2\"" "$W/pull/config.json")" = 2 ] || { echo "FAIL  the test main's config.json does not pin the test copies"; exit 1; }
git -C "$W" init -q; git -C "$W" add -A pull; git -C "$W" -c user.name=t -c user.email=t@t commit -q -m tree
C=$(git -C "$W" rev-parse HEAD)
git init -q --bare "$T/origin.git"; git -C "$W" push -q "$T/origin.git" HEAD:refs/heads/main
git -C "$W" remote add origin https://github.com/andrewcai8/superci
git -C "$W" config "url.$T/origin.git.insteadOf" https://github.com/andrewcai8/superci
D=$W/pull/deploy

# A release directory as build.sh writes it, its programs standing in for the Go binaries and carrying the commit.
R=$T/release; mkdir -p "$R/listener/bin" "$R/listener/deploy" "$R/listener/config" "$R/cache/bin"
printf '#!/bin/sh\n# %s\ncase "$1" in -version) echo %s ;; -validate) cat > /dev/null; echo valid ;; esac\n' "$C" "$C" > "$R/listener/bin/superci-pull"
# The shim is the real one, built as build.sh builds it: a missing `revision` variable leaves the commit out of the
# binary, and manifest.sh then refuses the whole listener tree.
(cd "$here/.." && CGO_ENABLED=0 GOFLAGS=-buildvcs=false go build -trimpath -ldflags="-s -w -X main.revision=$C" -o "$R/listener/bin/superci-shim" ./shim) || { echo "FAIL  the shim does not build"; exit 1; }
[ "$("$R/listener/bin/superci-shim" -version)" = "$C" ]; check $? "superci-shim -version prints the commit it was built from"
for a in amd64 arm64; do printf '#!/bin/sh\necho %s\n' "$C" > "$R/cache/bin/pullcache-linux-$a"; done
printf 'bootstrap %s\n' "$C" > "$T/bootstrap"; (cd "$T" && python3 -I -m zipfile -c "$R/cache/bin/pullcache-lambda-arm64.zip" bootstrap)
chmod 755 "$R"/listener/bin/* "$R"/cache/bin/*
for f in superci-pull.service superci-pull-sync.service superci-pull-sync.timer superci-pull-health.service superci-pull-health.timer sync.sh health.sh cloudwatch-agent.json logrotate.conf; do
  cp "$W/pull/deploy/$f" "$R/listener/deploy/$f"
done
cp "$W/pull/deploy/override.json" "$R/listener/config/config.json"
cp "$W/pull/cache/deploy/machine-up.sh" "$R/cache/bin/up.sh"
mkdir -p "$R/cache/bin/warm"; cp "$W/pull/readsets/superci-8cpu.list" "$R/cache/bin/warm/superci-8cpu.list"
printf '{"commit":"%s","built":"2026-11-25T00:00:00Z"}\n' "$C" > "$R/REVISION"

refuses "deploy/up.sh keeps the listener's template before a release" "keeps its boot until a release" "$D/up.sh" 203.0.113.7/32
grep -q create-launch-template-version "$FAKE_AWS/calls.log"; check $((1 - $?)) "and wrote no template version"
refuses "verify-release.sh fails with no release" "bin/RELEASE.json: missing" "$D/verify-release.sh" all

# The parity check (parity.sh) refuses a release whose machines would give a job another environment, before any upload.
sed 's/^umask .*/umask 0077/' "$here/testdata/parity-probe.txt" > "$T/probe.txt"
refuses "release.sh refuses a release whose jobs would run under umask 077" "umask: 0077, Blacksmith's job has 0022" "$D/release.sh" "$R" "$here/testdata/ssh"
[ ! -e "$FAKE_AWS/s3/superci-pull-243891825325/bin/RELEASE.json" ]; check $? "and uploaded nothing"
grep -v '^cmd dpkg-deb$' "$here/testdata/parity-probe.txt" > "$T/probe.txt"
refuses "release.sh refuses a release whose jobs lack a command Blacksmith's have" "cmd dpkg-deb: not on the job's PATH" "$D/release.sh" "$R" "$here/testdata/ssh"
sed 's/^tool node .*/tool node 20.19.0 \/usr\/local\/bin\/node/' "$here/testdata/parity-probe.txt" > "$T/probe.txt"
refuses "release.sh refuses a release whose jobs have another major of a key tool" "tool node: 20.19.0, Blacksmith's job has 22" "$D/release.sh" "$R" "$here/testdata/ssh"
grep -v 'NOPASSWD' "$here/testdata/parity-probe.txt" > "$T/probe.txt"
refuses "release.sh refuses a release whose jobs' sudo asks for a password" "sudo: the job lacks '(ALL : ALL) NOPASSWD: ALL'" "$D/release.sh" "$R" "$here/testdata/ssh"
sed 's/^ulimit -n .*/ulimit -n 1024/' "$here/testdata/parity-probe.txt" > "$T/probe.txt"
refuses "release.sh refuses a release whose jobs may open fewer files" "ulimit -n: 1024, Blacksmith's job has 65536" "$D/release.sh" "$R" "$here/testdata/ssh"
sed 's/^gid .*/gid 0 root/' "$here/testdata/parity-probe.txt" > "$T/probe.txt"
refuses "release.sh refuses a release whose jobs run in another group" "gid: 0 root, Blacksmith's job has 1001 runner" "$D/release.sh" "$R" "$here/testdata/ssh"
[ ! -e "$FAKE_AWS/s3/superci-pull-243891825325/bin/RELEASE.json" ]; check $? "and uploaded nothing"
cp "$here/testdata/parity-probe.txt" "$T/probe.txt"
# Each region's copy is tagged with the RunsOn build it was made from: copies of two builds would give jobs two images.
printf '243891825325\tavailable\t%s\t20261120075252\n' "$P1" > "$FAKE_AWS/images-us-east-1"
printf '243891825325\tavailable\t%s\t20261125104805\n' "$P2" > "$FAKE_AWS/images-us-east-2"
refuses "release.sh refuses a release whose regions' pins are copies of two builds" "jobs would run on two images" "$D/release.sh" "$R" "$here/testdata/ssh"
rm "$FAKE_AWS/images-us-east-1" "$FAKE_AWS/images-us-east-2"
# The pin is the image the machine boots, exactly: its id, in the account's own copy, and not a build EC2 lists by name.
touch "$FAKE_AWS/fail-terminate-once"; : > "$FAKE_AWS/calls.log"
out=$("$D/parity.sh" "$C" 2>&1); grep -qF "from $C: boot.sh, hydrate-stop.sh, the regions (config.json), the image (config.json's pin)" <<< "$out" &&
  grep -qF "parity: booting $P1 (the pin of $C's config.json for us-east-1, our copy of build 20261120075252" <<< "$out" && grep -qF "parity: PASS: a job on $P1" <<< "$out"
check $? "parity.sh boots the pinned image and passes a job environment that is Blacksmith's"
grep -qE -- "run-instances .*--image-id $P1( |$)" "$FAKE_AWS/calls.log"; check $? "and the machine it boots is exactly the pinned id"
! grep -q -- '--owners\|--filters Name=name\|135269210855' "$FAKE_AWS/calls.log"; check $? "and no image was looked up by name or owner, where a newer build could be found"
[ "$(grep -c 'ec2 terminate-instances .*i-0parity' "$FAKE_AWS/calls.log")" = 2 ] && grep -qF "parity: terminated i-0parity" <<< "$out"
check $? "and terminates its machine, trying again when the first terminate fails"
# The user data EC2 is given: boot.sh up to the job's branch and the probe, gzipped as the listener's launch template
# holds it (encodeUserData), under EC2's 16384 bytes.
python3 -I - "$FAKE_AWS/user-data" "$(cat "$FAKE_AWS/user-data.scheme")" << 'PY2'
import gzip, sys
data, scheme = open(sys.argv[1], "rb").read(), sys.argv[2]
text = gzip.decompress(data).decode() if data[:2] == b"\x1f\x8b" else ""
sys.exit(0 if scheme == "fileb" and len(data) < 16384 and text.startswith("#!/bin/bash") and "agent.py --probe" in text and "@MODE@" not in text else 1)
PY2
check $? "parity.sh gives its machine the rendered boot.sh gzipped, as raw bytes, under EC2's 16384"
# A machine that stops answering ends the check within its bound, and the machine is terminated.
: > "$FAKE_AWS/calls.log"; t0=$(date +%s)
PARITY_SSH_HANG=1 PARITY_TIMEOUT=3 timeout 60 "$D/parity.sh" "$C" > "$T/hang.out" 2>&1; rc=$?
[ $rc = 1 ] && [ $(( $(date +%s) - t0 )) -lt 30 ] && grep -qF "parity: FAIL: the machine phase did not finish in 3 s" "$T/hang.out" && grep -q 'ec2 terminate-instances .*i-0parity' "$FAKE_AWS/calls.log"
check $? "parity.sh ends within its bound when its ssh never returns, and terminates the machine"; [ $rc = 1 ] || tail -5 "$T/hang.out" | sed 's/^/      /'
[ "$("$D/parity.sh" --pins)" = "$(printf 'us-east-1 %s\nus-east-2 %s' "$P1" "$P2")" ]; check $? "parity.sh reads the pins from config.json"
"$here/parity.sh" --pins | grep -qE '^us-east-1 ami-[0-9a-f]{17}$' && "$here/parity.sh" --pins | grep -qE '^us-east-2 ami-[0-9a-f]{17}$' && [ "$("$here/parity.sh" --pins | wc -l)" = 2 ]
check $? "(and the checked-in config.json pins one image for each of its two regions)"
# Fail closed: a pin that is missing, not available or not ours is refused, and nothing is booted or uploaded.
refuse_pin() { # name, the images-us-east-1 file's content (empty: EC2 does not know the id), and what the refusal says
  local name=$1 row=$2 want=$3
  : > "$FAKE_AWS/calls.log"; printf '%s' "$row" > "$FAKE_AWS/images-us-east-1"
  refuses "$name" "$want" "$D/release.sh" "$R" "$here/testdata/ssh"
  ! grep -q 'run-instances' "$FAKE_AWS/calls.log"; check $? "and booted no machine"
  [ ! -e "$FAKE_AWS/s3/superci-pull-243891825325/bin/RELEASE.json" ]; check $? "and uploaded nothing"
  rm "$FAKE_AWS/images-us-east-1"
}
refuse_pin "release.sh refuses a pin EC2 no longer lists" "" "the pinned image $P1 in us-east-1: EC2 does not list it"
refuse_pin "release.sh refuses a pin that is another account's (RunsOn's own build)" "$(printf '135269210855\tavailable\t%s\t20261120075252\n' "$P1")" "owned by 135269210855, not 243891825325"
refuse_pin "release.sh refuses a pin that is not available" "$(printf '243891825325\tpending\t%s\t20261120075252\n' "$P1")" "is pending, not available"
refuse_pin "release.sh refuses a copy with no build tag" "$(printf '243891825325\tavailable\t%s\tNone\n' "$P1")" "has no superci-pull-image tag"
# A placeholder pin: a commit whose config.json still holds one cannot be released.
sed -i "s/$P1/ami-REPLACE-us-east-1/" "$W/pull/config.json"; git -C "$W" -c user.name=t -c user.email=t@t commit -q -am placeholder; PH=$(git -C "$W" rev-parse HEAD); git -C "$W" reset -q --hard "$C"
: > "$FAKE_AWS/calls.log"
refuses "parity.sh refuses a commit whose config.json still pins a placeholder image" "ami-REPLACE-us-east-1" "$D/parity.sh" "$PH"
! grep -q 'run-instances\|describe-images' "$FAKE_AWS/calls.log"; check $? "and asked EC2 nothing"
ok "release.sh releases the listener" env SSH="$here/testdata/ssh" "$D/release.sh" "$R" "$here/testdata/ssh"
[ "$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["parity"])' "$FAKE_AWS/s3/superci-pull-243891825325/bin/RELEASE.json")" = passed ]
check $? "and its RELEASE.json records that the parity check passed"
cmp -s "$R/listener/bin/superci-pull" "$FAKE_HOST/usr/local/bin/superci-pull"; check $? "the listener runs the released binary"
ok "cache/deploy/release.sh releases the cache, through cache/deploy/up.sh" "$W/pull/cache/deploy/release.sh" "$R"
[ "$(cat "$FAKE_AWS/lambda/us-east-2/superci-pull-cache-mirror")" = "$(python3 -I -c 'import base64,hashlib,sys; print(base64.b64encode(hashlib.sha256(open(sys.argv[1],"rb").read()).digest()).decode())' "$R/cache/bin/pullcache-lambda-arm64.zip")" ]
check $? "the functions run the released zip"
cmp -s "$R/cache/bin/warm/superci-8cpu.list" "$FAKE_AWS/s3/superci-pull-cache-243891825325-us-east-2/bin/warm/superci-8cpu.list"
check $? "the cache buckets hold superci-8cpu's read set, as RELEASE.json names it"
ok "verify-release.sh passes on the release" env SSH="$here/testdata/ssh" "$D/verify-release.sh" all
ok "deploy/up.sh moves the template once a release is in place" "$D/up.sh" 203.0.113.7/32
grep -q create-launch-template-version "$FAKE_AWS/calls.log"; check $? "and wrote a template version"

L=$FAKE_AWS/s3/superci-pull-243891825325; E=$FAKE_AWS/s3/superci-pull-cache-243891825325-us-east-1
V() { env SSH="$here/testdata/ssh" "$D/verify-release.sh" "$@"; }
cp "$L/deploy/sync.sh" "$T/sync.sh"; echo '# edit' >> "$L/deploy/sync.sh"
refuses "verify-release.sh finds an edited script" "deploy/sync.sh: sha256" V listener; cp "$T/sync.sh" "$L/deploy/sync.sh"
echo x > "$E/bin/stray"; refuses "verify-release.sh finds a file no release has" "bin/stray: not in RELEASE.json" V cache; rm "$E/bin/stray"
mkdir -p "$E/bin/hydrate"; echo '{}' > "$E/bin/hydrate/superci-4cpu.json"; ok "verify-release.sh lets an operator's hydration manifest be" V cache
touch "$FAKE_AWS/fail-list"; refuses "verify-release.sh fails a listing that fails" "could not be listed" V cache; rm "$FAKE_AWS/fail-list"
cp "$FAKE_HOST/usr/local/bin/superci-pull" "$T/running"; printf '#!/bin/sh\necho other\n' > "$FAKE_HOST/usr/local/bin/superci-pull"
refuses "verify-release.sh finds a listener running another binary" "listener runs sha256" V listener; cp "$T/running" "$FAKE_HOST/usr/local/bin/superci-pull"
echo junk >> "$E/bin/pullcache-lambda-arm64.zip"; : > "$FAKE_AWS/calls.log"
refuses "cache/deploy/up.sh refuses a zip that is not RELEASE.json's" "bin/pullcache-lambda-arm64.zip in superci-pull-cache-243891825325-us-east-1 has sha256" "$W/pull/cache/deploy/up.sh"
grep -q update-function-code "$FAKE_AWS/calls.log"; check $((1 - $?)) "and updated no function"
echo junk >> "$L/bin/superci-pull"
refuses "deploy/up.sh keeps the template when bin/superci-pull is not RELEASE.json's" "bin/superci-pull in superci-pull-243891825325 has sha256" "$D/up.sh" 203.0.113.7/32

cp -r "$R" "$T/bad"; rm "$T/bad/listener/bin/RELEASE.json"; printf '#!/bin/sh\necho a shim of no commit\n' > "$T/bad/listener/bin/superci-shim"
refuses "release.sh refuses a shim that does not carry the commit" "listener/bin/superci-shim does not carry $C" "$D/release.sh" "$T/bad" "$here/testdata/ssh"
rm -rf "$T/bad"; cp -r "$R" "$T/bad"; rm "$T/bad/listener/bin/RELEASE.json"; echo '# edit' >> "$T/bad/listener/deploy/health.sh"
refuses "release.sh refuses a script that is not the commit's" "listener/deploy/health.sh differs" "$D/release.sh" "$T/bad" "$here/testdata/ssh"
rm -rf "$T/bad"; cp -r "$R" "$T/bad"; rm "$T/bad/cache/bin/RELEASE.json"; echo /etc/ssh/ssh_host_ed25519_key >> "$T/bad/cache/bin/warm/superci-8cpu.list"
refuses "cache/deploy/release.sh refuses a read set that is not the commit's" "cache/bin/warm/superci-8cpu.list differs" "$W/pull/cache/deploy/release.sh" "$T/bad"
git -C "$W" config remote.origin.url https://github.com/someone/superci
refuses "release.sh refuses another origin" "not andrewcai8/superci" "$D/release.sh" "$R" "$here/testdata/ssh"
git -C "$W" config remote.origin.url https://github.com/andrewcai8/superci
git -C "$W" -c user.name=t -c user.email=t@t commit -q --allow-empty -m side; S=$(git -C "$W" rev-parse HEAD); git -C "$W" reset -q --hard "$C"
rm -rf "$T/bad"; cp -r "$R" "$T/bad"; rm "$T/bad/listener/bin/RELEASE.json"
printf '{"commit":"%s","built":"x"}\n' "$S" > "$T/bad/REVISION"
refuses "release.sh refuses a commit not on main" "is not on andrewcai8/superci main" "$D/release.sh" "$T/bad" "$here/testdata/ssh"

# A rollback to a commit from before the parity check and the pin, as main's 5d97ebc is: no pull/parity or
# deploy/parity.sh, no "image" in config.json, the image a literal ImageConfig of the NewFleet call (findImage's old rule:
# the pinned RunsOn build, or once RunsOn has deleted it the oldest newer one), an agent.py with no --probe. The check is
# this checkout's, the boot and image the commit's.
O=$T/old; mkdir -p "$O"; cp -r "$W/pull" "$O/pull"; rm -r "$O/pull/parity" "$O/pull/deploy/parity.sh"
python3 -I - "$O/pull" << 'PY2'
import re, sys
d = sys.argv[1]
m = open(f"{d}/main.go").read()
call = "NewFleet(ctx, cfg.Deployment, cfg.Account, cfg.Regions, cfg.Image)"
assert call in m, "main.go no longer calls " + call
m = m.replace(call, 'NewFleet(ctx, cfg.Deployment, cfg.Regions, ImageConfig{Owner: "135269210855", Name: "runs-on-v2.2-ubuntu24-full-x64-*", Pin: "20261102081424"})')
open(f"{d}/main.go", "w").write(m)
c = open(f"{d}/config.json").read()
c, n = re.subn(r'\n  "image": \{[^}]*\},', "", c)
assert n == 1, "config.json has no image map to take out"
open(f"{d}/config.json", "w").write(c)
a = open(f"{d}/agent.py").read()
a = re.sub(r'    if len\(argv\) == 3 and argv\[1\] == "--probe":\n.*\n', "", a)
open(f"{d}/agent.py", "w").write(a.replace("--probe", "-"))
PY2
G="git --git-dir=$W/.git --work-tree=$O"; GIT_INDEX_FILE=$T/old.index $G add -A pull
OLD=$(GIT_INDEX_FILE=$T/old.index $G write-tree | xargs $G -c user.name=t -c user.email=t@t commit-tree -p "$C" -m old)
$G push -q "$T/origin.git" "$OLD:refs/heads/main"
out=$("$D/parity.sh" "$OLD" 2>&1); grep -qF "from $OLD: boot.sh, hydrate-stop.sh, the regions (config.json), the image (main.go's ImageConfig, from before the pin)" <<< "$out" &&
  grep -qF ", agent.py (the commit's has no --probe)" <<< "$out" && grep -qF "parity: PASS" <<< "$out"
check $? "parity.sh checks a commit from before it and the pin: its boot and image, with this checkout's check and agent"
# Before the pin, the image is findImage's old rule over the builds EC2 lists by name: copies of two builds refuse.
printf 'runs-on-v2.2-ubuntu24-full-x64-20261120075252\tami-0east1\n' > "$FAKE_AWS/builds-us-east-1"
printf 'runs-on-v2.2-ubuntu24-full-x64-20261125104805\tami-0east2\n' > "$FAKE_AWS/builds-us-east-2"
refuses "parity.sh refuses a commit from before the pin whose regions would boot two builds" "jobs would run on two images" "$D/parity.sh" "$OLD"
rm "$FAKE_AWS/builds-us-east-1" "$FAKE_AWS/builds-us-east-2"
# and the pinned build is gone, so the oldest newer one.
printf '%s\n' 'runs-on-v2.2-ubuntu24-full-x64-20261020000000	ami-0older' 'runs-on-v2.2-ubuntu24-full-x64-20261125104805	ami-0newest' \
  'runs-on-v2.2-ubuntu24-full-x64-20261120075252	ami-0oldestnewer' > "$FAKE_AWS/builds"
: > "$FAKE_AWS/calls.log"
out=$("$D/parity.sh" "$OLD" 2>&1); grep -qF "parity: booting ami-0oldestnewer (build 20261120075252 of 135269210855/runs-on-v2.2-ubuntu24-full-x64-*, pin 20261102081424" <<< "$out" && grep -qF "parity: PASS: a job on ami-0oldestnewer" <<< "$out"
check $? "parity.sh boots the image a commit from before the pin would (findImage's old rule) and passes"; rm "$FAKE_AWS/builds"
grep -qE -- "run-instances .*--image-id ami-0oldestnewer( |$)" "$FAKE_AWS/calls.log"; check $? "and the machine it boots is that build"
# A commit that names neither a pin nor an ImageConfig boots this checkout's pin.
N=$T/neither; rm -rf "$N"; cp -r "$O" "$N"; sed -i 's/ImageConfig{[^}]*}/img/' "$N/pull/main.go"
GN="git --git-dir=$W/.git --work-tree=$N"; GIT_INDEX_FILE=$T/neither.index $GN add -A pull
NEITHER=$(GIT_INDEX_FILE=$T/neither.index $GN write-tree | xargs $GN -c user.name=t -c user.email=t@t commit-tree -p "$C" -m neither)
out=$("$D/parity.sh" "$NEITHER" 2>&1); grep -qF "the image (config.json's pin; the commit names none)" <<< "$out" && grep -qF "parity: booting $P1 " <<< "$out" && grep -qF "parity: PASS" <<< "$out"
check $? "parity.sh boots this checkout's pin for a commit that names no image"
rm -rf "$T/oldrel"; cp -r "$R" "$T/oldrel"; rm -f "$T/oldrel/listener/bin/RELEASE.json"
printf '{"commit":"%s","built":"2026-11-25T00:00:00Z"}\n' "$OLD" > "$T/oldrel/REVISION"
printf '#!/bin/sh\n# %s\ncase "$1" in -version) echo %s ;; -validate) cat > /dev/null; echo valid ;; esac\n' "$OLD" "$OLD" > "$T/oldrel/listener/bin/superci-pull"
(cd "$here/.." && CGO_ENABLED=0 GOFLAGS=-buildvcs=false go build -trimpath -ldflags="-s -w -X main.revision=$OLD" -o "$T/oldrel/listener/bin/superci-shim" ./shim)
ok "release.sh releases a rollback to a commit from before the parity check" env SSH="$here/testdata/ssh" "$D/release.sh" "$T/oldrel" "$here/testdata/ssh"
# An emergency release past the check: PARITY_SKIP's reason is printed and recorded, and no machine is booted.
sed 's/^umask .*/umask 0077/' "$here/testdata/parity-probe.txt" > "$T/probe.txt"; : > "$FAKE_AWS/calls.log"
rm -f "$T/oldrel/listener/bin/RELEASE.json"
out=$(PARITY_SKIP="incident 12: roll back now" SSH="$here/testdata/ssh" "$D/release.sh" "$T/oldrel" "$here/testdata/ssh" 2>&1); rc=$?
[ $rc = 0 ] && grep -qF "parity: SKIPPED for $OLD by PARITY_SKIP: incident 12: roll back now" <<< "$out" && ! grep -q run-instances "$FAKE_AWS/calls.log" &&
  [ "$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["parity"])' "$FAKE_AWS/s3/superci-pull-243891825325/bin/RELEASE.json")" = "skipped: incident 12: roll back now" ]
check $? "release.sh with PARITY_SKIP releases without the check, prints the reason and records it in RELEASE.json"; [ $rc = 0 ] || echo "$out" | tail -5 | sed 's/^/      /'
exit $failed
