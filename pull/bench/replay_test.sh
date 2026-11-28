#!/bin/bash
# replay.sh against a local origin, in a temporary repository: the tree it pushes, a refused push, and the identities it
# needs before the first push. Run by bench_scripts_test.go.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fails=0
check() { if [ "$1" = 0 ]; then echo "ok    $2"; else echo "FAIL  $2"; fails=$((fails + 1)); fi; }
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
git init -q --bare "$T/origin.git"
git init -q "$T/w" && cd "$T/w" || exit 1
echo a > a.txt && git add a.txt && git commit -qm base || exit 1
git remote add origin "$T/origin.git" && git push -q origin HEAD:refs/heads/main && git fetch -q origin || exit 1
base=$(git rev-parse origin/main)
# A commit of main's that another session's fetch brings into the same clone while the replay runs
idx=$T/idx
GIT_INDEX_FILE=$idx git read-tree "$base"
GIT_INDEX_FILE=$idx git update-index --add --cacheinfo "100644,$(echo b | git hash-object -w --stdin),backend/b.ts"
moved=$(git commit-tree "$(GIT_INDEX_FILE=$idx git write-tree)" -p "$base" -m moved)
leg() { printf '{"t": %s, "label": "superci-8cpu", "duration_s": 1, "name": "u", "run": 1}' "$1"; }
printf '{"bucket_s": 1, "jobs": [%s, %s, %s]}\n' "$(leg 0)" "$(leg 1)" "$(leg 2)" > "$T/trace.json"
printf '{"sizes": {"superci-8cpu": {"dispatch": true}, "superci-2cpu": {}}}\n' > "$T/override.json"
export SUPERCI_TEST=1 MAX_SLEEP=1 REPLAY_OVERRIDE=$T/override.json

# ---- the tree ----
( for _ in $(seq 100); do git --git-dir="$T/origin.git" rev-parse -q --verify refs/heads/ci/load-a > /dev/null && break; sleep 0.05; done
  git update-ref refs/remotes/origin/main "$moved" ) &
mover=$!
REPLAY_LABELS_CMD="echo superci-8cpu 5" bash "$here/replay.sh" ci/load-a "$T/trace.json" > "$T/out" 2>&1
check $? "a trace replay of 3 buckets runs"
wait "$mover"
[ "$(git rev-parse origin/main)" = "$moved" ]; check $? "and origin/main moved under it"
tip=$(git --git-dir="$T/origin.git" rev-parse refs/heads/ci/load-a)
[ "$(git rev-list --count "$base..$tip")" = 3 ]; check $? "one commit a bucket, on main as the replay found it"
bad=0
for c in $(git rev-list "$base..$tip"); do
  case "$(git diff --name-only "$c^" "$c")" in "" | .github/workflows/superci-load.yml) ;; *) bad=$((bad + 1)) ;; esac
  [ "$(git diff --name-only "$base" "$c")" = .github/workflows/superci-load.yml ] || bad=$((bad + 1))
done
[ "$bad" = 0 ]; check $? "each push differs from the one before it at most, and from the base exactly, by the workflow (main's backend/b.ts is in none)"

# ---- a refused push ----
git update-ref refs/remotes/origin/main "$base"
printf '#!/bin/sh\nn=$(cat "$GIT_DIR/../pushes" 2>/dev/null || echo 0); echo $((n + 1)) > "$GIT_DIR/../pushes"\n[ "$n" -lt 1 ] || { echo refused by the hook; exit 1; }\n' > "$T/origin.git/hooks/pre-receive"
chmod 755 "$T/origin.git/hooks/pre-receive"
REPLAY_LABELS_CMD="echo superci-8cpu 5" bash "$here/replay.sh" ci/load-b "$T/trace.json" > "$T/out" 2>&1
rc=$?
[ "$rc" != 0 ] && grep -q 'refused: bucket 1,' "$T/out" && ! grep -q 'pushed: bucket 1,' "$T/out"; check $? "a refused push stops the replay and is not reported pushed (rc $rc)"
[ "$(cat "$T/pushes")" = 2 ]; check $? "and nothing is pushed after it"
rm -f "$T/origin.git/hooks/pre-receive"

# ---- identities ----
REPLAY_LABELS_CMD="echo superci-8cpu 2" bash "$here/replay.sh" ci/load-c "$T/trace.json" > "$T/out" 2>&1
rc=$?
[ "$rc" != 0 ] && grep -q 'superci-8cpu: 2 idle, the trace.s peak holds 3: SHORT' "$T/out"; check $? "a label with fewer identities than the trace's peak is refused (rc $rc)"
! git --git-dir="$T/origin.git" rev-parse -q --verify refs/heads/ci/load-c > /dev/null; check $? "before any push"
REPLAY_LABELS_CMD="echo superci-4cpu 50" bash "$here/replay.sh" ci/load-c "$T/trace.json" > "$T/out" 2>&1
[ $? != 0 ] && grep -q 'superci-8cpu: 0 idle' "$T/out"; check $? "and so is a label GitHub lists no identity under"
printf '{"bucket_s": 1, "jobs": [%s, {"t": 0, "label": "superci-2cpu", "duration_s": 1, "name": "s", "run": 1}]}\n' "$(leg 0)" > "$T/mixed.json"
DRY_RUN= REPLAY_LABELS_CMD="echo superci-8cpu 1" bash "$here/replay.sh" ci/load-d "$T/mixed.json" > "$T/out" 2>&1
[ $? = 0 ] && grep -q "superci-2cpu: a scale set's label, no identities: not checked" "$T/out"; check $? "a scale set's label is not checked: it has no identities"
REPLAY_LABELS_CMD=false bash "$here/replay.sh" ci/load-c "$T/trace.json" > "$T/out" 2>&1
[ $? != 0 ] && grep -q refusing "$T/out" && ! git --git-dir="$T/origin.git" rev-parse -q --verify refs/heads/ci/load-c > /dev/null; check $? "a count that cannot be read refuses too"
# identity.sh is asked instead, with an aws that answers nothing: no call leaves this machine
mkdir -p "$T/nobin" && printf '#!/bin/sh\necho "$*" >> "%s/aws.log"\nexit 1\n' "$T" > "$T/nobin/aws" && chmod 755 "$T/nobin/aws"
env -u SUPERCI_TEST REPLAY_LABELS_CMD="echo superci-8cpu 5" PATH="$T/nobin:$PATH" AWS_ACCESS_KEY_ID= AWS_SECRET_ACCESS_KEY= AWS_SESSION_TOKEN= \
  bash "$here/replay.sh" ci/load-c "$T/trace.json" > "$T/out" 2>&1
[ $? != 0 ] && grep -q 'identity.sh labels' "$T/out" && grep -q 'sts get-caller-identity' "$T/aws.log"; check $? "without SUPERCI_TEST the test's command is ignored and identity.sh is asked (here its aws answers nothing)"
DRY_RUN=1 bash "$here/replay.sh" ci/load-c "$T/trace.json" > "$T/out" 2>&1
[ $? = 0 ] && grep -q 'bucket 0 at +0s' "$T/out"; check $? "a dry run reads no identities and pushes nothing"

[ "$fails" = 0 ] || { echo "$fails failed"; exit 1; }
