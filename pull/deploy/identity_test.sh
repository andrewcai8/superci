#!/bin/bash
# identity.sh and identity-host.sh end to end with AWS, SSH, the runner's config.sh and the listener's admin endpoint
# stubbed (testdata/identity): the host script that is pushed is the one that runs. Run by identity_scripts_test.go;
# needs bash, python3 and openssl.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
F=$here/testdata/identity
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
export FAKE=$T/fake FAKE_RUNNER=$T/runner PATH=$F:$PATH SSH=$F/listener-ssh AWS_ACCESS_KEY_ID= AWS_SECRET_ACCESS_KEY= AWS_SESSION_TOKEN=
# harden walks FS_ROOT: never this machine's own /, whichever way a test reaches the host script (the fake ssh points it
# at the fake host's tree), and HARDEN_OWNER is whoever runs the test, so no chown needs root.
mkdir -p "$T/fsroot-none"; export FS_ROOT=$T/fsroot-none HARDEN_OWNER=$(id -u):$(id -g)
export READY_TRIES=2 READY_SLEEP=0 LEASE_WAIT=0 LEASE_TRIES=2 RUN_RETRY_SLEEP=0 PEAK_TRIES=2 PEAK_WAIT=0 IDENTITY_LOCK=$T/lock
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
KEY=arn:aws:kms:us-east-1:243891825325:key/1234abcd-12ab-34cd-56ef-1234567890ab
flog() { : > "$FAKE/calls.log"; : > "$FAKE/ssh.log"; : > "$FAKE/curl.log"; : > "$FAKE/config.log"; : > "$FAKE/sleep.log"; rm -f "$FAKE/state.json" "$FAKE/state.seq"; }
flog

# ---- the account ----
for c in "up us-east-1a" "register 4cpu 1" "relabel 4cpu a b" "down us-east-1a" "status"; do
  refuses "identity.sh $c refuses another account" "refusing: account 111111111111 is not ci-runners" env FAKE_ACCOUNT=111111111111 "$ID" $c
done
[ "$(grep -vc ' sts get-caller-identity' "$FAKE/calls.log")" = 0 ]; check $? "and made no AWS call but the identity check"

# ---- the key ----
refuses "a key whose alias cannot be made is scheduled for deletion, not left to be made again" "the new key is scheduled for deletion" env FAKE_ALIAS_FAIL=1 "$ID" up us-east-1a
grep -q 'kms schedule-key-deletion' "$FAKE/calls.log" && [ ! -e "$FAKE/state/key" ]; check $? "and no key is left"
[ "$(count 'run-instances' "$FAKE/calls.log")" = 0 ]; check $? "and no host is launched"
flog
# A role up just made is not a principal KMS knows for some seconds (measured Nov 27: create-key refused 3 s after
# create-role). That answer is asked again; any other refusal is not. Each in a world of its own, which holds no shim,
# so up stops once the key is settled.
K1=$T/fake-key-lag; mkdir -p "$K1/state"
refuses "up waits out a role KMS does not know yet (and then refuses for want of a released shim)" "no released shim" env FAKE=$K1 FAKE_KEY_LAG=2 "$ID" up us-east-1a
[ "$(count 'kms create-key' "$K1/calls.log")" = 3 ] && [ -e "$K1/state/key" ] && [ "$(count 'kms create-alias' "$K1/calls.log")" = 1 ]
check $? "and makes the key on the third ask, under one alias"
K2=$T/fake-key-denied; mkdir -p "$K2/state"
refuses "a create-key refused for any other reason is not asked again" "refusing: create-key failed" env FAKE=$K2 FAKE_KEY_DENIED=1 "$ID" up us-east-1a
[ "$(count 'kms create-key' "$K2/calls.log")" = 1 ] && [ ! -e "$K2/state/key" ] && [ "$(count 'run-instances' "$K2/calls.log")" = 0 ]
check $? "and is asked once, makes no key and launches nothing"

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
has "--block-device-mappings DeviceName=/dev/sda1,Ebs={Encrypted=true,DeleteOnTermination=true}" "$c" "and encrypts the root volume, on the image's own root device"
[ "$(count 'kms create-key' "$c")" = 1 ] && [ "$(count 'kms create-alias --alias-name alias/superci-pull-identities' "$c")" = 1 ]; check $? "the key is made once, under its alias"
U=$FAKE/state/userdata-0001
has "$PIN" "$U" "user data pins the listener's public key"
has "KMS_KEY=$KEY" "$U" "and names the key the hosts upload under"
hasnt "PRIVATE KEY" "$U" "and holds no private key"
[ "$(wc -c < "$U")" -le 16000 ]; check $? "user data fits EC2's 16 KB"
has "https://203.0.113.9:8443" "$U" "and names the listener's address"
python3 -I -c 'import re,sys,base64,gzip; sys.stdout.buffer.write(gzip.decompress(base64.b64decode(re.search(r"echo .([A-Za-z0-9+/=]+). \| base64 -d \| gunzip", open(sys.argv[1]).read()).group(1))))' "$U" | cmp -s - "$here/identity-host.sh"
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
           "arn:aws:s3:::superci-pull-243891825325/bin/superci-shim", "arn:aws:s3:::superci-pull-243891825325/bin/RELEASE.json", "arn:aws:kms:us-east-1:243891825325:key/1234abcd-12ab-34cd-56ef-1234567890ab"}
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
check $? "the role may only use its prefix, the shim and the one key; the bucket denies unencrypted puts and others' reads, writes and deletes"
python3 -I - "$FAKE/state/role-policy.json" "$FAKE/state/bucket-policy.json" "$FAKE/state/key-policy.json" "$FAKE/state/bucket-encryption.json" << 'PY'
import json, sys
role, bucket, key, enc = (json.load(open(f)) for f in sys.argv[1:5])
KEY = "arn:aws:kms:us-east-1:243891825325:key/1234abcd-12ab-34cd-56ef-1234567890ab"
kms = [st for st in role["Statement"] if any(a.startswith("kms:") for a in (st["Action"] if isinstance(st["Action"], list) else [st["Action"]]))]
assert len(kms) == 1 and kms[0]["Resource"] == KEY, kms
assert sorted(kms[0]["Action"]) == ["kms:Decrypt", "kms:Encrypt", "kms:GenerateDataKey"], kms[0]["Action"]
assert kms[0]["Condition"]["StringLike"]["kms:EncryptionContext:aws:s3:arn"] == "arn:aws:s3:::superci-pull-identities-243891825325/superci-pull/*"
assert kms[0]["Condition"]["StringEquals"]["kms:ViaService"] == "s3.us-east-1.amazonaws.com"
rule = enc["Rules"][0]
assert rule["ApplyServerSideEncryptionByDefault"] == {"SSEAlgorithm": "aws:kms", "KMSMasterKeyID": KEY} and rule["BucketKeyEnabled"] is False, rule
sid = {s["Sid"]: s for s in bucket["Statement"]}["NoPutWithAnotherKey"]
assert sid["Effect"] == "Deny" and sid["Condition"]["StringNotEquals"]["s3:x-amz-server-side-encryption-aws-kms-key-id"] == KEY
principals = {}
for st in key["Statement"]:
    assert st["Effect"] == "Allow" and list(st["Principal"]) == ["AWS"], st
    principals[st["Sid"]] = (st["Principal"]["AWS"], set(st["Action"]))
admin, use = principals["TheOperatorAdministersTheKey"], principals["TheIdentityRoleEncryptsAndDecryptsThroughS3"]
root = principals["TheAccountCanStillAdministerTheKey"]
assert admin[0] == "arn:aws:iam::243891825325:role/ops", admin[0]
assert root[0] == "arn:aws:iam::243891825325:root" and root[1] == admin[1], root
assert not any(a.startswith(("kms:Encrypt", "kms:Decrypt", "kms:GenerateDataKey", "kms:ReEncrypt", "kms:CreateGrant", "kms:*")) for a in root[1]), "the account root administers and never uses the key"
assert use[0] == "arn:aws:iam::243891825325:role/superci-pull-identity" and use[1] == {"kms:Encrypt", "kms:Decrypt", "kms:GenerateDataKey"}, use
assert len(principals) == 3, principals
assert not any(a.startswith(("kms:Encrypt", "kms:Decrypt", "kms:GenerateDataKey", "kms:ReEncrypt", "kms:CreateGrant", "kms:*")) for a in admin[1]), "the operator administers and never uses the key"
PY
check $? "the key's policy names only the identity role (encrypt, decrypt), the operator and the account root (administer, never use: a deleted operator role cannot lock the key); the bucket's default is that key and it refuses another"
grep -q 'put-public-access-block' "$c" && grep -q 'put-bucket-encryption' "$c"; check $? "the bucket blocks public access and encrypts by default"
A=$FAKE/state/instances; ip_a=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]+"/i-0001"))["ip"])' "$A"); ip_b=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]+"/i-0002"))["ip"])' "$A")

# ---- one identity.sh at a time ----
exec 7> "$T/lock"; flock -n 7
for c in "register --rate 1 4cpu 1" "relabel 4cpu a b" "up us-east-1a" "down us-east-1c"; do
  refuses "identity.sh $c refuses while another run holds the lock" "holds $T/lock" "$ID" $c
done
exec 7>&-
ok "and runs once the lock is free" "$ID" up us-east-1a

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
[ "$(count '^token' "$FAKE/curl.log")" = 1 ] && has "settle id=a1b2c3 made=4 failed=0 refused=0" "$FAKE/curl.log" "one lease of 4, settled with the 4 made and nothing failed or refused"
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
refuses "a config.sh that GitHub refuses stops the whole command and names the identity" "superci-id-2cpu-0003 was not registered: GitHub refused it" env FAKE_CONFIG_FAIL=superci-id-2cpu-0003 "$ID" register --rate 1 2cpu 6 1
grep -q "GitHub refused it (429 or ConfigureTooManyRunners" <<< "$out"; check $? "saying so when its log shows the 429"
[ "$(count . "$FAKE/config.log")" = 3 ] && ! grep -q 'superci-id-2cpu-0004' "$FAKE/config.log"; check $? "no identity after it was tried"
grep -q "settle id=.* made=2 failed=0 refused=1" "$FAKE/curl.log"; check $? "the lease is settled with the 2 made and the refusal, which reaches the listener's budget"
grep -q "Wait at least 5 minutes" <<< "$out" && grep -q "do not run it again at once" <<< "$out"; check $? "and the operator is told to wait, not to run it again"
! grep -q "run again to continue" <<< "$out"; check $? "(it does not say run again)"
flog
refuses "a config.sh that fails for another reason stops the command too" "superci-id-2cpu-0003 was not registered" env FAKE_CONFIG_FAIL_PLAIN=superci-id-2cpu-0003 "$ID" register --rate 1 2cpu 6 1
grep -q "settle id=.* made=0 failed=1 refused=0" "$FAKE/curl.log"; check $? "the lease is settled as a failure, not a refusal"
! grep -q "GitHub refused it" <<< "$out"; check $? "and does not claim GitHub refused it"
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
ok "register across three hosts" env IDENTITY_AZS="us-east-1a us-east-1b us-east-1c" "$ID" register --rate 1 16cpu 6 1
for p in "1 b" "2 c" "3 a" "4 b" "5 c" "6 a"; do set -- $p
  [ -s "$IDB/us-east-1$2/superci-id-16cpu-000$1/.runner" ]; check $? "identity $1 is on host $((10#$1 % 3)) (us-east-1$2)"
done

# ---- identity i lives on IDENTITY_AZS[i mod n], whichever hosts are up ----
flog
ok "with three hosts up, the default AZs still hold identities on a and b only" "$ID" register --rate 1 4cpu 2 20
for p in "20 a" "21 b"; do set -- $p
  [ -s "$IDB/us-east-1$2/superci-id-4cpu-00$1/.runner" ]; check $? "identity $1 is on us-east-1$2, not on the third host"
done
[ -z "$(ls "$IDB/us-east-1c" | grep '4cpu-002')" ]; check $? "and nothing of them is on us-east-1c"
refuses "an AZ named twice is refused" "names an AZ twice" env IDENTITY_AZS="us-east-1a us-east-1a" "$ID" register --rate 1 4cpu 1
refuses "an AZ that is not one is refused" "az is one of" env IDENTITY_AZS="us-east-1a eu-west-1a" "$ID" register --rate 1 4cpu 1
python3 -I - "$FAKE/state/instances/i-0003" << 'PY'
import json, sys
i = json.load(open(sys.argv[1]))
i["state"] = "stopped"
json.dump(i, open(sys.argv[1], "w"))
PY
flog
refuses "register refuses while an expected host is down, though others are up" "refusing: us-east-1c has no running identity host" env IDENTITY_AZS="us-east-1a us-east-1b us-east-1c" "$ID" register --rate 1 4cpu 1 30
[ "$(count '^token' "$FAKE/curl.log")" = 0 ] && [ "$(count . "$FAKE/config.log")" = 0 ] && [ "$(count . "$FAKE/ssh.log")" = 0 ]; check $? "and takes no lease, registers nothing and reaches no host"
ok "a host that is not in the list may be down" "$ID" register --rate 1 4cpu 1 30
python3 -I - "$FAKE/state/instances/i-0003" << 'PY'
import json, sys
i = json.load(open(sys.argv[1]))
i["state"] = "running"
json.dump(i, open(sys.argv[1], "w"))
PY

# ---- a lease that has run out is not used ----
flog; printf '%s\n' -60 600 > "$FAKE/lease_ttls"
ok "a lease past its time is settled with nothing made, and a new one is asked for" "$ID" register --rate 1 4cpu 2 40
[ "$(count '^token' "$FAKE/curl.log")" = 2 ]; check $? "two leases"
sed -n '/^settle/p' "$FAKE/curl.log" | head -1 | grep -q "made=0 failed=0 refused=0"; check $? "the first was settled with none made"
sed -n '/^settle/p' "$FAKE/curl.log" | tail -1 | grep -q "made=2 failed=0 refused=0"; check $? "and both identities were registered under the second"
[ "$(count . "$FAKE/config.log")" = 2 ]; check $? "no registration was made under the lease that had run out"
flog; printf '%s\n' -60 -60 -60 > "$FAKE/lease_ttls"
refuses "leases that are always past their time end in a stop, not a loop" "no registration was made under" "$ID" register --rate 1 4cpu 2 42
[ "$(count . "$FAKE/config.log")" = 0 ]; check $? "with nothing registered"
rm -f "$FAKE/lease_ttls"

# ---- a call that hangs is cut off ----
flog; touch "$FAKE/ssh-hang"
start=$SECONDS
refuses "a host that never answers a registration is given up on" "superci-id-4cpu-0044 was not registered" env REGISTER_TIMEOUT=1 "$ID" register --rate 1 4cpu 1 44
[ $((SECONDS - start)) -lt 20 ]; check $? "within its timeout"
grep -q "no answer from the host" <<< "$out"; check $? "saying it gave no answer"
grep -q "settle id=.* made=0 failed=1 refused=0" "$FAKE/curl.log"; check $? "and the lease is settled with the failure"
rm -f "$FAKE/ssh-hang"; flog
start=$SECONDS
refuses "a config.sh that hangs is killed on the host" "superci-id-4cpu-0045 was not registered" env FAKE_CONFIG_HANG=superci-id-4cpu-0045 CONFIG_TIMEOUT=1 "$ID" register --rate 1 4cpu 1 45
[ $((SECONDS - start)) -lt 20 ]; check $? "within its timeout"
for h in $FAKE/hosts/*/opt/id; do [ ! -e "$h/superci-id-4cpu-0045" ]; done; check $? "and leaves no half-made identity tree"
grep -q "settle id=.* made=0 failed=1 refused=0" "$FAKE/curl.log"; check $? "and is settled as a failure"

# ---- the rate ----
flog
ok "register at the default rate" "$ID" register 64cpu 3 1
[ "$(awk '$1 >= 1' "$FAKE/sleep.log" | wc -l)" = 2 ] && awk '$1 >= 1 {if ($1 < 3 || $1 > 4.001) bad = 1} END {exit bad}' "$FAKE/sleep.log"; check $? "the default is 0.25 a second: each identity waits out 4 s from the start of the last"
flog
ok "register at 1 a second" "$ID" register --rate 1 64cpu 3 4
awk '$1 > 0.2 {n++; if ($1 > 1.001) bad = 1} END {exit bad || n != 2}' "$FAKE/sleep.log"; check $? "is paced at a second"
flog
for r in 1.5 2 0 x -1; do
  refuses "--rate $r is refused" "above 0 and at most 1" "$ID" register --rate $r 64cpu 1
done
[ "$(count '^token' "$FAKE/curl.log")" = 0 ] && [ "$(count . "$FAKE/config.log")" = 0 ]; check $? "a refused rate takes no lease and registers nothing"
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
r.append({"id": 5002, "name": "superci-pull-jit-0f3a", "status": "online", "busy": True, "labels": ["self-hosted", "superci-park", "superci-4cpu"]})
json.dump(r, open(p, "w"))' "$FAKE"
flog
ok "relabel moves one size to its label" "$ID" relabel 4cpu superci-park superci-4cpu
[ "$(count '^labels' "$FAKE/curl.log")" -ge 7 ]; check $? "a relabel for each of the identities of the size (one of them carried both labels)"
! grep -q 'labels id=5002' "$FAKE/curl.log"; check $? "and none for the runner that is not an identity"
python3 -I -c 'import json,sys
r = json.load(open(sys.argv[1] + "/github.json"))
assert all(("superci-4cpu" in x["labels"]) == x["name"].startswith("superci-id-4cpu-") for x in r if x["name"].startswith("superci-id-")), r
assert all("superci-park" not in x["labels"] for x in r if x["name"].startswith("superci-id-4cpu-"))
jit = [x for x in r if x["id"] == 5002]
assert jit and jit[0]["labels"] == ["self-hosted", "superci-park", "superci-4cpu"], "the non-identity runner was relabelled"' "$FAKE"
check $? "only that size's identities carry the new label, and the runner that is not an identity keeps its own"
N4=$(count '^labels' "$FAKE/curl.log")
awk -v n4="$N4" '$1 >= 1 {n++} END {exit n < n4 - 1}' "$FAKE/sleep.log"; check $? "paced at one a second at most: each relabel waits 1 s (60 a minute)"
flog
ok "relabel again" "$ID" relabel 4cpu superci-park superci-4cpu
[ "$(count '^labels' "$FAKE/curl.log")" = 0 ]; check $? "changes nothing"
flog
ok "relabel back is the rollback" "$ID" relabel 4cpu superci-4cpu superci-park
[ "$(count '^labels' "$FAKE/curl.log")" = "$N4" ] && [ "$(count '^token' "$FAKE/curl.log")" = 0 ]; check $? "$N4 relabels and no lease"
flog; printf '429 30 2\n' > "$FAKE/labels-limit"
refuses "relabel stops at the first 429" "GitHub answered 429" "$ID" relabel 4cpu superci-park superci-4cpu
[ "$(count '^labels' "$FAKE/curl.log")" = 3 ]; check $? "after 2 relabels and the one refused: no call after it"
grep -q "Retry-After is 30 s" <<< "$out"; check $? "and gives its Retry-After"
refuses "a relabel inside the Retry-After waits" "GitHub asked for a pause" "$ID" relabel 4cpu superci-park superci-4cpu
[ "$(count '^labels' "$FAKE/curl.log")" = 3 ]; check $? "without a call to GitHub"
rm -f "$FAKE/labels-limit" "$T/lock.retry-after"
flog; printf '403 0 0\n' > "$FAKE/labels-limit"
refuses "relabel stops at the first 403 too, and holds a minute when GitHub names no Retry-After" "GitHub answered 403" "$ID" relabel 4cpu superci-park superci-4cpu
[ "$(count '^labels' "$FAKE/curl.log")" = 1 ] && [ "$(( $(cat "$T/lock.retry-after") - $(date +%s) ))" -gt 0 ]; check $? "after a single call, and the pause is kept"
rm -f "$FAKE/labels-limit" "$T/lock.retry-after"
flog
ok "relabel runs on once the pause is over" "$ID" relabel 4cpu superci-park superci-4cpu
[ "$(count '^labels' "$FAKE/curl.log")" = "$((N4 - 2))" ]; check $? "relabelling the rest: the 2 that went through before the 429 are not touched again"
flog; "$ID" relabel 4cpu superci-4cpu superci-park > /dev/null 2>&1
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
printf 'BUCKET=superci-pull-identities-%s\nDEPLOYMENT=superci-pull\nRELEASE=superci-pull-%s\nKMS_KEY=%s\n' "$ACCOUNT" "$ACCOUNT" "$KEY" > "$N/conf"
newhost() { env ID_ROOT=$N/id RUNNER_SRC=$FAKE_RUNNER SHIM_BIN=$N/shim SHIM_ENV=$N/shim.env ID_CONF=$N/conf UNIT_DIR=$N/units ID_LOG=$N/log ID_AZ=us-east-1b ID_LOCK=$N/lock \
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
for want in MemoryMax=192M Restart=always RestartSec=5 StartLimitIntervalSec=600 StartLimitBurst=10 RUNNER_ALLOW_RUNASROOT=1; do
  has "$want" "$U" "every identity's unit has $want"
done
! grep -q '^KillMode=' "$U"; check $? "and keeps systemd's default KillMode, so a stop takes the listener and its shim down together"
printf 'tampered\n' >> "$REL/bin/superci-shim"; rm -rf "$N/id" "$N/enabled" "$N/ready" "$N/shim"
refuses "a replacement host refuses a shim that is not the release's" "refusing s3://superci-pull-$ACCOUNT/bin/superci-shim" newhost boot
{ [ ! -e "$N/id" ] || [ -z "$(ls "$N/id")" ]; } && [ ! -e "$N/enabled" ]; check $? "and starts nothing"

# ---- harden: nothing is left writable by others where root runs ----
# The machines' image as measured Nov 27: /usr/local/bin and /opt mode 777, the runner tree the runner user's, and
# Instance Connect's chain writable. Here the chain is 775, writable by its group alone, which no sweep of what others
# may write would touch and sshd still refuses. A sticky /tmp, /proc and a symlink are left as they are.
H2=$T/harden; R=$H2/fs; RS=$H2/runner; NL=$(printf 'a\nb')
mkdir -p "$R/usr/share/ec2-instance-connect" "$R/usr/local/bin" "$R/opt/hostedtoolcache/go" "$R/tmp" "$R/proc" "$R/etc" "$RS/bin" "$RS/externals"
for f in usr/share/ec2-instance-connect/eic_run_authorized_keys usr/share/ec2-instance-connect/eic_parse_authorized_keys usr/local/bin/node opt/hostedtoolcache/go/go etc/ok "opt/$NL"; do printf 'x\n' > "$R/$f"; done
chmod 777 "$R/usr/local/bin" "$R/usr/local/bin/node" "$R/opt" "$R/opt/hostedtoolcache" "$R/opt/hostedtoolcache/go" "$R/opt/hostedtoolcache/go/go" "$R/opt/$NL"
chmod 775 "$R/usr" "$R/usr/share" "$R/usr/share/ec2-instance-connect" "$R/usr/share/ec2-instance-connect/eic_run_authorized_keys" "$R/usr/share/ec2-instance-connect/eic_parse_authorized_keys"
chmod 1777 "$R/tmp"; printf 'p\n' > "$R/proc/x"; chmod 777 "$R/proc/x"; chmod 644 "$R/etc/ok"
ln -s /tmp "$R/usr/local/bin/link"
[ "$(id -u)" = 0 ] || { mkdir -m 000 "$R/opt/unreadable"; }
printf '#!/bin/sh\n' > "$RS/bin/Runner.Listener"; printf '#!/bin/sh\n' > "$RS/run.sh"; printf 't\n' > "$RS/run-helper.sh.template"; printf 'e\n' > "$RS/externals/node"
chmod 777 "$RS/bin" "$RS/bin/Runner.Listener" "$RS/run.sh" "$RS/externals/node"; chmod 775 "$RS/run-helper.sh.template"
hostenv=(ID_ROOT=$H2/id ID_CONF=$N/conf ID_LOG=$H2/log ID_LOCK=$H2/lock ID_AZ=us-east-1b)
hardened() { env FS_ROOT=$R RUNNER_SRC=$RS "${hostenv[@]}" bash "$HS" harden; }
ok "harden takes write from others everywhere, past a name with a newline and a directory it cannot read" hardened
left=$(find "$R" -path "$R/proc" -prune -o ! -type l -perm -0002 \( -type f -o \( -type d ! -perm -1000 \) \) -print 2> /dev/null)
[ -z "$left" ]; check $? "and leaves none outside /proc and sticky directories${left:+ (left: $left)}"
[ "$(stat -c %a "$R/usr/local/bin/node")" = 755 ] && [ "$(stat -c %a "$R/opt")" = 755 ] && [ "$(stat -c %a "$R/opt/$NL")" = 755 ] && [ "$(stat -c %a "$R/etc/ok")" = 644 ]
check $? "a 777 binary, /opt and the newline name become 755, a path already safe is as it was"
for p in "$R" "$R/usr" "$R/usr/share" "$R/usr/share/ec2-instance-connect" "$R/usr/share/ec2-instance-connect/eic_run_authorized_keys" "$R/usr/share/ec2-instance-connect/eic_parse_authorized_keys"; do
  [ $(( 0$(stat -c %a "$p") & 022 )) = 0 ] || echo "$p"
done | grep -q . ; check $((1 - $?)) "Instance Connect's scripts and every directory above them, writable by their group alone, are writable by neither group nor others, as sshd requires"
[ "$(stat -c %a "$R/tmp")" = 1777 ] && [ "$(stat -c %a "$R/proc/x")" = 777 ] && [ -L "$R/usr/local/bin/link" ]; check $? "a sticky /tmp, /proc and a symlink are left alone"
bad=$(find "$RS" ! -type l -perm /022 -print); [ -z "$bad" ]; check $? "the runner tree root runs is writable by its owner alone${bad:+ (left: $bad)}"
[ "$(id -u)" = 0 ] || chmod 755 "$R/opt/unreadable"
# A chmod that changes nothing: the sweep cannot clear the tree, and harden, and boot with it, refuse to go on.
mkdir -p "$H2/nochmod"; printf '#!/bin/sh\nexit 0\n' > "$H2/nochmod/chmod"; chmod 755 "$H2/nochmod/chmod"
chmod 777 "$R/opt"
refuses "harden fails while a path stays writable by others" "is still writable by others" env PATH="$H2/nochmod:$PATH" FS_ROOT=$R RUNNER_SRC=$RS "${hostenv[@]}" bash "$HS" harden
rm -rf "$N/id" "$N/enabled" "$N/ready"
refuses "and boot fails with it" "is still writable by others" env PATH="$H2/nochmod:$PATH" FS_ROOT=$R ID_ROOT=$N/id RUNNER_SRC=$RS SHIM_BIN=$N/shim SHIM_ENV=$N/shim.env ID_CONF=$N/conf UNIT_DIR=$N/units ID_LOG=$N/log ID_AZ=us-east-1b ID_LOCK=$N/lock READY_FILE=$N/ready FAKE_UNITS=$N/enabled bash "$HS" boot
[ ! -e "$N/enabled" ] && [ ! -e "$N/ready" ] && { [ ! -e "$N/id" ] || [ -z "$(ls "$N/id")" ]; }; check $? "and starts and restores nothing"
chmod 755 "$R/opt"
if [ "$(id -u)" != 0 ]; then
  # A chown that changes nothing leaves the chain this test's user's, not root's: sshd would refuse it, so harden does.
  mkdir -p "$H2/nochown"; printf '#!/bin/sh\nexit 0\n' > "$H2/nochown/chown"; chmod 755 "$H2/nochown/chown"
  refuses "harden fails while Instance Connect's chain is not root's" "is not root's alone" env PATH="$H2/nochown:$PATH" HARDEN_OWNER=root:root FS_ROOT=$R RUNNER_SRC=$RS "${hostenv[@]}" bash "$HS" harden
  for spelling in / // /. /tmp/..; do
    # (were the guard ever broken, the no-op chmod and chown and the timeout keep this machine as it is)
    refuses "harden walks / only as root, however FS_ROOT spells it ($spelling)" "harden walks / only as root" env PATH="$H2/nochmod:$H2/nochown:$PATH" FS_ROOT=$spelling RUNNER_SRC=$RS "${hostenv[@]}" timeout 10 bash "$HS" harden
  done
fi
first() { awk -v pat="$1" '/^boot\(\) \{/{b=1; next} b && /^}/{exit} b{n++; if ($0 ~ pat) {print n; exit}}' "$HS"; }
h=$(first '^  harden$'); o=$(first 'install |aws |cat >|systemctl|restore|start|chmod|chown')
[ -n "$h" ] && [ -n "$o" ] && [ "$h" -lt "$o" ]; check $? "boot hardens the host before it installs, restores or starts anything (line $h of boot, the first other action at $o)"

# ---- two runs on a host at once never delete each other's identity ----
C=$T/conc; mkdir -p "$C/units"
cp "$N/conf" "$C/conf"; printf '#!/bin/sh\n' > "$C/shim"; chmod 755 "$C/shim"
chost() { env ID_ROOT=$C/id RUNNER_SRC=$FAKE_RUNNER SHIM_BIN=$C/shim ID_CONF=$C/conf UNIT_DIR=$C/units ID_LOG=$C/log ID_AZ=us-east-1a ID_LOCK=$C/lock \
  READY_FILE=$C/ready FAKE_UNITS=$C/enabled bash "$HS" "$@"; }
flog
( printf 'TESTTOKEN\n' | FAKE_CONFIG_SLEEP=1 chost register superci-park https://github.com/Authentic-Intelligence - superci-id-4cpu-0099 > "$C/a.out" 2>&1 ) &
pa=$!
until [ -s "$FAKE/config.log" ]; do [ -e "/proc/$pa" ] || break; command -p sleep 0.1; done
( printf 'TESTTOKEN\n' | FAKE_CONFIG_SLEEP=1 chost register superci-park https://github.com/Authentic-Intelligence - superci-id-4cpu-0099 > "$C/b.out" 2>&1 ) &
pb=$!
wait $pa $pb
[ "$(count . "$FAKE/config.log")" = 1 ]; check $? "the second run found the first's identity and registered nothing (one config.sh)"
sort "$C/a.out" "$C/b.out" | tr '\n' ' ' | grep -q "adopted superci-id-4cpu-0099 registered superci-id-4cpu-0099"; check $? "one registered it and the other adopted it"
for f in .runner .credentials .credentials_rsaparams; do [ -s "$C/id/superci-id-4cpu-0099/$f" ]; done; check $? "and its files are all there"
[ "$(sort "$C/enabled" | uniq | wc -l)" = 1 ]; check $? "started once as one identity"
exec 7> "$C/lock"; flock 7
creg() { printf 'TESTTOKEN\n' | chost register superci-park https://github.com/Authentic-Intelligence - "$@"; }
export LOCK_WAIT=1
refuses "a run that cannot get the lock in time says so and changes nothing" "busy: another superci-id run holds" creg superci-id-4cpu-0098
unset LOCK_WAIT
exec 7>&-
[ ! -e "$C/id/superci-id-4cpu-0098" ]; check $? "and registers nothing"

# ---- the image is the one fleet.go runs ----
grep -qF "Owner: \"$(sed -n 's/^IMAGE_OWNER=//p' "$here/identity.sh")\", Name: \"$(sed -n "s/^IMAGE_NAME='\(.*\)'/\1/p" "$here/identity.sh")\", Pin: \"$(sed -n 's/^IMAGE_PIN=//p' "$here/identity.sh")\"" "$here/../main.go"
check $? "identity.sh pins the image main.go gives the machines"
exit $failed
