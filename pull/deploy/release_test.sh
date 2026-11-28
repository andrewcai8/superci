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
# The image's sshd takes no EC2 Instance Connect key (Nov 28: "Permission denied (publickey)" after a push that answered
# true): the user data puts this run's public key on a user of the check's own, and no key is pushed.
python3 -I - "$FAKE_AWS/user-data" "$FAKE_AWS/calls.log" << 'PY3'
import glob, gzip, sys
text = gzip.decompress(open(sys.argv[1], "rb").read()).decode()
calls = open(sys.argv[2]).read()
ok = ("useradd -m -s /bin/bash parity" in text and "/home/parity/.ssh/authorized_keys" in text and "ssh-ed25519 " in text
      and "parity ALL=(ALL) NOPASSWD:ALL" in text and "ec2-instance-connect" not in calls)
sys.exit(0 if ok else 1)
PY3
check $? "parity.sh's machine carries this run's key for its own user, and no Instance Connect key is pushed"
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
# The launch templates (templates.sh): a machine that is lost must be replaceable, so every template names an image EC2
# has (Nov 28: both identity hosts' templates named a deleted RunsOn image). The world is templates-<region>.json of the fake aws.
cat > "$T/world.py" << 'PY2'
import json, os, sys
root, p1, p2, mode = os.environ["FAKE_AWS"], sys.argv[1], sys.argv[2], sys.argv[3]
AL, GONE = "ami-0358f644d7299878d", "ami-0178d4ab7f659a2cc"
def tags(*extra):
    return dict({"purpose": "superci-pull"}, **dict(extra))
def lt(name, pin, *tg, versions=None, default=1):
    v = versions or {"1": pin}
    return {"id": "lt-" + name, "name": name, "tags": tags(*tg), "default": default, "latest": max(int(n) for n in v), "versions": v}
if mode == "clean":
    w = {"us-east-1": {"templates": [
            lt("superci-pull-identity-us-east-1a", p1, ("superci-pull-role", "identity")),
            lt("superci-pull-identity-us-east-1b", p1, ("superci-pull-role", "identity")),
            lt("superci-pull-listener", AL, ("superci-pull-role", "listener"), versions={"1": AL, "2": AL}, default=2),
            lt("superci-pull-superci-2cpu", p1, ("superci-pull-deployment", "superci-pull"), versions={"1": p1, "2": p1}, default=2)],
          "asg": {"template": "lt-superci-pull-listener", "version": "$Default"},
          "images": {AL: {"OwnerId": "137112412989", "State": "available", "DeprecationTime": "2027-02-01T00:00:00.000Z"}}},
         "us-east-2": {"templates": [
            lt("superci-pull-identity-us-east-2a", p2, ("superci-pull-role", "identity")),
            lt("superci-pull-superci-2cpu", p2, ("superci-pull-deployment", "superci-pull"), versions={"1": p2, "2": p2}, default=2)]}}
    for r, x in w.items():
        json.dump(x, open(os.path.join(root, "templates-" + r + ".json"), "w"))
else:  # a statement over the worlds: w["us-east-1"] ...
    w = {r: json.load(open(os.path.join(root, "templates-" + r + ".json"))) for r in ("us-east-1", "us-east-2")}
    T = lambda r, name: next(t for t in w[r]["templates"] if t["name"] == name)
    exec(mode)
    for r, x in w.items():
        json.dump(x, open(os.path.join(root, "templates-" + r + ".json"), "w"))
PY2
W() { python3 -I "$T/world.py" "$P1" "$P2" "$1"; }
TC() { "$D/templates.sh" check "$@"; }
W clean
ok "templates.sh passes a clean set: identities, dispatch and the listener on images EC2 has" TC
out=$(TC 2>&1); grep -qF "ok    us-east-1 superci-pull-identity-us-east-1a v1 (\$Default): image $P1" <<< "$out" && grep -qF "ok    us-east-1 superci-pull-superci-2cpu v2 (\$Latest): image $P1" <<< "$out" &&
  grep -qF "ok    us-east-1 superci-pull-listener v2 (its group's \$Default): image ami-0358f644d7299878d" <<< "$out" && grep -qF "note  us-east-1 superci-pull-listener v2 (its group's \$Default): image ami-0358f644d7299878d DeprecationTime 2027-02-01T00:00:00.000Z" <<< "$out"
check $? "and says which version and image each check read, and the listener image's DeprecationTime"
W 'w["us-east-1"]["templates"].append({"id": "lt-plane", "name": "superci-82flwf4vz2os-138733726419-1795550317020", "tags": {"purpose": "superci-pull"}, "default": 1, "latest": 1, "versions": {"1": "ami-0gone"}}); w["us-east-1"]["templates"].append({"id": "lt-dev", "name": "superci-pull-timeline-superci-2cpu", "tags": {"purpose": "superci-pull", "superci-pull-deployment": "superci-pull-timeline"}, "default": 1, "latest": 1, "versions": {"1": "ami-0gone"}})'
ok "templates.sh leaves superci's own plane's template and another deployment's alone" TC
W clean
# Each rule, on the identity templates (the ones release.sh checks before any upload).
rule() { # name, the world's change, what templates.sh says; release.sh refuses with the same and uploads nothing
  local name=$1 change=$2 want=$3
  W clean; W "$change"; : > "$FAKE_AWS/calls.log"
  refuses "templates.sh refuses $name" "$want" TC
  refuses "release.sh refuses $name" "$want" "$D/release.sh" "$R" "$here/testdata/ssh"
  [ ! -e "$FAKE_AWS/s3/superci-pull-243891825325/bin/RELEASE.json" ] && ! grep -q 's3 cp .*s3://superci-pull-243891825325' "$FAKE_AWS/calls.log"; check $? "and uploaded nothing"
}
rule "an identity template whose default names an image EC2 no longer lists (RunsOn deregistered it)" \
  'T("us-east-1", "superci-pull-identity-us-east-1a")["versions"]["1"] = "ami-0178d4ab7f659a2cc"; w["us-east-1"]["images"]["ami-0178d4ab7f659a2cc"] = None' \
  "us-east-1 superci-pull-identity-us-east-1a v1 (\$Default): image ami-0178d4ab7f659a2cc does not exist (EC2 does not list it)"
rule "an identity template that names the pin, which EC2 no longer lists" \
  'w["us-east-1"]["images"]["'$P1'"] = None' \
  "us-east-1 superci-pull-identity-us-east-1a v1 (\$Default): image $P1 does not exist (EC2 does not list it)"
rule "an identity template whose image is another account's" \
  'w["us-east-2"]["images"] = {"'$P2'": {"OwnerId": "135269210855", "State": "available"}}' \
  "us-east-2 superci-pull-identity-us-east-2a v1 (\$Default): image $P2 is owned by 135269210855, not 243891825325"
rule "an identity template whose image is not available" \
  'w["us-east-1"]["images"]["'$P1'"] = {"OwnerId": "243891825325", "State": "pending"}' \
  "image $P1 is pending, not available"
rule "an identity template on an image that is another account's, not the pin" \
  'T("us-east-1", "superci-pull-identity-us-east-1b")["versions"]["1"] = "ami-0bbbb5555cccc6666"; w["us-east-1"]["images"]["ami-0bbbb5555cccc6666"] = {"OwnerId": "135269210855", "State": "available"}' \
  "us-east-1 superci-pull-identity-us-east-1b v1 (\$Default): image ami-0bbbb5555cccc6666 is owned by 135269210855, not 243891825325"
rule "an identity template whose old version is usable but whose default is an image EC2 no longer lists" \
  'T("us-east-1", "superci-pull-identity-us-east-1a")["versions"] = {"1": "'$P1'", "2": "ami-0178d4ab7f659a2cc"}; T("us-east-1", "superci-pull-identity-us-east-1a")["latest"] = 2; T("us-east-1", "superci-pull-identity-us-east-1a")["default"] = 2; w["us-east-1"]["images"]["ami-0178d4ab7f659a2cc"] = None' \
  "us-east-1 superci-pull-identity-us-east-1a v2 (\$Default): image ami-0178d4ab7f659a2cc does not exist (EC2 does not list it)"
rule "a listener whose group launches an image EC2 no longer lists" \
  'w["us-east-1"]["images"]["ami-0358f644d7299878d"] = None' \
  "us-east-1 superci-pull-listener v2 (its group's \$Default): image ami-0358f644d7299878d does not exist"
rule "a listener whose group launches a version on a deprecated-and-gone image while \$Latest is fine" \
  'T("us-east-1", "superci-pull-listener")["versions"] = {"1": "ami-0old", "2": "ami-0358f644d7299878d"}; w["us-east-1"]["asg"]["version"] = "1"; w["us-east-1"]["images"]["ami-0old"] = None' \
  "us-east-1 superci-pull-listener v1 (its group's 1): image ami-0old does not exist"
# The dispatch templates: the listener launches $Latest and the version it makes is now the default, so both name the pin.
W clean; W 'T("us-east-1", "superci-pull-superci-2cpu")["versions"] = {"1": "ami-0178d4ab7f659a2cc", "2": "'$P1'"}; T("us-east-1", "superci-pull-superci-2cpu")["default"] = 1'
refuses "templates.sh refuses a dispatch template whose \$Default is not the pin" "us-east-1 superci-pull-superci-2cpu v1 (\$Default): names ami-0178d4ab7f659a2cc, not $P1" TC
ok "but not before an upload: --pre-upload leaves the dispatch templates to the listener this release starts" TC --pre-upload
W 'T("us-east-1", "superci-pull-superci-2cpu")["default"] = 2; T("us-east-2", "superci-pull-superci-2cpu")["versions"]["2"] = "ami-0bbbb5555cccc6666"'
refuses "templates.sh refuses a dispatch template whose \$Latest is not the pin" "us-east-2 superci-pull-superci-2cpu v2 (\$Latest): names ami-0bbbb5555cccc6666, not $P2" TC
W 'T("us-east-2", "superci-pull-superci-2cpu")["versions"]["2"] = "'$P2'"; w["us-east-2"]["images"] = {"'$P2'": None}'
refuses "templates.sh refuses a dispatch template on a pin EC2 does not list" "us-east-2 superci-pull-superci-2cpu v2 (\$Default): image $P2 does not exist" TC
# A retired template fails, with the line that deletes it, and is never deleted.
W clean; W 'w["us-east-1"]["templates"].append({"id": "lt-old96", "name": "superci-pull-superci-96cpu", "tags": {"purpose": "superci-pull"}, "default": 1, "latest": 1, "versions": {"1": "ami-0178d4ab7f659a2cc"}}); w["us-east-2"]["templates"].append({"id": "lt-odd", "name": "odd-name", "tags": {"purpose": "superci-pull"}, "default": 1, "latest": 1, "versions": {"1": "ami-0178d4ab7f659a2cc"}})'
: > "$FAKE_AWS/calls.log"
refuses "templates.sh fails a template no size, identity host or listener names, and prints its delete line" "aws --profile ci-runners ec2 delete-launch-template --region us-east-1 --launch-template-id lt-old96" TC
refuses "(by its tag too, when its name is another's)" "us-east-2 odd-name (lt-odd, default v1, latest v1) is retired" TC
! grep -q 'delete-launch-template' "$FAKE_AWS/calls.log"; check $? "and deleted nothing"
ok "a retired template is not checked before an upload: the release this check guards does not delete or wait on it" TC --pre-upload
W clean

# The dispatch templates are the listener's to make: a release whose pin is new finds them on the old image, uploads, and
# its restart makes them (the fake aws has no listener, so verify-release.sh's last check fails here, after the upload).
W 'T("us-east-1", "superci-pull-superci-2cpu")["versions"] = {"1": "ami-0178d4ab7f659a2cc", "2": "'$P1'"}; T("us-east-1", "superci-pull-superci-2cpu")["default"] = 1'
rm -rf "$T/r2"; cp -r "$R" "$T/r2"; rm -f "$T/r2/listener/bin/RELEASE.json"
out=$(env TEMPLATES_TRIES=1 SSH="$here/testdata/ssh" "$D/release.sh" "$T/r2" "$here/testdata/ssh" 2>&1); rc=$?
[ $rc != 0 ] && [ -e "$FAKE_AWS/s3/superci-pull-243891825325/bin/RELEASE.json" ] && grep -qF "FAIL  launch templates (templates.sh)" <<< "$out" && ! grep -qF "refusing: a launch template" <<< "$out"
check $? "release.sh uploads past a dispatch template that is not on the pin yet, and verify-release.sh after the restart fails on it"
rm -rf "$T/r2"; W clean
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
W 'T("us-east-1", "superci-pull-superci-2cpu")["default"] = 1; T("us-east-1", "superci-pull-superci-2cpu")["versions"]["1"] = "ami-0178d4ab7f659a2cc"'
out=$(env TEMPLATES_TRIES=1 SSH="$here/testdata/ssh" "$D/verify-release.sh" all 2>&1); rc=$?
[ $rc != 0 ] && grep -qF "us-east-1 superci-pull-superci-2cpu v1 (\$Default): names ami-0178d4ab7f659a2cc, not $P1" <<< "$out" && grep -qF "FAIL  launch templates (templates.sh)" <<< "$out" && grep -qF "release NOT verified: all" <<< "$out"
check $? "verify-release.sh fails a release whose dispatch template still has a default that is not the pin"
W clean
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
# A commit from before the pin has none: its pin is not checked, and its dispatch templates need only an image that is available.
out=$(TC --commit "$OLD" 2>&1); rc=$?
[ $rc = 0 ] && grep -qF "note  us-east-1: the commit names no image pin (it is from before the pin): its pin is not checked" <<< "$out"
check $? "templates.sh --commit of a commit from before the pin notes that it has none, and passes"

# A rollback releases a commit whose pin is not the one the templates name (Nov 28: a release of an older commit, whose
# "image" is our other copy of the machine image, was refused before any upload because the identity templates name
# today's pin). The released commit's own pin must be usable; the identity and listener templates need only an image
# that exists (and is ours, for an identity host); the dispatch templates are the commit's pin once its listener runs.
# A commit that pins PB for us-east-1, our other available copy, on main beside the commits above.
PB=ami-0eeee5555ffff6666
cp "$here/testdata/parity-probe.txt" "$T/probe.txt"
sed -i "s/$P1/$PB/" "$W/pull/config.json"; git -C "$W" -c user.name=t -c user.email=t@t commit -q -am "pin B"; CB=$(git -C "$W" rev-parse HEAD); git -C "$W" reset -q --hard "$C"
git -C "$W" push -q -f "$T/origin.git" "$CB:refs/heads/main"
rm -rf "$T/rb"; cp -r "$R" "$T/rb"; rm -f "$T/rb/listener/bin/RELEASE.json"
printf '{"commit":"%s","built":"2026-11-25T00:00:00Z"}\n' "$CB" > "$T/rb/REVISION"
printf '#!/bin/sh\n# %s\ncase "$1" in -version) echo %s ;; -validate) cat > /dev/null; echo valid ;; esac\n' "$CB" "$CB" > "$T/rb/listener/bin/superci-pull"
(cd "$here/.." && CGO_ENABLED=0 GOFLAGS=-buildvcs=false go build -trimpath -ldflags="-s -w -X main.revision=$CB" -o "$T/rb/listener/bin/superci-shim" ./shim)
RB() { rm -f "$T/rb/listener/bin/RELEASE.json" "$T/rb/listener.RELEASE.json"; env TEMPLATES_TRIES=1 SSH="$here/testdata/ssh" "$@" "$D/release.sh" "$T/rb" "$here/testdata/ssh"; }
RELEASED=$FAKE_AWS/s3/superci-pull-243891825325/bin/RELEASE.json
json() { python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1])).get(sys.argv[2], "none"))' "$@"; }
# The identity templates name pin A (P1); the dispatch templates are on B, as the listener of the rolled-back release makes them.
W clean; W 'T("us-east-1", "superci-pull-superci-2cpu")["versions"] = {"1": "'$P1'", "2": "'$PB'"}'
out=$(TC --pre-upload --commit "$CB" 2>&1); rc=$?
[ $rc = 0 ] && grep -qF "ok    us-east-1: the commit's pin $PB" <<< "$out" && grep -qF "note  us-east-1 superci-pull-identity-us-east-1a v1 (\$Default): image $P1 is not the commit's pin $PB" <<< "$out" &&
  grep -qF "launch templates ok: pre" <<< "$out"
check $? "templates.sh --pre-upload --commit passes identity templates on our other copy of the image, with a note"; [ $rc = 0 ] || echo "$out" | tail -5 | sed 's/^/      /'
out=$(RB 2>&1); rc=$?
[ $rc = 0 ] && grep -qF "image $P1 is not the commit's pin $PB" <<< "$out" && ! grep -qF "refusing: the commit's pin" <<< "$out" && [ "$(json "$RELEASED" commit)" = "$CB" ] && [ "$(json "$RELEASED" templates)" = passed ]
check $? "release.sh of that commit goes past the template check, uploads, and its RELEASE.json records templates: passed"; [ $rc = 0 ] || echo "$out" | tail -8 | sed 's/^/      /'
# The full check after the restart reads the commit the release is, not this checkout's config.json.
ok "verify-release.sh passes the release: dispatch templates on the released commit's pin, identity templates on our other copy" V listener
out=$(V listener 2>&1); grep -qF "note  us-east-1 superci-pull-identity-us-east-1a v1 (\$Default): image $P1 is not the commit's pin $PB" <<< "$out"
check $? "and notes the identity templates that are not the commit's pin"
ok "templates.sh --commit passes dispatch templates on the commit's pin, identity templates on our other copy" TC --commit "$CB"
W 'T("us-east-1", "superci-pull-superci-2cpu")["versions"]["2"] = "'$P1'"'
out=$(TC --commit "$CB" 2>&1); rc=$?
[ $rc != 0 ] && grep -qF "us-east-1 superci-pull-superci-2cpu v2 (\$Latest): names $P1, not $PB, config.json's pin for us-east-1" <<< "$out"
check $? "templates.sh --commit fails a dispatch template on another image than the commit's pin, though ours and available"
refuses "verify-release.sh fails it too" "us-east-1 superci-pull-superci-2cpu v2 (\$Latest): names $P1, not $PB" V listener
W 'T("us-east-1", "superci-pull-superci-2cpu")["versions"]["2"] = "'$PB'"'

# The released commit's own pin must be usable: refused, naming the region, the AMI and why, and nothing uploaded. (The
# parity check boots the pin too and refuses first, in its own words, so these releases skip it to reach the template check.)
cp "$RELEASED" "$T/released.before"
pin_refused() { # name, the change to the world, what is said
  local name=$1 change=$2 want=$3
  W clean; W "$change"; : > "$FAKE_AWS/calls.log"
  refuses "templates.sh --pre-upload --commit refuses $name" "$want" TC --pre-upload --commit "$CB"
  refuses "release.sh refuses $name" "$want" RB env PARITY_SKIP="parity is not under test"
  cmp -s "$RELEASED" "$T/released.before" && ! grep -q 's3 cp .*s3://superci-pull-243891825325' "$FAKE_AWS/calls.log"; check $? "and uploaded nothing"
}
pin_refused "a commit whose pin EC2 does not list" 'w["us-east-1"]["images"]["'$PB'"] = None' \
  "us-east-1: the commit's pin $PB does not exist (EC2 does not list it)"
pin_refused "a commit whose pin is another account's" 'w["us-east-1"]["images"]["'$PB'"] = {"OwnerId": "135269210855", "State": "available"}' \
  "us-east-1: the commit's pin $PB is owned by 135269210855, not 243891825325"
pin_refused "a commit whose pin is not available" 'w["us-east-1"]["images"]["'$PB'"] = {"OwnerId": "243891825325", "State": "pending"}' \
  "us-east-1: the commit's pin $PB is pending, not available"
# An identity template on an image EC2 does not list is still refused: the rollback relaxes the pin, not the image.
pin_refused "an identity template on an image EC2 does not list, though the commit's pin is fine" 'w["us-east-1"]["images"]["'$P1'"] = None' \
  "us-east-1 superci-pull-identity-us-east-1a v1 (\$Default): image $P1 does not exist (EC2 does not list it)"

# TEMPLATES_SKIP=<reason>: an emergency release past the template check, printed and recorded as PARITY_SKIP is; an empty one is refused.
W clean; W 'w["us-east-1"]["images"]["'$PB'"] = None'; : > "$FAKE_AWS/calls.log"
out=$(RB env PARITY_SKIP="parity is not under test" TEMPLATES_SKIP="incident 13: roll back now" 2>&1)
grep -qF "templates: SKIPPED for $CB by TEMPLATES_SKIP: incident 13: roll back now" <<< "$out" && ! grep -qF "refusing: the commit's pin" <<< "$out" &&
  [ "$(json "$RELEASED" templates)" = "skipped: incident 13: roll back now" ] && [ "$(json "$RELEASED" commit)" = "$CB" ] && [ "$(json "$RELEASED" parity)" = "skipped: parity is not under test" ]
check $? "release.sh with TEMPLATES_SKIP releases past the template check, prints the reason and records it in RELEASE.json"
grep -qF "release NOT verified: listener" <<< "$out"; check $? "and the verification after the restart still reports the pin EC2 does not list"
cp "$RELEASED" "$T/released.before"; : > "$FAKE_AWS/calls.log"
refuses "release.sh refuses an empty TEMPLATES_SKIP" "TEMPLATES_SKIP needs a reason" RB env PARITY_SKIP="parity is not under test" TEMPLATES_SKIP=
refuses "and a TEMPLATES_SKIP of spaces" "TEMPLATES_SKIP needs a reason" RB env PARITY_SKIP="parity is not under test" TEMPLATES_SKIP="  "
cmp -s "$RELEASED" "$T/released.before" && ! grep -q 's3 cp .*s3://superci-pull-243891825325\|run-instances' "$FAKE_AWS/calls.log"; check $? "and uploaded nothing"
W clean
exit $failed
