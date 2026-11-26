#!/bin/bash
# The identity hosts of the identity plane (README, "Identity hosts"): the instances that hold the persistent runner
# identities, each running its share of Runner.Listeners whose bin/Runner.Worker is the shim. Every call checks the
# account is ci-runners first. Every resource is tagged purpose=superci-pull and named superci-pull-identity[-<az>], so
# it is deleted by exact name (deploy/down.sh takes the instances by their tag). Each command can be run again.
#
#   up [--type T] <az>             launches the host of that AZ (us-east-1; T is r7i.4xlarge by default) and waits until
#                                  it has restored its share
#   register [--rate R] [--peak-ok] <size> <n> [first]
#                                  registers identities superci-id-<size>-<first> .. <first+n-1> (n at most 100) under the
#                                  park label, on host (number mod hosts); first is the number after the highest there is.
#                                  At most R a second (0.25 by default, 1 at most), in leases of at most 20, stopped at the
#                                  first failure, and held back while the budget is held or the organization registers over
#                                  3.5 a second (unless --peak-ok)
#   relabel <size> <from> <to>     moves every superci-id-<size>-* from one label to the other (no registration)
#   down <az>                      terminates that host; its identities stay registered, and `up <az>` restores them
#   status                         hosts, identities per host, listeners running, private memory per listener
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
SSH_USER=${IDENTITY_SSH_USER:-ubuntu}
# The machines' image, as fleet.go picks it (ImageConfig in main.go): the pinned build, or the oldest newer, or the newest.
IMAGE_OWNER=135269210855
IMAGE_NAME='runs-on-v2.2-ubuntu24-full-x64-*'
IMAGE_PIN=20261102081424

die() { echo "$*" >&2; exit 1; }
cmd=${1:-}
[ -n "$cmd" ] || die "usage: identity.sh up [--type T] <az> | register <size> <n> [first] | relabel <size> <from> <to> | down <az> | status"
shift
ACCOUNT=$($A sts get-caller-identity --query Account --output text)
[ "$ACCOUNT" = 243891825325 ] || { echo "refusing: account $ACCOUNT is not ci-runners" >&2; exit 1; }
BUCKET=superci-pull-identities-$ACCOUNT

here=$(cd "$(dirname "$0")" && pwd)
. "$here/release-lib.sh"
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

# hssh <instance id> <az> <ip> command...: runs it on the host with a key made for this run, pushed for the call.
hssh() {
  local id=$1 zone=$2 ip=$3; shift 3
  $A ec2-instance-connect send-ssh-public-key --instance-id "$id" --availability-zone "$zone" --instance-os-user "$SSH_USER" \
    --ssh-public-key "file://$kdir/key.pub" > /dev/null < /dev/null
  ssh -q -i "$kdir/key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$kdir/known" -o ConnectTimeout=10 "$SSH_USER@$ip" "$@"
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

ensure_bucket() {
  if ! $A s3api head-bucket --bucket "$BUCKET" 2> /dev/null; then
    $A s3api create-bucket --bucket "$BUCKET" > /dev/null
    $A s3api put-public-access-block --bucket "$BUCKET" --public-access-block-configuration \
      BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
    $A s3api put-bucket-tagging --bucket "$BUCKET" --tagging 'TagSet=[{Key=purpose,Value=superci-pull}]'
  fi
  $A s3api put-bucket-encryption --bucket "$BUCKET" --server-side-encryption-configuration \
    '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"aws:kms"}}]}'
  $A s3api put-bucket-policy --bucket "$BUCKET" --policy "$(render "$here/identity-bucket-policy.json" BUCKET="$BUCKET" ACCOUNT="$ACCOUNT" ROLE=$NAME OPERATOR="$(operator_arns)")"
  echo "identities bucket: s3://$BUCKET (SSE-KMS, public access blocked)"
}

ensure_role() {
  if ! $A iam get-role --role-name $NAME > /dev/null 2>&1; then
    $A iam create-role --role-name $NAME --tags $TAGS --assume-role-policy-document \
      '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}' > /dev/null
  fi
  $A iam put-role-policy --role-name $NAME --policy-name $NAME --policy-document \
    "$(render "$here/identity-policy.json" BUCKET="$BUCKET" DEPLOYMENT="$DEPLOYMENT" ACCOUNT="$ACCOUNT" RELEASE="$LISTENER_BUCKET")"
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
CONF
printf 'LISTENER=%s\nPIN=%s\n' '$1' '$2' > /etc/superci-shim.env
echo '$(base64 -w0 "$here/identity-host.sh")' | base64 -d > /usr/local/bin/superci-id
chmod 755 /usr/local/bin/superci-id
/usr/local/bin/superci-id boot
EOF
}

up() {
  local type=r7i.4xlarge az existing eip pin ami subnet sg ud shim id ip
  while [ $# -gt 0 ]; do
    case $1 in
      --type) type=${2:-}; shift; shift || true ;;
      --*) die "unknown flag $1" ;;
      *) break ;;
    esac
  done
  az=${1:-}
  [[ $type =~ ^[a-z0-9]+\.[a-z0-9]+$ ]] || die "--type is an instance type like r7i.4xlarge"
  check_az "$az"
  existing=$($A ec2 describe-instances --filters Name=tag:Name,Values=$NAME-"$az" Name=instance-state-name,Values=pending,running \
    --query 'Reservations[].Instances[].InstanceId' --output text)
  if [ -n "$existing" ] && [ "$existing" != None ]; then echo "already up: $existing ($NAME-$az)"; return 0; fi
  ensure_bucket; ensure_role
  sg=$(ensure_sg); echo "security group: $sg"
  shim=$(mktemp); released_object "$LISTENER_BUCKET" $R bin/superci-shim "$shim" || die "refusing: no released shim for the host to run"
  rm -f "$shim"
  eip=$($A ec2 describe-addresses --filters Name=tag:purpose,Values=superci-pull Name=tag:Name,Values=superci-pull-listener --query 'Addresses[0].PublicIp' --output text)
  [[ $eip =~ ^[0-9.]+$ ]] || die "refusing: the listener has no address ($eip)"
  pin=$(tls_pin); [[ $pin =~ ^sha256//[A-Za-z0-9+/=]+$ ]] || die "refusing: no public pin from /superci-pull/tls"
  ami=$(image); subnet=$($A ec2 describe-subnets --filters Name=default-for-az,Values=true Name=availability-zone,Values="$az" --query 'Subnets[0].SubnetId' --output text)
  [ "$subnet" != None ] || die "refusing: no default subnet in $az"
  ud=$(userdata "https://$eip:8443" "$pin")
  [ ${#ud} -le 16000 ] || die "refusing: user data is ${#ud} bytes, over EC2's 16 KB"
  for _ in 1 2 3 4 5 6; do # (a new instance profile takes a few seconds to be usable)
    id=$($A ec2 run-instances --image-id "$ami" --instance-type "$type" --subnet-id "$subnet" --security-group-ids "$sg" --associate-public-ip-address \
      --iam-instance-profile Name=$NAME --metadata-options HttpTokens=required,HttpPutResponseHopLimit=1,HttpEndpoint=enabled \
      --user-data "$ud" \
      --tag-specifications "ResourceType=instance,Tags=[{$TAGS},{Key=superci-pull-role,Value=identity},{Key=superci-pull-deployment,Value=$DEPLOYMENT},{Key=Name,Value=$NAME-$az}]" \
      "ResourceType=volume,Tags=[{$TAGS},{Key=Name,Value=$NAME-$az}]" --query 'Instances[0].InstanceId' --output text) && break
    sleep "${RUN_RETRY_SLEEP:-10}"
  done
  [ -n "${id:-}" ] || die "run-instances failed"
  echo "launched $id ($NAME-$az, $type, $ami)"
  $A ec2 wait instance-running --instance-ids "$id"
  ip=$($A ec2 describe-instances --instance-ids "$id" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
  for _ in $(seq 1 "${READY_TRIES:-60}"); do
    if hssh "$id" "$az" "$ip" 'test -f /run/superci-id-ready' < /dev/null 2> /dev/null; then
      echo "ready: $NAME-$az restored its share"
      return 0
    fi
    sleep "${READY_SLEEP:-10}"
  done
  die "$id is not ready (its boot log is /var/log/superci-id-boot.log on the host); run up $az again once it is"
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
# stopped at the first one that fails. A batch also never outlasts a lease (10 minutes) at the rate.
register() {
  local rate=0.25 peak_ok=0 size n first i name line az id ip tries=0 granted lease resp token url group gname k code
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
  mapfile -t HOSTS < <(hosts)
  [ ${#HOSTS[@]} -ge 1 ] || die "no identity host is running: up <az> first"
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
  local todo=() adopt=() held=0 orphans=0
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
    tries=0
    lease=$(printf '%s' "$resp" | jget lease); token=$(printf '%s' "$resp" | jget token)
    url=$(printf '%s' "$resp" | jget url); gname=$(printf '%s' "$resp" | jget group_name); unset resp
    [[ $url =~ ^https://github\.com/[A-Za-z0-9._/-]+$ ]] || die "the listener named no registration target ($url)"
    group=${gname:--}; [[ $group =~ ^[A-Za-z0-9._-]+$ ]] || die "bad runner group name"
    [[ $lease =~ ^[0-9a-f]+$ ]] || die "bad lease id"
    round_made=0; done_n=0
    for name in "${todo[@]:0:granted}"; do
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
        *) stop="$name was not registered"; break ;;
      esac
    done
    unset token
    code=$($SSH "curl -s -o /dev/null -w '%{http_code}' -X POST 'localhost:8080/lease/settle?id=$lease&made=$round_made'" || true)
    [ "$code" = 204 ] || echo "warning: lease $lease settled with $code (an expired lease was released already)" >&2
    made=$((made + round_made))
    todo=("${todo[@]:$done_n}")
    progress "batch:"
  done
  progress "done:"
  echo "registered $made, orphaned $orphans, already held $held"
  if [ -n "$stop" ] || [ "$orphans" -gt 0 ]; then
    echo "stopped: ${stop:-orphans}; run again to continue: identity.sh register $size $n $first" >&2
    exit 1
  fi
}

# ---- relabel ----

relabel() {
  local size=${1:-} from=${2:-} to=${3:-} ids bad=0 out chunk ok
  check_size "$size"; check_label "$from"; check_label "$to"
  ids=$(at_github | python3 -I -c 'import json,sys
for r in json.load(sys.stdin)["runners"]:
    if r["name"].startswith("superci-id-%s-" % sys.argv[1]) and sys.argv[2] in r["labels"]:
        print(r["id"])' "$size" "$from")
  [ -n "$ids" ] || { echo "nothing to relabel: no superci-id-$size-* carries $from"; return 0; }
  echo "relabelling $(wc -l <<< "$ids") identities of $size: $from -> $to"
  # One SSH to the listener for each 50, which the GitHub App's write rate allows at one a call every RELABEL_PAUSE.
  while read -r -a chunk; do
    out=$($SSH "for id in ${chunk[*]}; do if curl -sf -o /dev/null -X POST \"localhost:8080/identities/labels?id=\$id&labels=$to\"; then echo ok \$id; else echo fail \$id; fi; sleep ${RELABEL_PAUSE:-0.6}; done") || out=""
    ok=$(grep -c '^ok' <<< "$out" || true)
    echo "  relabelled $ok"
    bad=$((bad + ${#chunk[@]} - ok))
  done < <(xargs -n 50 <<< "$ids")
  [ "$bad" = 0 ] || die "$bad identities were not relabelled; run the same command again"
  echo "relabelled all"
}

# ---- down ----

down() {
  local az=${1:-} ids
  check_az "$az"
  ids=$($A ec2 describe-instances --filters Name=tag:Name,Values=$NAME-"$az" Name=tag:superci-pull-role,Values=identity \
    Name=tag:superci-pull-deployment,Values="$DEPLOYMENT" Name=instance-state-name,Values=pending,running,stopping,stopped \
    --query 'Reservations[].Instances[].InstanceId' --output text)
  if [ -z "$ids" ] || [ "$ids" = None ]; then echo "no host named $NAME-$az"; return 0; fi
  # shellcheck disable=SC2086
  $A ec2 terminate-instances --instance-ids $ids > /dev/null
  # shellcheck disable=SC2086
  $A ec2 wait instance-terminated --instance-ids $ids
  echo "terminated $ids ($NAME-$az); its identities stay registered at GitHub (offline) and in s3://$BUCKET, and up $az restores them"
}

# ---- status ----

status() {
  mapfile -t HOSTS < <(hosts)
  echo "identity hosts: ${#HOSTS[@]}"
  local line az id ip
  for line in "${HOSTS[@]}"; do
    read -r az id ip <<< "$line"
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
  *) die "usage: identity.sh up [--type T] <az> | register <size> <n> [first] | relabel <size> <from> <to> | down <az> | status" ;;
esac
