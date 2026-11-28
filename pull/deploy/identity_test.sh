#!/bin/bash
# identity.sh and identity-host.sh end to end with AWS, SSH, the runner's config.sh and the listener's admin endpoint
# stubbed (testdata/identity): the host script that is pushed is the one that runs. Run by identity_scripts_test.go;
# needs bash, python3 and openssl.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
F=$here/testdata/identity
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
chmod +x "$F/ss" 2> /dev/null || true # (a patch does not carry the mode of the stub it adds)
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
for c in "up us-east-1a" "register 4cpu 1" "relabel 4cpu a b" "down us-east-1a" "status" "labels"; do
  refuses "identity.sh $c refuses another account" "refusing: account 111111111111 is not ci-runners" env FAKE_ACCOUNT=111111111111 "$ID" $c
done
[ "$(grep -vc ' sts get-caller-identity' "$FAKE/calls.log")" = 0 ]; check $? "and made no AWS call but the identity check"

# ---- the key ----
refuses "a key whose alias cannot be made is scheduled for deletion, not left to be made again" "the new key is scheduled for deletion" env FAKE_ALIAS_FAIL=1 "$ID" up us-east-1a
grep -q 'kms schedule-key-deletion' "$FAKE/calls.log" && [ ! -e "$FAKE/state/key" ]; check $? "and no key is left"
[ "$(count 'run-instances\|auto-scaling-group' "$FAKE/calls.log")" = 0 ]; check $? "and no host is launched and no group made"
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
[ "$(count 'kms create-key' "$K2/calls.log")" = 1 ] && [ ! -e "$K2/state/key" ] && [ "$(count 'run-instances\|auto-scaling-group' "$K2/calls.log")" = 0 ]
check $? "and is asked once, makes no key and launches nothing"

# ---- up ----
refuses "up refuses while the release holds no shim" "refusing" "$ID" up us-east-1a
[ "$(count 'run-instances\|auto-scaling-group\|create-launch-template' "$FAKE/calls.log")" = 0 ]; check $? "and launches nothing, and makes no group or template"
release one
ok "up makes the host of an AZ a group of one, from a launch template" "$ID" up us-east-1a
c=$FAKE/calls.log
[ "$(count 'run-instances' "$c")" = 0 ]; check $? "no instance is launched by hand: the group's template launches it"
python3 -I - "$FAKE/state" << 'PY'
import json, os, sys
st = sys.argv[1]
t = json.load(open(os.path.join(st, "lt", "superci-pull-identity-us-east-1a")))
d = json.loads(t["versions"][0]["data"])
assert len(t["versions"]) == 1 and t["versions"][0]["desc"].startswith("sha256:"), t["versions"]
assert d["InstanceType"] == "r7i.4xlarge" and d["ImageId"] == "ami-1", d
assert d["MetadataOptions"] == {"HttpTokens": "required", "HttpPutResponseHopLimit": 1, "HttpEndpoint": "enabled"}, d["MetadataOptions"]
assert d["IamInstanceProfile"] == {"Name": "superci-pull-identity"}, d["IamInstanceProfile"]
ni = d["NetworkInterfaces"]
assert ni == [{"DeviceIndex": 0, "AssociatePublicIpAddress": True, "DeleteOnTermination": True, "Groups": ["sg-identity"]}], ni
assert d["BlockDeviceMappings"] == [{"DeviceName": "/dev/sda1", "Ebs": {"Encrypted": True, "DeleteOnTermination": True}}], d["BlockDeviceMappings"]
specs = {s["ResourceType"]: {kv["Key"]: kv["Value"] for kv in s["Tags"]} for s in d["TagSpecifications"]}
assert specs["instance"] == {"purpose": "superci-pull", "superci-pull-role": "identity", "superci-pull-deployment": "superci-pull", "Name": "superci-pull-identity-us-east-1a"}, specs["instance"]
assert specs["volume"] == {"purpose": "superci-pull", "Name": "superci-pull-identity-us-east-1a"}, specs["volume"]
g = json.load(open(os.path.join(st, "asg", "superci-pull-identity-us-east-1a")))
assert (g["min"], g["max"], g["desired"]) == (1, 1, 1), g
assert g["subnet"] == "subnet-a" and g["health"] == "EC2" and g["version"] == "$Default" and g["lt"] == t["id"], g
assert g["maintenance"] == "MinHealthyPercentage=0,MaxHealthyPercentage=100", g["maintenance"]
assert len(g["instances"]) == 1
PY
check $? "the template carries today's host (type, image, IMDSv2, profile, public address, encrypted root on the image's device, tags and volume tags) and the group is one instance, EC2 health, in the AZ's subnet, terminating before it launches"
[ "$(count 'create-launch-template ' "$c")" = 1 ] && [ "$(count 'create-auto-scaling-group' "$c")" = 1 ] && [ "$(count 'wait instance-running' "$c")" -ge 1 ]; check $? "one template and one group are made, and the host is waited for"
grep -q "ready: superci-pull-identity-us-east-1a restored its share" <<< "$out"; check $? "up waits until the host has restored its share"
[ ! -e "$FAKE/state/two-live" ]; check $? "and no second host was ever live on a name"
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
n0=$(wc -l < "$c")
ok "up again changes nothing" "$ID" up us-east-1a
tail -n +$((n0 + 1)) "$c" > "$T/up-again.calls"
[ "$(count 'create-launch-template\|create-auto-scaling-group\|update-auto-scaling-group\|attach-instances\|terminate-instances' "$T/up-again.calls")" = 0 ]; check $? "no template version, no group change, no instance ended or attached"
[ "$(ls "$FAKE/state/instances" | wc -l)" = 1 ]; check $? "and no second host"
grep -q "already up" <<< "$out"; check $? "and says so"
python3 -I -c 'import json,sys; t = json.load(open(sys.argv[1])); assert [v["n"] for v in t["versions"]] == [1] and t["default"] == 1, t' "$FAKE/state/lt/superci-pull-identity-us-east-1a"
check $? "the template is still its one version"
ok "up of a second AZ" "$ID" up us-east-1b
python3 -I - "$FAKE/state/role-policy.json" "$FAKE/state/bucket-policy.json" << 'EOF'
import json, sys
role, bucket = json.load(open(sys.argv[1])), json.load(open(sys.argv[2]))
allowed = {"arn:aws:s3:::superci-pull-identities-243891825325/superci-pull/*", "arn:aws:s3:::superci-pull-identities-243891825325",
           "arn:aws:s3:::superci-pull-243891825325/bin/superci-shim", "arn:aws:s3:::superci-pull-243891825325/bin/RELEASE.json", "arn:aws:kms:us-east-1:243891825325:key/1234abcd-12ab-34cd-56ef-1234567890ab",
           "arn:aws:autoscaling:us-east-1:243891825325:autoScalingGroup:*:autoScalingGroupName/superci-pull-identity-*"}
res = set()
assert "PublishWhoPollsGitHub" in {st["Sid"] for st in role["Statement"]}, "the host may not publish IdentitiesPolling"
for st in role["Statement"]:
    assert st["Effect"] == "Allow"
    if st["Sid"] == "ReadItsOwnGroup":  # a Describe call takes no resource: it is the one statement on "*"
        assert st["Action"] == "autoscaling:DescribeAutoScalingInstances" and st["Resource"] == "*", st
        continue
    if st["Sid"] == "PublishWhoPollsGitHub":  # a metric has no ARN: the namespace is the condition
        assert st["Action"] == "cloudwatch:PutMetricData" and st["Resource"] == "*" and st["Condition"] == {"StringEquals": {"cloudwatch:namespace": "superci-pull"}}, st
        continue
    acts = st["Action"] if isinstance(st["Action"], list) else [st["Action"]]
    assert all(a.startswith(("s3:Get", "s3:Put", "s3:List", "kms:")) and "*" not in a or a == "autoscaling:SetInstanceHealth" for a in acts), acts
    res |= set(st["Resource"] if isinstance(st["Resource"], list) else [st["Resource"]])
assert res <= allowed, res - allowed
sids = {s["Sid"]: s for s in bucket["Statement"]}
for need in ["TlsOnly", "NoPutWithoutKms", "NoPutWithoutAnEncryptionHeader", "OnlyTheIdentityHostsWrite", "OnlyTheIdentityHostsRead", "OnlyTheOperatorDeletes"]:
    assert sids[need]["Effect"] == "Deny", need
assert sids["OnlyTheOperatorDeletes"]["Condition"]["ArnNotLike"]["aws:PrincipalArn"] == ["arn:aws:iam::243891825325:role/ops", "arn:aws:iam::243891825325:role/*/ops"]
assert sids["OnlyTheIdentityHostsRead"]["Condition"]["ArnNotEquals"]["aws:PrincipalArn"] == "arn:aws:iam::243891825325:role/superci-pull-identity"
EOF
check $? "the role may only use its prefix, the shim, the one key, put metrics in the superci-pull namespace and mark an identity host's own group unhealthy; the bucket denies unencrypted puts and others' reads, writes and deletes"
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
python3 -I -c 'import json,sys; d = json.loads(json.load(open(sys.argv[1]))["versions"][0]["data"]); assert d["InstanceType"] == "r7i.2xlarge", d' "$FAKE/state/lt/superci-pull-identity-us-east-1c"
check $? "the flag sets the template's instance type"
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
# More than one SSH call's worth (50 a call): ssh takes the stdin of the loop that reads the chunks, so a relabel that does
# not keep it off ssh moves the first 50 and says "relabelled all" (Nov 28: 50 of 410).
python3 -I -c 'import json,sys
p = sys.argv[1] + "/github.json"
r = json.load(open(p))
r += [{"id": 7000 + n, "name": "superci-id-8cpu-%d" % n, "status": "online", "busy": False, "labels": ["self-hosted", "superci-park"]} for n in range(1, 121)]
json.dump(r, open(p, "w"))' "$FAKE"
flog
ok "relabel moves every chunk of 50, not the first alone" "$ID" relabel 8cpu superci-park superci-8cpu
n8=$(count '^labels id=7[01][0-9][0-9] labels=superci-8cpu$' "$FAKE/curl.log"); [ "$n8" = 120 ]; check $? "120 relabels for the 120 parked identities (made $n8)"
python3 -I -c 'import json,sys
r = [x for x in json.load(open(sys.argv[1] + "/github.json")) if 7000 < x["id"] <= 7120]
left = [x["name"] for x in r if "superci-8cpu" not in x["labels"] or "superci-park" in x["labels"]]
assert len(r) == 120 and not left, (len(r), left[:3])' "$FAKE"
check $? "and all 120 carry superci-8cpu and not the park label"
grep -q 'relabelled all 122 of 122' <<< "$out"; check $? "and it says how many it moved of how many (the 120 and the fixture's 2 parked)"
# GitHub keeps one of them under the old label though the listener answered 204: the relabel says so and fails
python3 -I -c 'import json,sys
p = sys.argv[1] + "/github.json"
r = json.load(open(p))
for x in r:
    if 7000 < x["id"] <= 7120:
        x["labels"] = ["self-hosted", "superci-8cpu"]
json.dump(r, open(p, "w"))' "$FAKE"
flog; printf '7005\n' > "$FAKE/labels-sticky"
refuses "a relabel GitHub did not keep is reported with the real count, never as all" "relabelled 122 of 122, and GitHub still lists 1 of superci-id-8cpu-* under superci-8cpu" "$ID" relabel 8cpu superci-8cpu superci-park
rm -f "$FAKE/labels-sticky"
python3 -I -c 'import json,sys
p = sys.argv[1] + "/github.json"
json.dump([x for x in json.load(open(p)) if not 7000 < x["id"] <= 7120], open(p, "w"))' "$FAKE"
# labels: the idle identities a label, from GitHub's list through the listener
python3 -I -c 'import json,sys
p = sys.argv[1] + "/github.json"
r = json.load(open(p))
r += [{"id": 7000 + n, "name": "superci-id-8cpu-%d" % n, "status": "offline" if n == 5 else "online", "busy": n == 4, "labels": ["self-hosted", "superci-8cpu"]} for n in range(1, 6)]
r += [{"id": 7100, "name": "superci-pull-jit-0a1b", "status": "online", "busy": False, "labels": ["self-hosted", "superci-8cpu"]}]
json.dump(r, open(p, "w"))' "$FAKE"
flog
ok "labels counts the idle identities under each label" "$ID" labels
want=$(python3 -I -c 'import collections,json,sys
r = json.load(open(sys.argv[1] + "/github.json"))
n = collections.Counter(l for x in r if x["name"].startswith("superci-id-") and x["status"] == "online" and not x["busy"] for l in x["labels"] if l.startswith("superci-"))
print("\n".join("%s %d" % kv for kv in sorted(n.items())))' "$FAKE")
n8=$(python3 -I -c 'import json,sys
print(sum(1 for x in json.load(open(sys.argv[1] + "/github.json")) if x["name"].startswith("superci-id-8cpu-") and x["status"] == "online" and not x["busy"] and "superci-8cpu" in x["labels"]))' "$FAKE")
[ "$out" = "$want" ] && grep -qx "superci-8cpu $n8" <<< "$out" && [ "$n8" -ge 3 ]; check $? "one line a label: superci-8cpu's idle identities (not the busy, the offline or the runner that is not an identity), nothing else printed"
[ ! -s "$FAKE/config.log" ] && [ "$(count '^labels' "$FAKE/curl.log")" = 0 ]; check $? "and it registers and relabels nothing"
python3 -I -c 'import json,sys
p = sys.argv[1] + "/github.json"
json.dump([x for x in json.load(open(p)) if not 7000 < x["id"] <= 7100], open(p, "w"))' "$FAKE"
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
flog
refuses "relabel refuses another size's label as its target (8cpu jobs would reach 4cpu machines)" "label is superci-park or superci-4cpu, not superci-8cpu" "$ID" relabel 4cpu superci-park superci-8cpu
refuses "relabel refuses another size's label as its source" "label is superci-park or superci-4cpu, not superci-8cpu" "$ID" relabel 4cpu superci-8cpu superci-park
refuses "relabel refuses a well-formed label that is neither the park label nor the size's" "label is superci-park or superci-4cpu, not custom" "$ID" relabel 4cpu superci-park custom
[ ! -s "$FAKE/curl.log" ] && [ ! -s "$FAKE/ssh.log" ]; check $? "and each refusal comes before any call to GitHub or the listener"

# ---- status ----
H=$FAKE/hosts/$ip_a; mkdir -p "$H/proc/101" "$H/proc/102"; printf '101\n102\n' > "$H/proc/pids"
ln -sf "$H/opt/id/superci-id-4cpu-0002" "$H/proc/101/cwd"; printf 'Rss: 9 kB\nPrivate_Dirty:     36864 kB\n' > "$H/proc/101/smaps_rollup"
ln -sf "$H/opt/id/superci-id-4cpu-0004" "$H/proc/102/cwd"; printf 'Rss: 9 kB\nPrivate_Dirty:     70000 kB\n' > "$H/proc/102/smaps_rollup"
# Only the listener of 0002 holds a session with GitHub's broker (an ESTABLISHED socket to port 443): 0004's listener runs without one.
printf 'ESTAB 0 0 10.0.0.5:40000 140.82.112.5:443 users:(("Runner.Listener",pid=101,fd=21))\n' > "$H/proc/ss"
ok "status" "$ID" status
grep -q "identity groups:" <<< "$out" && grep -q "superci-pull-identity-us-east-1a: min 1 max 1 desired 1: i-0001 InService Healthy" <<< "$out" && grep -q "superci-pull-identity-us-east-1b: min 1 max 1 desired 1: i-0002 InService Healthy" <<< "$out"; check $? "status shows each group, its size and its host's state"
! grep -q "is in no group" <<< "$out"; check $? "and no host outside a group"
grep -q "identity hosts: 3" <<< "$out" && grep -q "az us-east-1a identities [0-9]* listeners_running [0-9]* private_dirty_kib_avg 53432 max 70000 over_64mib 1" <<< "$out"; check $? "counts hosts, measures each listener's private memory and counts those over 64 MiB"
grep -q "at GitHub:" <<< "$out" && grep -q "memory they need on 3 hosts" <<< "$out" && grep -q "64 MiB private (its unit restarts it at MemoryMax 192M) + 60 MiB shared a host; 1,550 identities are about 97 GiB" <<< "$out"; check $? "and shows what GitHub holds and the memory budget"

grep -q "REST online" <<< "$out" && grep -q "label superci-4cpu: REST online [0-9]*, polling 1 (hosts: .*us-east-1a 1" <<< "$out"; check $? "status prints GitHub's online count and the hosts' polling count per label: one of the two listeners on host a holds a broker session"
grep -q "unreliable for broker sessions" <<< "$out"; check $? "and says GitHub's online status is not to be trusted for a session"
# A host that cannot count (here: ss lists a socket to port 443 and no process, as it does for a caller that is not root) is "polling unknown", never 0.
printf 'ESTAB 0 0 10.0.0.5:40000 140.82.112.5:443\n' > "$H/proc/ss"
ok "status with a host that cannot count its sessions" "$ID" status
grep -q "^i-0001 .* polling unknown$" <<< "$out"; check $? "prints polling unknown for that host's line"
grep -q "label superci-4cpu: REST online [0-9]*, polling unknown (hosts: .*us-east-1a unknown" <<< "$out" && ! grep -q "label superci-4cpu: .*polling [0-9]" <<< "$out"; check $? "and for the label it holds, never a count that has a 0 for the host in it"
rm -f "$H/proc/ss"

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
has "ExecStart=/usr/local/bin/superci-id health" "$N/units/superci-id-health.service" "every host runs its health check from a service"
has "OnUnitActiveSec=30s" "$N/units/superci-id-health.timer" "every 30 seconds"
tmo=$(sed -n 's/^TimeoutStartSec=//p' "$N/units/superci-id-health.service")
[ -n "$tmo" ] && [ "$tmo" -ge $((1550 * 213 / 295 * 2)) ]; check $? "a health run may last twice the restore of 1,550 identities at the measured 213 s for 295 (its revive reruns boot inside it; it is ${tmo:-unset} s)"
grep -q 'systemctl enable --now superci-id-health.timer' "$FAKE/systemctl.log"; check $? "and the timer is enabled at boot"
! grep -q '^KillMode=' "$U"; check $? "and keeps systemd's default KillMode, so a stop takes the listener and its shim down together"
printf 'tampered\n' >> "$REL/bin/superci-shim"; rm -rf "$N/id" "$N/enabled" "$N/ready" "$N/shim"
refuses "a replacement host refuses a shim that is not the release's" "refusing s3://superci-pull-$ACCOUNT/bin/superci-shim" newhost boot
{ [ ! -e "$N/id" ] || [ -z "$(ls "$N/id")" ]; } && [ ! -e "$N/enabled" ]; check $? "and starts nothing"

# ---- polling: which listeners hold a session with GitHub's broker ----
# A listener takes a job only through the session it holds with GitHub's broker, an ESTABLISHED socket to port 443; GitHub's
# REST status of a runner said "offline" for hundreds of listeners that held one (Nov 28). The host counts the sessions per size.
PH=$T/pollhost; mkdir -p "$PH/id" "$PH/proc" "$PH/log"
for n in superci-id-4cpu-0002 superci-id-4cpu-0004 superci-id-4cpu-0006 superci-id-4cpu-0008 superci-id-4cpu-0010 superci-id-8cpu-0001 superci-id-8cpu-0003 superci-id-16cpu-0001; do
  mkdir -p "$PH/id/$n"; : > "$PH/id/$n/.runner"
done
mkdir -p "$PH/id/not-an-identity" "$PH/id/superci-id-2cpu-0001"; : > "$PH/id/not-an-identity/.runner" # (no .runner at all for 2cpu-0001: not held)
lst() { # <pid> <identity dir>: a Runner.Listener of that identity
  mkdir -p "$PH/proc/$1"; ln -sfn "$PH/id/$2" "$PH/proc/$1/cwd"
}
lst 201 superci-id-4cpu-0002; lst 202 superci-id-4cpu-0004; lst 203 superci-id-4cpu-0006; lst 204 superci-id-4cpu-0010
lst 205 superci-id-8cpu-0001; lst 206 superci-id-8cpu-0003; lst 207 superci-id-2cpu-0001; lst 208 not-an-identity
lst 209 superci-id-4cpu-0008 # the process that holds the socket of 0008 is not a listener (aws, run from its directory)
cat > "$PH/proc/ss" <<'SS'
ESTAB 0 0 10.0.0.5:40001 140.82.112.5:443 users:(("Runner.Listener",pid=201,fd=21))
SYN-SENT 0 1 10.0.0.5:40002 140.82.112.5:443 users:(("Runner.Listener",pid=202,fd=21))
TIME-WAIT 0 0 10.0.0.5:40003 140.82.112.5:443
ESTAB 0 0 10.0.0.5:40004 169.254.169.254:80 users:(("Runner.Listener",pid=203,fd=21))
ESTAB 0 0 10.0.0.5:40005 140.82.112.6:443 users:(("Runner.Listener",pid=204,fd=21))
ESTAB 0 0 10.0.0.5:40006 140.82.112.7:443 users:(("Runner.Listener",pid=204,fd=22))
ESTAB 0 0 10.0.0.5:40007 140.82.112.8:443 users:(("Runner.Listener",pid=205,fd=21),("dotnet",pid=205,fd=21))
CLOSE-WAIT 0 0 10.0.0.5:40008 140.82.112.9:443 users:(("Runner.Listener",pid=206,fd=21))
ESTAB 0 0 10.0.0.5:40009 140.82.112.9:443 users:(("Runner.Listener",pid=207,fd=21))
ESTAB 0 0 10.0.0.5:40010 140.82.112.9:443 users:(("Runner.Listener",pid=208,fd=21))
ESTAB 0 0 10.0.0.5:40011 140.82.112.9:443 users:(("aws",pid=209,fd=5))
ESTAB 0 0 10.0.0.5:40012 140.82.112.9:443 users:(("Runner.Listener",pid=299,fd=21))
ESTAB 0 0 127.0.0.1:40013 127.0.0.1:443 users:(("Runner.Listener",pid=206,fd=22))
SS
pollenv=(ID_ROOT=$PH/id PROC_ROOT=$PH/proc ID_CONF=$N/conf ID_LOG=$PH/log ID_LOCK=$PH/lock ID_AZ=us-east-1a PATH=$F:$PATH)
polled() { env "${pollenv[@]}" bash "$HS" polling; }
ok "polling counts" polled
want=$(printf 'superci-16cpu 0\nsuperci-4cpu 2\nsuperci-8cpu 1\n')
[ "$out" = "$want" ]; check $? "the listeners that hold an established socket to port 443, per size label: 2 of the 5 of 4cpu, 1 of the 2 of 8cpu, 0 of 16cpu (not a connecting or closing socket, a socket to another port or to loopback, another process, a listener of no held identity, or the same listener twice)"
[ "$(env "${pollenv[@]}" bash "$HS" status | grep -c '^polling superci-16cpu 0 superci-4cpu 2 superci-8cpu 1$')" = 1 ]; check $? "and status says so, for identity.sh status to read"
rm -f "$FAKE/metrics.log"
printf '10.00 0.00\n' > "$T/uptime"
pubhealth() { env "${pollenv[@]}" FAKE=$FAKE HEALTH_UPTIME=$T/uptime HEALTH_DIR=$PH/hd bash "$HS" health; }
ok "a health run publishes them" pubhealth
python3 -I - "$FAKE/metrics.log" << 'PY'
import json, sys
calls = [json.loads(l) for l in open(sys.argv[1])]
assert [c["namespace"] for c in calls] == ["superci-pull"], calls
rows = calls[0]["data"]
got = {}
for r in rows:
    assert r["MetricName"] == "IdentitiesPolling" and r["Unit"] == "Count", r
    assert [d["Name"] for d in r["Dimensions"]] == ["Set", "AZ"], r
    got[r["Dimensions"][0]["Value"]] = (r["Dimensions"][1]["Value"], r["Value"])
assert got == {"superci-4cpu": ("us-east-1a", 2), "superci-8cpu": ("us-east-1a", 1), "superci-16cpu": ("us-east-1a", 0)}, got
PY
check $? "as IdentitiesPolling of namespace superci-pull, one value for each size the host holds (0 too, so a host that lost its sessions says so), with the Set and its AZ"
hasnt TOKEN "$FAKE/metrics.log" "and nothing secret is in them"
: > "$PH/proc/ss"; rm -f "$FAKE/metrics.log"; pubhealth > /dev/null
python3 -I -c 'import json,sys; c = [json.loads(l) for l in open(sys.argv[1])]; assert sorted(r["Value"] for r in c[0]["data"]) == [0, 0, 0], c' "$FAKE/metrics.log"
check $? "a host none of whose listeners has a session publishes 0 for each size, not nothing"
rm -f "$FAKE/metrics.log"; printf 'ESTAB 0 0 10.0.0.5:40001 140.82.112.5:443 users:(("Runner.Listener",pid=201,fd=21))\n' > "$PH/proc/ss"
out=$(env FAKE_METRICS_FAIL=1 "${pollenv[@]}" FAKE=$FAKE HEALTH_UPTIME=$T/uptime HEALTH_DIR=$PH/hd bash "$HS" health 2>&1); rc=$?
[ $rc = 0 ] && grep -q "IdentitiesPolling was not published" <<< "$out"; check $? "a refused publish does not fail the health run, which says so"
printf '#!/bin/sh\nexit 1\n' > "$PH/ss-bad"; mkdir -p "$PH/bin"; cp "$PH/ss-bad" "$PH/bin/ss"; chmod 755 "$PH/bin/ss"
out=$(env "${pollenv[@]}" PATH=$PH/bin:$PATH bash "$HS" polling 2>&1); rc=$?
[ $rc != 0 ] && grep -q "cannot list sockets" <<< "$out"; check $? "and an ss that fails is no count of 0 (it would read as every session lost)"
# ss shows another user's process only to root: established sockets to port 443 and no users: column anywhere is a caller that cannot see, not a host with no sessions.
printf 'ESTAB 0 0 10.0.0.5:40001 140.82.112.5:443\nESTAB 0 0 10.0.0.5:40002 140.82.112.6:443\n' > "$PH/proc/ss"
out=$(polled 2>&1); rc=$?
[ $rc = 1 ] && grep -q "no process is shown" <<< "$out" && ! grep -q '^superci-[0-9]*cpu ' <<< "$out"; check $? "established sockets to port 443 with no users: column on any line make polling fail (rc 1, no counts), not count 0"
rm -f "$FAKE/metrics.log"; out=$(pubhealth 2>&1); rc=$?
[ $rc = 0 ] && grep -q "IdentitiesPolling was not published" <<< "$out" && [ ! -s "$FAKE/metrics.log" ]; check $? "and the health run publishes nothing"
[ "$(env "${pollenv[@]}" bash "$HS" status | grep -c '^polling unknown$')" = 1 ]; check $? "and status says polling unknown"
printf 'ESTAB 0 0 10.0.0.5:22 10.0.0.9:51000\nESTAB 0 0 127.0.0.1:40003 127.0.0.1:443\n' > "$PH/proc/ss"
out=$(polled 2>&1); [ $? = 0 ] && grep -q '^superci-4cpu 0$' <<< "$out"; check $? "sockets that are not to a remote port 443 need no users: column: a count of 0 stands"
printf 'ESTAB 0 0 10.0.0.5:40001 140.82.112.5:443 users:(("Runner.Listener",pid=201,fd=21))\nESTAB 0 0 10.0.0.5:40002 140.82.112.6:443\n' > "$PH/proc/ss"
out=$(polled 2>&1); [ $? = 0 ] && grep -q '^superci-4cpu 1$' <<< "$out"; check $? "and one line with its process among others without counts: it is root that sees them"

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

# ---- the groups: each in a world of its own (its own AWS, hosts and release; the stubs keep $FAKE) ----
# A group is reconciled by the stub as EC2 does it (testdata/identity/aws), and state/two-live is written the moment two
# live instances carry one Name: for an identity host, two hosts on one share, two listeners on one identity.
release again
world() { # <dir>: a fresh AWS with the release and the listener's key
  mkdir -p "$1/state" "$1/s3"; cp -r "$REL" "$1/s3/"; cp "$FAKE/tls.json" "$1/"
  : > "$1/calls.log"; : > "$1/ssh.log"; : > "$1/curl.log"; : > "$1/config.log"; : > "$1/sleep.log"
}
wlog() { for f in calls ssh curl config sleep; do : > "$1/$f.log"; done; }
share() { # <world> <az> <name>...: identities stored in the world's bucket
  local w=$1 az=$2 n f; shift 2
  for n in "$@"; do mkdir -p "$w/s3/superci-pull-identities-$ACCOUNT/superci-pull/hosts/$az/$n"
    for f in .runner .credentials .credentials_rsaparams; do echo "$n $f" > "$w/s3/superci-pull-identities-$ACCOUNT/superci-pull/hosts/$az/$n/$f"; done; done
}
plainhost() { # <world> <az>: a host as identity.sh launched it before the groups, by hand, in no group; prints its id
  env FAKE=$1 aws ec2 run-instances --image-id ami-1 --instance-type r7i.4xlarge --subnet-id "subnet-${2: -1}" --security-group-ids sg-identity --associate-public-ip-address \
    --iam-instance-profile Name=superci-pull-identity --metadata-options HttpTokens=required --user-data plain \
    --tag-specifications "ResourceType=instance,Tags=[{Key=purpose,Value=superci-pull},{Key=superci-pull-role,Value=identity},{Key=superci-pull-deployment,Value=superci-pull},{Key=Name,Value=superci-pull-identity-$2}]" --query 'Instances[0].InstanceId' --output text
}
seedhost() { # <world> <ip> <name>...: the identities a healthy plain host holds and runs
  local w=$1 ip=$2 n; shift 2
  mkdir -p "$w/hosts/$ip/opt/id"; : > "$w/hosts/$ip/enabled"
  for n in "$@"; do mkdir -p "$w/hosts/$ip/opt/id/$n"; : > "$w/hosts/$ip/opt/id/$n/.runner"; echo "$n" >> "$w/hosts/$ip/enabled"; done
}
gjson() { python3 -I -c 'import json,sys; g = json.load(open(sys.argv[1] + "/state/asg/" + sys.argv[2])); print(g["min"], g["max"], g["desired"], *g["instances"])' "$1" "$2"; } # "<min> <max> <desired> <ids>"
istate() { python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1] + "/state/instances/" + sys.argv[2]))["state"])' "$1" "$2"; }
ninst() { ls "$1/state/instances" 2> /dev/null | wc -l; }
line() { grep -n -- "$2" "$1" | head -1 | cut -d: -f1; } # the first line of a log with it
GA=superci-pull-identity-us-east-1a

# ---- adopting a plain host: it is attached, and a second host never exists ----
M=$T/fake-adopt; world "$M"; share "$M" us-east-1a superci-id-4cpu-0002 superci-id-4cpu-0004 superci-id-4cpu-0006
[ "$(plainhost "$M" us-east-1a)" = i-0001 ]; check $? "(a plain host of us-east-1a, as before the groups)"
seedhost "$M" 192.0.2.11 superci-id-4cpu-0002 superci-id-4cpu-0004 superci-id-4cpu-0006
wlog "$M"
ok "status names a host that is in no group" env FAKE=$M "$ID" status
grep -q "i-0001 is in no group: nothing replaces it if it is lost; up us-east-1a adopts it" <<< "$out" && grep -q "none: every host is plain" <<< "$out"; check $? "and says nothing replaces it"
wlog "$M"
ok "up adopts the plain host into a new group" env FAKE=$M "$ID" up us-east-1a
grep -q "adopted the plain host i-0001 into $GA: no second host" <<< "$out"; check $? "and says so"
mc=$M/calls.log
[ "$(count 'run-instances\|terminate-instances' "$mc")" = 0 ] && [ "$(ninst "$M")" = 1 ] && [ "$(istate "$M" i-0001)" = running ]; check $? "no instance is launched or ended: the plain host is the group's instance, still running"
[ ! -e "$M/state/two-live" ]; check $? "and never are two hosts live on one name"
[ "$(gjson "$M" $GA)" = "1 1 1 i-0001" ]; check $? "the group holds exactly that host (min 1, max 1, desired 1)"
c1=$(line "$mc" 'create-auto-scaling-group'); c2=$(line "$mc" 'attach-instances'); c3=$(line "$mc" 'update-auto-scaling-group.*--min-size 1')
sed -n "${c1}p" "$mc" | grep -q -- '--min-size 0 --max-size 1 --desired-capacity 0' && [ "$c1" -lt "$c2" ] && [ "$c2" -lt "$c3" ]
check $? "in that order: an empty group (nothing launches), the host attached, then min 1"
grep -q 'superci-id install-health' "$M/ssh.log" && [ -s "$M/hosts/192.0.2.11/units/superci-id-health.timer" ]; check $? "the host gets this checkout's script and its health timer"
python3 -I -c 'import json,sys; t = json.load(open(sys.argv[1])); assert [v["n"] for v in t["versions"]] == [1], t' "$M/state/lt/$GA"; check $? "and the group has its template, for the host's next replacement"
# the stub's detector is no tautology: a second group on the same template launches a second host on the name
env FAKE=$M aws autoscaling create-auto-scaling-group --auto-scaling-group-name rogue --launch-template 'LaunchTemplateId=lt-0001,Version=$Default' --min-size 1 --max-size 1 --desired-capacity 1 --vpc-zone-identifier subnet-a
grep -q "launched while i-0001 runs" "$M/state/two-live" 2> /dev/null; check $? "(the stub sees a second live host on the name when one is launched beside the first)"
env FAKE=$M aws autoscaling delete-auto-scaling-group --auto-scaling-group-name rogue --force-delete; rm -f "$M/state/two-live"
[ "$(plainhost "$M" us-east-1a)" = i-0003 ]; wlog "$M"
refuses "up refuses while a group's host and a plain one both exist" "both would run the share of us-east-1a" env FAKE=$M "$ID" up us-east-1a
[ "$(count 'terminate-instances\|attach-instances\|update-auto-scaling-group\|create-auto-scaling-group' "$mc")" = 0 ]; check $? "and changes nothing"
ok "down ends the group's host and the plain one, and deletes the group" env FAKE=$M "$ID" down us-east-1a
[ ! -e "$M/state/asg/$GA" ] && [ "$(istate "$M" i-0001)" = terminated ] && [ "$(istate "$M" i-0003)" = terminated ]; check $? "so none is left"

# an adoption that is refused: the plain host runs alone and the group holds nothing
M2=$T/fake-adopt-refused; world "$M2"; share "$M2" us-east-1a superci-id-4cpu-0002
plainhost "$M2" us-east-1a > /dev/null; seedhost "$M2" 192.0.2.11 superci-id-4cpu-0002; touch "$M2/attach-fail"; wlog "$M2"
refuses "an attach that is refused says what stands" "was not attached: it still runs alone and $GA holds nothing" env FAKE=$M2 "$ID" up us-east-1a
[ "$(gjson "$M2" $GA)" = "0 1 0" ] && [ "$(ninst "$M2")" = 1 ] && [ "$(istate "$M2" i-0001)" = running ] && [ ! -e "$M2/state/two-live" ]; check $? "the group is empty (desired 0), the plain host runs and no second one was launched"
rm -f "$M2/attach-fail"
ok "up again resumes the adoption" env FAKE=$M2 "$ID" up us-east-1a
[ "$(gjson "$M2" $GA)" = "1 1 1 i-0001" ] && [ "$(ninst "$M2")" = 1 ] && [ ! -e "$M2/state/two-live" ]; check $? "the host is attached, and still no second host"

# a plain host that is not healthy is not adopted: the health timer it would be given would revive, then replace, a live host
MU=$T/fake-adopt-sick; world "$MU"; share "$MU" us-east-1a superci-id-4cpu-0002 superci-id-4cpu-0004 superci-id-4cpu-0006
plainhost "$MU" us-east-1a > /dev/null; seedhost "$MU" 192.0.2.11 superci-id-4cpu-0002 superci-id-4cpu-0004 superci-id-4cpu-0006; sed -i '$d' "$MU/hosts/192.0.2.11/enabled"; wlog "$MU"
refuses "up refuses to adopt a plain host that runs 2 of the 3 listeners of its share" "refusing to adopt the plain host i-0001: not healthy: 2 of 3 listeners are running" env FAKE=$MU "$ID" up us-east-1a
[ "$(count 'create-auto-scaling-group\|attach-instances\|update-auto-scaling-group\|terminate-instances' "$MU/calls.log")" = 0 ] && [ ! -e "$MU/state/asg/$GA" ] && [ "$(ninst "$MU")" = 1 ] && [ "$(istate "$MU" i-0001)" = running ]; check $? "and changes nothing: no group, no attach, the host runs on"
rm -rf "$MU/hosts/192.0.2.11/opt/id/superci-id-4cpu-0006"; seedhost "$MU" 192.0.2.11 superci-id-4cpu-0002 superci-id-4cpu-0004
refuses "nor one that has not restored an identity of its share" "not healthy: 1 of 3 stored identities are not restored on this host" env FAKE=$MU "$ID" up us-east-1a
seedhost "$MU" 192.0.2.11 superci-id-4cpu-0002 superci-id-4cpu-0004 superci-id-4cpu-0006
ok "up adopts it once every listener of its share runs" env FAKE=$MU "$ID" up us-east-1a
[ "$(gjson "$MU" $GA)" = "1 1 1 i-0001" ] && [ "$(ninst "$MU")" = 1 ]; check $? "(the one host, attached)"

# --replace: the plain host is gone before the group launches its own
M3=$T/fake-replace; world "$M3"; share "$M3" us-east-1a superci-id-4cpu-0002
plainhost "$M3" us-east-1a > /dev/null; wlog "$M3"
ok "up --replace ends the plain host, then the group launches its own" env FAKE=$M3 "$ID" up --replace us-east-1a
mc=$M3/calls.log
t1=$(line "$mc" 'terminate-instances'); t2=$(line "$mc" 'wait instance-terminated'); t3=$(line "$mc" 'create-auto-scaling-group')
[ -n "$t1" ] && [ "$t1" -lt "$t2" ] && [ "$t2" -lt "$t3" ]; check $? "terminated and waited for before the group is made"
[ ! -e "$M3/state/two-live" ] && [ "$(istate "$M3" i-0001)" = terminated ] && [ "$(gjson "$M3" $GA)" = "1 1 1 i-0002" ]; check $? "so the group's host never ran beside the old one"

# ---- a host that is lost is replaced, and restores its share ----
HW=$T/fake-heal; world "$HW"; share "$HW" us-east-1a superci-id-4cpu-0002 superci-id-4cpu-0004 superci-id-4cpu-0006 superci-id-4cpu-0008; share "$HW" us-east-1b superci-id-4cpu-0001 superci-id-4cpu-0003
wlog "$HW"
ok "up on an empty AZ makes the group, whose host is ready" env FAKE=$HW "$ID" up us-east-1a
[ "$(gjson "$HW" $GA)" = "1 1 1 i-0001" ] && [ "$(count 'run-instances' "$HW/calls.log")" = 0 ]; check $? "the group launched it"
env FAKE=$HW aws ec2 terminate-instances --instance-ids i-0001 # EC2 lost it, with no person involved
env FAKE=$HW aws autoscaling describe-auto-scaling-groups --auto-scaling-group-names $GA --output json | grep -q '"InstanceId": "i-0002"'; check $? "the group, asked after the loss, holds a new host"
[ "$(gjson "$HW" $GA)" = "1 1 1 i-0002" ] && [ "$(istate "$HW" i-0001)" = terminated ] && [ "$(istate "$HW" i-0002)" = running ] && [ ! -e "$HW/state/two-live" ]; check $? "(one host, the lost one gone, never two on the name)"
cmp -s "$HW/state/userdata-0001" "$HW/state/userdata-0002"; check $? "launched from the same template: the same user data"
python3 -I -c 'import json,sys; a, b = (json.load(open(sys.argv[1] + "/state/instances/" + i)) for i in ("i-0001", "i-0002")); assert a["tags"] == b["tags"] and b["tags"]["Name"] == "superci-pull-identity-us-east-1a" and b["tags"]["superci-pull-role"] == "identity", (a, b)' "$HW"
check $? "with the same tags, which the dispatcher and identity.sh find a host by"
# the replacement boots from that user data (never run as written: its script and conf, in a root of this test's own)
X=$T/healed; mkdir -p "$X/units"
python3 -I -c 'import re,sys,base64,gzip; t = open(sys.argv[1]).read(); open(sys.argv[2], "wb").write(gzip.decompress(base64.b64decode(re.search(r"echo .([A-Za-z0-9+/=]+). \| base64 -d \| gunzip", t).group(1)))); open(sys.argv[3], "w").write(re.search(r"<< .CONF.\n(.*?)\nCONF\n", t, re.S).group(1) + "\n")' "$HW/state/userdata-0002" "$X/superci-id" "$X/conf"
cmp -s "$X/superci-id" "$here/identity-host.sh"; check $? "the user data carries this checkout's host script"
env FAKE=$HW ID_ROOT=$X/id RUNNER_SRC=$FAKE_RUNNER SHIM_BIN=$X/shim SHIM_ENV=$X/shim.env ID_CONF=$X/conf UNIT_DIR=$X/units ID_LOG=$X/log ID_AZ=us-east-1a ID_LOCK=$X/lock READY_FILE=$X/ready FAKE_UNITS=$X/enabled \
  bash "$X/superci-id" boot > "$X/boot.out" 2>&1; check $? "the replacement boots"
[ "$(ls "$X/id" | tr '\n' ' ')" = "superci-id-4cpu-0002 superci-id-4cpu-0004 superci-id-4cpu-0006 superci-id-4cpu-0008 " ] && [ "$(sort "$X/enabled" | tr '\n' ' ')" = "$(ls "$X/id" | tr '\n' ' ')" ] && [ -e "$X/ready" ]
check $? "and restores exactly its AZ's share, starts it and is ready, with no hand to run anything"
[ ! -s "$HW/config.log" ] && [ "$(count '^token' "$HW/curl.log")" = 0 ]; check $? "registering nothing"
ok "up after the heal finds the group whole" env FAKE=$HW "$ID" up us-east-1a
grep -q "already up" <<< "$out" && [ "$(count 'create-launch-template' "$HW/calls.log")" = 1 ]; check $? "and adds no template version"

# ---- the host's own health: listeners that do not run, a boot that did not finish ----
hip=192.0.2.12; HD=$HW/hosts/$hip; hid=i-0002
mkdir -p "$HD/opt/id"; for n in 0002 0004 0006 0008; do mkdir -p "$HD/opt/id/superci-id-4cpu-$n"; : > "$HD/opt/id/superci-id-4cpu-$n/.runner"; done
all4() { printf 'superci-id-4cpu-%s\n' 0002 0004 0006 0008 > "$HD/enabled"; }
hh() { # <uptime seconds> [state dir]: one health tick on the host $hid ($hip), as its timer runs it, over the ssh stub
  printf '%s.00 0.00\n' "$1" > "$T/uptime"
  env FAKE=$HW HEALTH_UPTIME=$T/uptime HEALTH_DIR=${2:-$T/hd} ID_INSTANCE=$hid "$F/ssh" -q -i k -o x "ubuntu@$hip" 'sudo superci-id health' 2>&1
}
marks() { count 'autoscaling set-instance-health' "$HW/calls.log"; }
starts() { count 'enable --now --no-block superci-id@' "$HW/systemctl.log"; }
: > "$HW/systemctl.log"; rm -f "$T/hd"
: > "$HD/enabled"; hh 500 > /dev/null
[ "$(marks)" = 0 ] && [ "$(starts)" = 0 ] && [ ! -e "$T/hd/first" ]; check $? "a host under 15 minutes old is not judged, whatever runs"
all4; hh 2000 > /dev/null; hh 2030 > /dev/null
[ "$(marks)" = 0 ] && [ "$(starts)" = 0 ] && [ ! -e "$T/hd/first" ]; check $? "a host whose listeners all run is not touched"
printf 'superci-id-4cpu-0002\n' > "$HD/enabled"
hh 2000 > /dev/null; hh 2030 > /dev/null
[ "$(starts)" = 0 ] && [ -e "$T/hd/first" ]; check $? "under half of them running for under 60 s: noted, nothing done"
out=$(hh 2060)
[ "$(starts)" = 4 ] && [ "$(sort -u "$HD/enabled" | wc -l)" = 4 ] && [ "$(marks)" = 0 ] && grep -q "reviving: 1 of 4 listeners are running" <<< "$out"; check $? "for 60 s: every listener is started again first (not the instance replaced)"
hh 2090 > /dev/null
[ ! -e "$T/hd/first" ] && [ ! -e "$T/hd/restarted" ] && [ "$(marks)" = 0 ]; check $? "and a host that is cured is not marked, and its episode ends"
touch "$HW/units-wont-start"; rm -rf "$T/hd"; printf 'superci-id-4cpu-0002\n' > "$HD/enabled"
for t in 2000 2060 2120; do hh $t > /dev/null; done
[ "$(marks)" = 0 ] && [ "$(starts)" = 8 ]; check $? "listeners that stay down 60 s after the revive are not yet cause to replace the host (it waits 120 s)"
[ "$(ninst "$HW")" = 2 ]; check $? "(nothing was launched meanwhile)"
out=$(hh 2180)
[ "$(marks)" = 1 ] && grep -q "asking the Auto Scaling group to replace i-0002" <<< "$out" && grep -q -- '--health-status Unhealthy --no-should-respect-grace-period' "$HW/calls.log"; check $? "120 s after the revive the host asks its group to replace it (Unhealthy, grace period not respected)"
[ "$(istate "$HW" i-0002)" = terminated ] && [ "$(gjson "$HW" $GA)" = "1 1 1 i-0003" ] && [ ! -e "$HW/state/two-live" ]; check $? "the group ends it before it launches the next, and the next is the group's one host"
[ ! -e "$HW/state/mark-with-listeners" ] && grep -qF "systemctl stop superci-id@*" "$HW/systemctl.log" && [ ! -s "$HD/enabled" ]; check $? "every listener was stopped before the mark: the host that goes holds no session while its replacement boots"
hh 2240 > /dev/null; [ "$(marks)" = 1 ]; check $? "and the host asks once"
# a host not yet 20 minutes old asks later: a dud image would be replaced in a loop otherwise
hip=192.0.2.13; hid=i-0003; HD=$HW/hosts/$hip; mkdir -p "$HD/opt/id" "$HD/bin"; cp "$here/identity-host.sh" "$HD/bin/superci-id"; for n in 0002 0004 0006 0008; do mkdir -p "$HD/opt/id/superci-id-4cpu-$n"; : > "$HD/opt/id/superci-id-4cpu-$n/.runner"; done
: > "$HD/enabled"; rm -rf "$T/hd2"
for t in 900 960 1100; do out=$(hh $t "$T/hd2"); done
[ "$(marks)" = 1 ] && grep -q "not asking to be replaced yet" <<< "$out"; check $? "a host younger than 20 minutes is not replaced, and says so"
out=$(hh 1200 "$T/hd2")
[ "$(marks)" = 2 ] && [ "$(gjson "$HW" $GA)" = "1 1 1 i-0004" ] && [ ! -e "$HW/state/two-live" ]; check $? "at 20 minutes it is, and again no second host on the name"
# boot did not finish: it is run again, and a host that does not finish is replaced
rm -f "$HW/units-wont-start"
hip=192.0.2.14; hid=i-0004; HD=$HW/hosts/$hip; mkdir -p "$HD/bin"; cp "$here/identity-host.sh" "$HD/bin/superci-id"; rm -f "$HD/run/superci-id-ready"; rm -rf "$T/hd3" "$HD/opt/id"
hh 2000 "$T/hd3" > /dev/null
out=$(hh 2060 "$T/hd3")
grep -q "reviving: boot did not finish" <<< "$out" && [ -e "$HD/run/superci-id-ready" ] && [ "$(ls "$HD/opt/id" | wc -l)" = 4 ]; check $? "a host that is not ready runs its boot again, and restores its share"
[ "$(marks)" = 2 ]; check $? "(without replacing the host)"
rm -f "$HD/run/superci-id-ready"; rm -rf "$T/hd4"
echo 0 > "$HD/run/superci-id-booting" # (the boot began at uptime 0: it is the boot's age that makes it stuck)
exec 7> "$HD/run/superci-id.lock"; flock 7
hh 2000 "$T/hd4" > /dev/null; [ ! -e "$T/hd4/first" ] && [ "$(marks)" = 2 ]; check $? "a boot that holds the host's lock is busy, not sick (for 40 minutes)"
hh 2400 "$T/hd4" > /dev/null; hh 2460 "$T/hd4" > /dev/null
[ "$(marks)" = 2 ]; check $? "past it, 120 s must pass before the host asks"
out=$(hh 2520 "$T/hd4")
[ "$(marks)" = 3 ] && grep -q "boot has held the lock for 2520 s" <<< "$out" && [ "$(gjson "$HW" $GA)" = "1 1 1 i-0005" ]; check $? "a boot that never ends is replaced"
[ ! -e "$HW/state/mark-with-listeners" ] && [ ! -s "$HD/enabled" ]; check $? "(its listeners stopped first)"
exec 7>&-
# a boot run again whose S3 sync fails: not ready, never healthy, then replaced
hip=192.0.2.15; hid=i-0005; HD=$HW/hosts/$hip; mkdir -p "$HD/bin"; cp "$here/identity-host.sh" "$HD/bin/superci-id"; rm -f "$HD/run/superci-id-ready"; rm -rf "$T/hd5"
touch "$HW/s3-sync-fail"
hh 2000 "$T/hd5" > /dev/null
out=$(hh 2060 "$T/hd5")
grep -q "reviving: boot did not finish" <<< "$out" && grep -q "boot failed again" <<< "$out" && [ ! -e "$HD/run/superci-id-ready" ]; check $? "a revive whose S3 sync fails again leaves the host not ready (a boot in an || context kept going, restored 0 and touched ready)"
[ "$(marks)" = 3 ]; check $? "(not replaced yet)"
hh 2120 "$T/hd5" > /dev/null; [ "$(marks)" = 3 ] && [ ! -e "$HD/run/superci-id-ready" ]; check $? "(nor 60 s on: the 120 s run from the end of the revive)"
out=$(hh 2180 "$T/hd5")
[ "$(marks)" = 4 ] && grep -q "asking the Auto Scaling group to replace i-0005" <<< "$out" && [ "$(gjson "$HW" $GA)" = "1 1 1 i-0006" ] && [ ! -e "$HW/state/two-live" ]; check $? "and the host that cannot restore its share is marked Unhealthy and replaced"
# a partial restore is not healthy: 3 of the 4 identities of the share restored and running is a failing host
rm -f "$HW/s3-sync-fail"
hip=192.0.2.16; hid=i-0006; HD=$HW/hosts/$hip; mkdir -p "$HD/bin"; cp "$here/identity-host.sh" "$HD/bin/superci-id"; rm -rf "$T/hd6"
seedhost "$HW" $hip superci-id-4cpu-0002 superci-id-4cpu-0004 superci-id-4cpu-0006
hh 2000 "$T/hd6" > /dev/null; hh 2030 "$T/hd6" > /dev/null
[ -e "$T/hd6/first" ] && [ "$(marks)" = 4 ]; check $? "3 of the 4 identities of the share restored and running is not healthy (at least half was enough before)"
out=$(hh 2060 "$T/hd6")
grep -q "reviving: 1 of 4 stored identities are not restored on this host" <<< "$out" && [ -s "$HD/opt/id/superci-id-4cpu-0008/.runner" ] && [ "$(sort -u "$HD/enabled" | wc -l)" = 4 ] && [ "$(marks)" = 4 ]; check $? "the revive runs boot, which restores the missing identity and starts it"
hh 2090 "$T/hd6" > /dev/null; [ ! -e "$T/hd6/first" ] && [ "$(marks)" = 4 ]; check $? "and the host is healthy then, not replaced"
# a boot an operator runs on a host that has been up for days (what up's refusal tells them to do) is busy, not stuck
hh 2000 "$T/hd6" > /dev/null # (the host i-0006, healthy)
seedhost "$HW" $hip superci-id-4cpu-0002 superci-id-4cpu-0004 superci-id-4cpu-0006 superci-id-4cpu-0008
rm -f "$HD/run/superci-id-ready"; echo 4700 > "$HD/run/superci-id-booting"; rm -rf "$T/hd7"
exec 7> "$HD/run/superci-id.lock"; flock 7
m0=$(marks); s0=$(count 'systemctl stop' "$HW/systemctl.log")
for t in 5000 5060 5120 5180 5240; do hh $t "$T/hd7" > /dev/null; done
[ "$(marks)" = "$m0" ] && [ "$(count 'systemctl stop' "$HW/systemctl.log")" = "$s0" ] && [ "$(sort -u "$HD/enabled" | wc -l)" = 4 ] && [ ! -e "$T/hd7/first" ]; check $? "a boot that has run 300 s on a host up for 5,000 s is not stuck: its listeners are not stopped and the host is not marked, however long the host has been up"
echo 2000 > "$HD/run/superci-id-booting"; rm -rf "$T/hd8"
hh 5000 "$T/hd8" > /dev/null; hh 5060 "$T/hd8" > /dev/null
[ "$(marks)" = "$m0" ]; check $? "a boot that has held the lock past the limit (3,000 s) is stuck, and waits its 120 s"
out=$(hh 5120 "$T/hd8")
[ "$(marks)" = $((m0 + 1)) ] && grep -q "boot has held the lock for 3120 s" <<< "$out" && [ ! -e "$HW/state/mark-with-listeners" ] && [ ! -s "$HD/enabled" ] && [ "$(gjson "$HW" $GA)" = "1 1 1 i-0007" ]; check $? "then its listeners stop and the group's host is marked and replaced"
exec 7>&-

# a plain host (in no group) is never stopped by its health check: nothing would replace it
MP=$T/fake-plain-health; world "$MP"; share "$MP" us-east-1a superci-id-4cpu-0002 superci-id-4cpu-0004 superci-id-4cpu-0006 superci-id-4cpu-0008
plainhost "$MP" us-east-1a > /dev/null; seedhost "$MP" 192.0.2.11 superci-id-4cpu-0002 superci-id-4cpu-0004 superci-id-4cpu-0006 superci-id-4cpu-0008
printf 'superci-id-4cpu-0002\n' > "$MP/hosts/192.0.2.11/enabled"; mkdir -p "$MP/hosts/192.0.2.11/bin"; cp "$here/identity-host.sh" "$MP/hosts/192.0.2.11/bin/superci-id"
touch "$MP/units-wont-start"; : > "$MP/systemctl.log"
hp() { printf '%s.00 0.00\n' "$1" > "$T/uptime"; env FAKE=$MP HEALTH_UPTIME=$T/uptime HEALTH_DIR=$T/hdp ID_INSTANCE=i-0001 "$F/ssh" -q -i k -o x ubuntu@192.0.2.11 'sudo superci-id health' 2>&1; }
for t in 2000 2060 2120; do hp $t > /dev/null; done
out=$(hp 2180); hp 2240 > /dev/null
grep -q "in no group of us-east-1a (None): nothing would replace it" <<< "$out" && [ "$(count 'stop superci-id@' "$MP/systemctl.log")" = 0 ] && [ "$(sort -u "$MP/hosts/192.0.2.11/enabled" | wc -l)" = 1 ] && [ "$(count 'set-instance-health' "$MP/calls.log")" = 0 ] && [ "$(istate "$MP" i-0001)" = running ]; check $? "a plain host that stays unhealthy keeps its listeners, is not marked, and the health check says why"
! grep -q "in no group" <<< "$(hp 2300)"; check $? "(and says it once an episode)"

# an AZ the host cannot read is no share: boot and check refuse it, and nothing starts
AZH=$T/azhost; mkdir -p "$AZH/units"; cp "$N/conf" "$AZH/conf"
azenv=(ID_ROOT=$AZH/id RUNNER_SRC=$FAKE_RUNNER SHIM_BIN=$AZH/shim SHIM_ENV=$AZH/shim.env ID_CONF=$AZH/conf UNIT_DIR=$AZH/units ID_LOG=$AZH/log ID_LOCK=$AZH/lock READY_FILE=$AZH/ready FAKE_UNITS=$AZH/enabled)
refuses "boot refuses a host whose AZ it cannot read (no instance metadata)" "no valid availability zone (got ''): refusing" env -u ID_AZ "${azenv[@]}" bash "$HS" boot
refuses "boot refuses a region's name for an AZ" "no valid availability zone (got 'us-east-1'): refusing" env ID_AZ=us-east-1 "${azenv[@]}" bash "$HS" boot
refuses "check refuses it too, rather than reading an empty share as healthy" "not healthy: no valid availability zone" env -u ID_AZ "${azenv[@]}" bash "$HS" check
refuses "and an AZ of another region" "no valid availability zone (got 'eu-west-1a')" env ID_AZ=eu-west-1a "${azenv[@]}" bash "$HS" boot
[ ! -e "$AZH/ready" ] && [ ! -e "$AZH/enabled" ] && [ ! -e "$AZH/id" -o -z "$(ls "$AZH/id" 2> /dev/null)" ]; check $? "and nothing was restored, started or marked ready"
# register and adopt refuse it before any registration or upload: an identity stored under hosts//<name> is restored by no host
# a held identity adopt would upload (all its files, non-empty), so "nothing uploaded" means the refusal, not a missing file
mkdir -p "$AZH/id/superci-id-4cpu-0002"; for f in .runner .credentials .credentials_rsaparams; do echo x > "$AZH/id/superci-id-4cpu-0002/$f"; done
cp_before=$(count "s3 cp" "$FAKE/calls.log")
refuses "register refuses a host whose AZ it cannot read, before reading a token or registering" "no valid availability zone (got ''): refusing" env -u ID_AZ "${azenv[@]}" bash "$HS" register superci-park https://github.com/o - superci-id-4cpu-0009 <<< "a-token"
refuses "adopt refuses it too, so no identity is stored under no share" "no valid availability zone (got ''): refusing" env -u ID_AZ "${azenv[@]}" bash "$HS" adopt superci-id-4cpu-0002
[ "$(count "s3 cp" "$FAKE/calls.log")" = "$cp_before" ] && [ ! -e "$AZH/id/superci-id-4cpu-0009" ]; check $? "and nothing was registered or uploaded to S3"
rm -rf "$AZH/id/superci-id-4cpu-0002"

# check fails closed: a host whose share cannot be listed is not adopted
MS=$T/fake-adopt-unlistable; world "$MS"; share "$MS" us-east-1a superci-id-4cpu-0002 superci-id-4cpu-0004
plainhost "$MS" us-east-1a > /dev/null; seedhost "$MS" 192.0.2.11 superci-id-4cpu-0002 superci-id-4cpu-0004; touch "$MS/s3-list-fail"; wlog "$MS"
refuses "up refuses to adopt a plain host whose share it cannot list in S3" "not healthy: the identities stored in S3 for us-east-1a cannot be listed, so the host cannot be verified" env FAKE=$MS "$ID" up us-east-1a
! grep -q "Nothing was changed" <<< "$out" && grep -q "now has this checkout's superci-id (pushed to read it); the group, the template and the instance are unchanged" <<< "$out"; check $? "and says what changed: the pushed script, nothing else"
[ "$(count 'create-auto-scaling-group\|attach-instances\|terminate-instances' "$MS/calls.log")" = 0 ] && [ ! -e "$MS/state/asg/$GA" ]; check $? "no group, no attach"
rm -f "$MS/s3-list-fail"
ok "up adopts it once S3 can be listed" env FAKE=$MS "$ID" up us-east-1a
[ "$(gjson "$MS" $GA)" = "1 1 1 i-0001" ]; check $? "(the one host, attached)"
grep -q 'autoscaling:SetInstanceHealth' "$FAKE/state/role-policy.json"; check $? "(the role may do it, on the identity groups)"

# ---- down: the group goes to 0 and is deleted, so nothing heals the host back ----
DW=$T/fake-down; world "$DW"; share "$DW" us-east-1a superci-id-4cpu-0002; share "$DW" us-east-1b superci-id-4cpu-0001
ok "up of two AZs" env FAKE=$DW "$ID" up us-east-1a; ok "up of the other" env FAKE=$DW "$ID" up us-east-1b
wlog "$DW"
ok "down of one AZ" env FAKE=$DW "$ID" down us-east-1a
dc=$DW/calls.log
d1=$(line "$dc" 'update-auto-scaling-group.*--desired-capacity 0'); d2=$(line "$dc" 'terminate-instances'); d3=$(line "$dc" 'delete-auto-scaling-group')
sed -n "${d1}p" "$dc" | grep -q -- '--min-size 0 --max-size 0 --desired-capacity 0' && [ "$d1" -lt "$d2" ] && [ "$d2" -lt "$d3" ]; check $? "the group is set to 0 first, then the host ended, then the group deleted"
[ ! -e "$DW/state/asg/$GA" ] && [ "$(istate "$DW" i-0001)" = terminated ]; check $? "the group is gone and so is its host"
env FAKE=$DW aws autoscaling describe-auto-scaling-groups --output json > /dev/null
[ "$(ninst "$DW")" = 2 ] && [ "$(istate "$DW" i-0002)" = running ] && [ "$(gjson "$DW" superci-pull-identity-us-east-1b)" = "1 1 1 i-0002" ]; check $? "and, with time passing, it is not healed back; the other AZ's host is as it was"
grep -q "deleted group $GA, so nothing heals it back" <<< "$out"; check $? "and says so"
[ -e "$DW/state/lt/$GA" ]; check $? "the template stays for the next up"
[ -s "$DW/s3/superci-pull-identities-$ACCOUNT/superci-pull/hosts/us-east-1a/superci-id-4cpu-0002/.runner" ]; check $? "and the identities stay in S3"
ok "down again" env FAKE=$DW "$ID" down us-east-1a
grep -q "no host named superci-pull-identity-us-east-1a" <<< "$out"; check $? "finds nothing to do"
wlog "$DW"
ok "up again restores the AZ with a new group from the same template" env FAKE=$DW "$ID" up us-east-1a
[ "$(gjson "$DW" $GA)" = "1 1 1 i-0003" ] && [ "$(count 'create-launch-template' "$dc")" = 0 ] && [ ! -e "$DW/state/two-live" ]; check $? "one host, and the template unchanged (no new version)"
# a group left at 0 (a down that stopped halfway) is set back to one by up
env FAKE=$DW aws autoscaling update-auto-scaling-group --auto-scaling-group-name $GA --min-size 0 --max-size 0 --desired-capacity 0
ok "up on a group left at 0" env FAKE=$DW "$ID" up us-east-1a
[ "$(gjson "$DW" $GA)" = "1 1 1 i-0004" ] && [ ! -e "$DW/state/two-live" ]; check $? "sets it to one and launches a host"

# ---- changed user data is a new template version; an unchanged one is not ----
VW=$T/fake-version; world "$VW"; share "$VW" us-east-1a superci-id-4cpu-0002
ok "up" env FAKE=$VW "$ID" up us-east-1a
D2=$T/deploy2; mkdir -p "$D2"; cp "$here"/identity.sh "$here"/identity-host.sh "$here"/release-lib.sh "$here"/ssh.sh "$here"/identity-*.json "$D2/"
printf '# a change of the host script, and so of the user data\n' >> "$D2/identity-host.sh"
wlog "$VW"
ok "up with changed user data" env FAKE=$VW timeout 120 "$D2/identity.sh" up us-east-1a
vc=$VW/calls.log
python3 -I -c 'import json,sys; t = json.load(open(sys.argv[1])); v = t["versions"]; assert [x["n"] for x in v] == [1, 2] and t["default"] == 2 and v[0]["desc"] != v[1]["desc"], t' "$VW/state/lt/$GA"
check $? "makes a second template version and its default"
[ "$(count 'create-launch-template-version' "$vc")" = 1 ] && [ "$(count 'modify-launch-template.*--default-version 2' "$vc")" = 1 ]; check $? "(one version, then the default set)"
[ "$(gjson "$VW" $GA)" = "1 1 1 i-0001" ] && [ "$(count 'terminate-instances\|run-instances' "$vc")" = 0 ] && grep -qF '"version": "$Default"' "$VW/state/asg/$GA"; check $? "the group follows the default, and the running host is left as it is"
wlog "$VW"
ok "up again with the same user data" env FAKE=$VW timeout 120 "$D2/identity.sh" up us-east-1a
[ "$(count 'create-launch-template-version\|modify-launch-template' "$vc")" = 0 ]; check $? "adds no version"
env FAKE=$VW aws ec2 terminate-instances --instance-ids i-0001; env FAKE=$VW aws autoscaling describe-auto-scaling-groups --output json > /dev/null
grep -q 'a change of the host script' "$VW/state/userdata-0002" || python3 -I -c 'import re,sys,base64,gzip; t = open(sys.argv[1]).read(); sys.exit(0 if b"a change of the host script" in gzip.decompress(base64.b64decode(re.search(r"echo .([A-Za-z0-9+/=]+). \| base64 -d \| gunzip", t).group(1))) else 1)' "$VW/state/userdata-0002"
check $? "and the next host the group launches takes the new user data"

# ---- the policies and down.sh ----
python3 -I - "$here/listener-policy.json" << 'PY'
import json, sys
st = {s["Sid"]: s for s in json.load(open(sys.argv[1]))["Statement"]}
deny = next(s for s in st.values() if s["Effect"] == "Deny" and "ec2:ModifyLaunchTemplate" in s["Action"])
assert set(deny["Condition"]["StringEquals"]["aws:ResourceTag/superci-pull-role"]) == {"listener", "identity"}, deny
PY
check $? "the listener's role may not launch from, change or delete an identity host's template (tagged superci-pull-role=identity), as its own"
python3 -I -c 'import json,sys; json.load(open(sys.argv[1]))' "$here/identity-policy.json"; check $? "(identity-policy.json is JSON)"
dg=$(grep -n 'delete-auto-scaling-group.*"\$g"' "$here/down.sh" | head -1 | cut -d: -f1); dt=$(grep -n 'terminate-instances' "$here/down.sh" | head -1 | cut -d: -f1)
[ -n "$dg" ] && [ "$dg" -lt "$dt" ]; check $? "deploy/down.sh deletes the identity groups before it terminates every instance, or they would be launched again"

# ---- the image is the one fleet.go runs ----
grep -qF "Owner: \"$(sed -n 's/^IMAGE_OWNER=//p' "$here/identity.sh")\", Name: \"$(sed -n "s/^IMAGE_NAME='\(.*\)'/\1/p" "$here/identity.sh")\", Pin: \"$(sed -n 's/^IMAGE_PIN=//p' "$here/identity.sh")\"" "$here/../main.go"
check $? "identity.sh pins the image main.go gives the machines"
exit $failed
