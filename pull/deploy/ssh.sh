#!/bin/bash
# Runs a command (or a shell) on the listener's instance over SSH with a key made for this call alone, pushed with EC2
# Instance Connect: no key pair is kept anywhere. Port 22 of the listener's security group admits the admin address
# up.sh was given.
#
#   pull/deploy/ssh.sh [command]
set -euo pipefail
A="aws --profile ci-runners --region us-east-1"
read -r ID ZONE IP < <($A ec2 describe-instances --filters Name=tag:Name,Values=superci-pull-listener Name=instance-state-name,Values=running \
  --query 'Reservations[0].Instances[0].[InstanceId,Placement.AvailabilityZone,PublicIpAddress]' --output text)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
ssh-keygen -q -t ed25519 -N '' -f "$tmp/key"
$A ec2-instance-connect send-ssh-public-key --instance-id "$ID" --availability-zone "$ZONE" --instance-os-user ec2-user --ssh-public-key "file://$tmp/key.pub" > /dev/null
ssh -q -i "$tmp/key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$tmp/known" -o ConnectTimeout=10 "ec2-user@$IP" "$@"
