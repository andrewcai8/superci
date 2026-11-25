#!/bin/bash
# On the listener instance: builds superci-pull from this checkout, installs it and (re)starts its service. Extra
# arguments are passed to the listener as flags (e.g. -pool=20).
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH=/usr/local/go/bin:$PATH
go build -o /tmp/superci-pull .
sudo install -m 755 /tmp/superci-pull /usr/local/bin/superci-pull
T=$(curl -s -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token)
IP=$(curl -s -H "X-aws-ec2-metadata-token: $T" http://169.254.169.254/latest/meta-data/public-ipv4)
sg() { aws ec2 describe-security-groups --region "$1" --filters Name=group-name,Values=superci-pull-runner --query 'SecurityGroups[0].GroupId' --output text; }
echo "FLAGS=-public-ip=$IP -security-groups=us-east-1=$(sg us-east-1),us-east-2=$(sg us-east-2) $*" | sudo tee /etc/superci-pull.env
sudo install -m 644 deploy/superci-pull.service /etc/systemd/system/superci-pull.service
sudo systemctl daemon-reload
sudo systemctl enable superci-pull >/dev/null 2>&1
sudo systemctl restart superci-pull
