#!/bin/bash
# The identity hosts of the identity plane (README, "Identity hosts"): the instances that hold the persistent runner
# identities, each running its share of Runner.Listeners whose bin/Runner.Worker is the shim. Every call checks the
# account is ci-runners first. Every resource is tagged purpose=superci-pull and named superci-pull-identity[-<az>], so
# it is deleted by exact name (deploy/down.sh takes the instances by their tag). Each command can be run again.
#
#   up [--type T] [--replace] <az>
#                                  makes the host of that AZ (us-east-1; T is r7i.4xlarge by default) a one-instance Auto
#                                  Scaling group (superci-pull-identity-<az>, min = max = desired = 1) from a launch
#                                  template of the same name, and waits until the host has restored its share. EC2 replaces a
#                                  host that is lost or impaired, and the host marks itself unhealthy when its listeners do
#                                  not run (superci-id health); the replacement restores the share at boot, as up does. Run
#                                  again it changes nothing, except that changed user data is a new template version
#                                  (the group's default, taken by the next replacement). A plain host from before the
#                                  groups is adopted, never joined by a second one: see "adopting a plain host" below
#   register [--rate R] [--peak-ok] <size> <n> [first]
#                                  registers identities superci-id-<size>-<first> .. <first+n-1> (n at most 100) under the
#                                  park label, identity i on the host of IDENTITY_AZS[i mod their number] (us-east-1a
#                                  us-east-1b by default), and refuses while any of those hosts is down; first is the
#                                  number after the highest there is. At most R a second (0.25 by default, 1 at most), in
#                                  leases of at most 20 (a new lease once one has run out), stopped at the first failure,
#                                  and held back while the budget is held or the organization registers over 3.5 a second
#                                  (unless --peak-ok). A refusal by GitHub is passed to the listener's budget, which holds
#                                  every ask for a while: wait, do not run it again at once
#   relabel <size> <from> <to>     moves every superci-id-<size>-* from one label to the other (no registration), at most
#                                  60 a minute, stopped at the first 403 or 429 and held until its Retry-After has passed
#   down <az>                      sets that AZ's group to 0 and deletes it (so nothing heals the host back), terminates the
#                                  host; its identities stay registered, and `up <az>` restores them
#   status                         groups, hosts, identities per host, listeners running, private memory per listener
#
# Adopting a plain host. Two Runner.Listeners on one identity conflict at GitHub, so a host and its replacement must never
# restore one share at once. A host that `up` finds with the AZ's name and no group is therefore ATTACHED to a new group
# (attach-instances): it is the group's instance from then on, nothing restores anything, no identity goes offline. Were
# that refused, `up --replace <az>` terminates the plain host and waits for it to be gone before the group launches its
# own (the identities are offline for the rebuild, 284 s measured Nov 27, the other AZ carrying the peak). Launching the
# group's host first and terminating the plain one after is never done: the new host would restore and start the share
# (213 s of the 284) while the old one still ran it. `up` refuses outright while a group's host and a plain one both exist.
#
# up, register, relabel and down run one at a time (flock on IDENTITY_LOCK): two registers at once would race for the same
# identities, and the host takes the same care for its own files.
#
# The operator's SSH to the listener is `SSH` (deploy/ssh.sh by default); to a host, a key made for each call and pushed
# with EC2 Instance Connect. A registration token goes from the listener's admin endpoint to a host's stdin through this
# script's memory: never in an argument, a file or the output. The lease behind it (listener /identities/token) bounds
# the rate; the lease is settled with the number actually made.
set -euo pipefail
A="aws --profile ci-runners --region us-east-1"
R=us-east-1
DEPLOYMENT=${DEPLOYMENT:-superci-pull}
NAME=superci-pull-identity
TAGS="Key=purpose,Value=superci-pull"
PARK=superci-park
# The AZs that hold identities, in the order identity i picks from: it lives on AZS[i mod their number], whichever hosts are up.
IDENTITY_AZS=${IDENTITY_AZS:-us-east-1a us-east-1b}
read -ra AZS <<< "$IDENTITY_AZS"
KEY_ALIAS=alias/superci-pull-identities
SSH_USER=${IDENTITY_SSH_USER:-ubuntu}
# The machines' image, as fleet.go picks it (ImageConfig in main.go): the pinned build, or the oldest newer, or the newest.
IMAGE_OWNER=135269210855
IMAGE_NAME='runs-on-v2.2-ubuntu24-full-x64-*'
IMAGE_PIN=20261102081424

die() { echo "$*" >&2; exit 1; }
cmd=${1:-}
[ -n "$cmd" ] || die "usage: identity.sh up [--type T] [--replace] <az> | register <size> <n> [first] | relabel <size> <from> <to> | down <az> | status"
shift
ACCOUNT=$($A sts get-caller-identity --query Account --output text)
[ "$ACCOUNT" = 243891825325 ] || { echo "refusing: account $ACCOUNT is not ci-runners" >&2; exit 1; }
BUCKET=superci-pull-identities-$ACCOUNT

here=$(cd "$(dirname "$0")" && pwd)
. "$here/release-lib.sh"
LOCK=${IDENTITY_LOCK:-${XDG_RUNTIME_DIR:-$HOME/.cache}/superci-identity.lock}
case $cmd in
  up | register | relabel | down)
    mkdir -p "$(dirname "$LOCK")"; exec 8> "$LOCK"
    flock -n 8 || die "another identity.sh run holds $LOCK: wait for it to end" ;;
esac
SSH=${SSH:-$here/ssh.sh}
kdir=$(mktemp -d); trap 'rm -rf "$kdir"' EXIT
ssh-keygen -q -t ed25519 -N '' -f "$kdir/key"

jget() { python3 -I -c 'import json,sys; v = json.load(sys.stdin).get(sys.argv[1]); print("" if v is None else v)' "$1"; }

# render <file> KEY=VALUE...: the file with each @KEY@ replaced.
render() {
  python3 -I -c 'import sys
s = open(sys.argv[1]).read()
for kv in sys.argv[2:]:
    k, v = kv.split("=", 1)
    s = s.replace("@" + k + "@", v)
print(s)' "$@"
}

check_az() { [[ ${1:-} =~ ^us-east-1[a-f]$ ]] || die "az is one of us-east-1a to us-east-1f"; }
check_size() { [[ ${1:-} =~ ^[0-9]+cpu$ ]] || die "size is like 4cpu"; }
check_label() { [[ ${1:-} =~ ^[A-Za-z0-9._-]+$ ]] || die "label is letters, digits, . _ -"; }

# hosts: "<az> <instance id> <public ip>" of the running identity hosts of this deployment, in AZ order.
hosts() {
  $A ec2 describe-instances --filters Name=tag:superci-pull-role,Values=identity Name=tag:superci-pull-deployment,Values="$DEPLOYMENT" \
    Name=instance-state-name,Values=running --query 'Reservations[].Instances[].[Placement.AvailabilityZone,InstanceId,PublicIpAddress]' --output text | sort
}

# resolve_hosts: HOSTS is the running host of each of IDENTITY_AZS, in that order, so identity i is on HOSTS[i mod n]
# whichever other hosts are up. It refuses while any of them is down, or up twice.
resolve_hosts() {
  local az line n running
  [ ${#AZS[@]} -ge 1 ] || die "IDENTITY_AZS names no AZ"
  [ "$(printf '%s\n' "${AZS[@]}" | sort -u | wc -l)" = ${#AZS[@]} ] || die "IDENTITY_AZS names an AZ twice ($IDENTITY_AZS)"
  mapfile -t running < <(hosts)
  HOSTS=()
  for az in "${AZS[@]}"; do
    check_az "$az"
    n=0
    for line in "${running[@]}"; do
      if [[ $line == "$az"[[:space:]]* ]]; then n=$((n + 1)); HOSTS+=("$line"); fi
    done
    [ "$n" = 1 ] || die "refusing: $az has $([ "$n" = 0 ] && echo 'no running identity host' || echo "$n identity hosts"); identity i lives on IDENTITY_AZS[i mod ${#AZS[@]}] ($IDENTITY_AZS), so up $az first"
  done
}

# hssh <instance id> <az> <ip> command...: runs it on the host with a key made for this run, pushed for the call, and
# gives up on it after HOST_TIMEOUT seconds.
hssh() {
  local id=$1 zone=$2 ip=$3; shift 3
  $A ec2-instance-connect send-ssh-public-key --instance-id "$id" --availability-zone "$zone" --instance-os-user "$SSH_USER" \
    --ssh-public-key "file://$kdir/key.pub" > /dev/null < /dev/null
  timeout "${HOST_TIMEOUT:-300}" ssh -q -i "$kdir/key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$kdir/known" -o ConnectTimeout=10 "$SSH_USER@$ip" "$@"
}
# hpush <host line>: the current host script, so the host runs the script of this checkout.
hpush() {
  local az id ip; read -r az id ip <<< "$1"
  hssh "$id" "$az" "$ip" 'sudo tee /usr/local/bin/superci-id > /dev/null && sudo chmod 755 /usr/local/bin/superci-id' < "$here/identity-host.sh"
}

# ---- up ----

operator_arns() {
  [ -z "${IDENTITY_OPERATOR_ARN:-}" ] || { printf '["%s"]\n' "$IDENTITY_OPERATOR_ARN"; return; }
  $A sts get-caller-identity --query Arn --output text | python3 -I -c 'import json,re,sys
arn = sys.stdin.read().strip()
m = re.fullmatch(r"arn:aws:sts::(\d+):assumed-role/([^/]+)/.+", arn)
if m:
    print(json.dumps(["arn:aws:iam::%s:role/%s" % m.group(1, 2), "arn:aws:iam::%s:role/*/%s" % m.group(1, 2)]))
elif re.fullmatch(r"arn:aws:iam::\d+:user/.+", arn):
    print(json.dumps([arn]))
else:
    sys.exit("cannot name the operator from " + arn)'
}

# operator_principal: the role or user the operator is, as a principal of the key's policy (no wildcards there).
operator_principal() {
  operator_arns | python3 -I -c 'import json,sys; print(json.dumps(json.load(sys.stdin)[0]))'
}

# ensure_key: sets KEY_ARN, the customer-managed key behind alias/superci-pull-identities, made once. Its policy names the
# identity role (encrypt and decrypt through S3, nothing else) and the operator (administer, never use), and no one else.
ensure_key() {
  local policy
  policy=$(render "$here/identity-key-policy.json" ACCOUNT="$ACCOUNT" ROLE=$NAME OPERATOR="$(operator_principal)")
  KEY_ARN=$($A kms describe-key --key-id $KEY_ALIAS --query KeyMetadata.Arn --output text 2> /dev/null) || KEY_ARN=""
  if [ -z "$KEY_ARN" ] || [ "$KEY_ARN" = None ]; then
    # The role ensure_role just made is not a principal KMS knows for some seconds (IAM is eventually consistent):
    # create-key refuses a policy naming it with MalformedPolicyDocumentException, so that answer alone is asked again.
    local try
    for try in $(seq 1 "${KEY_TRIES:-12}"); do
      if KEY_ARN=$($A kms create-key --description 'superci-pull identity files: persistent runner credentials' --tags TagKey=purpose,TagValue=superci-pull \
        --policy "$policy" --query KeyMetadata.Arn --output text 2> "$kdir/create-key.err"); then break; fi
      if ! grep -q MalformedPolicyDocumentException "$kdir/create-key.err" || [ "$try" = "${KEY_TRIES:-12}" ]; then
        cat "$kdir/create-key.err" >&2; die "refusing: create-key failed"
      fi
      sleep "${RUN_RETRY_SLEEP:-10}"
    done
    [[ $KEY_ARN =~ ^arn:aws:kms:us-east-1:$ACCOUNT:key/[0-9a-f-]+$ ]] || die "refusing: create-key answered $KEY_ARN"
    # A key with no alias would be made again by the next run.
    $A kms create-alias --alias-name $KEY_ALIAS --target-key-id "$KEY_ARN" ||
      { $A kms schedule-key-deletion --key-id "$KEY_ARN" --pending-window-in-days 7 > /dev/null; die "the alias $KEY_ALIAS was not made; the new key is scheduled for deletion, run again"; }
  else
    [[ $KEY_ARN =~ ^arn:aws:kms:us-east-1:$ACCOUNT:key/[0-9a-f-]+$ ]] || die "refusing: $KEY_ALIAS is $KEY_ARN"
    $A kms put-key-policy --key-id "$KEY_ARN" --policy-name default --policy "$policy"
  fi
  echo "identity key: $KEY_ALIAS ($KEY_ARN)"
}

ensure_bucket() {
  if ! $A s3api head-bucket --bucket "$BUCKET" 2> /dev/null; then
    $A s3api create-bucket --bucket "$BUCKET" > /dev/null
    $A s3api put-public-access-block --bucket "$BUCKET" --public-access-block-configuration \
      BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
    $A s3api put-bucket-tagging --bucket "$BUCKET" --tagging 'TagSet=[{Key=purpose,Value=superci-pull}]'
  fi
  $A s3api put-bucket-encryption --bucket "$BUCKET" --server-side-encryption-configuration \
    "{\"Rules\":[{\"ApplyServerSideEncryptionByDefault\":{\"SSEAlgorithm\":\"aws:kms\",\"KMSMasterKeyID\":\"$KEY_ARN\"},\"BucketKeyEnabled\":false}]}"
  $A s3api put-bucket-policy --bucket "$BUCKET" --policy "$(render "$here/identity-bucket-policy.json" BUCKET="$BUCKET" ACCOUNT="$ACCOUNT" ROLE=$NAME KEY_ARN="$KEY_ARN" OPERATOR="$(operator_arns)")"
  echo "identities bucket: s3://$BUCKET (SSE-KMS with $KEY_ALIAS, public access blocked)"
}

ensure_role() {
  if ! $A iam get-role --role-name $NAME > /dev/null 2>&1; then
    $A iam create-role --role-name $NAME --tags $TAGS --assume-role-policy-document \
      '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}' > /dev/null
  fi
  ensure_key # (its policy names the role, so the role comes first, and the role's policy names the key)
  $A iam put-role-policy --role-name $NAME --policy-name $NAME --policy-document \
    "$(render "$here/identity-policy.json" ACCOUNT="$ACCOUNT" BUCKET="$BUCKET" DEPLOYMENT="$DEPLOYMENT" KEY_ARN="$KEY_ARN" RELEASE="$LISTENER_BUCKET")"
  if ! $A iam get-instance-profile --instance-profile-name $NAME > /dev/null 2>&1; then
    $A iam create-instance-profile --instance-profile-name $NAME --tags $TAGS > /dev/null
    $A iam add-role-to-instance-profile --instance-profile-name $NAME --role-name $NAME
  fi
  echo "role and instance profile: $NAME"
}

ensure_sg() { # prints the id of the hosts' security group: SSH from the admin address the listener's group admits
  local vpc id cidr
  vpc=$($A ec2 describe-vpcs --filters Name=isDefault,Values=true --query 'Vpcs[0].VpcId' --output text)
  id=$($A ec2 describe-security-groups --filters Name=vpc-id,Values="$vpc" Name=group-name,Values=$NAME --query 'SecurityGroups[0].GroupId' --output text)
  if [ "$id" = None ]; then
    id=$($A ec2 create-security-group --vpc-id "$vpc" --group-name $NAME --description 'superci-pull identity hosts: SSH for the admin, no other ingress' \
      --tag-specifications "ResourceType=security-group,Tags=[{$TAGS},{Key=Name,Value=$NAME}]" --query GroupId --output text)
  fi
  cidr=$($A ec2 describe-security-groups --filters Name=group-name,Values=superci-pull-listener \
    --query 'SecurityGroups[0].IpPermissions[?FromPort==`22`].IpRanges[].CidrIp' --output text)
  [[ $cidr =~ ^[0-9.]+/[0-9]+$ ]] || die "refusing: the listener's group admits no single admin address on port 22 ($cidr)"
  $A ec2 authorize-security-group-ingress --group-id "$id" --protocol tcp --port 22 --cidr "$cidr" > /dev/null 2>&1 || true
  echo "$id"
}

image() {
  $A ec2 describe-images --owners $IMAGE_OWNER self --filters "Name=name,Values=$IMAGE_NAME" Name=state,Values=available \
    --query 'Images[].[ImageId,Name]' --output text | python3 -I -c 'import sys
rows = sorted((l.split() for l in sys.stdin if l.strip()), key=lambda r: r[1].rsplit("-", 1)[1])
if not rows:
    sys.exit("no image")
print(next((r for r in rows if r[1].rsplit("-", 1)[1] >= sys.argv[1]), rows[-1])[0])' "$IMAGE_PIN"
}

# tls_pin: the pin of the listener's public key, the only half of /superci-pull/tls that leaves the pipe.
tls_pin() {
  $A ssm get-parameter --name /superci-pull/tls --with-decryption --query Parameter.Value --output text |
    python3 -I -c 'import json,sys; sys.stdout.write(json.load(sys.stdin)["Cert"])' |
    openssl x509 -pubkey -noout | openssl pkey -pubin -outform DER | openssl dgst -sha256 -binary | base64 -w0 | sed 's|^|sha256//|'
}

userdata() { # <listener url> <pin>
  cat << EOF
#!/bin/bash
set -euo pipefail
exec > >(tee /var/log/superci-id-boot.log) 2>&1
( dpkg -s ec2-instance-connect > /dev/null 2>&1 || { apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ec2-instance-connect; } ) || :
cat > /etc/superci-id.conf << 'CONF'
BUCKET=$BUCKET
DEPLOYMENT=$DEPLOYMENT
RELEASE=$LISTENER_BUCKET
KMS_KEY=$KEY_ARN
CONF
printf 'LISTENER=%s\nPIN=%s\n' '$1' '$2' > /etc/superci-shim.env
echo '$(gzip -9n < "$here/identity-host.sh" | base64 -w0)' | base64 -d | gunzip > /usr/local/bin/superci-id
chmod 755 /usr/local/bin/superci-id
/usr/local/bin/superci-id boot
EOF
}

# group_name <az>: the Auto Scaling group (and launch template) of an AZ's host.
group_name() { echo "$NAME-$1"; }

# group_info <az>: "none", or "<min> <max> <desired> <instance>=<lifecycle>/<health>..." of the AZ's group.
group_info() {
  $A autoscaling describe-auto-scaling-groups --auto-scaling-group-names "$(group_name "$1")" --output json | python3 -I -c 'import json,sys
gs = json.load(sys.stdin)["AutoScalingGroups"]
if not gs:
    print("none")
else:
    g = gs[0]
    print(g["MinSize"], g["MaxSize"], g["DesiredCapacity"], *("%s=%s/%s" % (i["InstanceId"], i["LifecycleState"], i["HealthStatus"]) for i in g["Instances"]))'
}

# template_data <type> <ami> <root device> <security group> <user data>: the launch template's data. Every launch of the
# group (the first, and each replacement) is this instance: what up launched on its own before the groups.
template_data() {
  python3 -I -c 'import base64, json, sys
typ, ami, rootdev, sg, ud, name, deployment = sys.argv[1:8]
tags = [{"Key": "purpose", "Value": "superci-pull"}]
print(json.dumps({
    "ImageId": ami, "InstanceType": typ, "IamInstanceProfile": {"Name": sys.argv[8]},
    "NetworkInterfaces": [{"DeviceIndex": 0, "AssociatePublicIpAddress": True, "DeleteOnTermination": True, "Groups": [sg]}],
    "BlockDeviceMappings": [{"DeviceName": rootdev, "Ebs": {"Encrypted": True, "DeleteOnTermination": True}}],
    "MetadataOptions": {"HttpTokens": "required", "HttpPutResponseHopLimit": 1, "HttpEndpoint": "enabled"},
    "UserData": base64.b64encode(ud.encode()).decode(),
    "TagSpecifications": [
        {"ResourceType": "instance", "Tags": tags + [{"Key": "superci-pull-role", "Value": "identity"}, {"Key": "superci-pull-deployment", "Value": deployment}, {"Key": "Name", "Value": name}]},
        {"ResourceType": "volume", "Tags": tags + [{"Key": "Name", "Value": name}]}],
}, sort_keys=True, separators=(",", ":")))' "$@" "$NAME"
}

# ensure_template <az> <template data>: sets TEMPLATE_ID. The data's sha256 is the version's description, so a run again
# with the same data adds no version, and a changed one (user data, image, type) adds one and makes it the group's default.
ensure_template() {
  local az=$1 data=$2 lt=$NAME-$1 sum have v try
  sum=sha256:$(printf '%s' "$data" | sha256sum | cut -d' ' -f1)
  TEMPLATE_ID=$($A ec2 describe-launch-templates --filters Name=launch-template-name,Values="$lt" --query 'LaunchTemplates[0].LaunchTemplateId' --output text)
  if [ "$TEMPLATE_ID" = None ] || [ -z "$TEMPLATE_ID" ]; then
    for try in 1 2 3 4 5 6; do # (a new instance profile takes a few seconds to be usable)
      TEMPLATE_ID=$($A ec2 create-launch-template --launch-template-name "$lt" --launch-template-data "$data" --version-description "$sum" \
        --tag-specifications "ResourceType=launch-template,Tags=[{$TAGS},{Key=superci-pull-role,Value=identity}]" --query LaunchTemplate.LaunchTemplateId --output text) && break
      [ "$try" != 6 ] || die "create-launch-template failed"
      sleep "${RUN_RETRY_SLEEP:-10}"
    done
    echo "launch template: $TEMPLATE_ID ($lt, version 1)"
    return 0
  fi
  have=$($A ec2 describe-launch-template-versions --launch-template-id "$TEMPLATE_ID" --versions '$Default' --query 'LaunchTemplateVersions[0].VersionDescription' --output text)
  if [ "$have" = "$sum" ]; then echo "launch template: $TEMPLATE_ID ($lt, unchanged)"; return 0; fi
  v=$($A ec2 create-launch-template-version --launch-template-id "$TEMPLATE_ID" --launch-template-data "$data" --version-description "$sum" --query LaunchTemplateVersion.VersionNumber --output text)
  $A ec2 modify-launch-template --launch-template-id "$TEMPLATE_ID" --default-version "$v" > /dev/null
  echo "launch template: $TEMPLATE_ID ($lt, new version $v is the default; a host takes it when it is next replaced)"
}

# plain_hosts <az>: the live instances of the AZ's name that are in no group, which restored the share before the groups.
plain_hosts() {
  local az=$1 all in_group id
  all=$($A ec2 describe-instances --filters Name=tag:Name,Values=$NAME-"$az" Name=tag:superci-pull-role,Values=identity Name=tag:superci-pull-deployment,Values="$DEPLOYMENT" \
    Name=instance-state-name,Values=pending,running --query 'Reservations[].Instances[].InstanceId' --output text)
  [ "$all" != None ] || all=""
  in_group=$(group_info "$az")
  for id in $all; do
    case " $in_group " in *" $id="*) ;; *) echo "$id" ;; esac
  done
}

# wait_group_host <az>: sets HOST_ID and HOST_IP, the group's instance once it is InService.
wait_group_host() {
  local az=$1 info try
  for try in $(seq 1 "${GROUP_TRIES:-30}"); do
    info=$(group_info "$az")
    HOST_ID=$(tr ' ' '\n' <<< "$info" | sed -n 's|^\(i-[0-9a-f]*\)=InService/.*|\1|p' | head -1)
    [ -z "$HOST_ID" ] || break
    sleep "${READY_SLEEP:-10}"
  done
  [ -n "$HOST_ID" ] || die "the group $(group_name "$az") has no InService instance yet ($info); run up $az again once it has"
  $A ec2 wait instance-running --instance-ids "$HOST_ID"
  HOST_IP=$($A ec2 describe-instances --instance-ids "$HOST_ID" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
}

# wait_ready <az> <id> <ip>: until the host has restored its share.
wait_ready() {
  local az=$1 id=$2 ip=$3
  for _ in $(seq 1 "${READY_TRIES:-60}"); do
    if hssh "$id" "$az" "$ip" 'test -f /run/superci-id-ready' < /dev/null 2> /dev/null; then
      echo "ready: $NAME-$az restored its share"
      return 0
    fi
    sleep "${READY_SLEEP:-10}"
  done
  die "$id is not ready (its boot log is /var/log/superci-id-boot.log on the host); run up $az again once it is"
}

# install_health <az> <id> <ip>: the current host script, and its health timer, on a host that was not booted with them.
install_health() {
  hpush "$1 $2 $3"
  hssh "$2" "$1" "$3" 'sudo superci-id install-health' < /dev/null
}

up() {
  local type=r7i.4xlarge az eip pin ami subnet sg ud shim rootdev data replace=0 gname info min max desired plain have fresh=0 pip verdict
  while [ $# -gt 0 ]; do
    case $1 in
      --type) type=${2:-}; shift; shift || true ;;
      --replace) replace=1; shift ;;
      --*) die "unknown flag $1" ;;
      *) break ;;
    esac
  done
  az=${1:-}
  [[ $type =~ ^[a-z0-9]+\.[a-z0-9]+$ ]] || die "--type is an instance type like r7i.4xlarge"
  check_az "$az"
  gname=$(group_name "$az")
  ensure_role; ensure_bucket
  sg=$(ensure_sg); echo "security group: $sg"
  shim=$(mktemp); released_object "$LISTENER_BUCKET" $R bin/superci-shim "$shim" || die "refusing: no released shim for the host to run"
  rm -f "$shim"
  eip=$($A ec2 describe-addresses --filters Name=tag:purpose,Values=superci-pull Name=tag:Name,Values=superci-pull-listener --query 'Addresses[0].PublicIp' --output text)
  [[ $eip =~ ^[0-9.]+$ ]] || die "refusing: the listener has no address ($eip)"
  pin=$(tls_pin); [[ $pin =~ ^sha256//[A-Za-z0-9+/=]+$ ]] || die "refusing: no public pin from /superci-pull/tls"
  ami=$(image); subnet=$($A ec2 describe-subnets --filters Name=default-for-az,Values=true Name=availability-zone,Values="$az" --query 'Subnets[0].SubnetId' --output text)
  [ "$subnet" != None ] || die "refusing: no default subnet in $az"
  # The root volume is encrypted: its device is the image's own, or the mapping would add a second volume instead.
  rootdev=$($A ec2 describe-images --image-ids "$ami" --query 'Images[0].RootDeviceName' --output text)
  [[ $rootdev =~ ^/dev/[a-z0-9]+$ ]] || die "refusing: $ami names no root device ($rootdev)"
  ud=$(userdata "https://$eip:8443" "$pin")
  [ ${#ud} -le 16000 ] || die "refusing: user data is ${#ud} bytes, over EC2's 16 KB"
  data=$(template_data "$type" "$ami" "$rootdev" "$sg" "$ud" "$NAME-$az" "$DEPLOYMENT")
  ensure_template "$az" "$data"

  info=$(group_info "$az"); plain=$(plain_hosts "$az")
  have=$(tr ' ' '\n' <<< "$info" | sed -n 's|^\(i-[0-9a-f]*\)=.*|\1|p')
  # A group's host and a plain one of the same name are two hosts on one share: nothing here picks between them.
  if [ -n "$have" ] && [ -n "$plain" ]; then
    die "refusing: $gname has a host ($(echo $have)) and so has a plain one ($(echo $plain)): both would run the share of $az. Run down $az (it ends both, the identities stay), then up $az"
  fi
  if [ -n "$plain" ] && [ "$replace" = 1 ]; then
    # Gone first, then the group's own host: never both on the share.
    echo "replacing the plain host $(echo $plain): terminating it, and waiting for it to be gone, before the group launches one"
    # shellcheck disable=SC2086
    $A ec2 terminate-instances --instance-ids $plain > /dev/null
    # shellcheck disable=SC2086
    $A ec2 wait instance-terminated --instance-ids $plain
    plain=""
  fi
  if [ -n "$have" ]; then
    read -r min max desired _ <<< "$info"
    if [ "$min $max $desired" = "1 1 1" ]; then
      echo "already up: $gname (one instance; replaced when lost or unhealthy)"
    else
      $A autoscaling update-auto-scaling-group --auto-scaling-group-name "$gname" --min-size 1 --max-size 1 --desired-capacity 1
      echo "$gname was at $min/$max/$desired: set to one instance"
    fi
  elif [ -n "$plain" ]; then
    [ "$(wc -w <<< "$plain")" = 1 ] || die "refusing: $(wc -w <<< "$plain") plain hosts are named $NAME-$az ($(echo $plain)); run down $az first"
    # Adopt it: an empty group takes the running host as its instance. Nothing is launched and nothing restores the share.
    $A ec2 wait instance-running --instance-ids "$plain"
    # The health timer it is given judges the host at once: one that is not healthy now would be revived, then replaced.
    pip=$($A ec2 describe-instances --instance-ids "$plain" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
    hpush "$az $plain $pip"
    verdict=$(hssh "$plain" "$az" "$pip" 'sudo superci-id check' < /dev/null 2>&1) ||
      die "refusing to adopt the plain host $plain: $verdict. This host now has this checkout's superci-id (pushed to read it); the group, the template and the instance are unchanged. Make it healthy (sudo superci-id boot on it restores and starts what is missing) and run up $az again, or run up --replace $az to rebuild it"
    if [ "$info" = none ]; then create_group "$gname" "$subnet" 0 0
    else $A autoscaling update-auto-scaling-group --auto-scaling-group-name "$gname" --min-size 0 --max-size 1 --desired-capacity 0; fi
    $A autoscaling attach-instances --auto-scaling-group-name "$gname" --instance-ids "$plain" ||
      die "the plain host $plain was not attached: it still runs alone and $gname holds nothing (desired 0), so no second host exists. Run up $az again, or up --replace $az to rebuild it"
    $A autoscaling update-auto-scaling-group --auto-scaling-group-name "$gname" --min-size 1
    echo "adopted the plain host $plain into $gname: no second host, no identity restored or moved"
  else
    if [ "$info" = none ]; then create_group "$gname" "$subnet" 1 1
    else $A autoscaling update-auto-scaling-group --auto-scaling-group-name "$gname" --min-size 1 --max-size 1 --desired-capacity 1; fi
    fresh=1
    echo "group $gname: its instance restores the share of $az at boot"
  fi
  wait_group_host "$az"
  echo "host $HOST_ID ($NAME-$az, $type, $ami) in $gname"
  wait_ready "$az" "$HOST_ID" "$HOST_IP"
  # A host that was not booted from this checkout's user data (adopted, or already up) gets its script and health timer.
  [ "$fresh" = 1 ] || install_health "$az" "$HOST_ID" "$HOST_IP"
}

# create_group <name> <subnet> <min> <desired>: the AZ's group, max 1, EC2 health, terminate before launch.
create_group() {
  $A autoscaling create-auto-scaling-group --auto-scaling-group-name "$1" --launch-template "LaunchTemplateId=$TEMPLATE_ID,Version=\$Default" \
    --min-size "$3" --max-size 1 --desired-capacity "$4" --vpc-zone-identifier "$2" --health-check-type EC2 --health-check-grace-period 300 \
    --instance-maintenance-policy MinHealthyPercentage=0,MaxHealthyPercentage=100 \
    --tags "ResourceId=$1,ResourceType=auto-scaling-group,Key=purpose,Value=superci-pull,PropagateAtLaunch=false" \
    "ResourceId=$1,ResourceType=auto-scaling-group,Key=superci-pull-role,Value=identity,PropagateAtLaunch=false"
}

# ---- register ----

# complete_in_s3: "<name> <az>" of every identity whose three files are in the bucket.
complete_in_s3() {
  $A s3api list-objects-v2 --bucket "$BUCKET" --prefix "$DEPLOYMENT/hosts/" --query 'Contents[].Key' --output text | python3 -I -c 'import re,sys
have = {}
for k in sys.stdin.read().split():
    m = re.fullmatch(r"[^/]+/hosts/([^/]+)/([^/]+)/(\.runner|\.credentials|\.credentials_rsaparams)", k)
    if m:
        have.setdefault((m.group(2), m.group(1)), set()).add(m.group(3))
for (name, az), files in sorted(have.items()):
    if len(files) == 3:
        print(name, az)'
}

at_github() { # the identities GitHub lists, as the listener's /identities shows them
  $SSH "curl -sf localhost:8080/identities" || die "the listener's /identities did not answer"
}

# fmt_dur <seconds>: 75 -> 1m15s
fmt_dur() { awk -v s="$1" 'BEGIN { s = int(s + 0.5); if (s >= 3600) printf "%dh%02dm", s / 3600, (s % 3600) / 60; else if (s >= 60) printf "%dm%02ds", s / 60, s % 60; else printf "%ds", s }'; }

# quiet: waits until the listener's budget is neither held nor busy with the organization's own jobs: not throttled
# after a refusal, the organization's rate known and at most 3.5 a second. Fails after PEAK_TRIES polls (PEAK_WAIT apart).
quiet() {
  local why tries=0
  while :; do
    why=$($SSH "curl -sf localhost:8080/state" | python3 -I -c 'import json,sys
b = json.load(sys.stdin)["budget"]
if b.get("throttled"):
    print("the budget is held after GitHub refused a registration")
elif not b.get("known"):
    print("the organization rate is not known yet")
elif b["org_rate"] > 3.5:
    print("the organization is registering %.2f a second, above 3.5" % b["org_rate"])') || die "the listener's /state did not answer"
    [ -z "$why" ] && return 0
    tries=$((tries + 1))
    [ "$tries" -le "${PEAK_TRIES:-60}" ] || { echo "not registering: $why, for $((tries - 1)) polls (--peak-ok registers anyway)" >&2; return 1; }
    echo "waiting: $why (poll $tries)"
    sleep "${PEAK_WAIT:-30}"
  done
}

# Registering identities spends the organization's cap like any registration: at most --rate a second (default 0.25, never
# above 1), one identity at a time over all hosts, in leases of at most 20 each settled before the next is asked, and
# stopped at the first one that fails. A registration is only started while its lease has longer to run than the
# registration may take (REGISTER_TIMEOUT), else a new lease is asked for.
register() {
  local rate=0.25 peak_ok=0 size n first i name line az id ip tries=0 granted lease resp token url group gname k code
  local lease_exp failed_n refused_n HOST_TIMEOUT=${REGISTER_TIMEOUT:-180}
  while [ $# -gt 0 ]; do
    case $1 in
      --rate) rate=${2:-}; shift; shift || true ;;
      --peak-ok) peak_ok=1; shift ;;
      --*) die "unknown flag $1" ;;
      *) break ;;
    esac
  done
  size=${1:-}; n=${2:-}; first=${3:-}
  [[ $rate =~ ^[0-9]*\.?[0-9]+$ ]] && awk -v r="$rate" 'BEGIN { exit !(r > 0 && r <= 1) }' || die "--rate is registrations a second: above 0 and at most 1"
  check_size "$size"
  [[ $n =~ ^[0-9]+$ ]] && [ "$n" -ge 1 ] && [ "$n" -le 100 ] || die "n is 1 to 100 identities a call"
  [ -z "$first" ] || { [[ $first =~ ^[0-9]+$ ]] && [ "$first" -ge 1 ] || die "first is a number from 1"; }
  resolve_hosts
  [ ${#HOSTS[@]} -ge 2 ] || echo "warning: one identity host; the plane needs two, in different AZs" >&2
  local s3names gh
  s3names=$(complete_in_s3)
  gh=$(at_github | python3 -I -c 'import json,sys
for r in json.load(sys.stdin)["runners"]:
    print(r["name"])')
  if [ -z "$first" ]; then
    first=$(printf '%s\n%s\n' "$s3names" "$gh" | python3 -I -c 'import re,sys
top = 0
for l in sys.stdin:
    m = re.fullmatch(r"superci-id-%s-(\d+)(?: .*)?" % sys.argv[1], l.strip())
    if m:
        top = max(top, int(m.group(1)))
print(top + 1)' "$size")
  fi
  local todo=() adopt=() held=0 orphans=0 refused=0
  for ((i = first; i < first + n; i++)); do
    name=$(printf 'superci-id-%s-%04d' "$size" "$i")
    if grep -q "^$name " <<< "$s3names"; then held=$((held + 1))
    elif grep -qx "$name" <<< "$gh"; then adopt+=("$name")
    else todo+=("$name"); fi
  done
  echo "$size $first..$((first + n - 1)): ${#todo[@]} to register, ${#adopt[@]} registered but not stored, $held already held"
  for line in "${HOSTS[@]}"; do hpush "$line"; done

  # Registered at GitHub with no files in S3: the host that registered it uploads what it still has. No registration.
  for name in "${adopt[@]}"; do
    i=$((10#${name##*-}))
    read -r az id ip <<< "${HOSTS[$((i % ${#HOSTS[@]}))]}"
    line=$(hssh "$id" "$az" "$ip" "sudo superci-id adopt $name" < /dev/null) || line="failed $name: host"
    echo "$az: $line"
    case $line in orphan* | failed*) orphans=$((orphans + 1)) ;; esac
  done

  local interval batchmax target=${#todo[@]} made=0 stop="" t0 next_at=0 now round_made done_n elapsed
  interval=$(awk -v r="$rate" 'BEGIN { printf "%.3f", 1 / r }')
  batchmax=$(awk -v r="$rate" 'BEGIN { b = int(480 * r); print (b < 1 ? 1 : (b > 20 ? 20 : b)) }')
  t0=$(date +%s.%N)
  progress() {
    elapsed=$(awk -v a="$(date +%s.%N)" -v b="$t0" 'BEGIN { print a - b }')
    echo "$1 made $made of $target in $(fmt_dur "$elapsed"); $((target - made)) left, about $(fmt_dur "$(awk -v e="$elapsed" -v m="$made" -v left=$((target - made)) -v i="$interval" 'BEGIN { print left * (m > 0 ? e / m : i) }')") at this pace ($rate/s)"
  }
  while [ ${#todo[@]} -gt 0 ] && [ -z "$stop" ]; do
    if [ "$peak_ok" = 0 ] && ! quiet; then stop="the budget is busy"; break; fi
    k=${#todo[@]}; [ "$k" -le "$batchmax" ] || k=$batchmax
    resp=$($SSH "curl -sf -X POST 'localhost:8080/identities/token?n=$k'") || die "the listener refused the token call"
    granted=$(printf '%s' "$resp" | jget granted)
    if [ "${granted:-0}" = 0 ]; then
      tries=$((tries + 1))
      [ "$tries" -le "${LEASE_TRIES:-40}" ] || { stop="the budget granted nothing in $((tries - 1)) tries"; break; }
      echo "the budget granted nothing now (try $tries); waiting"
      sleep "${LEASE_WAIT:-15}"
      continue
    fi
    lease=$(printf '%s' "$resp" | jget lease); token=$(printf '%s' "$resp" | jget token)
    url=$(printf '%s' "$resp" | jget url); gname=$(printf '%s' "$resp" | jget group_name)
    lease_exp=$(date -d "$(printf '%s' "$resp" | jget lease_expires)" +%s 2> /dev/null) || die "the listener named no valid lease_expires"
    unset resp
    [[ $url =~ ^https://github\.com/[A-Za-z0-9._/-]+$ ]] || die "the listener named no registration target ($url)"
    group=${gname:--}; [[ $group =~ ^[A-Za-z0-9._-]+$ ]] || die "bad runner group name"
    [[ $lease =~ ^[0-9a-f]+$ ]] || die "bad lease id"
    round_made=0; done_n=0; failed_n=0; refused_n=0
    for name in "${todo[@]:0:granted}"; do
      if [ "$(date +%s)" -ge $((lease_exp - HOST_TIMEOUT)) ]; then echo "lease $lease has too little time left for a registration; settling it and asking for another"; break; fi
      now=$(date +%s.%N)
      awk -v a="$next_at" -v b="$now" 'BEGIN { exit !(a > b) }' && sleep "$(awk -v a="$next_at" -v b="$now" 'BEGIN { printf "%.3f", a - b }')"
      next_at=$(awk -v a="$(date +%s.%N)" -v i="$interval" 'BEGIN { printf "%.3f", a + i }')
      i=$((10#${name##*-}))
      read -r az id ip <<< "${HOSTS[$((i % ${#HOSTS[@]}))]}"
      line=$(printf '%s\n' "$token" | hssh "$id" "$az" "$ip" "sudo superci-id register $PARK $url $group $name") || true
      echo "  $az: ${line:-failed $name: no answer from the host}"
      case $line in
        "registered $name") round_made=$((round_made + 1)); done_n=$((done_n + 1)) ;;
        "unstored $name"*) round_made=$((round_made + 1)); stop="$name registered but its files were not stored: run again, it is stored without a new registration"; break ;;
        "refused $name"*) refused_n=1; refused=1; stop="$name was not registered: GitHub refused it"; break ;;
        *) failed_n=$((failed_n + 1)); stop="$name was not registered"; break ;;
      esac
    done
    unset token
    # A refusal goes to the budget with the settlement: it holds every ask, ours and the machines', while the cap clears.
    code=$($SSH "curl -s -o /dev/null -w '%{http_code}' -X POST 'localhost:8080/lease/settle?id=$lease&made=$round_made&failed=$failed_n&refused=$refused_n'" || true)
    [ "$code" = 204 ] || echo "warning: lease $lease settled with $code (an expired lease was released already$([ "$refused_n" = 0 ] || echo ', but its refusal reached the budget'))" >&2
    made=$((made + round_made))
    todo=("${todo[@]:$done_n}")
    progress "batch:"
    if [ "$round_made" -gt 0 ]; then tries=0
    elif [ -z "$stop" ]; then
      tries=$((tries + 1))
      [ "$tries" -le "${LEASE_TRIES:-40}" ] || stop="no registration was made under $((tries - 1)) leases in a row"
    fi
  done
  progress "done:"
  echo "registered $made, orphaned $orphans, already held $held"
  if [ "$refused" = 1 ]; then
    echo "stopped: $stop. GitHub's cap of registrations is spent: the listener's budget is held now. Wait at least 5 minutes (the cap's window) and let /state show it not held; do not run it again at once. Then: identity.sh register $size $n $first" >&2
    exit 1
  fi
  if [ -n "$stop" ] || [ "$orphans" -gt 0 ]; then
    echo "stopped: ${stop:-orphans}; run again to continue: identity.sh register $size $n $first" >&2
    exit 1
  fi
}

# ---- relabel ----

# GitHub's write limits are secondary ones: relabel makes at most one PUT a second (60 a minute), stops at the first 403 or
# 429 and keeps its Retry-After in a file beside the lock, which the next relabel waits out.
relabel() {
  local size=${1:-} from=${2:-} to=${3:-} l ids bad=0 out chunk ok stopline pause not_before now
  check_size "$size"; check_label "$from"; check_label "$to"
  # The dispatcher trusts an identity's label for its size, so a label of another size would send its jobs to the wrong machines.
  for l in "$from" "$to"; do
    [ "$l" = "$PARK" ] || [ "$l" = "superci-$size" ] || die "label is $PARK or superci-$size, not $l"
  done
  not_before=$(cat "$LOCK.retry-after" 2> /dev/null || echo 0); now=$(date +%s)
  [[ $not_before =~ ^[0-9]+$ ]] || not_before=0
  [ "$now" -ge "$not_before" ] || die "GitHub asked for a pause: wait $((not_before - now)) s more (until $(date -d "@$not_before" +%T)), then run the same command again"
  ids=$(at_github | python3 -I -c 'import json,sys
for r in json.load(sys.stdin)["runners"]:
    if r["name"].startswith("superci-id-%s-" % sys.argv[1]) and sys.argv[2] in r["labels"]:
        print(r["id"])' "$size" "$from")
  [ -n "$ids" ] || { echo "nothing to relabel: no superci-id-$size-* carries $from"; return 0; }
  echo "relabelling $(wc -l <<< "$ids") identities of $size: $from -> $to"
  # One SSH to the listener for each 50. The listener answers GitHub's 403 and 429 as 429 with the Retry-After. ssh reads
  # its stdin, which here is the list of chunks, so the call gets /dev/null: given the loop's stdin it took every chunk after
  # the first, and a relabel of 410 moved 50 and said "relabelled all" (Nov 28).
  local total moved=0 left
  total=$(wc -l <<< "$ids")
  while read -r -a chunk; do
    out=$($SSH "h=\$(mktemp); for id in ${chunk[*]}; do
      code=\$(curl -s -o /dev/null -D \"\$h\" -w '%{http_code}' -X POST \"localhost:8080/identities/labels?id=\$id&labels=$to\")
      if [ \"\$code\" = 204 ]; then echo ok \$id
      elif [ \"\$code\" = 403 ] || [ \"\$code\" = 429 ]; then echo \"stop \$id \$code \$(sed -n 's/^[Rr]etry-[Aa]fter: *\\([0-9]*\\).*/\\1/p' \"\$h\")\"; break
      else echo fail \$id \$code; fi
      sleep 1
    done; rm -f \"\$h\"" < /dev/null) || out=""
    ok=$(grep -c '^ok' <<< "$out" || true)
    moved=$((moved + ok))
    echo "  relabelled $ok ($moved of $total)"
    stopline=$(grep '^stop ' <<< "$out" || true)
    if [ -n "$stopline" ]; then
      read -r _ id code pause <<< "$stopline"
      pause=${pause:-60}
      printf '%s\n' "$(($(date +%s) + pause))" > "$LOCK.retry-after"
      die "GitHub answered $code to runner $id: stopped after $ok of this 50 (and nothing after it). Retry-After is $pause s: wait it out, then run the same command again"
    fi
    bad=$((bad + ${#chunk[@]} - ok))
  done < <(xargs -n 50 <<< "$ids")
  [ "$bad" = 0 ] || die "$bad identities were not relabelled; run the same command again"
  # What GitHub lists afterwards decides, not the count of answers: an identity still under the old label is reported.
  left=$(at_github | python3 -I -c 'import json,sys
print(sum(1 for r in json.load(sys.stdin)["runners"] if r["name"].startswith("superci-id-%s-" % sys.argv[1]) and sys.argv[2] in r["labels"]))' "$size" "$from")
  [ "$left" = 0 ] || die "relabelled $moved of $total, and GitHub still lists $left of superci-id-$size-* under $from; run the same command again"
  echo "relabelled all $moved of $total"
}

# ---- down ----

down() {
  local az=${1:-} ids gname info
  check_az "$az"
  gname=$(group_name "$az")
  ids=$($A ec2 describe-instances --filters Name=tag:Name,Values=$NAME-"$az" Name=tag:superci-pull-role,Values=identity \
    Name=tag:superci-pull-deployment,Values="$DEPLOYMENT" Name=instance-state-name,Values=pending,running,stopping,stopped \
    --query 'Reservations[].Instances[].InstanceId' --output text)
  [ "$ids" != None ] || ids=""
  info=$(group_info "$az")
  if [ -z "$ids" ] && [ "$info" = none ]; then echo "no host named $NAME-$az"; return 0; fi
  # The group goes to 0 first, so nothing heals the host back; then the host is ended by name (a plain host, or a straggler).
  if [ "$info" != none ]; then
    $A autoscaling update-auto-scaling-group --auto-scaling-group-name "$gname" --min-size 0 --max-size 0 --desired-capacity 0
  fi
  if [ -n "$ids" ]; then
    # shellcheck disable=SC2086
    $A ec2 terminate-instances --instance-ids $ids > /dev/null
    # shellcheck disable=SC2086
    $A ec2 wait instance-terminated --instance-ids $ids
  fi
  [ "$info" = none ] || $A autoscaling delete-auto-scaling-group --auto-scaling-group-name "$gname" --force-delete
  echo "terminated ${ids:-nothing} ($NAME-$az)$([ "$info" = none ] || echo "; deleted group $gname, so nothing heals it back (its launch template stays)"); its identities stay registered at GitHub (offline) and in s3://$BUCKET, and up $az restores them"
}

# ---- status ----

status() {
  mapfile -t HOSTS < <(hosts)
  echo "identity groups:"
  $A autoscaling describe-auto-scaling-groups --output json | python3 -I -c 'import json,sys
rows = [g for g in json.load(sys.stdin)["AutoScalingGroups"] if g["AutoScalingGroupName"].startswith(sys.argv[1] + "-")]
for g in sorted(rows, key=lambda g: g["AutoScalingGroupName"]):
    hosts = ", ".join("%s %s %s" % (i["InstanceId"], i["LifecycleState"], i["HealthStatus"]) for i in g["Instances"]) or "no instance"
    print("  %s: min %d max %d desired %d: %s" % (g["AutoScalingGroupName"], g["MinSize"], g["MaxSize"], g["DesiredCapacity"], hosts))
if not rows:
    print("  none: every host is plain, and nothing replaces one that is lost (up <az> adopts it into a group)")' "$NAME"
  echo "identity hosts: ${#HOSTS[@]}"
  local line az id ip
  for line in "${HOSTS[@]}"; do
    read -r az id ip <<< "$line"
    case " $(group_info "$az") " in *" $id="*) ;; *) echo "$id is in no group: nothing replaces it if it is lost; up $az adopts it" ;; esac
    hpush "$line"
    echo "$id $(hssh "$id" "$az" "$ip" 'sudo superci-id status' < /dev/null || echo 'status failed')"
  done
  echo "stored in s3://$BUCKET:"
  complete_in_s3 | awk '{n[$2]++} END {for (a in n) printf "  %s %d identities\n", a, n[a]}' | sort
  echo "at GitHub:"
  at_github | python3 -I -c 'import collections,json,sys
runners = json.load(sys.stdin)["runners"]
by = collections.Counter(l for r in runners for l in r["labels"] if l not in ("self-hosted", "Linux", "X64", "ARM64"))
print("  %d identities, %d online, %d busy" % (len(runners), sum(r["status"] == "online" for r in runners), sum(bool(r["busy"]) for r in runners)))
for l, c in sorted(by.items()):
    print("  label %s: %d" % (l, c))
print("  memory they need on %d hosts: %.1f GiB" % (int(sys.argv[1]), (len(runners) * 64 + 60 * int(sys.argv[1])) / 1024))' "${#HOSTS[@]}"
  echo "budget per listener: 64 MiB private (its unit restarts it at MemoryMax 192M) + 60 MiB shared a host; 1,550 identities are about 97 GiB"
}

case "$cmd" in
  up) up "$@" ;;
  register) register "$@" ;;
  relabel) relabel "$@" ;;
  down) down "$@" ;;
  status) status "$@" ;;
  *) die "usage: identity.sh up [--type T] [--replace] <az> | register <size> <n> [first] | relabel <size> <from> <to> | down <az> | status" ;;
esac
