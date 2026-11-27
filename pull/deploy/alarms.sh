#!/bin/bash
# Creates, or updates, the alarms on the listener's metrics (namespace superci-pull), their SNS topic, the daily AWS
# budget that publishes to the same topic, and the dashboard superci-pull. It subscribes no one: give the topic a
# destination with the command it prints last. DAILY_BUDGET_USD (default 600) is the budget's daily amount: it notifies
# at 80% and 100% of what the account's day has actually cost (AWS Budgets reads Cost Explorer, hours behind).
#
#   pull/deploy/alarms.sh
set -euo pipefail
A="aws --profile ci-runners --region us-east-1"
[ "$($A sts get-caller-identity --query Account --output text)" = 243891825325 ] || { echo "refusing: not ci-runners" >&2; exit 1; }
here=$(cd "$(dirname "$0")" && pwd)
TOPIC=$($A sns create-topic --name superci-pull-alarms --tags Key=purpose,Value=superci-pull --query TopicArn --output text)
alarm() { # name description metric statistic comparison threshold period evaluations datapoints missing
  local stat=(--statistic "$4")
  [[ $4 == p* ]] && stat=(--extended-statistic "$4")
  $A cloudwatch put-metric-alarm --alarm-name "superci-pull-$1" --alarm-description "$2" --namespace superci-pull --metric-name "$3" \
    "${stat[@]}" --comparison-operator "$5" --threshold "$6" --period "$7" --evaluation-periods "$8" --datapoints-to-alarm "$9" \
    --treat-missing-data "${10}" --alarm-actions "$TOPIC" --ok-actions "$TOPIC" --tags Key=purpose,Value=superci-pull
  echo "alarm superci-pull-$1"
}
math() { # name description metrics comparison threshold evaluations datapoints missing
  $A cloudwatch put-metric-alarm --alarm-name "superci-pull-$1" --alarm-description "$2" --metrics "$3" \
    --comparison-operator "$4" --threshold "$5" --evaluation-periods "$6" --datapoints-to-alarm "$7" --treat-missing-data "$8" \
    --alarm-actions "$TOPIC" --ok-actions "$TOPIC" --tags Key=purpose,Value=superci-pull
  echo "alarm superci-pull-$1"
}
# A gap in the heartbeat is filled with zeros: CloudWatch ignores "missing is breaching" while older points remain in
# its evaluation range, so a plain alarm on Heartbeat stayed OK through a 5-minute outage.
math listener-down "No heartbeat from the listener for 3 minutes: the process or its instance is down (systemd and the Auto Scaling group should bring it back)" \
  '[{"Id":"beat","MetricStat":{"Metric":{"Namespace":"superci-pull","MetricName":"Heartbeat"},"Period":60,"Stat":"Sum"},"ReturnData":false},
    {"Id":"filled","Expression":"FILL(beat, 0)","Label":"Heartbeat, gaps as 0","ReturnData":true}]' \
  LessThanThreshold 1 3 3 breaching
alarm jobs-stalled "A set's jobs have waited 10 minutes with none of its jobs started: launches, registrations or the spend cap are stuck" \
  StalledSets Maximum GreaterThanOrEqualToThreshold 1 60 2 2 notBreaching
alarm oldest-job-wait "The job that has waited longest for a runner has waited over 60 s, two minutes running: registrations the budget turns away, launches or boots are holding a job (a wait the budget causes is no refusal by GitHub, and QueueWait shows a job only once it starts)" \
  OldestWaitingJobAgeS Maximum GreaterThanThreshold 60 60 2 2 notBreaching
alarm registration-refusals "GitHub refused a runner registration (ConfigureTooManyRunners): the organization reached its cap of 1,500 in 5 minutes. A page, never a canary rollback: the cap is the organization's, and a job moved from Blacksmith takes one registration on either fleet" \
  RegistrationRefusals Sum GreaterThanThreshold 0 300 1 1 notBreaching
alarm registration-errors "A runner registration failed other than by GitHub's cap: GitHub's API, a runner name GitHub already held, or EC2 refusing the issued tag" \
  RegistrationErrors Sum GreaterThanThreshold 0 300 1 1 notBreaching
alarm queue-wait-p90 "Jobs on superci waited over 60 s at p90 in two 5-minute periods running" \
  QueueWait p90 GreaterThanThreshold 60 300 2 2 notBreaching
alarm pool-at-floor "A set's pool has stayed at the hour's floor, or at 0, for 5 minutes while more of its jobs waited than it had idle runners: the pool reads no arrivals" \
  PoolsAtFloor Minimum GreaterThanOrEqualToThreshold 1 60 5 5 notBreaching
alarm orphans "Machines told to end still running, runners whose removal keeps failing, or machines of no configured set, for 10 minutes" \
  Orphans Maximum GreaterThanThreshold 0 300 2 2 notBreaching
math spend-daily "Today's estimated spend passed 80% of the daily cap the listener runs with: at the cap it launches nothing and CI on superci stops until 00:00 UTC" \
  '[{"Id":"spent","MetricStat":{"Metric":{"Namespace":"superci-pull","MetricName":"SpendTodayUSD"},"Period":300,"Stat":"Maximum"},"ReturnData":false},
    {"Id":"cap","MetricStat":{"Metric":{"Namespace":"superci-pull","MetricName":"SpendDailyCapUSD"},"Period":300,"Stat":"Maximum"},"ReturnData":false},
    {"Id":"share","Expression":"IF(cap > 0, 100 * spent / cap, 0)","Label":"Spend today, % of the daily cap","ReturnData":true}]' \
  GreaterThanThreshold 80 1 1 notBreaching
alarm spend-capped "A spend cap is reached: the listener launches nothing and keeps no pool until the day or month turns, or the cap is raised" \
  SpendCapped Maximum GreaterThanOrEqualToThreshold 1 300 1 1 notBreaching
alarm launch-shortfall "EC2 started fewer machines than asked for 15 minutes running (spot capacity, or a quota: vCPUs, gp3 storage)" \
  LaunchShortfall Sum GreaterThanThreshold 0 300 3 3 notBreaching
alarm org-rate-unknown "The listener has not measured the organization's registration rate for 15 minutes, so the estimate gauges nothing (admission does not read it)" \
  OrgRateKnown Maximum LessThanThreshold 1 300 3 3 notBreaching
# Harm to jobs, in the job.record counts the listener publishes. Baseline, Nov 26: 4.5% of finished jobs failed over 24 h
# (217 of 4,786 with 587 cancelled), about 230 jobs an hour; a fifth failing over 15 minutes is four times that. Quiet
# hours have under 20 jobs in 15 minutes, so a rate means nothing there and reads 0.
math job-failure-rate "Over 20% of the jobs that finished in 15 minutes failed, two such periods of three, with at least 20 finishing: a plane fault (machines lost, a bad release, a registration failing) fails jobs that tests do not. The baseline is 4.5%" \
  '[{"Id":"failed","MetricStat":{"Metric":{"Namespace":"superci-pull","MetricName":"JobsCompleted","Dimensions":[{"Name":"Result","Value":"failure"}]},"Period":900,"Stat":"Sum"},"ReturnData":false},
    {"Id":"passed","MetricStat":{"Metric":{"Namespace":"superci-pull","MetricName":"JobsCompleted","Dimensions":[{"Name":"Result","Value":"success"}]},"Period":900,"Stat":"Sum"},"ReturnData":false},
    {"Id":"rate","Expression":"IF(failed + passed >= 20, 100 * failed / (failed + passed), 0)","Label":"Failed, % of failed + succeeded jobs","ReturnData":true}]' \
  GreaterThanThreshold 20 3 2 notBreaching
# Money spent on machines while nothing is routed to them: every started job is a QueueWait sample, and the idle part of the
# hour's spend is the machines with no job. $8 an hour is about 20 idle 4-vCPU machines; the pool's floor overnight costs
# about $1 an hour in all.
math idle-spend-unrouted "Over 8 USD an hour of estimated spend sat on machines with no job for 30 minutes, and no job started in that time: the pool or its machines are kept for work that is not coming, or jobs are not reaching them (jobs-stalled and oldest-job-wait say if jobs wait)" \
  '[{"Id":"idle","MetricStat":{"Metric":{"Namespace":"superci-pull","MetricName":"SpendIdlePerHourUSD"},"Period":300,"Stat":"Average"},"ReturnData":false},
    {"Id":"started","MetricStat":{"Metric":{"Namespace":"superci-pull","MetricName":"QueueWait"},"Period":300,"Stat":"SampleCount"},"ReturnData":false},
    {"Id":"unrouted","Expression":"IF(FILL(started, 0) < 1 AND idle > 8, idle, 0)","Label":"Idle spend an hour with no job started","ReturnData":true}]' \
  GreaterThanThreshold 8 6 6 notBreaching
# The daily AWS budget publishes to the topic the alarms use. A topic accepts a budget's message only with a policy that
# names the service, which is added to the topic's own policy once.
POLICY=$($A sns get-topic-attributes --topic-arn "$TOPIC" --query Attributes.Policy --output text)
NEWPOLICY=$(python3 - "$TOPIC" "$POLICY" <<'E'
import json, sys
topic, policy = sys.argv[1], json.loads(sys.argv[2])
acct = topic.split(":")[4]
if not any(s.get("Sid") == "AllowBudgets" for s in policy["Statement"]):
    policy["Statement"].append({"Sid": "AllowBudgets", "Effect": "Allow", "Principal": {"Service": "budgets.amazonaws.com"}, "Action": "SNS:Publish", "Resource": topic,
                                "Condition": {"StringEquals": {"aws:SourceAccount": acct}}})
print(json.dumps(policy))
E
)
[ "$NEWPOLICY" = "$(python3 -c 'import json,sys; print(json.dumps(json.loads(sys.argv[1])))' "$POLICY")" ] || $A sns set-topic-attributes --topic-arn "$TOPIC" --attribute-name Policy --attribute-value "$NEWPOLICY"
ACCOUNT=243891825325
BUDGET=$(python3 -c 'import json,os; print(json.dumps({"BudgetName": "superci-pull-daily", "BudgetLimit": {"Amount": os.environ.get("DAILY_BUDGET_USD", "600"), "Unit": "USD"}, "TimeUnit": "DAILY", "BudgetType": "COST"}))')
NOTES=$(python3 - "$TOPIC" <<'E'
import json, sys
print(json.dumps([{"Notification": {"NotificationType": "ACTUAL", "ComparisonOperator": "GREATER_THAN", "Threshold": t, "ThresholdType": "PERCENTAGE"},
                   "Subscribers": [{"SubscriptionType": "SNS", "Address": sys.argv[1]}]} for t in (80, 100)]))
E
)
if $A budgets describe-budget --account-id $ACCOUNT --budget-name superci-pull-daily > /dev/null 2>&1; then
  $A budgets update-budget --account-id $ACCOUNT --new-budget "$BUDGET"
  for t in 80 100; do # a notification that exists answers DuplicateRecordException, which is what the rerun wants
    out=$($A budgets create-notification --account-id $ACCOUNT --budget-name superci-pull-daily --notification "NotificationType=ACTUAL,ComparisonOperator=GREATER_THAN,Threshold=$t,ThresholdType=PERCENTAGE" \
      --subscribers "SubscriptionType=SNS,Address=$TOPIC" 2>&1) || case $out in *DuplicateRecordException*) ;; *) echo "budget notification at $t%: $out" >&2; exit 1 ;; esac
  done
else
  $A budgets create-budget --account-id $ACCOUNT --budget "$BUDGET" --notifications-with-subscribers "$NOTES"
fi
echo "budget superci-pull-daily: ${DAILY_BUDGET_USD:-600} USD a day, notifying $TOPIC at 80% and 100% of the day's actual cost"
$A cloudwatch put-dashboard --dashboard-name superci-pull --dashboard-body "file://$here/dashboard.json" --query 'DashboardValidationMessages' --output text
echo "dashboard superci-pull: https://us-east-1.console.aws.amazon.com/cloudwatch/home?region=us-east-1#dashboards/dashboard/superci-pull"
echo "topic: $TOPIC"
echo "subscribe a destination: $A sns subscribe --topic-arn $TOPIC --protocol email --notification-endpoint <address>"
