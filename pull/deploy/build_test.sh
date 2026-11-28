#!/bin/bash
# build.sh and build-check.sh with AWS, ssh and the build host's tools stubbed: the host's tests run under the machines'
# python3 (the parity file's), and refuse under another before any test runs. Run by build_test.go; needs bash, git, tar.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
failed=0
check() { if [ "$1" = 0 ]; then echo "ok    $2"; else echo "FAIL  $2"; failed=1; fi; }
refuses() { local name=$1 want=$2; shift 2; out=$("$@" 2>&1); local rc=$?
  if [ $rc != 0 ] && grep -qF -- "$want" <<< "$out"; then check 0 "$name"; else check 1 "$name (rc $rc)"; echo "$out" | tail -5 | sed 's/^/      /'; fi; }
PARITY=$here/../parity/blacksmith-ubuntu-2404.txt
WANT=$(sed -n 's/^tool python3 \([0-9]*\.[0-9]*\).*/\1/p' "$PARITY")
[[ $WANT =~ ^[0-9]+\.[0-9]+$ ]]; check $? "the parity file names the machines' python3 ($WANT)"

# build-check.sh: the host's tools are stubs that log what they are asked; python3 says the version in $PYV.
H=$T/host; B=$T/bin; mkdir -p "$H" "$B" "$T/pull/parity"; cp "$PARITY" "$T/pull/parity/"
cat > "$H/python3" << 'STUB'
#!/bin/bash
echo "python3 $*" >> "$CALLS"
case "$*" in *version_info*) echo "$PYV" ;; esac
STUB
# gofmt lists $GOFMT_OUT as unformatted; go vet exits $GO_VET_RC.
printf '#!/bin/bash\necho "gofmt $*" >> "$CALLS"\n[ -z "${GOFMT_OUT:-}" ] || echo "$GOFMT_OUT"\n' > "$H/gofmt"
printf '#!/bin/bash\necho "go $*" >> "$CALLS"\n[ "$1" != vet ] || exit "${GO_VET_RC:-0}"\n' > "$H/go"
chmod +x "$H"/*
export CALLS=$T/calls
C() { env PATH="$H:$PATH" PYV="$1" "$here/build-check.sh" "$T/pull" "${2:-$PARITY}"; }
: > "$CALLS"
refuses "build-check.sh refuses tests under the build host's python3 (3.9)" "the tests' python3 is 3.9, the machines' is $WANT" C 3.9
[ ! -s "$CALLS" ] || ! grep -q '^go \|^gofmt ' "$CALLS"; check $? "and ran no Go test"
: > "$CALLS"
refuses "build-check.sh refuses a python3 newer than the machines'" "the tests' python3 is 3.13, the machines' is $WANT" C 3.13
out=$(C "$WANT" 2>&1); rc=$?
[ $rc = 0 ] && grep -qF "build-check: python3 $WANT, the machines'" <<< "$out"; check $? "build-check.sh passes under the machines' python3"
grep -q '^go test -count=1 ./\.\.\.$' "$CALLS" && grep -q '^go vet ./\.\.\.$' "$CALLS" && grep -qx 'python3 -I -m unittest discover -s . -p test_agent.py' "$CALLS"
check $? "and runs the Go tests and agent.py's tests, from the pull directory, under that python3"
: > "$CALLS"
CF() { env GOFMT_OUT=fleet.go PATH="$H:$PATH" PYV="$1" "$here/build-check.sh" "$T/pull" "$PARITY"; }
CV() { env GO_VET_RC=1 PATH="$H:$PATH" PYV="$1" "$here/build-check.sh" "$T/pull" "$PARITY"; }
refuses "build-check.sh refuses an unformatted file" "gofmt would change: fleet.go" CF "$WANT"
! grep -q '^go \|unittest' "$CALLS"; check $? "and runs no vet and no test after it"
: > "$CALLS"
refuses "build-check.sh refuses a go vet failure" "go vet failed" CV "$WANT"
! grep -q '^go test\|unittest' "$CALLS"; check $? "and runs no test after it"
sed 's/^tool python3 .*/tool python3 3.13/' "$PARITY" > "$T/parity13.txt"
refuses "build-check.sh takes the machines' python3 from the parity file it is given, not a number of its own" "the machines' is 3.13" C "$WANT" "$T/parity13.txt"
grep -v '^tool python3 ' "$PARITY" > "$T/parity-none.txt"
refuses "build-check.sh refuses a parity file with no python3" "has no 'tool python3 <major.minor>' line" C "$WANT" "$T/parity-none.txt"
[ "$("$here/build-check.sh" --want "$PARITY")" = "$WANT" ] && [ "$("$here/build-check.sh" --want "$T/parity13.txt")" = 3.13 ]; check $? "build-check.sh --want reads the version from the file"

# build.sh against a commit of a tree, its instance an stub: what the host installs and runs.
W=$T/work; mkdir -p "$W"; cp -r "$here/.." "$W/pull"
git -C "$W" init -q; git -C "$W" add -A pull; git -C "$W" -c user.name=t -c user.email=t@t commit -q -m tree
mkdir -p "$T/out" "$T/fake"
for f in superci-pull superci-shim pullcache-linux-amd64 pullcache-linux-arm64 pullcache-lambda-arm64.zip; do echo "$f" > "$T/out/$f"; done
ln -s "$here/testdata/aws" "$B/aws"
cat > "$B/ssh" << 'STUB'
#!/bin/bash
# The build host: answers ready, keeps what it is sent and the script it is told to run, and returns the programs.
cmd=${*: -1}
case "$cmd" in
  "test -f /tmp/go-ready") exit 0 ;;
  "mkdir -p src && tar -C src -xzf -") cat > /dev/null ;;
  "cat > build-check.sh") cat > "$FAKE_BUILD/build-check.sh" ;;
  "cat > parity.txt") cat > "$FAKE_BUILD/parity.txt" ;;
  "tar -C /tmp/out -cf - .") tar -C "$FAKE_OUT" -cf - . ;;
  *) printf '%s\n' "$cmd" > "$FAKE_BUILD/script" ;;
esac
STUB
chmod +x "$B/ssh"
export FAKE_AWS=$T/aws FAKE_BUILD=$T/fake FAKE_OUT=$T/out; mkdir -p "$FAKE_AWS"
BUILD() { env PATH="$B:$PATH" "$W/pull/deploy/build.sh" "$@"; }
out=$(BUILD "$T/release" 2>&1); rc=$?
[ $rc = 0 ] && [ -f "$T/release/listener/bin/superci-pull" ] && grep -q "terminated" <<< "$out"; check $? "build.sh builds against the stubbed host and terminates it"; [ $rc = 0 ] || echo "$out" | tail -5 | sed 's/^/      /'
ud=$(cat "$FAKE_AWS/user-data")
grep -qF "dnf install -y -q git python$WANT && mkdir /opt/py && ln -s /usr/bin/python$WANT /opt/py/python3 &&" <<< "$ud"
check $? "the build host installs the python3 the parity file names ($WANT), beside its own, as python3 of a directory of its own"
script=$(cat "$FAKE_BUILD/script")
grep -qF 'cd ~/src/pull && PATH=/opt/py:$PATH bash ~/build-check.sh . ~/parity.txt' <<< "$script" && ! grep -q 'go test' <<< "$script"
check $? "and runs its tests only through build-check.sh, with that python3 first on the PATH"
cmp -s "$FAKE_BUILD/build-check.sh" "$here/build-check.sh" && cmp -s "$FAKE_BUILD/parity.txt" "$PARITY"; check $? "which is this checkout's, with the parity file of the commit built"
[ "$(grep -n 'build-check.sh \. ' <<< "$script" | cut -d: -f1)" -lt "$(grep -n 'go build' <<< "$script" | head -1 | cut -d: -f1)" ]; check $? "before anything is built"
# A commit whose parity file names no python3 builds nothing and starts no machine.
sed -i '/^tool python3 /d' "$W/pull/parity/blacksmith-ubuntu-2404.txt"; git -C "$W" -c user.name=t -c user.email=t@t commit -q -am nopy
: > "$FAKE_AWS/calls.log"
refuses "build.sh refuses a commit whose parity file names no python3" "has no 'tool python3 <major.minor>' line" BUILD "$T/release2"
! grep -q run-instances "$FAKE_AWS/calls.log"; check $? "and started no machine"
exit $failed
