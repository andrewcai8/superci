#!/bin/bash
# Builds one release of a commit (HEAD by default) on a tagged arm64 build instance (no compile runs where this is
# started), and terminates the instance. The source is `git archive` of the commit, so nothing outside it (an edit,
# an ignored file) is built or released. Writes the release directory (mode 700; see release-lib.sh):
#   REVISION                     the commit and the build time
#   listener/bin/superci-pull    the listener, tested first
#   listener/deploy/*            its units and scripts, listener/config/config.json its override (deploy/override.json)
#   cache/bin/pullcache-linux-{amd64,arm64}, cache/bin/pullcache-lambda-arm64.zip, cache/bin/up.sh (cache/deploy/machine-up.sh)
# Each Go binary carries the commit (-X main.revision; -trimpath keeps it out of `go version -m`): `superci-pull
# -version` and `pullcache version` print it. release.sh and cache/deploy/release.sh release it. The instance is
# reached with a key made for the run (EC2 Instance Connect): no key pair.
#
#   pull/deploy/build.sh <new release directory> [commit]
set -euo pipefail
OUT=${1:?release directory}
A="aws --profile ci-runners"
R=us-east-1
[ "$($A sts get-caller-identity --query Account --output text)" = 243891825325 ] || { echo "refusing: not ci-runners" >&2; exit 1; }
[ ! -e "$OUT" ] || { echo "refusing: $OUT exists" >&2; exit 1; }
here=$(cd "$(dirname "$0")/.." && pwd)
REV=$(git -C "$here" rev-parse --verify "${2:-HEAD}^{commit}")
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/src"
git -C "$here/.." archive "$REV" pull | tar -C "$tmp/src" -xf -
src=$tmp/src/pull
[ -f "$src/go.mod" ] || { echo "refusing: $REV has no pull/go.mod" >&2; exit 1; }
GO=$(cat "$src/go.mod" "$src/cache/go.mod" | sed -n 's/^go //p' | sort -V | tail -1)
AMI=$($A ssm get-parameter --region $R --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64 --query Parameter.Value --output text)
SG=$($A ec2 describe-security-groups --region $R --filters Name=group-name,Values=superci-pull-listener --query 'SecurityGroups[0].GroupId' --output text)
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
tar -C "$tmp/src" -czf - pull | $S 'mkdir -p src && tar -C src -xzf -'
$S "set -e; export PATH=/usr/local/go/bin:\$PATH GOFLAGS=-buildvcs=false CGO_ENABLED=0 GOOS=linux; X='-X main.revision=$REV'; mkdir /tmp/out
cd ~/src/pull && test -z \"\$(gofmt -l .)\" && go vet ./... && go test -count=1 ./...
go build -trimpath -ldflags=\"\$X\" -o /tmp/out/superci-pull .
cd cache && for a in amd64 arm64; do GOARCH=\$a go build -trimpath -ldflags=\"-s -w \$X\" -o /tmp/out/pullcache-linux-\$a .; done
mkdir /tmp/fn && GOARCH=arm64 go build -tags lambda.norpc -trimpath -ldflags=\"-s -w \$X\" -o /tmp/fn/bootstrap .
cd /tmp/fn && python3 -m zipfile -c /tmp/out/pullcache-lambda-arm64.zip bootstrap"
push
umask 077
mkdir -p "$OUT/listener/bin" "$OUT/listener/deploy" "$OUT/listener/config" "$OUT/cache/bin" "$tmp/out"
$S 'tar -C /tmp/out -cf - .' | tar -C "$tmp/out" -xf -
install -m 755 "$tmp/out/superci-pull" "$OUT/listener/bin/superci-pull"
for f in pullcache-linux-amd64 pullcache-linux-arm64 pullcache-lambda-arm64.zip; do install -m 755 "$tmp/out/$f" "$OUT/cache/bin/$f"; done
for f in superci-pull.service superci-pull-sync.service superci-pull-sync.timer superci-pull-health.service superci-pull-health.timer sync.sh health.sh cloudwatch-agent.json logrotate.conf; do
  install -m 644 "$src/deploy/$f" "$OUT/listener/deploy/$f"
done
install -m 644 "$src/deploy/override.json" "$OUT/listener/config/config.json"
install -m 644 "$src/cache/deploy/machine-up.sh" "$OUT/cache/bin/up.sh"
printf '{"commit":"%s","built":"%s"}\n' "$REV" "$(date -u +%FT%TZ)" > "$OUT/REVISION"
(cd "$OUT" && find . -type f | LC_ALL=C sort | xargs sha256sum)
echo "built $OUT at $REV"
