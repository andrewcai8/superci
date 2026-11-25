#!/bin/bash
# Creates, or finds, every AWS resource superci-pull needs in account ci-runners, each tagged purpose=superci-pull:
# the listener's role and instance profile, a pull-owned copy of the GitHub App credentials, the spend parameter, the
# release bucket, the security groups of the listener (us-east-1) and of the runners (both regions), an SSH key pair,
# the listener's Elastic IP, its launch template and its Auto Scaling group of one. Run again at will: it changes
# nothing that exists except the launch template (a new version) and the role's policy. Prints ids, never a secret.
# Release the binary (release.sh) before the group's first instance boots, and run alarms.sh for the alarms.
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
BUCKET=$NAME-$ACCOUNT

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
$A ssm get-parameter --region $PRIMARY --name /superci-pull/spend >/dev/null 2>&1 ||
  $A ssm put-parameter --region $PRIMARY --name /superci-pull/spend --type String --tags $TAGS --value '{}' >/dev/null
echo "parameters: $APP_PARAM /superci-pull/spend (the listener makes /superci-pull/tls)"

if ! $A s3api head-bucket --bucket $BUCKET 2>/dev/null; then
  $A s3api create-bucket --region $PRIMARY --bucket $BUCKET >/dev/null
  $A s3api put-public-access-block --bucket $BUCKET --public-access-block-configuration \
    BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
  $A s3api put-bucket-versioning --bucket $BUCKET --versioning-configuration Status=Enabled
  $A s3api put-bucket-tagging --bucket $BUCKET --tagging 'TagSet=[{Key=purpose,Value=superci-pull}]'
fi
echo "release bucket: s3://$BUCKET"

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

RUNNER_SGS=""
for r in $REGIONS; do
  g=$(sg $r $NAME-runner 'superci-pull runners: egress only')
  echo "runner security group $r: $g"
  RUNNER_SGS="${RUNNER_SGS:+$RUNNER_SGS,}$r=$g"
done
LSG=$(sg $PRIMARY $NAME-listener 'superci-pull listener: JIT endpoint for runners, SSH for its admin')
$A ec2 authorize-security-group-ingress --region $PRIMARY --group-id "$LSG" --protocol tcp --port 8443 --cidr 0.0.0.0/0 >/dev/null 2>&1 || true
$A ec2 authorize-security-group-ingress --region $PRIMARY --group-id "$LSG" --protocol tcp --port 22 --cidr "$ADMIN_CIDR" >/dev/null 2>&1 || true
echo "listener security group: $LSG"

if ! $A ec2 describe-key-pairs --region $PRIMARY --key-names $NAME >/dev/null 2>&1; then
  $A ec2 import-key-pair --region $PRIMARY --key-name $NAME --public-key-material "fileb://$SSH_PUB" \
    --tag-specifications "ResourceType=key-pair,Tags=[{$TAGS}]" >/dev/null
fi

ALLOC=$($A ec2 describe-addresses --region $PRIMARY --filters Name=tag:purpose,Values=superci-pull Name=tag:Name,Values=$NAME-listener --query 'Addresses[0].AllocationId' --output text)
if [ "$ALLOC" = None ]; then
  ALLOC=$($A ec2 allocate-address --region $PRIMARY --domain vpc \
    --tag-specifications "ResourceType=elastic-ip,Tags=[{$TAGS},{Key=Name,Value=$NAME-listener}]" --query AllocationId --output text)
fi
EIP=$($A ec2 describe-addresses --region $PRIMARY --allocation-ids "$ALLOC" --query 'Addresses[0].PublicIp' --output text)
echo "listener address: $EIP ($ALLOC)"

AMI=$($A ssm get-parameter --region $PRIMARY --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64 --query Parameter.Value --output text)
USERDATA=$(sed -e "s|@BUCKET@|$BUCKET|; s|@EIP_ALLOC@|$ALLOC|; s|@EIP@|$EIP|; s|@RUNNER_SGS@|$RUNNER_SGS|" "$here/listener-boot.sh" | base64 -w0)
DATA=$(cat <<JSON
{"ImageId":"$AMI","InstanceType":"c7g.large","KeyName":"$NAME","IamInstanceProfile":{"Name":"$NAME"},
 "SecurityGroupIds":["$LSG"],"UserData":"$USERDATA",
 "MetadataOptions":{"HttpTokens":"required","HttpPutResponseHopLimit":1},
 "BlockDeviceMappings":[{"DeviceName":"/dev/xvda","Ebs":{"VolumeSize":30,"VolumeType":"gp3","DeleteOnTermination":true}}],
 "TagSpecifications":[{"ResourceType":"instance","Tags":[{"Key":"purpose","Value":"superci-pull"},{"Key":"Name","Value":"$NAME-listener"}]},
                      {"ResourceType":"volume","Tags":[{"Key":"purpose","Value":"superci-pull"}]}]}
JSON
)
LT=$($A ec2 describe-launch-templates --region $PRIMARY --filters Name=launch-template-name,Values=$NAME-listener --query 'LaunchTemplates[0].LaunchTemplateId' --output text)
if [ "$LT" = None ]; then
  for i in 1 2 3 4 5 6; do # (a new instance profile takes a few seconds to be usable)
    LT=$($A ec2 create-launch-template --region $PRIMARY --launch-template-name $NAME-listener --launch-template-data "$DATA" \
      --tag-specifications "ResourceType=launch-template,Tags=[{$TAGS}]" --query LaunchTemplate.LaunchTemplateId --output text) && break
    sleep 10
  done
else
  V=$($A ec2 create-launch-template-version --region $PRIMARY --launch-template-id "$LT" --launch-template-data "$DATA" --query LaunchTemplateVersion.VersionNumber --output text)
  $A ec2 modify-launch-template --region $PRIMARY --launch-template-id "$LT" --default-version "$V" >/dev/null
fi
echo "listener launch template: $LT"

SUBNETS=$($A ec2 describe-subnets --region $PRIMARY --filters Name=default-for-az,Values=true \
  Name=availability-zone,Values=us-east-1a,us-east-1b,us-east-1c,us-east-1d,us-east-1f --query 'Subnets[].SubnetId' --output text | tr '\t' ',')
if ! $A s3api head-object --bucket $BUCKET --key bin/superci-pull >/dev/null 2>&1; then
  echo "no release in s3://$BUCKET yet: run release.sh, then this again to start the listener's group"
  exit 0
fi
if [ "$($A autoscaling describe-auto-scaling-groups --region $PRIMARY --auto-scaling-group-names $NAME-listener --query 'length(AutoScalingGroups)' --output text)" = 0 ]; then
  $A autoscaling create-auto-scaling-group --region $PRIMARY --auto-scaling-group-name $NAME-listener \
    --launch-template "LaunchTemplateId=$LT,Version=\$Default" --min-size 1 --max-size 1 --desired-capacity 1 \
    --vpc-zone-identifier "$SUBNETS" --health-check-type EC2 --health-check-grace-period 300 \
    --tags "ResourceId=$NAME-listener,ResourceType=auto-scaling-group,Key=purpose,Value=superci-pull,PropagateAtLaunch=true"
fi
echo "listener group: $NAME-listener (one instance; replaced when unhealthy)"
