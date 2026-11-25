#!/bin/bash
# Builds one release on a tagged arm64 build instance (no compile runs where this is started), copies it back, and
# terminates the instance: the listener (tested first), the cache's daemons and function, and the machine start
# script, each Go binary carrying the commit it was built from (-X main.revision; -trimpath keeps it out of `go version -m`).
# The commit is HEAD, with -dirty when pull/ has changes git does not hold; release.sh and cache/deploy/release.sh
# release only a clean commit of origin's main. The instance is reached with a key made for the run (EC2 Instance
# Connect): no key pair.
#
#   pull/deploy/build.sh <output directory>   writes superci-pull, pullcache-linux-{amd64,arm64},
#                                             pullcache-lambda-arm64.zip, up.sh and REVISION (commit, build time)
set -euo pipefail
OUT=${1:?output directory}
A="aws --profile ci-runners"
R=us-east-1
[ "$($A sts get-caller-identity --query Account --output text)" = 243891825325 ] || { echo "refusing: not ci-runners" >&2; exit 1; }
here=$(cd "$(dirname "$0")/.." && pwd)
GO=$(cat "$here/go.mod" "$here/cache/go.mod" | sed -n 's/^go //p' | sort -V | tail -1)
REV=$(git -C "$here" rev-parse HEAD)
[ -z "$(git -C "$here" status --porcelain -- .)" ] || REV=$REV-dirty
AMI=$($A ssm get-parameter --region $R --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64 --query Parameter.Value --output text)
SG=$($A ec2 describe-security-groups --region $R --filters Name=group-name,Values=superci-pull-listener --query 'SecurityGroups[0].GroupId' --output text)
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
ssh-keygen -q -t ed25519 -N '' -f "$tmp/key"
ID=$($A ec2 run-instances --region $R --image-id "$AMI" --instance-type c7g.2xlarge --security-group-ids "$SG" \
  --associate-public-ip-address --instance-initiated-shutdown-behavior terminate --metadata-options HttpTokens=required \
  --block-device-mappings 'DeviceName=/dev/xvda,Ebs={VolumeSize=40,VolumeType=gp3,DeleteOnTermination=true}' \
  --tag-specifications "ResourceType=instance,Tags=[{Key=purpose,Value=superci-pull},{Key=Name,Value=superci-pull-build}]" "ResourceType=volume,Tags=[{Key=purpose,Value=superci-pull}]" \
  --user-data "#!/bin/bash
( sleep 3600; poweroff ) &
curl -sfL https://go.dev/dl/go$GO.linux-arm64.tar.gz | tar -C /usr/local -xz && touch /tmp/go-ready" --query 'Instances[0].InstanceId' --output text)
trap '$A ec2 terminate-instances --region $R --instance-ids $ID >/dev/null; echo "terminated $ID"; rm -rf "$tmp"' EXIT
$A ec2 wait instance-running --region $R --instance-ids "$ID"
read -r IP ZONE < <($A ec2 describe-instances --region $R --instance-ids "$ID" --query 'Reservations[0].Instances[0].[PublicIpAddress,Placement.AvailabilityZone]' --output text)
push() { $A ec2-instance-connect send-ssh-public-key --region $R --instance-id "$ID" --availability-zone "$ZONE" --instance-os-user ec2-user --ssh-public-key "file://$tmp/key.pub" >/dev/null; }
S="ssh -i $tmp/key -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 ec2-user@$IP"
until push && $S test -f /tmp/go-ready 2>/dev/null; do sleep 5; done
push
tar -C "$here/.." -czf - pull | $S 'mkdir -p src && tar -C src -xzf -'
$S "set -e; export PATH=/usr/local/go/bin:\$PATH GOFLAGS=-buildvcs=false CGO_ENABLED=0 GOOS=linux; X='-X main.revision=$REV'; mkdir /tmp/out
cd ~/src/pull && test -z \"\$(gofmt -l .)\" && go vet ./... && go test -count=1 ./...
go build -trimpath -ldflags=\"\$X\" -o /tmp/out/superci-pull .
cd cache && for a in amd64 arm64; do GOARCH=\$a go build -trimpath -ldflags=\"-s -w \$X\" -o /tmp/out/pullcache-linux-\$a .; done
mkdir /tmp/fn && GOARCH=arm64 go build -tags lambda.norpc -trimpath -ldflags=\"-s -w \$X\" -o /tmp/fn/bootstrap .
cd /tmp/fn && python3 -m zipfile -c /tmp/out/pullcache-lambda-arm64.zip bootstrap
cp ~/src/pull/cache/deploy/machine-up.sh /tmp/out/up.sh"
push
mkdir -p "$OUT"
$S 'tar -C /tmp/out -cf - .' | tar -C "$OUT" -xf -
printf '{"commit":"%s","built":"%s"}\n' "$REV" "$(date -u +%FT%TZ)" > "$OUT/REVISION"
(cd "$OUT" && sha256sum superci-pull pullcache-linux-amd64 pullcache-linux-arm64 pullcache-lambda-arm64.zip up.sh)
echo "built $OUT at $REV"
