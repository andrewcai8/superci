#!/bin/bash
# Creates, or updates, the alarms on the listener's metrics (namespace superci-pull), their SNS topic and the
# dashboard superci-pull. It subscribes no one: give the topic a destination with the command it prints last.
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
alarm registration-refusals "GitHub refused a runner registration (ConfigureTooManyRunners): the organization reached its cap of 1,500 in 5 minutes" \
  RegistrationRefusals Sum GreaterThanThreshold 0 300 1 1 notBreaching
alarm registration-errors "A runner registration failed other than by GitHub's cap: GitHub's API, a runner name GitHub already held, or EC2 refusing the issued tag" \
  RegistrationErrors Sum GreaterThanThreshold 0 300 1 1 notBreaching
alarm queue-wait-p90 "Jobs on superci waited over 60 s at p90 in two 5-minute periods running" \
  QueueWait p90 GreaterThanThreshold 60 300 2 2 notBreaching
alarm pool-at-floor "A set's pool has stayed at the hour's floor for 5 minutes while more of its jobs waited than it had idle runners: the pool reads no arrivals" \
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
alarm org-rate-unknown "The listener has not measured the organization's registration rate for 15 minutes, so it registers at its blind rate only" \
  OrgRateKnown Maximum LessThanThreshold 1 300 3 3 notBreaching
$A cloudwatch put-dashboard --dashboard-name superci-pull --dashboard-body "file://$here/dashboard.json" --query 'DashboardValidationMessages' --output text
echo "dashboard superci-pull: https://us-east-1.console.aws.amazon.com/cloudwatch/home?region=us-east-1#dashboards/dashboard/superci-pull"
echo "topic: $TOPIC"
echo "subscribe a destination: $A sns subscribe --topic-arn $TOPIC --protocol email --notification-endpoint <address>"
