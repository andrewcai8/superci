#!/bin/bash
# Creates, or updates, the alarms on the listener's metrics (namespace superci-pull) and their SNS topic. Subscribe
# the operator with: aws --profile ci-runners sns subscribe --topic-arn <topic> --protocol email --notification-endpoint <address>
#
#   pull/deploy/alarms.sh [daily cap in dollars, 1000]
set -euo pipefail
DAILY=${1:-1000}
A="aws --profile ci-runners --region us-east-1"
[ "$($A sts get-caller-identity --query Account --output text)" = 243891825325 ] || { echo "refusing: not ci-runners" >&2; exit 1; }
TOPIC=$($A sns create-topic --name superci-pull-alarms --tags Key=purpose,Value=superci-pull --query TopicArn --output text)
alarm() { # name description metric statistic comparison threshold period evaluations datapoints missing
  local stat=(--statistic "$4")
  [[ $4 == p* ]] && stat=(--extended-statistic "$4")
  $A cloudwatch put-metric-alarm --alarm-name "superci-pull-$1" --alarm-description "$2" --namespace superci-pull --metric-name "$3" \
    "${stat[@]}" --comparison-operator "$5" --threshold "$6" --period "$7" --evaluation-periods "$8" --datapoints-to-alarm "$9" \
    --treat-missing-data "${10}" --alarm-actions "$TOPIC" --ok-actions "$TOPIC" --tags Key=purpose,Value=superci-pull
  echo "alarm superci-pull-$1"
}
# A gap in the heartbeat is filled with zeros: CloudWatch ignores "missing is breaching" while older points remain in
# its evaluation range, so a plain alarm on Heartbeat stayed OK through a 5-minute outage.
$A cloudwatch put-metric-alarm --alarm-name superci-pull-listener-down \
  --alarm-description "No heartbeat from the listener for 3 minutes: the process or its instance is down (systemd and the Auto Scaling group should bring it back)" \
  --metrics '[{"Id":"beat","MetricStat":{"Metric":{"Namespace":"superci-pull","MetricName":"Heartbeat"},"Period":60,"Stat":"Sum"},"ReturnData":false},
              {"Id":"filled","Expression":"FILL(beat, 0)","Label":"Heartbeat, gaps as 0","ReturnData":true}]' \
  --comparison-operator LessThanThreshold --threshold 1 --evaluation-periods 3 --datapoints-to-alarm 3 --treat-missing-data breaching \
  --alarm-actions "$TOPIC" --ok-actions "$TOPIC" --tags Key=purpose,Value=superci-pull
echo "alarm superci-pull-listener-down"
alarm registration-refusals "GitHub refused a runner registration (ConfigureTooManyRunners): the organization reached its cap of 1,500 in 5 minutes" \
  RegistrationRefusals Sum GreaterThanThreshold 0 300 1 1 notBreaching
alarm queue-wait-p90 "Jobs on superci waited over 60 s at p90 in two 5-minute periods running" \
  QueueWait p90 GreaterThanThreshold 60 300 2 2 notBreaching
alarm orphans "Machines told to end still running, runners whose removal keeps failing, or machines of no configured set, for 10 minutes" \
  Orphans Maximum GreaterThanThreshold 0 300 2 2 notBreaching
alarm spend-daily "Today's estimated spend passed 80% of the daily cap ($DAILY)" \
  SpendTodayUSD Maximum GreaterThanThreshold "$(( DAILY * 8 / 10 ))" 300 1 1 notBreaching
alarm spend-capped "A spend cap is reached: the listener launches nothing and keeps no pool until the day or month turns, or the cap is raised" \
  SpendCapped Maximum GreaterThanOrEqualToThreshold 1 300 1 1 notBreaching
alarm org-rate-unknown "The listener has not measured the organization's registration rate for 15 minutes, so it registers at its blind rate only" \
  OrgRateKnown Maximum LessThanThreshold 1 300 3 3 notBreaching
echo "topic: $TOPIC"
