#!/bin/bash
# User data of the listener's instance, which its Auto Scaling group (of one) starts and replaces. It takes the
# listener's Elastic IP (the address machines reach), installs the released binary and units from S3, and starts the
# service. Placeholders are filled by up.sh.
set -euxo pipefail
exec > >(tee /var/log/superci-pull-boot.log) 2>&1
BUCKET=@BUCKET@
T=$(curl -s -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 300' http://169.254.169.254/latest/api/token)
ID=$(curl -s -H "X-aws-ec2-metadata-token: $T" http://169.254.169.254/latest/meta-data/instance-id)
for i in $(seq 1 30); do
  aws ec2 associate-address --region us-east-1 --allocation-id @EIP_ALLOC@ --instance-id "$ID" --allow-reassociation && break
  sleep 5
done
mkdir -p /etc/superci-pull
echo "BUCKET=$BUCKET" > /etc/superci-pull/bucket
aws s3 cp "s3://$BUCKET/bin/superci-pull" /usr/local/bin/superci-pull
chmod 755 /usr/local/bin/superci-pull
aws s3 cp --recursive "s3://$BUCKET/deploy/" /tmp/superci-deploy/
install -m 755 /tmp/superci-deploy/sync.sh /usr/local/bin/superci-pull-sync
install -m 755 /tmp/superci-deploy/health.sh /usr/local/bin/superci-pull-health
install -m 644 /tmp/superci-deploy/*.service /tmp/superci-deploy/*.timer /etc/systemd/system/
aws s3 cp "s3://$BUCKET/config/config.json" /etc/superci-pull/config.json || echo '{}' > /etc/superci-pull/config.json
chmod 644 /etc/superci-pull/config.json
echo "FLAGS=-public-ip=@EIP@ -security-groups=@RUNNER_SGS@" > /etc/superci-pull.env
systemctl daemon-reload
systemctl enable --now superci-pull.service superci-pull-sync.timer superci-pull-health.timer
