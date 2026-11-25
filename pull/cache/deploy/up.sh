#!/bin/bash
# Creates, or finds, what the pull fleet's cache needs in account ci-runners, in us-east-1 and us-east-2, each
# tagged purpose=superci-pull-cache: an S3 gateway endpoint in each region's default VPC (the machines' VPC; the
# traffic to the bucket never leaves the region and costs no data transfer), a bucket in each region (private,
# encrypted, versioned, expiring by age, the default branch's entries replicated to the other region), the machines'
# role and instance profile (read and write of the cache's prefixes only, usable only through the endpoint), a
# second, unversioned bucket per region for an upload's blocks, and
# the replication role. Run again at will: it changes nothing that exists but the policies, which it writes anew.
# Prints ids, never a secret.
#
#   pull/cache/deploy/up.sh
set -euo pipefail

A="aws --profile ci-runners"
ACCOUNT=243891825325
[ "$($A sts get-caller-identity --query Account --output text)" = $ACCOUNT ] || { echo "refusing: not the ci-runners account" >&2; exit 1; }
REGIONS="us-east-1 us-east-2"
TAG="purpose=superci-pull-cache"
TAGS="Key=purpose,Value=superci-pull-cache"
ROLE=superci-pull-cache-machine
REPL=superci-pull-cache-replication
here=$(cd "$(dirname "$0")" && pwd)
bucket() { echo "superci-pull-cache-$ACCOUNT-$1"; }
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT

declare -A VPCE
for r in $REGIONS; do
  vpc=$($A ec2 describe-vpcs --region "$r" --filters Name=isDefault,Values=true --query 'Vpcs[0].VpcId' --output text)
  id=$($A ec2 describe-vpc-endpoints --region "$r" --filters Name=vpc-id,Values="$vpc" Name=tag:purpose,Values=superci-pull-cache Name=vpc-endpoint-state,Values=available,pending --query 'VpcEndpoints[0].VpcEndpointId' --output text)
  if [ "$id" = None ]; then
    rtbs=$($A ec2 describe-route-tables --region "$r" --filters Name=vpc-id,Values="$vpc" --query 'RouteTables[].RouteTableId' --output text)
    id=$($A ec2 create-vpc-endpoint --region "$r" --vpc-id "$vpc" --vpc-endpoint-type Gateway --service-name "com.amazonaws.$r.s3" \
      --route-table-ids $rtbs --tag-specifications "ResourceType=vpc-endpoint,Tags=[{$TAGS}]" --query VpcEndpoint.VpcEndpointId --output text)
  fi
  VPCE[$r]=$id
  echo "gateway endpoint $r: $id (default VPC $vpc)"
done

for r in $REGIONS; do
 for kind in entries stage; do
  b=$(bucket "$r"); [ $kind = stage ] && b=$b-stage
  if ! $A s3api head-bucket --bucket "$b" >/dev/null 2>&1; then
    if [ "$r" = us-east-1 ]; then $A s3api create-bucket --region "$r" --bucket "$b" >/dev/null
    else $A s3api create-bucket --region "$r" --bucket "$b" --create-bucket-configuration "LocationConstraint=$r" >/dev/null; fi
  fi
  $A s3api put-bucket-tagging --bucket "$b" --tagging "TagSet=[{$TAGS}]"
  $A s3api put-public-access-block --bucket "$b" --public-access-block-configuration BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
  $A s3api put-bucket-encryption --bucket "$b" --server-side-encryption-configuration '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"},"BucketKeyEnabled":true}]}'
  # (The entries' bucket is versioned, as replication needs; the blocks' is not, so deleting a block frees it.)
  if [ $kind = entries ]; then
    $A s3api put-bucket-versioning --bucket "$b" --versioning-configuration Status=Enabled
    $A s3api put-bucket-lifecycle-configuration --bucket "$b" --lifecycle-configuration "file://$here/lifecycle.json"
  else
    $A s3api put-bucket-lifecycle-configuration --bucket "$b" --lifecycle-configuration "file://$here/lifecycle-stage.json"
  fi
  cat > "$tmp/policy-$r.json" <<JSON
{
  "Version": "2012-10-17",
  "Statement": [
    { "Sid": "TlsOnly", "Effect": "Deny", "Principal": "*", "Action": "s3:*",
      "Resource": ["arn:aws:s3:::$b", "arn:aws:s3:::$b/*"], "Condition": { "Bool": { "aws:SecureTransport": "false" } } },
    { "Sid": "MachinesOnlyThroughTheirEndpoint", "Effect": "Deny", "Principal": "*", "Action": "s3:*",
      "Resource": ["arn:aws:s3:::$b", "arn:aws:s3:::$b/*"],
      "Condition": { "ArnEquals": { "aws:PrincipalArn": "arn:aws:iam::$ACCOUNT:role/$ROLE" }, "StringNotEquals": { "aws:SourceVpce": "${VPCE[$r]}" } } }
  ]
}
JSON
  $A s3api put-bucket-policy --bucket "$b" --policy "file://$tmp/policy-$r.json"
  echo "bucket $r: $b"
 done
done

B1=$(bucket us-east-1); B2=$(bucket us-east-2); S1=$B1-stage; S2=$B2-stage
trust='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"%s"},"Action":"sts:AssumeRole"}]}'
if ! $A iam get-role --role-name $ROLE >/dev/null 2>&1; then
  $A iam create-role --role-name $ROLE --tags $TAGS --assume-role-policy-document "$(printf "$trust" ec2.amazonaws.com)" >/dev/null
fi
# Read and write of the cache's own prefixes in the two buckets; nothing is deleted but a finished upload's blocks.
# (ListBucket has no prefix condition: S3 answers a missing key with 403 instead of 404 to a role without it.)
cat > "$tmp/machine.json" <<JSON
{
  "Version": "2012-10-17",
  "Statement": [
    { "Sid": "Program", "Effect": "Allow", "Action": "s3:GetObject", "Resource": ["arn:aws:s3:::$B1/bin/*", "arn:aws:s3:::$B2/bin/*"] },
    { "Sid": "Entries", "Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"],
      "Resource": ["arn:aws:s3:::$B1/v1/*", "arn:aws:s3:::$B2/v1/*"] },
    { "Sid": "Blocks", "Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"], "Resource": ["arn:aws:s3:::$S1/u/*", "arn:aws:s3:::$S2/u/*"] },
    { "Sid": "List", "Effect": "Allow", "Action": "s3:ListBucket", "Resource": ["arn:aws:s3:::$B1", "arn:aws:s3:::$B2", "arn:aws:s3:::$S1", "arn:aws:s3:::$S2"] }
  ]
}
JSON
$A iam put-role-policy --role-name $ROLE --policy-name $ROLE --policy-document "file://$tmp/machine.json"
if ! $A iam get-instance-profile --instance-profile-name $ROLE >/dev/null 2>&1; then
  $A iam create-instance-profile --instance-profile-name $ROLE --tags $TAGS >/dev/null
  $A iam add-role-to-instance-profile --instance-profile-name $ROLE --role-name $ROLE
fi
echo "machine role and instance profile: $ROLE"

if ! $A iam get-role --role-name $REPL >/dev/null 2>&1; then
  $A iam create-role --role-name $REPL --tags $TAGS --assume-role-policy-document "$(printf "$trust" s3.amazonaws.com)" >/dev/null
fi
cat > "$tmp/repl.json" <<JSON
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow", "Action": ["s3:GetReplicationConfiguration", "s3:ListBucket"], "Resource": ["arn:aws:s3:::$B1", "arn:aws:s3:::$B2"] },
    { "Effect": "Allow", "Action": ["s3:GetObjectVersionForReplication", "s3:GetObjectVersionAcl", "s3:GetObjectVersionTagging"], "Resource": ["arn:aws:s3:::$B1/v1/d/*", "arn:aws:s3:::$B2/v1/d/*"] },
    { "Effect": "Allow", "Action": ["s3:ReplicateObject", "s3:ReplicateTags"], "Resource": ["arn:aws:s3:::$B1/v1/d/*", "arn:aws:s3:::$B2/v1/d/*"] }
  ]
}
JSON
$A iam put-role-policy --role-name $REPL --policy-name $REPL --policy-document "file://$tmp/repl.json"
# (A new role is not usable by S3 at once.)
for src in "$B1 $B2 us-east-1" "$B2 $B1 us-east-2"; do
  set -- $src
  cat > "$tmp/replication.json" <<JSON
{ "Role": "arn:aws:iam::$ACCOUNT:role/$REPL",
  "Rules": [ { "ID": "default-branch", "Status": "Enabled", "Priority": 1, "Filter": { "Prefix": "v1/d/" },
    "DeleteMarkerReplication": { "Status": "Disabled" }, "Destination": { "Bucket": "arn:aws:s3:::$2", "StorageClass": "STANDARD" } } ] }
JSON
  for i in 1 2 3 4 5 6; do
    $A s3api put-bucket-replication --region "$3" --bucket "$1" --replication-configuration "file://$tmp/replication.json" 2>/dev/null && break
    sleep 10
  done
  echo "replication $1 -> $2: v1/d/"
done
