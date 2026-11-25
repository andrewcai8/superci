#!/bin/bash
# Removes the pull cache's endpoints, functions, schedules and roles in ci-runners. The buckets (and what they hold)
# stay unless -buckets is given, which empties and deletes them.
set -uo pipefail
A="aws --profile ci-runners"
ACCOUNT=243891825325
[ "$($A sts get-caller-identity --query Account --output text)" = $ACCOUNT ] || { echo "refusing: not the ci-runners account" >&2; exit 1; }
for r in us-east-1 us-east-2; do
  b=superci-pull-cache-$ACCOUNT-$r
  $A events remove-targets --region $r --rule superci-pull-cache-janitor --ids janitor >/dev/null 2>&1
  $A events delete-rule --region $r --name superci-pull-cache-janitor 2>/dev/null
  $A s3api put-bucket-notification-configuration --region $r --bucket $b --notification-configuration '{}' 2>/dev/null
  for f in superci-pull-cache-broker superci-pull-cache-janitor superci-pull-cache-mirror; do
    $A lambda delete-function --region $r --function-name $f 2>/dev/null && echo "$r: deleted function $f"
    $A logs delete-log-group --region $r --log-group-name /aws/lambda/$f 2>/dev/null
  done
  $A s3 rm --region $r "s3://$b-stage" --recursive >/dev/null 2>&1; [ "${1:-}" = -buckets ] && $A s3api delete-bucket --region $r --bucket $b-stage 2>/dev/null
  $A s3api delete-bucket-replication --region $r --bucket $b 2>/dev/null
  for id in $($A ec2 describe-vpc-endpoints --region $r --filters Name=tag:purpose,Values=superci-pull-cache --query 'VpcEndpoints[].VpcEndpointId' --output text); do
    $A ec2 delete-vpc-endpoints --region $r --vpc-endpoint-ids "$id" >/dev/null && echo "$r: deleted endpoint $id"
  done
  if [ "${1:-}" = -buckets ]; then
    $A s3api delete-bucket-policy --bucket $b
    $A s3 rm --region $r "s3://$b" --recursive >/dev/null
    $A s3api list-object-versions --bucket $b --query '[Versions,DeleteMarkers][][].{Key:Key,VersionId:VersionId}' --output json |
      python3 -c 'import json,sys
for o in json.load(sys.stdin) or []: print(o["Key"], o["VersionId"])' | while read -r k v; do $A s3api delete-object --bucket $b --key "$k" --version-id "$v" >/dev/null; done
    $A s3api delete-bucket --region $r --bucket $b && echo "$r: deleted bucket $b"
  fi
done
$A iam remove-role-from-instance-profile --instance-profile-name superci-pull-cache-machine --role-name superci-pull-cache-machine
$A iam delete-instance-profile --instance-profile-name superci-pull-cache-machine
for role in superci-pull-cache-machine superci-pull-cache-writer superci-pull-cache-broker superci-pull-cache-janitor superci-pull-cache-mirror superci-pull-cache-replication; do
  $A iam delete-role-policy --role-name $role --policy-name $role 2>/dev/null
  $A iam delete-role --role-name $role 2>/dev/null && echo "deleted role $role"
done
