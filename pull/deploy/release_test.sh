#!/bin/bash
# The release end to end with AWS and the listener's SSH stubbed (testdata/aws, testdata/ssh): from a commit of a
# main, release.sh and cache/deploy/release.sh (with the real cache/deploy/up.sh) succeed and leave verify-release.sh
# passing, and every refusal the release relies on refuses. Run by release_test.go; needs bash, git and python3.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
export FAKE_AWS=$T/aws FAKE_HOST=$T/host PATH=$here/testdata:$PATH
mkdir -p "$FAKE_AWS" "$FAKE_HOST/usr/local/bin" "$FAKE_HOST/tmp"
printf '#!/bin/sh\nexit 0\n' > "$FAKE_HOST/usr/local/bin/superci-pull-sync"; chmod 755 "$FAKE_HOST/usr/local/bin/superci-pull-sync"
failed=0
check() { if [ "$1" = 0 ]; then echo "ok    $2"; else echo "FAIL  $2"; failed=1; fi; }
ok() { local name=$1; shift; out=$("$@" 2>&1); local rc=$?; check $rc "$name"; [ $rc = 0 ] || echo "$out" | tail -5 | sed 's/^/      /'; }
refuses() { local name=$1 want=$2; shift 2; out=$("$@" 2>&1); local rc=$?
  if [ $rc != 0 ] && grep -qF -- "$want" <<< "$out"; then check 0 "$name"; else check 1 "$name (rc $rc)"; echo "$out" | tail -5 | sed 's/^/      /'; fi; }

# A main of andrewcai8/superci holding this tree, reached through a local remote.
W=$T/work; mkdir -p "$W"; cp -r "$here/.." "$W/pull"
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
printf '{"commit":"%s","built":"2026-11-25T00:00:00Z"}\n' "$C" > "$R/REVISION"

refuses "deploy/up.sh keeps the listener's template before a release" "keeps its boot until a release" "$D/up.sh" 203.0.113.7/32
grep -q create-launch-template-version "$FAKE_AWS/calls.log"; check $((1 - $?)) "and wrote no template version"
refuses "verify-release.sh fails with no release" "bin/RELEASE.json: missing" "$D/verify-release.sh" all

ok "release.sh releases the listener" env SSH="$here/testdata/ssh" "$D/release.sh" "$R" "$here/testdata/ssh"
cmp -s "$R/listener/bin/superci-pull" "$FAKE_HOST/usr/local/bin/superci-pull"; check $? "the listener runs the released binary"
ok "cache/deploy/release.sh releases the cache, through cache/deploy/up.sh" "$W/pull/cache/deploy/release.sh" "$R"
[ "$(cat "$FAKE_AWS/lambda/us-east-2/superci-pull-cache-mirror")" = "$(python3 -I -c 'import base64,hashlib,sys; print(base64.b64encode(hashlib.sha256(open(sys.argv[1],"rb").read()).digest()).decode())' "$R/cache/bin/pullcache-lambda-arm64.zip")" ]
check $? "the functions run the released zip"
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
git -C "$W" config remote.origin.url https://github.com/someone/superci
refuses "release.sh refuses another origin" "not andrewcai8/superci" "$D/release.sh" "$R" "$here/testdata/ssh"
git -C "$W" config remote.origin.url https://github.com/andrewcai8/superci
git -C "$W" -c user.name=t -c user.email=t@t commit -q --allow-empty -m side; S=$(git -C "$W" rev-parse HEAD); git -C "$W" reset -q --hard "$C"
rm -rf "$T/bad"; cp -r "$R" "$T/bad"; rm "$T/bad/listener/bin/RELEASE.json"
printf '{"commit":"%s","built":"x"}\n' "$S" > "$T/bad/REVISION"
refuses "release.sh refuses a commit not on main" "is not on andrewcai8/superci main" "$D/release.sh" "$T/bad" "$here/testdata/ssh"
exit $failed
