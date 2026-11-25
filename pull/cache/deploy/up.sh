#!/bin/bash
# Creates, or finds, what the pull fleet's cache needs in account ci-runners, in us-east-1 and us-east-2, each
# tagged purpose=superci-pull-cache. In each region: an S3 gateway endpoint in the default VPC (the machines' VPC;
# traffic to the bucket never leaves the region and costs no data transfer); an entries bucket (private, encrypted,
# versioning suspended, its entries kept by use, written once per key) and a stage bucket for an upload's blocks; the broker
# function, which gives a job credentials to write its own scope; and the janitor function, run every 15 minutes,
# which keeps the entries under a size and deletes the ones nobody restores; and the mirror function, which copies
# each entry the default branch saves into the other region's bucket, written once there too. Once: the machines'
# role and instance profile (read the entries, call the broker, write only the markers of use under a/), the writer
# role the broker narrows per job, and the broker's and janitor's own roles.
# Run again at will: it changes nothing that exists but the policies, settings and code, which it writes anew.
# Prints ids, never a secret.
#
#   pull/cache/deploy/up.sh          the functions' code is bin/pullcache-lambda-arm64.zip in each region's bucket
#   CAP_GB=1000                      what the janitor keeps of a region's entries at most
#   MAX_IDLE=720h                    an entry nobody restored for this long is deleted whatever the size
#   ISSUER=<iss>                     the issuer the job tokens carry, where it is not GitHub's (https://token.actions.githubusercontent.com):
#                                    kept in each bucket's bin/issuer, which the broker (set here) and every machine's daemon (read by
#                                    machine-up.sh at boot) both take, so the two agree and a later run without ISSUER keeps it.
#                                    ISSUER=default removes it. The refusal in `journalctl -u pullcache` names the issuer to use.
set -euo pipefail

A="aws --profile ci-runners"
ACCOUNT=243891825325
[ "$($A sts get-caller-identity --query Account --output text)" = $ACCOUNT ] || { echo "refusing: not the ci-runners account" >&2; exit 1; }
REGIONS="us-east-1 us-east-2"
TAGS="Key=purpose,Value=superci-pull-cache"
MACHINE=superci-pull-cache-machine
WRITER=superci-pull-cache-writer
BROKER=superci-pull-cache-broker
JANITOR=superci-pull-cache-janitor
MIRROR=superci-pull-cache-mirror
CAP_GB=${CAP_GB:-1000}
MAX_IDLE=${MAX_IDLE:-720h}
here=$(cd "$(dirname "$0")" && pwd)
bucket() { echo "superci-pull-cache-$ACCOUNT-$1"; }
arn() { echo "arn:aws:iam::$ACCOUNT:role/$1"; }
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

# Buckets. S3 replication is not used: it needs versioning and writes over an entry the peer saved. The mirror
# function (below) copies the default branch's entries with If-None-Match instead, so the entries' bucket needs no
# versioning, and it is suspended (a deleted entry frees its bytes at once instead of a day later).
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
  if [ $kind = entries ]; then
    $A s3api delete-bucket-replication --region "$r" --bucket "$b" 2>/dev/null || true
    if [ "$($A s3api get-bucket-versioning --bucket "$b" --query Status --output text)" = Enabled ]; then
      $A s3api put-bucket-versioning --bucket "$b" --versioning-configuration Status=Suspended
    fi
    $A s3api put-bucket-lifecycle-configuration --bucket "$b" --lifecycle-configuration "file://$here/lifecycle.json" >/dev/null
  else
    $A s3api put-bucket-lifecycle-configuration --bucket "$b" --lifecycle-configuration "file://$here/lifecycle-stage.json" >/dev/null
  fi
  # Every write into v1/ that creates an object must carry If-None-Match: *, whoever makes it: an entry is never
  # written over. (CreateMultipartUpload and UploadPartCopy create nothing yet; CompleteMultipartUpload does.)
  once=""
  [ $kind = entries ] && once=',
    { "Sid": "EntriesWrittenOnce", "Effect": "Deny", "Principal": "*", "Action": "s3:PutObject", "Resource": "arn:aws:s3:::'"$b"'/v1/*",
      "Condition": { "Null": { "s3:if-none-match": "true" }, "Bool": { "s3:ObjectCreationOperation": "true" } } }'
  cat > "$tmp/policy.json" <<JSON
{
  "Version": "2012-10-17",
  "Statement": [
    { "Sid": "TlsOnly", "Effect": "Deny", "Principal": "*", "Action": "s3:*",
      "Resource": ["arn:aws:s3:::$b", "arn:aws:s3:::$b/*"], "Condition": { "Bool": { "aws:SecureTransport": "false" } } },
    { "Sid": "MachinesOnlyThroughTheirEndpoint", "Effect": "Deny", "Principal": "*", "Action": "s3:*",
      "Resource": ["arn:aws:s3:::$b", "arn:aws:s3:::$b/*"],
      "Condition": { "ArnEquals": { "aws:PrincipalArn": ["$(arn $MACHINE)", "$(arn $WRITER)"] }, "StringNotEquals": { "aws:SourceVpce": "${VPCE[$r]}" } } }$once
  ]
}
JSON
  $A s3api put-bucket-policy --bucket "$b" --policy "file://$tmp/policy.json"
  echo "bucket $r: $b"
 done
done

B1=$(bucket us-east-1); B2=$(bucket us-east-2); S1=$B1-stage; S2=$B2-stage
trust_service='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"%s"},"Action":"sts:AssumeRole"}]}'
role() { # name trust-document policy-file
  if ! $A iam get-role --role-name "$1" >/dev/null 2>&1; then
    $A iam create-role --role-name "$1" --tags $TAGS --assume-role-policy-document "$2" >/dev/null || return 1
  else
    $A iam update-assume-role-policy --role-name "$1" --policy-document "$2" || return 1
  fi
  $A iam put-role-policy --role-name "$1" --policy-name "$1" --policy-document "file://$3" || return 1
  echo "role $1"
}
logs() { echo "{ \"Sid\": \"Logs\", \"Effect\": \"Allow\", \"Action\": [\"logs:CreateLogStream\", \"logs:PutLogEvents\"], \"Resource\": [\"arn:aws:logs:us-east-1:$ACCOUNT:log-group:/aws/lambda/$1:*\", \"arn:aws:logs:us-east-2:$ACCOUNT:log-group:/aws/lambda/$1:*\"] }"; }

# The machines: read the program and the entries, list the entries (S3 answers a missing key with 403 instead of
# 404 to a role without ListBucket), call the broker, and write the marker of use of an entry they restore (a/, an
# empty object per entry per day, read only by the janitor as a hint of what to evict last). A job's step can take
# these, and what it could do with them is keep an entry alive or let one go first: no entry is ever written.
cat > "$tmp/machine.json" <<JSON
{
  "Version": "2012-10-17",
  "Statement": [
    { "Sid": "Read", "Effect": "Allow", "Action": "s3:GetObject",
      "Resource": ["arn:aws:s3:::$B1/bin/*", "arn:aws:s3:::$B2/bin/*", "arn:aws:s3:::$B1/v1/*", "arn:aws:s3:::$B2/v1/*", "arn:aws:s3:::$B1/a/*", "arn:aws:s3:::$B2/a/*"] },
    { "Sid": "Mark", "Effect": "Allow", "Action": "s3:PutObject", "Resource": ["arn:aws:s3:::$B1/a/*", "arn:aws:s3:::$B2/a/*"] },
    { "Sid": "List", "Effect": "Allow", "Action": "s3:ListBucket", "Resource": ["arn:aws:s3:::$B1", "arn:aws:s3:::$B2"] },
    { "Sid": "Broker", "Effect": "Allow", "Action": "lambda:InvokeFunction",
      "Resource": ["arn:aws:lambda:us-east-1:$ACCOUNT:function:$BROKER", "arn:aws:lambda:us-east-2:$ACCOUNT:function:$BROKER"] }
  ]
}
JSON
role $MACHINE "$(printf "$trust_service" ec2.amazonaws.com)" "$tmp/machine.json"
if ! $A iam get-instance-profile --instance-profile-name $MACHINE >/dev/null 2>&1; then
  $A iam create-instance-profile --instance-profile-name $MACHINE --tags $TAGS >/dev/null
  $A iam add-role-to-instance-profile --instance-profile-name $MACHINE --role-name $MACHINE
fi

cat > "$tmp/broker.json" <<JSON
{ "Version": "2012-10-17", "Statement": [
    { "Sid": "Writer", "Effect": "Allow", "Action": "sts:AssumeRole", "Resource": "$(arn $WRITER)" }, $(logs $BROKER) ] }
JSON
role $BROKER "$(printf "$trust_service" lambda.amazonaws.com)" "$tmp/broker.json"

# The writer: every scope's writes, so it is used only through the broker, whose session policy narrows each job's
# credentials to that job's scope. Nothing else may assume it.
cat > "$tmp/writer.json" <<JSON
{
  "Version": "2012-10-17",
  "Statement": [
    { "Sid": "Entries", "Effect": "Allow", "Action": ["s3:PutObject", "s3:AbortMultipartUpload"], "Resource": ["arn:aws:s3:::$B1/v1/*", "arn:aws:s3:::$B2/v1/*"] },
    { "Sid": "Blocks", "Effect": "Allow", "Action": ["s3:PutObject", "s3:GetObject", "s3:DeleteObject"], "Resource": ["arn:aws:s3:::$S1/u/*", "arn:aws:s3:::$S2/u/*"] }
  ]
}
JSON
for _ in 1 2 3 4 5 6; do # (a new role is not at once a principal another role's trust can name)
  role $WRITER "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Principal\":{\"AWS\":\"$(arn $BROKER)\"},\"Action\":\"sts:AssumeRole\"}]}" "$tmp/writer.json" 2>/dev/null && break
  sleep 10
done
$A iam get-role --role-name $WRITER >/dev/null

# The janitor lists entries and markers and deletes entries, each only if it is still the version it listed (a
# conditional delete: DeleteObject, with the ETag), and deletes what lies under a/ and is not a marker.
cat > "$tmp/janitor.json" <<JSON
{ "Version": "2012-10-17", "Statement": [
    { "Sid": "List", "Effect": "Allow", "Action": "s3:ListBucket", "Resource": ["arn:aws:s3:::$B1", "arn:aws:s3:::$B2"] },
    { "Sid": "Evict", "Effect": "Allow", "Action": "s3:DeleteObject", "Resource": ["arn:aws:s3:::$B1/v1/*", "arn:aws:s3:::$B2/v1/*", "arn:aws:s3:::$B1/a/*", "arn:aws:s3:::$B2/a/*"] }, $(logs $JANITOR) ] }
JSON
role $JANITOR "$(printf "$trust_service" lambda.amazonaws.com)" "$tmp/janitor.json"

# The mirror: read the default branch's entries in either bucket and create them in the other, written once (the
# bucket policy refuses a create without If-None-Match, from this role too). ListBucket so a missing key is 404.
cat > "$tmp/mirror.json" <<JSON
{ "Version": "2012-10-17", "Statement": [
    { "Sid": "Read", "Effect": "Allow", "Action": "s3:GetObject", "Resource": ["arn:aws:s3:::$B1/v1/d/*", "arn:aws:s3:::$B2/v1/d/*"] },
    { "Sid": "List", "Effect": "Allow", "Action": "s3:ListBucket", "Resource": ["arn:aws:s3:::$B1", "arn:aws:s3:::$B2"] },
    { "Sid": "Create", "Effect": "Allow", "Action": ["s3:PutObject", "s3:AbortMultipartUpload"], "Resource": ["arn:aws:s3:::$B1/v1/d/*", "arn:aws:s3:::$B2/v1/d/*"] }, $(logs $MIRROR) ] }
JSON
role $MIRROR "$(printf "$trust_service" lambda.amazonaws.com)" "$tmp/mirror.json"

# The old replication role, from before the regions stopped replicating.
if $A iam get-role --role-name superci-pull-cache-replication >/dev/null 2>&1; then
  $A iam delete-role-policy --role-name superci-pull-cache-replication --policy-name superci-pull-cache-replication 2>/dev/null || true
  $A iam delete-role --role-name superci-pull-cache-replication && echo "deleted role superci-pull-cache-replication"
fi

# function name region env memory timeout
function_up() {
  local name=$1 r=$2 env=$3 mem=$4 timeout=$5 b; b=$(bucket "$2")
  local code="S3Bucket=$b,S3Key=bin/pullcache-lambda-arm64.zip"
  if ! $A s3api head-object --region "$r" --bucket "$b" --key bin/pullcache-lambda-arm64.zip >/dev/null 2>&1; then
    echo "function $name $r: no bin/pullcache-lambda-arm64.zip in $b yet, skipped"; return 1
  fi
  $A logs create-log-group --region "$r" --log-group-name "/aws/lambda/$name" --tags purpose=superci-pull-cache 2>/dev/null || true
  $A logs put-retention-policy --region "$r" --log-group-name "/aws/lambda/$name" --retention-in-days 14
  if $A lambda get-function --region "$r" --function-name "$name" >/dev/null 2>&1; then
    $A lambda update-function-code --region "$r" --function-name "$name" --s3-bucket "$b" --s3-key bin/pullcache-lambda-arm64.zip >/dev/null
    $A lambda wait function-updated-v2 --region "$r" --function-name "$name"
    $A lambda update-function-configuration --region "$r" --function-name "$name" --environment "$env" --memory-size "$mem" --timeout "$timeout" >/dev/null
  else
    for _ in 1 2 3 4 5 6; do # (a new role is not at once usable by Lambda)
      $A lambda create-function --region "$r" --function-name "$name" --runtime provided.al2023 --architectures arm64 --handler bootstrap \
        --role "$(arn "$name")" --code "$code" --environment "$env" --memory-size "$mem" --timeout "$timeout" --tags purpose=superci-pull-cache >/dev/null 2>&1 && break
      sleep 10
    done
  fi
  $A lambda wait function-updated-v2 --region "$r" --function-name "$name"
  echo "function $name $r"
}

case ${ISSUER:-} in *[,{}\ ]*) echo "refusing: ISSUER holds a comma, a brace or a space" >&2; exit 1 ;; esac
for r in $REGIONS; do
  b=$(bucket "$r")
  jwks=""; [ -n "${BROKER_JWKS_URL:-}" ] && jwks=",PULLCACHE_JWKS_URL=$BROKER_JWKS_URL"
  case ${ISSUER:-} in
    "") ;;
    default) $A s3 rm --only-show-errors --region "$r" "s3://$b/bin/issuer" || true ;;
    *) printf '%s' "$ISSUER" | $A s3 cp --only-show-errors --region "$r" - "s3://$b/bin/issuer" ;;
  esac
  iss=$($A s3 cp --only-show-errors --region "$r" "s3://$b/bin/issuer" - 2>/dev/null || true)
  issuer=""; [ -n "$iss" ] && { issuer=",PULLCACHE_ISSUER=$iss"; echo "issuer $r: $iss"; }
  function_up $BROKER "$r" "Variables={PULLCACHE_LAMBDA=broker,PULLCACHE_BUCKET=$b,PULLCACHE_STAGE_BUCKET=$b-stage,PULLCACHE_WRITER_ROLE=$(arn $WRITER),PULLCACHE_DEFAULT_BRANCH=main$jwks$issuer}" 256 10 || continue
  function_up $JANITOR "$r" "Variables={PULLCACHE_LAMBDA=janitor,PULLCACHE_BUCKET=$b,PULLCACHE_CAP_GB=$CAP_GB,PULLCACHE_MAX_IDLE=$MAX_IDLE}" 1024 600 || continue
  rule=$($A events put-rule --region "$r" --name $JANITOR --schedule-expression 'rate(15 minutes)' --tags "$TAGS" --query RuleArn --output text)
  $A lambda add-permission --region "$r" --function-name $JANITOR --statement-id events --action lambda:InvokeFunction \
    --principal events.amazonaws.com --source-arn "$rule" >/dev/null 2>&1 || true
  $A events put-targets --region "$r" --rule $JANITOR --targets "Id=janitor,Arn=arn:aws:lambda:$r:$ACCOUNT:function:$JANITOR" >/dev/null
  echo "janitor $r: every 15 minutes, cap $CAP_GB GB, idle limit $MAX_IDLE"
  peer=us-east-2; [ "$r" = us-east-2 ] && peer=us-east-1
  function_up $MIRROR "$r" "Variables={PULLCACHE_LAMBDA=mirror,PULLCACHE_BUCKET=$b,PULLCACHE_PEER_BUCKET=$(bucket $peer),PULLCACHE_PEER_REGION=$peer}" 256 300 || continue
  $A lambda add-permission --region "$r" --function-name $MIRROR --statement-id s3 --action lambda:InvokeFunction --principal s3.amazonaws.com \
    --source-arn "arn:aws:s3:::$b" --source-account $ACCOUNT >/dev/null 2>&1 || true
  # The bucket's whole notification configuration: this one rule.
  $A s3api put-bucket-notification-configuration --region "$r" --bucket "$b" --notification-configuration \
    "{\"LambdaFunctionConfigurations\":[{\"Id\":\"mirror\",\"LambdaFunctionArn\":\"arn:aws:lambda:$r:$ACCOUNT:function:$MIRROR\",\"Events\":[\"s3:ObjectCreated:*\"],\"Filter\":{\"Key\":{\"FilterRules\":[{\"Name\":\"prefix\",\"Value\":\"v1/d/\"}]}}}]}"
  # How long a save of main's waits for its copy in the other region; a job there misses it until then.
  $A logs put-metric-filter --region "$r" --log-group-name "/aws/lambda/$MIRROR" --filter-name lag --filter-pattern '{ $.ev = "cache.mirror" && $.outcome = "copied" }' \
    --metric-transformations metricName=MirrorLagMs,metricNamespace=superci-pull-cache,metricValue='$.lag_ms',unit=Milliseconds
  $A cloudwatch put-metric-alarm --region "$r" --alarm-name superci-pull-cache-mirror-lag --namespace superci-pull-cache --metric-name MirrorLagMs \
    --statistic Maximum --period 900 --evaluation-periods 1 --threshold 300000 --comparison-operator GreaterThanThreshold --treat-missing-data notBreaching \
    --alarm-description "A save of main's took over 5 minutes to reach the other region (superci-pull-cache-mirror)" --tags "$TAGS"
  echo "mirror $r: the default branch's new entries are copied to $(bucket $peer)"
done
