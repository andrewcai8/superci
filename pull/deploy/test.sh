#!/bin/bash
# Vets and tests both Go modules, pull/ (the listener) and pull/cache/ (the cache), on a tagged arm64 instance (no
# compile runs where this is started), prints the results, and terminates the instance. The instance is tagged
# purpose=superci-pull-cache and reached through EC2 Instance Connect with a key made here for the run: no key pair.
#
#   pull/deploy/test.sh
set -euo pipefail
A="aws --profile ci-runners"
R=us-east-1
[ "$($A sts get-caller-identity --query Account --output text)" = 243891825325 ] || { echo "refusing: not ci-runners" >&2; exit 1; }
here=$(cd "$(dirname "$0")/.." && pwd)
GO=$(cat "$here/go.mod" "$here/cache/go.mod" | sed -n 's/^go //p' | sort -V | tail -1)
AMI=$($A ssm get-parameter --region $R --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64 --query Parameter.Value --output text)
SG=$($A ec2 describe-security-groups --region $R --filters Name=group-name,Values=superci-pull-listener --query 'SecurityGroups[0].GroupId' --output text)
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
ssh-keygen -q -t ed25519 -N '' -f "$tmp/key"
ID=$($A ec2 run-instances --region $R --image-id "$AMI" --instance-type c7g.2xlarge --security-group-ids "$SG" \
  --associate-public-ip-address --instance-initiated-shutdown-behavior terminate --metadata-options HttpTokens=required \
  --block-device-mappings 'DeviceName=/dev/xvda,Ebs={VolumeSize=40,VolumeType=gp3,DeleteOnTermination=true}' \
  --tag-specifications "ResourceType=instance,Tags=[{Key=purpose,Value=superci-pull-cache},{Key=Name,Value=superci-pull-cache-test}]" "ResourceType=volume,Tags=[{Key=purpose,Value=superci-pull-cache}]" \
  --user-data "#!/bin/bash
( sleep 3600; poweroff ) &
dnf install -y -q git && curl -sfL https://go.dev/dl/go$GO.linux-arm64.tar.gz | tar -C /usr/local -xz && touch /tmp/go-ready" --query 'Instances[0].InstanceId' --output text)
trap '$A ec2 terminate-instances --region $R --instance-ids $ID >/dev/null; echo "terminated $ID"; rm -rf "$tmp"' EXIT
echo "instance $ID (purpose=superci-pull-cache)"
$A ec2 wait instance-running --region $R --instance-ids "$ID"
read -r IP ZONE < <($A ec2 describe-instances --region $R --instance-ids "$ID" --query 'Reservations[0].Instances[0].[PublicIpAddress,Placement.AvailabilityZone]' --output text)
$A ec2-instance-connect send-ssh-public-key --region $R --instance-id "$ID" --availability-zone "$ZONE" --instance-os-user ec2-user --ssh-public-key "file://$tmp/key.pub" >/dev/null
S="ssh -i $tmp/key -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 ec2-user@$IP"
until $S test -f /tmp/go-ready 2>/dev/null; do sleep 5; $A ec2-instance-connect send-ssh-public-key --region $R --instance-id "$ID" --availability-zone "$ZONE" --instance-os-user ec2-user --ssh-public-key "file://$tmp/key.pub" >/dev/null; done
tar -C "$here/.." -czf - pull | $S 'mkdir -p src && tar -C src -xzf -'
$S 'set -e; export PATH=/usr/local/go/bin:$PATH GOFLAGS=-buildvcs=false; go version
for m in pull pull/cache; do
  echo "== $m"; cd ~/src/$m
  [ -z "$(gofmt -l .)" ] || { echo "gofmt: unformatted: $(gofmt -l .)"; exit 1; }; echo "gofmt: clean"
  go vet ./... && echo "vet: clean"
  go test -count=1 -v ./... > /tmp/test.log 2>&1 || { grep -E "^(--- FAIL|FAIL|panic)" /tmp/test.log; exit 1; }
  echo "tests: $(grep -c "^--- PASS" /tmp/test.log) passed, $(grep -c "^--- FAIL" /tmp/test.log) failed, $(grep -c "^--- SKIP" /tmp/test.log) skipped"
  grep "^ok" /tmp/test.log
done'
