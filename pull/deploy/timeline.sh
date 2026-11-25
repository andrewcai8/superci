#!/bin/bash
# What the job timeline reads that the listener does not write: EC2's own state-change notifications (pending, running,
# shutting-down, terminated, with the time EC2 made each change) in the log group /superci-pull/ec2-state of each region,
# and the Logs Insights queries saved from the "Timeline:" widgets of dashboard.json (alarms.sh puts the dashboard
# itself). Run again at will: it changes nothing that exists but the saved queries.
#
#   pull/deploy/timeline.sh
set -euo pipefail
A="aws --profile ci-runners"
[ "$($A sts get-caller-identity --query Account --output text)" = 243891825325 ] || { echo "refusing: not ci-runners" >&2; exit 1; }
here=$(cd "$(dirname "$0")" && pwd)
ACCOUNT=243891825325
GROUP=/superci-pull/ec2-state
for r in us-east-1 us-east-2; do
  if [ "$($A logs describe-log-groups --region $r --log-group-name-prefix $GROUP --query 'length(logGroups)' --output text)" = 0 ]; then
    $A logs create-log-group --region $r --log-group-name $GROUP --tags purpose=superci-pull
    $A logs put-retention-policy --region $r --log-group-name $GROUP --retention-in-days 30
  fi
  # EventBridge may write to the group only with a resource policy that names it.
  $A logs put-resource-policy --region $r --policy-name superci-pull-ec2-state --policy-document "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Sid\":\"EventsToLogs\",\"Effect\":\"Allow\",\"Principal\":{\"Service\":[\"events.amazonaws.com\",\"delivery.logs.amazonaws.com\"]},\"Action\":[\"logs:CreateLogStream\",\"logs:PutLogEvents\"],\"Resource\":\"arn:aws:logs:$r:$ACCOUNT:log-group:$GROUP:*\"}]}" > /dev/null
  $A events put-rule --region $r --name superci-pull-ec2-state --state ENABLED --tags Key=purpose,Value=superci-pull \
    --description "EC2 state changes, for the superci-pull job timeline" \
    --event-pattern '{"source":["aws.ec2"],"detail-type":["EC2 Instance State-change Notification"],"detail":{"state":["pending","running","shutting-down","terminated"]}}' > /dev/null
  $A events put-targets --region $r --rule superci-pull-ec2-state --targets "Id=log,Arn=arn:aws:logs:$r:$ACCOUNT:log-group:$GROUP" > /dev/null
  echo "$r: rule superci-pull-ec2-state -> $GROUP"
done

# CloudWatch keeps the listener's log 30 days and a speed claim spans months: `timeline export --day D` copies a day of
# each group to this bucket (gzipped JSON lines, the log's own form; derive tables from them with a batch query).
EXPORT=superci-pull-timeline-$ACCOUNT
if ! $A s3api head-bucket --bucket $EXPORT 2> /dev/null; then
  $A s3api create-bucket --bucket $EXPORT --region us-east-1 > /dev/null
  $A s3api put-public-access-block --bucket $EXPORT --public-access-block-configuration BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
  $A s3api put-bucket-tagging --bucket $EXPORT --tagging 'TagSet=[{Key=purpose,Value=superci-pull}]'
fi
POLICY=$(python3 - "$EXPORT" "$ACCOUNT" <<'E'
import json, sys
b, acct = sys.argv[1:]
svc = ["logs.us-east-1.amazonaws.com", "logs.us-east-2.amazonaws.com"]
print(json.dumps({"Version": "2012-10-17", "Statement": [
    {"Effect": "Allow", "Principal": {"Service": svc}, "Action": "s3:GetBucketAcl", "Resource": f"arn:aws:s3:::{b}",
     "Condition": {"StringEquals": {"aws:SourceAccount": acct}}},
    {"Effect": "Allow", "Principal": {"Service": svc}, "Action": "s3:PutObject", "Resource": f"arn:aws:s3:::{b}/*",
     "Condition": {"StringEquals": {"s3:x-amz-acl": "bucket-owner-full-control", "aws:SourceAccount": acct}}}]}))
E
)
$A s3api put-bucket-policy --bucket $EXPORT --policy "$POLICY"
echo "export bucket: s3://$EXPORT"

# The queries of the Timeline widgets, saved where Logs Insights lists them (us-east-1, the listener's group).
python3 - "$here/dashboard.json" <<'E' | while IFS=$'\t' read -r name query; do
import json, sys
for w in json.load(open(sys.argv[1]))["widgets"]:
    if w["type"] == "log" and w["properties"]["title"].startswith("Timeline:"):
        print(w["properties"]["title"].replace("Timeline: ", "superci-pull timeline/"), w["properties"]["query"].split(" | ", 1)[1], sep="\t")
E
  id=$($A logs describe-query-definitions --region us-east-1 --query-definition-name-prefix "$name" --query "queryDefinitions[?name=='$name']|[0].queryDefinitionId" --output text)
  [ "$id" = None ] && id=
  $A logs put-query-definition --region us-east-1 --name "$name" ${id:+--query-definition-id "$id"} --log-group-names /superci-pull/listener --query-string "$query" > /dev/null
  echo "saved query: $name"
done
