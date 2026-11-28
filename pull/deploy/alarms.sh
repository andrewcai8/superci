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
# The identity alarms read IdentitiesPolling, which only the identity hosts publish, and a host publishes it only once it runs the identity-host.sh that
# does (identity.sh up, then hpush or status: see the README's rollout order). An alarm created before that reads no data for the metric and stays OK
# (identity-none would never fire), so nothing is written unless CloudWatch lists a series of it for every AZ the alarms sum over. IDENTITY_AZS lists
# those AZs (default us-east-1a us-east-1b, as identity.sh reads it). A first install, with no host publishing yet, sets IDENTITY_ALARMS_UNPUBLISHED=1.
read -ra IDENTITY_AZ_LIST <<< "${IDENTITY_AZS:-us-east-1a us-east-1b}"
listed=$($A cloudwatch list-metrics --namespace superci-pull --metric-name IdentitiesPolling --query 'Metrics[].Dimensions[?Name==`AZ`].Value[]' --output text) \
  || { echo "refusing: could not list the IdentitiesPolling series" >&2; exit 1; }
for az in "${IDENTITY_AZ_LIST[@]}"; do
  tr '\t' '\n' <<< "$listed" | grep -qx -- "$az" && continue
  [ "${IDENTITY_ALARMS_UNPUBLISHED:-}" = 1 ] && { echo "warning: no IdentitiesPolling series for $az; IDENTITY_ALARMS_UNPUBLISHED=1 creates the identity alarms anyway" >&2; continue; }
  echo "refusing: no IdentitiesPolling series for $az in superci-pull, so identity-none and identity-starved would read nothing and stay OK. Roll the identity hosts out first (README: identity.sh up, hpush/status, check polling), or on a first install set IDENTITY_ALARMS_UNPUBLISHED=1" >&2
  exit 1
done
TOPIC=$($A sns create-topic --name superci-pull-alarms --tags Key=purpose,Value=superci-pull --query TopicArn --output text)
alarm() { # name description metric statistic comparison threshold period evaluations datapoints missing [set]
  local stat=(--statistic "$4") dim=()
  [[ $4 == p* ]] && stat=(--extended-statistic "$4")
  [ -n "${11:-}" ] && dim=(--dimensions "Name=Set,Value=${11}")
  $A cloudwatch put-metric-alarm --alarm-name "superci-pull-$1" --alarm-description "$2" --namespace superci-pull --metric-name "$3" \
    "${stat[@]}" ${dim[@]+"${dim[@]}"} --comparison-operator "$5" --threshold "$6" --period "$7" --evaluation-periods "$8" --datapoints-to-alarm "$9" \
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
# jobs-stalled and oldest-job-wait below are the listener-wide pages: the worst set's reading, whichever set it is. A canary
# rolls back on its own sets' alarms (jobs-stalled-<size>, oldest-job-wait-<size>, further down) and on listener-down and
# spend-capped, which stop every set, never on another size's wait.
alarm jobs-stalled "A set's jobs have waited 10 minutes with none of its jobs started: launches, registrations or the spend cap are stuck. A page; each set's own is jobs-stalled-<size>" \
  StalledSets Maximum GreaterThanOrEqualToThreshold 1 60 2 2 notBreaching
alarm oldest-job-wait "The job that has waited longest for a runner, of any set, has waited over 60 s, two minutes running: registrations the budget turns away, launches or boots are holding a job (a wait the budget causes is no refusal by GitHub, and QueueWait shows a job only once it starts). A page; each set's own is oldest-job-wait-<size>" \
  OldestWaitingJobAgeS Maximum GreaterThanThreshold 60 60 2 2 notBreaching
alarm registration-refusals "GitHub refused a runner registration (ConfigureTooManyRunners): the organization reached its cap of 1,500 in 5 minutes. A page, never a canary rollback: the cap is the organization's, and a job moved from Blacksmith takes one registration on either fleet" \
  RegistrationRefusals Sum GreaterThanThreshold 0 300 1 1 notBreaching
alarm registration-errors "A runner registration failed other than by GitHub's cap: GitHub's API, a runner name GitHub already held, or EC2 refusing the issued tag" \
  RegistrationErrors Sum GreaterThanThreshold 0 300 1 1 notBreaching
alarm queue-wait-p90 "Jobs on superci waited over 60 s at p90 in two 5-minute periods running" \
  QueueWait p90 GreaterThanThreshold 60 300 2 2 notBreaching
alarm image-refused "A region's pinned machine image was refused at the listener's start (gone, not available, or not owned by us): that region launches nothing and no job runs on it, and the listener never boots another build. Fix the pin (README, Moving the image pin) and release" \
  ImageRefused Maximum GreaterThanOrEqualToThreshold 1 60 2 2 notBreaching
alarm pool-at-floor "A set's pool has stayed at the hour's floor, or at 0, for 5 minutes while more of its jobs waited than it had idle runners: the pool reads no arrivals" \
  PoolsAtFloor Minimum GreaterThanOrEqualToThreshold 1 60 5 5 notBreaching
alarm orphans "Machines told to end still running, runners whose removal keeps failing, or machines of no configured set, for 10 minutes" \
  Orphans Maximum GreaterThanThreshold 0 300 2 2 notBreaching
math spend-daily "Today's estimated spend passed 80% of the daily cap the listener runs with: at the cap it launches nothing and CI on superci stops until 00:00 UTC" \
  '[{"Id":"spent","MetricStat":{"Metric":{"Namespace":"superci-pull","MetricName":"SpendTodayUSD"},"Period":300,"Stat":"Maximum"},"ReturnData":false},
    {"Id":"cap","MetricStat":{"Metric":{"Namespace":"superci-pull","MetricName":"SpendDailyCapUSD"},"Period":300,"Stat":"Maximum"},"ReturnData":false},
    {"Id":"share","Expression":"IF(cap > 0, 100 * spent / cap, 0)","Label":"Spend today, % of the daily cap","ReturnData":true}]' \
  GreaterThanThreshold 80 1 1 notBreaching
alarm spend-capped "A spend cap is reached: every set keeps no pool; a dispatch set still gives each job it holds a machine, and canaries roll back to Blacksmith on this alarm. At 110% of the cap a dispatch set launches nothing and fails at once a job still waiting for a machine; a scale set launches nothing from the cap. Until the day or month turns, or the cap is raised" \
  SpendCapped Maximum GreaterThanOrEqualToThreshold 1 300 1 1 notBreaching
# Spot is thin for the m8azn types, and superci-4cpu asks m8azn on demand before a slower spot type (on_demand_first), outside
# on_demand_share: a long shortage in both regions puts its machines on demand (m8azn.xlarge 0.41 USD an hour against Blacksmith's 0.48).
# 4 vCPU alone ran 16% on demand on Nov 26. The share is over those sets alone (superci-2cpu, spot only, would dilute a fleet-wide
# one), and fewer than 10 of their machines make a share of nothing, so it reads 0.
math on-demand-share "Over 40% of the machines of the sets that ask m8azn on demand only when its spot is short (on_demand_first; the on_demand sizes, superci-4cpu and superci-8cpu since Nov 28, ask on demand first by design and are not read) were on demand for 30 minutes, with at least 10 running: m8azn spot is short, and the rule is paying on-demand prices (m8azn.xlarge: 0.41 USD an hour) with nothing but the daily cap behind it" \
  '[{"Id":"machines","MetricStat":{"Metric":{"Namespace":"superci-pull","MetricName":"OnDemandFirstMachines"},"Period":300,"Stat":"Maximum"},"ReturnData":false},
    {"Id":"share","MetricStat":{"Metric":{"Namespace":"superci-pull","MetricName":"OnDemandFirstShare"},"Period":300,"Stat":"Average"},"ReturnData":false},
    {"Id":"od","Expression":"IF(machines >= 10, share, 0)","Label":"On-demand share of the on-demand-first sets machines","ReturnData":true}]' \
  GreaterThanThreshold 0.4 6 6 notBreaching
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
# A dispatch size's job waits at GitHub for a free identity before its identity's shim brings it to the dispatcher, where
# OldestWaitingJobAgeS and the job's queue time start, so a size short of identities waits unseen by every alarm above. The two
# alarms below read IdentitiesPolling, which each identity host publishes every 30 s (identity-host.sh health): the Runner.Listeners
# of a size holding a session with GitHub's broker, one series a host (dimensions Set and AZ), summed here over IDENTITY_AZS (the
# hosts' AZs, as identity.sh reads them; rerun this script after adding a host in another). They do not read GitHub's REST status
# of a runner, which the listener still publishes as IdentitiesOnline and IdentitiesBusy but which is not to be trusted for a
# session: from 03:05Z on Nov 28 it read up to 620 of 620 identities offline while their listeners kept taking jobs, and
# identity-none rolled a canary back on it.
# A host cannot see a label, though: an identity parked under superci-park has a listener with a session like any other, and the host counts it for the size
# in its name. So both alarms read the lower of two counts, IdentitiesLabeled (the listener's listing of the identities group: how many identities carry the
# size's label, whatever their REST status, since the labels are what that list holds correctly) and IdentitiesPolling summed over the hosts. All parked, or a
# relabel half done, is caught through the labeled count; listeners without a session, or hosts that are down, through the polling count. A listing older than
# 15 minutes publishes no IdentitiesLabeled, and then both alarms read no data and stay OK, as they did before.
# Jobs only count in identity-starved once an identity's shim has brought them, so a size with no identity polling reads 0 jobs: identity-none
# below is the alarm for that. A size that is not dispatched has no DispatchRunning (the listener publishes it for each dispatch set, every
# minute) and stays OK, its hosts' polling never read; so does a size whose hosts publish nothing while the listener does not either.
# polling_metrics <size> <stat>: POLL_METRICS holds one metric a host AZ (ids p0, p1, ...), POLL_SUM their sum, a missing host as 0.
polling_metrics() {
  local i=0 az
  POLL_METRICS=""; POLL_SUM=""
  for az in "${IDENTITY_AZ_LIST[@]}"; do
    POLL_METRICS="$POLL_METRICS"'{"Id":"p'"$i"'","MetricStat":{"Metric":{"Namespace":"superci-pull","MetricName":"IdentitiesPolling","Dimensions":[{"Name":"Set","Value":"superci-'"$1"'"},{"Name":"AZ","Value":"'"$az"'"}]},"Period":60,"Stat":"'"$2"'"},"ReturnData":false},'
    POLL_SUM="$POLL_SUM${POLL_SUM:+ + }FILL(p$i, 0)"
    i=$((i + 1))
  done
}
starved() { # size
  polling_metrics "$1" Minimum
  math "identity-starved-$1" "Superci-$1's running and waiting jobs are over 70% of the lower of its identities labeled for it (IdentitiesLabeled) and polling GitHub's broker (IdentitiesPolling, over the hosts) for 10 minutes: its jobs will wait at GitHub for a free identity, unseen by oldest-job-wait (register more identities, relabel parked ones, or look at the hosts)" \
    '[{"Id":"running","MetricStat":{"Metric":{"Namespace":"superci-pull","MetricName":"DispatchRunning","Dimensions":[{"Name":"Set","Value":"superci-'"$1"'"}]},"Period":60,"Stat":"Maximum"},"ReturnData":false},
      {"Id":"waiting","MetricStat":{"Metric":{"Namespace":"superci-pull","MetricName":"JobsWaiting","Dimensions":[{"Name":"Set","Value":"superci-'"$1"'"}]},"Period":60,"Stat":"Maximum"},"ReturnData":false},
      {"Id":"labeled","MetricStat":{"Metric":{"Namespace":"superci-pull","MetricName":"IdentitiesLabeled","Dimensions":[{"Name":"Set","Value":"superci-'"$1"'"}]},"Period":60,"Stat":"Minimum"},"ReturnData":false},
      '"$POLL_METRICS"'{"Id":"polled","Expression":"'"$POLL_SUM"'","Label":"Identities polling, over the hosts","ReturnData":false},
      {"Id":"online","Expression":"IF(labeled < polled, labeled, polled)","Label":"Identities that carry the label and poll: the lower of the two","ReturnData":false},
      {"Id":"share","Expression":"IF(online > 0, 100 * (FILL(running, 0) + FILL(waiting, 0)) / online, IF(FILL(running, 0) + FILL(waiting, 0) > 0, 1000, 0))","Label":"Running and waiting jobs, % of identities labeled and polling","ReturnData":true}]' \
    GreaterThanThreshold 70 10 10 notBreaching
}
# A size with no identity labeled and polling takes no job: its jobs queue at GitHub, where no shim has brought them, so nothing above counts them. The
# sum is read where the listener publishes the size (running >= 0 is true there and missing elsewhere, where the alarm stays OK; the 1 is
# only for a CloudWatch that answers the false branch, and is no breach); a host that publishes nothing counts 0, so a size whose hosts are down alarms.
none() { # size
  polling_metrics "$1" Minimum
  math "identity-none-$1" "Superci-$1 has no identity that carries its label and polls GitHub's broker for 5 minutes (the lower of IdentitiesLabeled and IdentitiesPolling over the hosts): no job of the size can start, and none is counted by identity-starved or oldest-job-wait (identities parked or removed at GitHub, listeners without a session, or their hosts down)" \
    '[{"Id":"running","MetricStat":{"Metric":{"Namespace":"superci-pull","MetricName":"DispatchRunning","Dimensions":[{"Name":"Set","Value":"superci-'"$1"'"}]},"Period":60,"Stat":"Maximum"},"ReturnData":false},
      {"Id":"labeled","MetricStat":{"Metric":{"Namespace":"superci-pull","MetricName":"IdentitiesLabeled","Dimensions":[{"Name":"Set","Value":"superci-'"$1"'"}]},"Period":60,"Stat":"Minimum"},"ReturnData":false},
      '"$POLL_METRICS"'{"Id":"polled","Expression":"'"$POLL_SUM"'","Label":"Identities polling, over the hosts","ReturnData":false},
      {"Id":"usable","Expression":"IF(running >= 0, IF(labeled < polled, labeled, polled), 1)","Label":"Identities that carry the label and poll: the lower of the two","ReturnData":true}]' \
    LessThanThreshold 1 5 5 notBreaching
}
# One set's own wait and stall, on the Set dimension, with the listener-wide alarms' threshold, periods and missing-data rule. A canary
# reads these for the sets its job ran on (pull/tools/gate/gate.py), so a size with a cold machine or a stuck launch pages
# and rolls back its own jobs and no other size's.
perset() { # size
  alarm "oldest-job-wait-$1" "Superci-$1's longest-waiting job has waited over 60 s, two minutes running: its registrations, launches or boots are holding a job. Rolls back a canary that ran on this set, and no other" \
    OldestWaitingJobAgeS Maximum GreaterThanThreshold 60 60 2 2 notBreaching "superci-$1"
  alarm "jobs-stalled-$1" "Superci-$1's jobs have waited 10 minutes with none of its jobs started: its launches, registrations or the spend cap are stuck. Rolls back a canary that ran on this set, and no other" \
    StalledSets Maximum GreaterThanOrEqualToThreshold 1 60 2 2 notBreaching "superci-$1"
}
for size in 2cpu 4cpu 8cpu 16cpu 32cpu; do starved "$size"; none "$size"; perset "$size"; done
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
