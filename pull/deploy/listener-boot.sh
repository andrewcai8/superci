#!/bin/bash
# User data of the listener's instance, which its Auto Scaling group (of one) starts and replaces. It takes the
# listener's Elastic IP (the address machines reach), installs the released binary and units from S3, and starts the
# service. Placeholders are filled by up.sh.
set -euo pipefail
exec > >(tee /var/log/superci-pull-boot.log) 2>&1
BUCKET=@BUCKET@
T=$(curl -s -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 300' http://169.254.169.254/latest/api/token)
ID=$(curl -s -H "X-aws-ec2-metadata-token: $T" http://169.254.169.254/latest/meta-data/instance-id)
unset T
set -x
# Machines reach the listener at the Elastic IP only: an instance that cannot take it asks to be replaced.
eip=""
for i in $(seq 1 30); do
  aws ec2 associate-address --region us-east-1 --allocation-id @EIP_ALLOC@ --instance-id "$ID" --allow-reassociation && { eip=@EIP@; break; }
  sleep 5
done
if [ -z "$eip" ]; then
  echo "superci-pull: the Elastic IP could not be taken; asking the Auto Scaling group to replace this instance"
  aws autoscaling set-instance-health --region us-east-1 --instance-id "$ID" --health-status Unhealthy
  exit 1
fi
mkdir -p /etc/superci-pull
echo "BUCKET=$BUCKET" > /etc/superci-pull/bucket
echo "$eip" > /etc/superci-pull/eip
aws s3 cp "s3://$BUCKET/bin/superci-pull" /usr/local/bin/superci-pull
chmod 755 /usr/local/bin/superci-pull
aws s3 cp --recursive "s3://$BUCKET/deploy/" /tmp/superci-deploy/
install -m 755 /tmp/superci-deploy/sync.sh /usr/local/bin/superci-pull-sync
install -m 755 /tmp/superci-deploy/health.sh /usr/local/bin/superci-pull-health
install -m 644 /tmp/superci-deploy/*.service /tmp/superci-deploy/*.timer /etc/systemd/system/
# An override the listener would refuse is not installed: the listener starts on the built-in config instead of
# failing every start (sync.sh keeps refusing it until it is fixed).
aws s3 cp "s3://$BUCKET/config/config.json" /tmp/superci-config.json || echo '{}' > /tmp/superci-config.json
if /usr/local/bin/superci-pull -validate -config /tmp/superci-config.json; then
  install -m 644 /tmp/superci-config.json /etc/superci-pull/config.json
else
  echo "superci-pull: the override config in s3://$BUCKET/config/config.json is invalid; starting on the built-in config"
  echo '{}' > /etc/superci-pull/config.json
  chmod 644 /etc/superci-pull/config.json
fi
install -d -m 755 /var/log/superci-pull
install -m 644 /tmp/superci-deploy/logrotate.conf /etc/logrotate.d/superci-pull
install -m 644 /tmp/superci-deploy/cloudwatch-agent.json /etc/superci-pull/cloudwatch-agent.json
echo "FLAGS=-public-ip=@EIP@ -security-groups=@RUNNER_SGS@" > /etc/superci-pull.env
systemctl daemon-reload
systemctl enable --now superci-pull.service superci-pull-sync.timer superci-pull-health.timer
# The log shipper comes after the listener is serving (installing it takes about two minutes), and does not fail the boot.
dnf install -y -q amazon-cloudwatch-agent &&
  /opt/aws/amazon-cloudwatch-agent/bin/amazon-cloudwatch-agent-ctl -a fetch-config -m ec2 -s -c file:/etc/superci-pull/cloudwatch-agent.json || :
