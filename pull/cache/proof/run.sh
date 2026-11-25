#!/bin/bash
# The proof, on one machine of the pull fleet's image with the machines' role (no GitHub involved): runs the real
# actions/cache (a checkout of its release in $AC, run by the runner's own wrapped Node) against this checkout's
# cache server and the real bucket, with job tokens signed by a key of ours (the server is told where its JWKS is,
# and so is the broker: up.sh with BROKER_JWKS_URL set to an address of $P/jwks the function can read).
#
#   proof/run.sh big|scope|iam    needs: ~/bin/mint, /usr/local/bin/pullcache, the checkout in $AC, sudo
#
# Env: AC (default ~/ac), MODE=ebs|shm (where the job's workspace and temp are; default ebs, as a machine's job),
#      SIZE_MB (default 2000).
set -euo pipefail
AC=${AC:-$HOME/ac}
MODE=${MODE:-ebs}
SIZE_MB=${SIZE_MB:-2000}
P=$HOME/proof
NODE=/home/runner/externals/node24/bin/node
BUCKET=superci-pull-cache-243891825325-us-east-1
case $MODE in shm) BASE=/dev/shm/job ;; *) BASE=$HOME/job ;; esac
mkdir -p "$BASE/ws" "$BASE/tmp" "$BASE/state"; [ "${1:-}" = restore ] || rm -rf "$BASE/ws"/* "$BASE/tmp"/*

serve() {
  pgrep -f 'http.server 9099' > /dev/null || { (cd "$P" && setsid nohup python3 -m http.server 9099 --bind 127.0.0.1 < /dev/null > /dev/null 2>&1 &) ; sleep 1; }
  sudo systemctl stop pullcache 2> /dev/null || true
  sudo systemctl stop pullcache-proof 2> /dev/null || true
  sudo systemctl reset-failed pullcache-proof 2> /dev/null || true
  sudo systemd-run --quiet --unit=pullcache-proof --property=Restart=always \
    --setenv=PULLCACHE_BUCKET=$BUCKET --setenv=PULLCACHE_REGION=us-east-1 --setenv=PULLCACHE_EXTERNALS=/home/runner/externals \
    --setenv=PULLCACHE_JWKS_URL=http://127.0.0.1:9099/jwks /usr/local/bin/pullcache serve
  for _ in $(seq 50); do [ -s /home/runner/externals/pullcache.address ] && break; sleep 0.1; done
  # The address file may be the previous run's: wait until what it names answers.
  for _ in $(seq 50); do curl -s -o /dev/null "$(cat /home/runner/externals/pullcache.address)/x" && break; sleep 0.1; done
}

token() { "$HOME/bin/mint" token "$P" 77 "$@"; }

# action KIND TOKEN KEY PATH [RESTORE_KEYS]: one step of actions/cache, as the runner runs it. Prints its log;
# its outputs go to $BASE/state/output.
action() {
  local kind=$1 tok=$2 key=$3 path=$4 rk=${5:-}
  : > "$BASE/state/output"; : > "$BASE/state/state"
  cd "$BASE/ws" && env -i PATH="$PATH" HOME="$HOME" TMPDIR="$BASE/tmp" \
    GITHUB_WORKSPACE="$BASE/ws" RUNNER_TEMP="$BASE/tmp" RUNNER_OS=Linux GITHUB_REPOSITORY=acme/mono GITHUB_REF="${REF:-refs/heads/main}" \
    GITHUB_SERVER_URL=https://github.com GITHUB_EVENT_NAME=push GITHUB_OUTPUT="$BASE/state/output" GITHUB_STATE="$BASE/state/state" \
    ACTIONS_RESULTS_URL=https://results-receiver.actions.githubusercontent.com/ ACTIONS_RUNTIME_TOKEN="$tok" ACTIONS_CACHE_SERVICE_V2=true \
    "INPUT_PATH=$path" "INPUT_KEY=$key" "INPUT_RESTORE-KEYS=$rk" INPUT_ENABLECROSSOSARCHIVE=false INPUT_FAIL-ON-CACHE-MISS=false INPUT_LOOKUP-ONLY=false \
    "$NODE" "$AC/dist/$kind/index.js" 2>&1
}

sum() { (cd "$BASE/ws" && find "$1" -type f -print0 | sort -z | xargs -0 sha256sum | sha256sum | cut -c1-16); }
hit() { grep -A1 '^cache-hit<<' "$BASE/state/output" | grep -q '^true$' && echo hit || echo miss; }
check() { if [ "$2" = "$3" ]; then echo "PASS $1: $2"; else echo "FAIL $1: got '$2', want '$3'"; FAILED=1; fi; }
FAILED=0
now() { date +%s.%N; }
secs() { awk "BEGIN { printf \"%.1f\", $2 - $1 }"; }

big() {
  serve
  echo "wrapper in place: $([ -x "${NODE%node}node.real" ] && echo yes || echo NO), runs as node $("$NODE" --version)"
  mkdir -p "$BASE/ws/data"
  for i in $(seq 1 $((SIZE_MB / 100))); do head -c 100M /dev/urandom > "$BASE/ws/data/f$i"; done
  local want; want=$(sum data)
  local tok; tok=$(token refs/heads/main:rw)
  echo "== save $(du -sm "$BASE/ws/data" | cut -f1) MiB of incompressible files ($MODE)"
  local t0 t1; t0=$(now)
  action save-only "$tok" "big-$RANDOM-$$" data | tee "$BASE/save.log" | grep -E 'Cache Size|saved|Failed|rror|warning' || true
  t1=$(now); echo "save step: $(secs "$t0" "$t1") s"
  local key; key=$(grep -o 'big-[0-9]*-[0-9]*' "$BASE/save.log" | head -1)
  sudo journalctl -u pullcache-proof --no-pager -o cat | grep -E "save \"$key\"" | tail -3
  rm -rf "$BASE/ws/data"; sync
  echo "== restore"
  t0=$(now)
  action restore-only "$tok" "$key" data | tee "$BASE/restore.log" | grep -E 'Cache Size|Received|restored|Failed|rror|warning' | tail -4 || true
  t1=$(now); echo "restore step: $(secs "$t0" "$t1") s"
  sudo journalctl -u pullcache-proof --no-pager -o cat | grep -E "restore \"$key\": sent" | tail -1
  check "restored files identical" "$(sum data)" "$want"
  check "cache-hit output" "$(hit)" hit
}

# restore KEY: the entry `big` saved (when its files are gone from the workspace), read again.
restore() {
  serve
  local tok; tok=$(token refs/heads/main:rw)
  rm -rf "$BASE/ws/data" "$BASE/tmp"/*; sync
  local t0 t1; t0=$(now)
  action restore-only "$tok" "$1" data | grep -E 'Cache Size|restored|warning' || true
  t1=$(now); echo "restore step: $(secs "$t0" "$t1") s"
  sudo journalctl -u pullcache-proof --no-pager -o cat | grep -E "restore \"$1\"" | tail -2
}

scope() {
  serve
  local run=$$-$RANDOM; local D=deps-$run OP=only-pr7-$run RO=ro-key-$run
  local main pr7 pr8 ro; main=$(token refs/heads/main:rw); pr7=$(token refs/pull/7/merge:rw refs/heads/main:r)
  pr8=$(token refs/pull/8/merge:rw refs/heads/main:r); ro=$(token refs/heads/main:r)
  mkdir -p "$BASE/ws/small"
  head -c 5M /dev/urandom > "$BASE/ws/small/a"; local A; A=$(sum small)
  action save-only "$main" "$D" small > "$BASE/s1.log"; grep -q 'Cache saved' "$BASE/s1.log" && echo "PASS main saves deps" || { echo "FAIL main saves deps"; cat "$BASE/s1.log"; FAILED=1; }
  local mainkey="v1/d/77/refs%2Fheads%2Fmain"; local before after
  before=$(aws s3api list-objects-v2 --region us-east-1 --bucket $BUCKET --prefix "$mainkey/" --query 'Contents[].[Key,Size,ETag]' --output text)
  rm -rf "$BASE/ws/small"
  REF=refs/pull/7/merge action restore-only "$pr7" "$D" small > "$BASE/s2.log"
  check "pr 7 reads main's entry" "$(hit) $(sum small)" "hit $A"
  head -c 5M /dev/urandom > "$BASE/ws/small/a"; local B; B=$(sum small)
  REF=refs/pull/7/merge action save-only "$pr7" "$D" small > "$BASE/s3.log"
  grep -q 'Cache saved' "$BASE/s3.log" && echo "PASS pr 7 saves the same key into its own scope" || { echo "FAIL pr 7 save"; cat "$BASE/s3.log"; FAILED=1; }
  after=$(aws s3api list-objects-v2 --region us-east-1 --bucket $BUCKET --prefix "$mainkey/" --query 'Contents[].[Key,Size,ETag]' --output text)
  check "main's object untouched by the pull request (key, size, etag)" "$after" "$before"
  rm -rf "$BASE/ws/small"; action restore-only "$main" "$D" small > /dev/null; check "main still reads its own" "$(sum small)" "$A"
  rm -rf "$BASE/ws/small"; REF=refs/pull/7/merge action restore-only "$pr7" "$D" small > /dev/null; check "pr 7 now reads its own first" "$(sum small)" "$B"
  rm -rf "$BASE/ws/small"; REF=refs/pull/8/merge action restore-only "$pr8" "$D" small > /dev/null; check "pr 8 still reads main's, not pr 7's" "$(sum small)" "$A"
  head -c 1M /dev/urandom > "$BASE/ws/small/a"
  REF=refs/pull/7/merge action save-only "$pr7" "$OP" small > /dev/null
  rm -rf "$BASE/ws/small"; REF=refs/pull/8/merge action restore-only "$pr8" "$OP" small > /dev/null; check "pr 8 cannot read pr 7's entry" "$(hit)" miss
  mkdir -p "$BASE/ws/small"; head -c 1M /dev/urandom > "$BASE/ws/small/a"
  local n1 n2; n1=$(aws s3api list-objects-v2 --region us-east-1 --bucket $BUCKET --prefix v1/ --query 'length(Contents)')
  action save-only "$ro" "$RO" small > "$BASE/s4.log"
  n2=$(aws s3api list-objects-v2 --region us-east-1 --bucket $BUCKET --prefix v1/ --query 'length(Contents)')
  check "a read-only token writes nothing" "$n2" "$n1"
  grep -E 'denied|warning|Failed' "$BASE/s4.log" | head -2 || true
  local forged; forged=$(sed 's/\.[^.]*$/.AAAA/' <<< "$main")
  check "a forged token is refused (HTTP status)" "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $forged" -H 'content-type: application/json' -d '{"key":"deps","version":"abc"}' "$(cat /home/runner/externals/pullcache.address)/twirp/github.actions.results.api.v1.CacheService/GetCacheEntryDownloadURL")" 401
  echo "objects now:"; aws s3api list-objects-v2 --region us-east-1 --bucket $BUCKET --prefix v1/ --query 'Contents[].[Key,Size]' --output text
}

# try CMD...: allowed, or the S3 error it got (AccessDenied, PreconditionFailed).
try() {
  if "$@" > "$BASE/try.out" 2>&1; then echo allowed; return; fi
  grep -o -m1 -E 'AccessDenied|PreconditionFailed' "$BASE/try.out" || { sed 's/^/    /' "$BASE/try.out" >&2; echo error; }
}
put() { aws s3api put-object --region us-east-1 --bucket "$1" --key "$2" --body "$BASE/probe" "${@:3}"; }

# creds NAME TOKEN: the broker's credentials for the token, into $BASE/creds-NAME (never printed); prints the
# broker's refusal, or what the credentials are for and how long they last.
creds() {
  local f="$BASE/creds-$1"
  rm -f "$f"
  ( umask 077; aws lambda invoke --region us-east-1 --function-name superci-pull-cache-broker --cli-binary-format raw-in-base64-out \
      --payload "{\"token\":\"$2\"}" "$f.json" > "$f.meta" )
  python3 -c '
import datetime, json, os, sys
f = sys.argv[1]
r = json.load(open(f + ".json"))
os.remove(f + ".json")
if "errorMessage" in r:
    print(r["errorMessage"]); sys.exit(0)
fd = os.open(f, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
os.write(fd, ("AWS_ACCESS_KEY_ID=%s\nAWS_SECRET_ACCESS_KEY=%s\nAWS_SESSION_TOKEN=%s\n" % (r["access_key_id"], r["secret_access_key"], r["session_token"])).encode())
os.close(fd)
left = datetime.datetime.fromisoformat(r["expires"].replace("Z", "+00:00")) - datetime.datetime.now(datetime.timezone.utc)
print("issued for %s and %s, %d minutes left" % (r["grant"]["Entries"], r["grant"]["Stage"], left.total_seconds() // 60))
' "$f"
}
as() { local who=$1; shift; (set -a; . "$BASE/creds-$who"; set +a; "$@"); }

# iam: what the machine's own role, and what a job's credentials from the broker, can write, tried on the bucket.
iam() {
  local run=$$-$RANDOM v=0123456789abcdef
  local main="v1/d/77/refs%2Fheads%2Fmain/$v" pr="v1/r/77/refs%2Fpull%2F7%2Fmerge/$v" other="v1/r/78/refs%2Fpull%2F7%2Fmerge/$v"
  head -c 1024 /dev/urandom > "$BASE/probe"
  check "machine role: create an entry in main's scope" "$(try put $BUCKET "$main/m-$run" --if-none-match '*')" AccessDenied
  check "machine role: create an entry in a pull request's scope" "$(try put $BUCKET "$pr/m-$run" --if-none-match '*')" AccessDenied
  check "machine role: put a block" "$(try put $BUCKET-stage "u/77/refs%2Fpull%2F7%2Fmerge/m-$run")" AccessDenied
  check "machine role: read" "$(try aws s3api head-object --region us-east-1 --bucket $BUCKET --key bin/up.sh)" allowed
  echo "broker, pull request 7: $(creds pr7 "$(token refs/pull/7/merge:rw refs/heads/main:r)")"
  echo "broker, main: $(creds main "$(token refs/heads/main:rw)")"
  echo "broker, a read-only token: $(creds ro "$(token refs/heads/main:r)")"
  check "a read-only token gets no credentials" "$([ -e "$BASE/creds-ro" ] && echo issued || echo none)" none
  check "pull request: create an entry in main's scope" "$(try as pr7 put $BUCKET "$main/p-$run" --if-none-match '*')" AccessDenied
  check "pull request: create an entry in another repository's scope" "$(try as pr7 put $BUCKET "$other/p-$run" --if-none-match '*')" AccessDenied
  check "pull request: create an entry in its own scope" "$(try as pr7 put $BUCKET "$pr/p-$run" --if-none-match '*')" allowed
  check "pull request: write over its own entry without If-None-Match" "$(try as pr7 put $BUCKET "$pr/p-$run")" AccessDenied
  check "pull request: write over its own entry with If-None-Match" "$(try as pr7 put $BUCKET "$pr/p-$run" --if-none-match '*')" PreconditionFailed
  check "pull request: create an entry without If-None-Match" "$(try as pr7 put $BUCKET "$pr/q-$run")" AccessDenied
  check "pull request: put a block in main's stage" "$(try as pr7 put $BUCKET-stage "u/77/refs%2Fheads%2Fmain/p-$run")" AccessDenied
  check "pull request: put a block in its own stage" "$(try as pr7 put $BUCKET-stage "u/77/refs%2Fpull%2F7%2Fmerge/p-$run")" allowed
  check "pull request: delete its own block" "$(try as pr7 aws s3api delete-object --region us-east-1 --bucket $BUCKET-stage --key "u/77/refs%2Fpull%2F7%2Fmerge/p-$run")" allowed
  check "pull request: delete an entry" "$(try as pr7 aws s3api delete-object --region us-east-1 --bucket $BUCKET --key "$pr/p-$run")" AccessDenied
  check "main: create an entry in main's scope" "$(try as main put $BUCKET "$main/m-$run" --if-none-match '*')" allowed
  rm -f "$BASE"/creds-*
  echo "probe entries: $main/m-$run $pr/p-$run"
}

"$@"
[ "$FAILED" = 0 ] && echo "ALL PASS" || { echo "SOME FAILED"; exit 1; }
