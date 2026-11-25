#!/bin/bash
# Removes everything superci-pull made: the scale set at GitHub (deleted by the listener with its own credentials),
# then every resource tagged purpose=superci-pull in us-east-1 and us-east-2 (machines, the listener, launch
# templates, security groups, the key pair, the credentials' copy) and the listener's role. Touches nothing else.
#
#   pull/deploy/down.sh <ssh command for the listener, e.g. "ssh -i key ec2-user@1.2.3.4">
set -uo pipefail
SSH=${1:?ssh command for the listener}
A="aws --profile ci-runners"
NAME=superci-pull
[ "$($A sts get-caller-identity --query Account --output text)" = 243891825325 ] || { echo "refusing: not ci-runners" >&2; exit 1; }

$SSH 'sudo systemctl disable --now superci-pull; sudo /usr/local/bin/superci-pull -delete-scale-set'

for r in us-east-1 us-east-2; do
  ids=$($A ec2 describe-instances --region $r --filters Name=tag:purpose,Values=superci-pull Name=instance-state-name,Values=pending,running,stopping,stopped --query 'Reservations[].Instances[].InstanceId' --output text)
  if [ -n "$ids" ]; then
    $A ec2 terminate-instances --region $r --instance-ids $ids >/dev/null
    $A ec2 wait instance-terminated --region $r --instance-ids $ids
  fi
  echo "$r: terminated ${ids:-nothing}"
  for lt in $($A ec2 describe-launch-templates --region $r --filters Name=tag:purpose,Values=superci-pull --query 'LaunchTemplates[].LaunchTemplateId' --output text); do
    $A ec2 delete-launch-template --region $r --launch-template-id "$lt" >/dev/null && echo "$r: deleted launch template $lt"
  done
  for sg in $($A ec2 describe-security-groups --region $r --filters Name=tag:purpose,Values=superci-pull --query 'SecurityGroups[].GroupId' --output text); do
    $A ec2 delete-security-group --region $r --group-id "$sg" && echo "$r: deleted security group $sg"
  done
done
$A ec2 delete-key-pair --region us-east-1 --key-name $NAME >/dev/null && echo "deleted key pair $NAME"
$A ssm delete-parameter --region us-east-1 --name /superci-pull/github-app && echo "deleted /superci-pull/github-app"
$A iam remove-role-from-instance-profile --instance-profile-name $NAME --role-name $NAME
$A iam delete-instance-profile --instance-profile-name $NAME
$A iam delete-role-policy --role-name $NAME --policy-name $NAME
$A iam delete-role --role-name $NAME && echo "deleted role and instance profile $NAME"
