#!/bin/bash
# Creates, or finds, every AWS resource superci-pull needs in account ci-runners, each tagged purpose=superci-pull:
# the listener's role and instance profile, a pull-owned copy of the GitHub App credentials, the security groups of
# the listener (us-east-1) and of the runners (both regions), an SSH key pair, and the listener instance itself.
# Run again at will: it changes nothing that exists. Prints ids and addresses, never a secret.
#
#   pull/deploy/up.sh <admin-cidr> <ssh-public-key-file>
set -euo pipefail

ADMIN_CIDR=${1:?admin CIDR for SSH, e.g. 203.0.113.7/32}
SSH_PUB=${2:?ssh public key file}
A="aws --profile ci-runners"
PRIMARY=us-east-1
REGIONS="us-east-1 us-east-2"
NAME=superci-pull
TAGS="Key=purpose,Value=superci-pull"
SOURCE_APP=/superci/82flwf4vz2os/GITHUB_APP
APP_PARAM=/superci-pull/github-app
ACCOUNT=$($A sts get-caller-identity --query Account --output text)
[ "$ACCOUNT" = 243891825325 ] || { echo "refusing: account $ACCOUNT is not ci-runners" >&2; exit 1; }

here=$(cd "$(dirname "$0")" && pwd)

if ! $A iam get-role --role-name $NAME >/dev/null 2>&1; then
  $A iam create-role --role-name $NAME --tags $TAGS --assume-role-policy-document \
    '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}' >/dev/null
fi
$A iam put-role-policy --role-name $NAME --policy-name $NAME --policy-document "file://$here/listener-policy.json"
if ! $A iam get-instance-profile --instance-profile-name $NAME >/dev/null 2>&1; then
  $A iam create-instance-profile --instance-profile-name $NAME --tags $TAGS >/dev/null
  $A iam add-role-to-instance-profile --instance-profile-name $NAME --role-name $NAME
fi
echo "role and instance profile: $NAME"

# The App's credentials, copied into a parameter of our own so the listener does not depend on superci's plane. The
# value goes from one call to the other through a private file and is never printed.
if ! $A ssm get-parameter --region $PRIMARY --name $APP_PARAM >/dev/null 2>&1; then
  tmp=$(mktemp); chmod 600 "$tmp"; trap 'rm -f "$tmp"' EXIT
  $A ssm get-parameter --region $PRIMARY --name $SOURCE_APP --with-decryption --query Parameter.Value --output text > "$tmp"
  $A ssm put-parameter --region $PRIMARY --name $APP_PARAM --type SecureString --tags $TAGS --value "file://$tmp" >/dev/null
  rm -f "$tmp"
fi
echo "app credentials: $APP_PARAM"

sg() { # region name description -> id
  local vpc id
  vpc=$($A ec2 describe-vpcs --region "$1" --filters Name=isDefault,Values=true --query 'Vpcs[0].VpcId' --output text)
  id=$($A ec2 describe-security-groups --region "$1" --filters Name=vpc-id,Values="$vpc" Name=group-name,Values="$2" --query 'SecurityGroups[0].GroupId' --output text)
  if [ "$id" = None ]; then
    id=$($A ec2 create-security-group --region "$1" --vpc-id "$vpc" --group-name "$2" --description "$3" \
      --tag-specifications "ResourceType=security-group,Tags=[{$TAGS}]" --query GroupId --output text)
  fi
  echo "$id"
}

for r in $REGIONS; do echo "runner security group $r: $(sg $r $NAME-runner 'superci-pull runners: egress only')"; done
LSG=$(sg $PRIMARY $NAME-listener 'superci-pull listener: JIT endpoint for runners, SSH for its admin')
$A ec2 authorize-security-group-ingress --region $PRIMARY --group-id "$LSG" --protocol tcp --port 8443 --cidr 0.0.0.0/0 >/dev/null 2>&1 || true
$A ec2 authorize-security-group-ingress --region $PRIMARY --group-id "$LSG" --protocol tcp --port 22 --cidr "$ADMIN_CIDR" >/dev/null 2>&1 || true
echo "listener security group: $LSG"

if ! $A ec2 describe-key-pairs --region $PRIMARY --key-names $NAME >/dev/null 2>&1; then
  $A ec2 import-key-pair --region $PRIMARY --key-name $NAME --public-key-material "fileb://$SSH_PUB" \
    --tag-specifications "ResourceType=key-pair,Tags=[{$TAGS}]" >/dev/null
fi

ID=$($A ec2 describe-instances --region $PRIMARY --filters Name=tag:purpose,Values=superci-pull Name=tag:Name,Values=$NAME-listener \
  Name=instance-state-name,Values=pending,running,stopping,stopped --query 'Reservations[0].Instances[0].InstanceId' --output text)
if [ "$ID" = None ]; then
  AMI=$($A ssm get-parameter --region $PRIMARY --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64 --query Parameter.Value --output text)
  SUBNET=$($A ec2 describe-subnets --region $PRIMARY --filters Name=default-for-az,Values=true Name=availability-zone,Values=us-east-1a --query 'Subnets[0].SubnetId' --output text)
  for i in 1 2 3 4 5 6; do # (a new instance profile takes a few seconds to be usable)
    ID=$($A ec2 run-instances --region $PRIMARY --image-id "$AMI" --instance-type t4g.medium --key-name $NAME \
      --subnet-id "$SUBNET" --security-group-ids "$LSG" --associate-public-ip-address \
      --iam-instance-profile Name=$NAME --metadata-options HttpTokens=required,HttpPutResponseHopLimit=1 \
      --block-device-mappings 'DeviceName=/dev/xvda,Ebs={VolumeSize=30,VolumeType=gp3,DeleteOnTermination=true}' \
      --tag-specifications "ResourceType=instance,Tags=[{$TAGS},{Key=Name,Value=$NAME-listener}]" "ResourceType=volume,Tags=[{$TAGS}]" \
      --query 'Instances[0].InstanceId' --output text 2>/dev/null) && break
    sleep 10
  done
  $A ec2 wait instance-running --region $PRIMARY --instance-ids "$ID"
fi
IP=$($A ec2 describe-instances --region $PRIMARY --instance-ids "$ID" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
echo "listener: $ID at $IP"
