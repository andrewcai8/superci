#!/bin/bash
# identity.sh and identity-host.sh end to end with AWS, SSH, the runner's config.sh and the listener's admin endpoint
# stubbed (testdata/identity): the host script that is pushed is the one that runs. Run by identity_scripts_test.go;
# needs bash, python3 and openssl.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
F=$here/testdata/identity
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
export FAKE=$T/fake FAKE_RUNNER=$T/runner PATH=$F:$PATH SSH=$F/listener-ssh AWS_ACCESS_KEY_ID= AWS_SECRET_ACCESS_KEY= AWS_SESSION_TOKEN=
export READY_TRIES=2 READY_SLEEP=0 LEASE_WAIT=0 LEASE_TRIES=2 RELABEL_PAUSE=0 RUN_RETRY_SLEEP=0 PEAK_TRIES=2 PEAK_WAIT=0
# identity.sh under a timeout, so a loop that never ends fails the test instead of hanging it
printf '#!/bin/bash\nexec timeout 120 %s "$@"\n' "$here/identity.sh" > "$T/id"; chmod 755 "$T/id"; ID=$T/id
ACCOUNT=243891825325; REL=$FAKE/s3/superci-pull-$ACCOUNT; IDB=$FAKE/s3/superci-pull-identities-$ACCOUNT/superci-pull/hosts
mkdir -p "$FAKE/state" "$REL/bin"; cp -r "$F/runner" "$T/runner"
failed=0
check() { if [ "$1" = 0 ]; then echo "ok    $2"; else echo "FAIL  $2"; failed=1; fi; }
ok() { local name=$1; shift; out=$("$@" 2>&1); local rc=$?; check $rc "$name"; [ $rc = 0 ] || echo "$out" | tail -8 | sed 's/^/      /'; }
refuses() { local name=$1 want=$2; shift 2; out=$("$@" 2>&1); local rc=$?
  if [ $rc != 0 ] && grep -qF -- "$want" <<< "$out"; then check 0 "$name"; else check 1 "$name (rc $rc)"; echo "$out" | tail -8 | sed 's/^/      /'; fi; }
count() { grep -c -- "$1" "$2" 2> /dev/null || true; }
has() { grep -qF -- "$1" "$2"; check $? "$3"; }
hasnt() { grep -qF -- "$1" "$2"; check $((1 - $?)) "$3"; }

# The listener's key, as /superci-pull/tls keeps it (a throwaway one), and the pin machines and shims take of its public half.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 2 -subj /CN=test -keyout "$T/key.pem" -out "$T/cert.pem" 2> /dev/null
python3 -I -c 'import json,sys; print(json.dumps({"Cert": open(sys.argv[1]).read(), "Key": open(sys.argv[2]).read()}))' "$T/cert.pem" "$T/key.pem" > "$FAKE/tls.json"
PIN=sha256//$(openssl x509 -in "$T/cert.pem" -pubkey -noout | openssl pkey -pubin -outform DER | openssl dgst -sha256 -binary | base64 -w0)
release() { # the release bucket holds this shim, named by bin/RELEASE.json
  printf '#!/bin/sh\n# shim %s\n' "$1" > "$REL/bin/superci-shim"
  printf '{"commit":"c","built":"b","sha256":{"bin/superci-pull":"%s","bin/superci-shim":"%s"}}\n' "$(printf x | sha256sum | cut -d' ' -f1)" "$(sha256sum < "$REL/bin/superci-shim" | cut -d' ' -f1)" > "$REL/bin/RELEASE.json"
}
flog() { : > "$FAKE/calls.log"; : > "$FAKE/ssh.log"; : > "$FAKE/curl.log"; : > "$FAKE/config.log"; : > "$FAKE/sleep.log"; rm -f "$FAKE/state.json" "$FAKE/state.seq"; }
flog

# ---- the account ----
for c in "up us-east-1a" "register 4cpu 1" "relabel 4cpu a b" "down us-east-1a" "status"; do
  refuses "identity.sh $c refuses another account" "refusing: account 111111111111 is not ci-runners" env FAKE_ACCOUNT=111111111111 "$ID" $c
done
[ "$(grep -vc ' sts get-caller-identity' "$FAKE/calls.log")" = 0 ]; check $? "and made no AWS call but the identity check"

# ---- up ----
refuses "up refuses while the release holds no shim" "refusing" "$ID" up us-east-1a
[ "$(count run-instances "$FAKE/calls.log")" = 0 ]; check $? "and launches nothing"
release one
ok "up launches the host of an AZ" "$ID" up us-east-1a
c=$FAKE/calls.log
for want in "--instance-type r7i.4xlarge" "--image-id ami-1" "HttpTokens=required,HttpPutResponseHopLimit=1" "Key=superci-pull-role,Value=identity" \
  "Key=superci-pull-deployment,Value=superci-pull" "Key=Name,Value=superci-pull-identity-us-east-1a" "Key=purpose,Value=superci-pull" "--iam-instance-profile Name=superci-pull-identity" "--subnet-id subnet-a"; do
  has "$want" "$c" "run-instances carries $want"
done
has "ResourceType=volume,Tags=[{Key=purpose,Value=superci-pull}" "$c" "and tags the volume"
U=$FAKE/state/userdata-0001
has "$PIN" "$U" "user data pins the listener's public key"
hasnt "PRIVATE KEY" "$U" "and holds no private key"
[ "$(wc -c < "$U")" -le 16000 ]; check $? "user data fits EC2's 16 KB"
has "https://203.0.113.9:8443" "$U" "and names the listener's address"
python3 -I -c 'import re,sys,base64; sys.stdout.buffer.write(base64.b64decode(re.search(r"echo .([A-Za-z0-9+/=]+). \| base64 -d", open(sys.argv[1]).read()).group(1)))' "$U" | cmp -s - "$here/identity-host.sh"
check $? "and carries the host script of this checkout"
grep -q 'superci-id boot' "$U"; check $? "and runs its boot"
ok "up again changes nothing" "$ID" up us-east-1a
[ "$(count 'run-instances' "$c")" = 1 ]; check $? "and launches no second host"
grep -q "already up" <<< "$out"; check $? "and says so"
ok "up of a second AZ" "$ID" up us-east-1b
python3 -I - "$FAKE/state/role-policy.json" "$FAKE/state/bucket-policy.json" << 'EOF'
import json, sys
role, bucket = json.load(open(sys.argv[1])), json.load(open(sys.argv[2]))
allowed = {"arn:aws:s3:::superci-pull-identities-243891825325/superci-pull/*", "arn:aws:s3:::superci-pull-identities-243891825325",
           "arn:aws:s3:::superci-pull-243891825325/bin/superci-shim", "arn:aws:s3:::superci-pull-243891825325/bin/RELEASE.json", "arn:aws:kms:us-east-1:243891825325:key/*"}
res = set()
for st in role["Statement"]:
    assert st["Effect"] == "Allow"
    acts = st["Action"] if isinstance(st["Action"], list) else [st["Action"]]
    assert all(a.startswith(("s3:Get", "s3:Put", "s3:List", "kms:")) and "*" not in a for a in acts), acts
    res |= set(st["Resource"] if isinstance(st["Resource"], list) else [st["Resource"]])
assert res <= allowed, res - allowed
sids = {s["Sid"]: s for s in bucket["Statement"]}
for need in ["TlsOnly", "NoPutWithoutKms", "NoPutWithoutAnEncryptionHeader", "OnlyTheIdentityHostsWrite", "OnlyTheIdentityHostsRead", "OnlyTheOperatorDeletes"]:
    assert sids[need]["Effect"] == "Deny", need
assert sids["OnlyTheOperatorDeletes"]["Condition"]["ArnNotLike"]["aws:PrincipalArn"] == ["arn:aws:iam::243891825325:role/ops", "arn:aws:iam::243891825325:role/*/ops"]
assert sids["OnlyTheIdentityHostsRead"]["Condition"]["ArnNotEquals"]["aws:PrincipalArn"] == "arn:aws:iam::243891825325:role/superci-pull-identity"
EOF
check $? "the role may only use its prefix, the shim and KMS; the bucket denies unencrypted puts and others' reads, writes and deletes"
grep -q 'put-public-access-block' "$c" && grep -q 'put-bucket-encryption' "$c"; check $? "the bucket blocks public access and encrypts by default"
A=$FAKE/state/instances; ip_a=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]+"/i-0001"))["ip"])' "$A"); ip_b=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]+"/i-0002"))["ip"])' "$A")

# ---- register: the token, the lease, the partition ----
flog
ok "register registers n identities" "$ID" register --rate 1 4cpu 4
grep -q "registered 4" <<< "$out"; check $? "and says it made 4"
hasnt TESTTOKEN "$FAKE/calls.log" "no token in an aws call"
hasnt TESTTOKEN "$FAKE/ssh.log" "no token in any ssh argument"
hasnt TESTTOKEN "$FAKE/config.log" "no token in a config.sh argument"
grep -q 'token_in_env=yes' "$FAKE/config.log" && ! grep -q 'token_in_env=no' "$FAKE/config.log"; check $? "config.sh got the token in its environment"
grep -rq TESTTOKEN "$FAKE" ; check $((1 - $?)) "the token is on no file"
hasnt TESTTOKEN <(echo "$out") "and not in the output"
[ "$(count '^token' "$FAKE/curl.log")" = 1 ] && has "settle id=a1b2c3 made=4" "$FAKE/curl.log" "one lease of 4, settled with the 4 made"
for want in "--labels superci-park --no-default-labels" "--disableupdate" "--runnergroup superci-pull-identities" "--url https://github.com/Authentic-Intelligence"; do
  has "$want" "$FAKE/config.log" "config.sh ran with $want"
done
hasnt "--token" "$FAKE/config.log" "and without --token"
for i in 1 3; do [ -s "$IDB/us-east-1b/superci-id-4cpu-000$i/.credentials_rsaparams" ]; check $? "identity $i is on host 1 of 2 (us-east-1b)"; done
for i in 2 4; do [ -s "$IDB/us-east-1a/superci-id-4cpu-000$i/.credentials" ]; check $? "identity $i is on host 0 of 2 (us-east-1a)"; done
[ ! -e "$IDB/us-east-1a/superci-id-4cpu-0001" ]; check $? "and on no other host"
[ "$(sort "$FAKE/hosts/$ip_b/enabled" | tr '\n' ' ')" = "superci-id-4cpu-0001 superci-id-4cpu-0003 " ]; check $? "each host runs its own share"

# ---- register again ----
flog
ok "register again" "$ID" register --rate 1 4cpu 4 1
grep -q "0 to register" <<< "$out" && grep -q "4 already held" <<< "$out"; check $? "finds all 4 held, in S3 and at GitHub"
[ "$(count '^token' "$FAKE/curl.log")" = 0 ] && [ "$(count . "$FAKE/config.log")" = 0 ]; check $? "and takes no lease and registers nothing"
ok "register with no first continues after the highest" "$ID" register --rate 1 4cpu 2
[ "$(count 'name superci-id-4cpu-0005' "$FAKE/config.log")" = 1 ] && [ "$(count 'name superci-id-4cpu-0006' "$FAKE/config.log")" = 1 ]; check $? "as 5 and 6"

# ---- the first failure stops the command ----
flog
refuses "a config.sh that fails stops the whole command and names the identity" "superci-id-2cpu-0003 was not registered" env FAKE_CONFIG_FAIL=superci-id-2cpu-0003 "$ID" register --rate 1 2cpu 6 1
grep -q "GitHub refused it (429 or ConfigureTooManyRunners" <<< "$out"; check $? "saying when its log shows GitHub's refusal"
[ "$(count . "$FAKE/config.log")" = 3 ] && ! grep -q 'superci-id-2cpu-0004' "$FAKE/config.log"; check $? "no identity after it was tried"
grep -q "settle id=.* made=2" "$FAKE/curl.log"; check $? "the lease is settled with the 2 made"
grep -q "run again to continue: identity.sh register 2cpu 6 1" <<< "$out"; check $? "and the re-run is named"
flog
ok "run again continues from the failed identity" "$ID" register --rate 1 2cpu 6 1
[ "$(count . "$FAKE/config.log")" = 4 ] && grep -q 'superci-id-2cpu-0003' "$FAKE/config.log" && ! grep -q 'superci-id-2cpu-0001' "$FAKE/config.log"; check $? "registering 3 to 6 only"
for i in 1 2 3 4 5 6; do [ -n "$(ls "$IDB"/*/superci-id-2cpu-000$i/.runner 2> /dev/null)" ]; check $? "2cpu identity $i is stored once"; done

# ---- an upload that fails after the registration: it is told apart, stops the command, and is stored by a re-run ----
flog; touch "$FAKE/upload-fail"
refuses "an upload that fails is reported" "superci-id-8cpu-0001 registered but its files were not stored" "$ID" register --rate 1 8cpu 2 1
grep -q "settle id=.* made=1" "$FAKE/curl.log"; check $? "the registration made still counts against the lease"
[ "$(count . "$FAKE/config.log")" = 1 ]; check $? "and the second identity was not tried"
rm "$FAKE/upload-fail"; flog
ok "register again stores it" "$ID" register --rate 1 8cpu 2 1
grep -q "1 registered but not stored" <<< "$out"; check $? "found at GitHub and not in S3"
[ "$(count . "$FAKE/config.log")" = 1 ] && grep -q 'superci-id-8cpu-0002' "$FAKE/config.log"; check $? "registering only the second"
for i in 1 2; do [ -n "$(ls "$IDB"/*/superci-id-8cpu-000$i/.runner 2> /dev/null)" ]; check $? "8cpu identity $i is in S3 now"; done

# ---- the budget grants less, or nothing ----
flog; printf '2\n' > "$FAKE/grants"
ok "a lease of fewer than asked registers that many and asks again" "$ID" register --rate 1 32cpu 3 1
[ "$(count '^token' "$FAKE/curl.log")" = 2 ] && grep -q "n=3 granted=2" "$FAKE/curl.log" && grep -q "n=1 granted=1" "$FAKE/curl.log"; check $? "a lease of 2, then of 1"
flog; printf '0\n0\n0\n' > "$FAKE/grants"
refuses "a budget that grants nothing stops, and registers nothing" "granted nothing" "$ID" register --rate 1 32cpu 6 4
[ "$(count . "$FAKE/config.log")" = 0 ]; check $? "so no registration was made"
rm -f "$FAKE/grants"
refuses "n is at most 100" "n is 1 to 100" "$ID" register --rate 1 4cpu 101
refuses "n is at least 1" "n is 1 to 100" "$ID" register --rate 1 4cpu 0

# ---- three hosts: identity i on host i mod 3 ----
refuses "up refuses a type that is not one" "--type is an instance type" "$ID" up --type 'r7i;x' us-east-1c
ok "up of a third AZ, on the smaller type" "$ID" up --type r7i.2xlarge us-east-1c
[ "$(count '--instance-type r7i.2xlarge' "$FAKE/calls.log")" = 1 ] && [ "$(count '--instance-type r7i.4xlarge' "$FAKE/calls.log")" = 0 ]; check $? "the flag sets the instance type"
flog
ok "register across three hosts" "$ID" register --rate 1 16cpu 6 1
for p in "1 b" "2 c" "3 a" "4 b" "5 c" "6 a"; do set -- $p
  [ -s "$IDB/us-east-1$2/superci-id-16cpu-000$1/.runner" ]; check $? "identity $1 is on host $((10#$1 % 3)) (us-east-1$2)"
done

# ---- the rate ----
flog
ok "register at the default rate" "$ID" register 64cpu 3 1
[ "$(awk '$1 >= 1' "$FAKE/sleep.log" | wc -l)" = 2 ] && awk '$1 >= 1 {if ($1 < 3 || $1 > 4.001) bad = 1} END {exit bad}' "$FAKE/sleep.log"; check $? "the default is 0.25 a second: each identity waits out 4 s from the start of the last"
flog
ok "register at 1 a second" "$ID" register --rate 1 64cpu 3 4
awk '$1 > 0.2 {n++; if ($1 > 1.001) bad = 1} END {exit bad || n != 2}' "$FAKE/sleep.log"; check $? "is paced at a second"
for r in 1.5 2 0 x -1; do
  refuses "--rate $r is refused" "above 0 and at most 1" "$ID" register --rate $r 64cpu 1
done
[ "$(count '^token' "$FAKE/curl.log")" = 0 ] || [ "$(count '^token' "$FAKE/curl.log")" = 1 ]; check 0 "(a refused rate takes no lease)"
flog
refuses "--rate over 1 takes no lease" "above 0 and at most 1" "$ID" register --rate 5 64cpu 2
[ "$(count '^token' "$FAKE/curl.log")" = 0 ] && [ "$(count . "$FAKE/config.log")" = 0 ]; check $? "and registers nothing"

# ---- leases of at most 20, each settled before the next is asked ----
flog
ok "22 identities" "$ID" register --rate 1 128cpu 22 1
[ "$(awk '/^(token|settle)/ {printf "%s ", $1}' "$FAKE/curl.log")" = "token settle token settle " ] && grep -q "n=20 granted=20" "$FAKE/curl.log" && grep -q "n=2 granted=2" "$FAKE/curl.log"; check $? "are two leases, 20 then 2, the first settled before the second is asked"
grep -q "made=20" "$FAKE/curl.log" && grep -q "made=2" "$FAKE/curl.log"; check $? "each settled with what it made"
grep -q "made 22 of 22 in" <<< "$out" && grep -q "0 left, about 0s at this pace" <<< "$out"; check $? "and the end reports the count, the time taken and what is left"

# ---- not at peak ----
flog; echo '{"budget":{"known":true,"org_rate":1.0,"throttled":true}}' > "$FAKE/state.json"
refuses "a held budget stops register" "not registering: the budget is held" "$ID" register --rate 1 256cpu 2 1
[ "$(count '^token' "$FAKE/curl.log")" = 0 ] && [ "$(count . "$FAKE/config.log")" = 0 ]; check $? "which asks for no lease and registers nothing"
echo '{"budget":{"known":true,"org_rate":3.6,"throttled":false}}' > "$FAKE/state.json"
refuses "an organization over 3.5 a second stops register" "above 3.5" "$ID" register --rate 1 256cpu 2 1
echo '{"budget":{"known":true,"org_rate":3.5,"throttled":false}}' > "$FAKE/state.json"
ok "3.5 a second is not over it" "$ID" register --rate 1 256cpu 1 1
echo '{"budget":{"known":false,"org_rate":0,"throttled":false}}' > "$FAKE/state.json"
refuses "an organization rate not known yet stops register" "not known yet" "$ID" register --rate 1 256cpu 2 2
echo '{"budget":{"known":true,"org_rate":4.9,"throttled":true}}' > "$FAKE/state.json"; flog
ok "--peak-ok registers anyway" "$ID" register --rate 1 --peak-ok 256cpu 2 2
[ "$(count '^state' "$FAKE/curl.log")" = 0 ] && [ "$(count . "$FAKE/config.log")" = 2 ]; check $? "without looking at the state"
flog; printf '%s\n' '{"budget":{"known":true,"org_rate":1,"throttled":true}}' '{"budget":{"known":true,"org_rate":1,"throttled":false}}' > "$FAKE/state.seq"
ok "register waits for the budget to be quiet, then registers" "$ID" register --rate 1 256cpu 2 4
grep -q "waiting: the budget is held" <<< "$out" && [ "$(count . "$FAKE/config.log")" = 2 ] && [ "$(count '^state' "$FAKE/curl.log")" = 2 ]; check $? "after one poll"
flog

# ---- relabel ----
python3 -I -c 'import json,sys
p = sys.argv[1] + "/github.json"
r = json.load(open(p))
r.append({"id": 5001, "name": "superci-id-4cpu-9001", "status": "online", "busy": False, "labels": ["self-hosted", "superci-park", "superci-4cpu"]})
json.dump(r, open(p, "w"))' "$FAKE"
flog
ok "relabel moves one size to its label" "$ID" relabel 4cpu superci-park superci-4cpu
[ "$(count '^labels' "$FAKE/curl.log")" = 7 ]; check $? "7 relabels for the 7 identities of the size (one of them carried both labels)"
python3 -I -c 'import json,sys
r = json.load(open(sys.argv[1] + "/github.json"))
assert all(("superci-4cpu" in x["labels"]) == x["name"].startswith("superci-id-4cpu-") for x in r), r
assert all("superci-park" not in x["labels"] for x in r if x["name"].startswith("superci-id-4cpu-"))' "$FAKE"
check $? "only that size carries the new label"
flog
ok "relabel again" "$ID" relabel 4cpu superci-park superci-4cpu
[ "$(count '^labels' "$FAKE/curl.log")" = 0 ]; check $? "changes nothing"
flog
ok "relabel back is the rollback" "$ID" relabel 4cpu superci-4cpu superci-park
[ "$(count '^labels' "$FAKE/curl.log")" = 7 ] && [ "$(count '^token' "$FAKE/curl.log")" = 0 ]; check $? "7 relabels and no lease"
refuses "relabel refuses a label that is not a name" "label is letters" "$ID" relabel 4cpu 'a;b' c

# ---- status ----
H=$FAKE/hosts/$ip_a; mkdir -p "$H/proc/101" "$H/proc/102"; printf '101\n102\n' > "$H/proc/pids"
ln -sf "$H/opt/id/superci-id-4cpu-0002" "$H/proc/101/cwd"; printf 'Rss: 9 kB\nPrivate_Dirty:     36864 kB\n' > "$H/proc/101/smaps_rollup"
ln -sf "$H/opt/id/superci-id-4cpu-0004" "$H/proc/102/cwd"; printf 'Rss: 9 kB\nPrivate_Dirty:     70000 kB\n' > "$H/proc/102/smaps_rollup"
ok "status" "$ID" status
grep -q "identity hosts: 3" <<< "$out" && grep -q "az us-east-1a identities [0-9]* listeners_running [0-9]* private_dirty_kib_avg 53432 max 70000 over_64mib 1" <<< "$out"; check $? "counts hosts, measures each listener's private memory and counts those over 64 MiB"
grep -q "at GitHub:" <<< "$out" && grep -q "memory they need on 3 hosts" <<< "$out" && grep -q "64 MiB private (its unit restarts it at MemoryMax 192M) + 60 MiB shared a host; 1,550 identities are about 97 GiB" <<< "$out"; check $? "and shows what GitHub holds and the memory budget"

# ---- down, and a replacement host restores its share ----
ok "down terminates a host by its name" "$ID" down us-east-1b
grep -q '"state": "terminated"' "$A/i-0002"; check $? "that host only"
grep -q '"state": "running"' "$A/i-0001"; check $? "the others run on"
[ -s "$IDB/us-east-1b/superci-id-4cpu-0001/.runner" ]; check $? "its identities stay in S3"
ok "down again" "$ID" down us-east-1b
grep -q "no host named superci-pull-identity-us-east-1b" <<< "$out"; check $? "finds none"

HS=$here/identity-host.sh
N=$T/newhost; mkdir -p "$N"
printf 'BUCKET=superci-pull-identities-%s\nDEPLOYMENT=superci-pull\nRELEASE=superci-pull-%s\n' "$ACCOUNT" "$ACCOUNT" > "$N/conf"
newhost() { env ID_ROOT=$N/id RUNNER_SRC=$FAKE_RUNNER SHIM_BIN=$N/shim SHIM_ENV=$N/shim.env ID_CONF=$N/conf UNIT_DIR=$N/units ID_LOG=$N/log ID_AZ=us-east-1b \
  READY_FILE=$N/ready FAKE_UNITS=$N/enabled bash "$HS" "$@"; }
mkdir -p "$N/units"; flog; cp "$FAKE/github.json" "$T/github.before"
ok "a replacement host boots" newhost boot
mine=$(ls "$IDB/us-east-1b"); have=$(ls "$N/id")
[ "$mine" = "$have" ] && [ -n "$mine" ]; check $? "restores exactly its AZ's share ($(echo $have | wc -w) identities)"
for n in $mine; do cmp -s "$IDB/us-east-1b/$n/.credentials" "$N/id/$n/.credentials" || echo "differs: $n"; done | grep -q . ; check $((1 - $?)) "with each identity's files as they were"
cmp -s "$REL/bin/superci-shim" "$N/id/superci-id-4cpu-0001/bin/Runner.Worker" && cmp -s "$REL/bin/superci-shim" "$N/shim"; check $? "each Runner.Worker is the released shim"
[ "$(stat -c %i "$N/id/superci-id-4cpu-0001/bin/Runner.Listener")" = "$(stat -c %i "$FAKE_RUNNER/bin/Runner.Listener")" ]; check $? "and the listener binary is the image's, hard-linked"
[ "$(sort "$N/enabled" | tr '\n' ' ')" = "$(echo $mine | tr ' ' '\n' | sort | tr '\n' ' ')" ]; check $? "every restored identity is started"
[ ! -s "$FAKE/config.log" ] && cmp -s "$FAKE/github.json" "$T/github.before" && [ "$(count '^token' "$FAKE/curl.log")" = 0 ]; check $? "and registers nothing"
[ -e "$N/ready" ]; check $? "and marks itself ready"
U=$N/units/superci-id@.service
grep -qxF "Environment=DOTNET_gcServer=0 DOTNET_GCgen0size=0x200000 DOTNET_GCHeapHardLimit=0x6000000 DOTNET_TieredPGO=0 DOTNET_TC_QuickJitForLoops=1 DOTNET_GCConserveMemory=7" "$U"
check $? "every identity's unit sets exactly the measured GC environment"
for want in MemoryMax=192M Restart=always RestartSec=2 KillMode=process RUNNER_ALLOW_RUNASROOT=1; do
  has "$want" "$U" "every identity's unit has $want"
done
printf 'tampered\n' >> "$REL/bin/superci-shim"; rm -rf "$N/id" "$N/enabled" "$N/ready" "$N/shim"
refuses "a replacement host refuses a shim that is not the release's" "refusing s3://superci-pull-$ACCOUNT/bin/superci-shim" newhost boot
[ ! -e "$N/id" ] || [ -z "$(ls "$N/id")" ]; [ ! -e "$N/enabled" ]; check $? "and starts nothing"

# ---- the image is the one fleet.go runs ----
grep -qF "Owner: \"$(sed -n 's/^IMAGE_OWNER=//p' "$here/identity.sh")\", Name: \"$(sed -n "s/^IMAGE_NAME='\(.*\)'/\1/p" "$here/identity.sh")\", Pin: \"$(sed -n 's/^IMAGE_PIN=//p' "$here/identity.sh")\"" "$here/../main.go"
check $? "identity.sh pins the image main.go gives the machines"
exit $failed
