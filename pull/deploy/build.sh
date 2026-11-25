#!/bin/bash
# Builds and tests the listener on a tagged arm64 build instance (no compile runs where this is started), copies the
# binary back, and terminates the instance.
#
#   pull/deploy/build.sh <ssh private key whose public half is the superci-pull key pair> <output binary>
set -euo pipefail
KEY=${1:?ssh key}
OUT=${2:?output path}
A="aws --profile ci-runners"
R=us-east-1
[ "$($A sts get-caller-identity --query Account --output text)" = 243891825325 ] || { echo "refusing: not ci-runners" >&2; exit 1; }
here=$(cd "$(dirname "$0")/.." && pwd)
GO=$(sed -n 's/^go //p' "$here/go.mod")
AMI=$($A ssm get-parameter --region $R --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64 --query Parameter.Value --output text)
SG=$($A ec2 describe-security-groups --region $R --filters Name=group-name,Values=superci-pull-listener --query 'SecurityGroups[0].GroupId' --output text)
ID=$($A ec2 run-instances --region $R --image-id "$AMI" --instance-type c7g.2xlarge --key-name superci-pull --security-group-ids "$SG" \
  --associate-public-ip-address --instance-initiated-shutdown-behavior terminate --metadata-options HttpTokens=required \
  --block-device-mappings 'DeviceName=/dev/xvda,Ebs={VolumeSize=40,VolumeType=gp3,DeleteOnTermination=true}' \
  --tag-specifications "ResourceType=instance,Tags=[{Key=purpose,Value=superci-pull},{Key=Name,Value=superci-pull-build}]" "ResourceType=volume,Tags=[{Key=purpose,Value=superci-pull}]" \
  --user-data "#!/bin/bash
( sleep 3600; poweroff ) &
curl -sfL https://go.dev/dl/go$GO.linux-arm64.tar.gz | tar -C /usr/local -xz && touch /tmp/go-ready" --query 'Instances[0].InstanceId' --output text)
trap '$A ec2 terminate-instances --region $R --instance-ids $ID >/dev/null; echo "terminated $ID"' EXIT
$A ec2 wait instance-running --region $R --instance-ids "$ID"
IP=$($A ec2 describe-instances --region $R --instance-ids "$ID" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
S="ssh -i $KEY -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 ec2-user@$IP"
until $S test -f /tmp/go-ready 2>/dev/null; do sleep 5; done
tar -C "$here/.." -czf - pull | $S 'mkdir -p src && tar -C src -xzf -'
$S 'cd src/pull && export PATH=/usr/local/go/bin:$PATH GOFLAGS=-buildvcs=false && test -z "$(gofmt -l .)" && go vet ./... && go test -count=1 ./... && CGO_ENABLED=0 go build -trimpath -o /tmp/superci-pull .'
$S 'cat /tmp/superci-pull' > "$OUT"
chmod 755 "$OUT"
echo "built $OUT $(sha256sum "$OUT" | cut -c1-12)"
