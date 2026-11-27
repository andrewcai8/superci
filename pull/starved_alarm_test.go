package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// putAlarm is one put-metric-alarm call of alarms.sh, with its flags by name.
type putAlarm map[string]string

// starvedAlarms runs alarms.sh up to its budget, with an aws that records each put-metric-alarm instead of calling AWS.
func starvedAlarms(t *testing.T) map[string]putAlarm {
	t.Helper()
	raw, err := os.ReadFile("deploy/alarms.sh")
	if err != nil {
		t.Fatal(err)
	}
	head, _, ok := strings.Cut(string(raw), "# The daily AWS budget")
	if !ok {
		t.Fatal("alarms.sh has no budget section to cut at")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "calls")
	stub := `#!/bin/bash
case "$*" in
  *"sts get-caller-identity"*) echo 243891825325 ;;
  *"sns create-topic"*) echo arn:aws:sns:us-east-1:243891825325:superci-pull-alarms ;;
  *put-metric-alarm*) printf '%s\0' "$@" >> "$CALLS"; printf '\x1e' >> "$CALLS" ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "alarms.sh")
	if err := os.WriteFile(script, []byte(head), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "CALLS="+out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("alarms.sh: %v\n%s", err, b)
	}
	calls, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	alarms := map[string]putAlarm{}
	for _, rec := range strings.Split(string(calls), "\x1e") {
		args := strings.Split(strings.TrimSuffix(rec, "\x00"), "\x00")
		a := putAlarm{}
		for i := 0; i+1 < len(args); i++ {
			if strings.HasPrefix(args[i], "--") {
				a[args[i]] = args[i+1]
			}
		}
		if a["--alarm-name"] != "" {
			alarms[a["--alarm-name"]] = a
		}
	}
	return alarms
}

func TestEachDispatchSizeHasAnAlarmOnItsRunningAndWaitingJobsAgainstItsIdentitiesOnline(t *testing.T) {
	alarms := starvedAlarms(t)
	for _, size := range []string{"2cpu", "4cpu", "8cpu", "16cpu", "32cpu"} {
		a := alarms["superci-pull-identity-starved-"+size]
		if a == nil {
			t.Errorf("no alarm for superci-%s", size)
			continue
		}
		if a["--comparison-operator"] != "GreaterThanThreshold" || a["--threshold"] != "70" || a["--evaluation-periods"] != "10" || a["--datapoints-to-alarm"] != "10" || a["--treat-missing-data"] != "notBreaching" {
			t.Errorf("%s: %v, want over 70 for 10 of 10 one-minute periods, missing data not breaching", size, a)
		}
		var metrics []struct {
			ID         string
			Expression string
			MetricStat struct {
				Metric struct {
					MetricName string
					Dimensions []struct{ Name, Value string }
				}
				Period int
			}
			ReturnData bool
		}
		if err := json.Unmarshal([]byte(a["--metrics"]), &metrics); err != nil {
			t.Fatalf("%s: metrics are not JSON: %v\n%s", size, err, a["--metrics"])
		}
		seen := map[string]string{}
		returned := 0
		for _, m := range metrics {
			if m.ReturnData {
				returned++
			}
			if m.Expression != "" {
				continue
			}
			d := m.MetricStat.Metric.Dimensions
			if len(d) != 1 || d[0].Name != "Set" || d[0].Value != "superci-"+size || m.MetricStat.Period != 60 {
				t.Errorf("%s: %s reads %+v, want the Set superci-%s at 60 s", size, m.ID, m.MetricStat, size)
			}
			seen[m.ID] = m.MetricStat.Metric.MetricName
		}
		if fmt.Sprint(seen) != "map[online:IdentitiesOnline running:DispatchRunning waiting:JobsWaiting]" || returned != 1 {
			t.Errorf("%s: reads %v, returns %d series", size, seen, returned)
		}
	}
}

func TestEachDispatchSizeHasAnAlarmForHavingNoIdentityOnline(t *testing.T) {
	alarms := starvedAlarms(t)
	for _, size := range []string{"2cpu", "4cpu", "8cpu", "16cpu", "32cpu"} {
		a := alarms["superci-pull-identity-none-"+size]
		if a == nil {
			t.Errorf("no identity-none alarm for superci-%s", size)
			continue
		}
		if a["--comparison-operator"] != "LessThanThreshold" || a["--threshold"] != "1" || a["--evaluation-periods"] != "5" || a["--datapoints-to-alarm"] != "5" || a["--treat-missing-data"] != "notBreaching" {
			t.Errorf("%s: %v, want under 1 for 5 of 5 one-minute periods, missing data not breaching", size, a)
		}
		var metrics []struct {
			ID         string
			Expression string
			MetricStat struct {
				Metric struct {
					Namespace, MetricName string
					Dimensions            []struct{ Name, Value string }
				}
				Period int
				Stat   string
			}
			ReturnData bool
		}
		if err := json.Unmarshal([]byte(a["--metrics"]), &metrics); err != nil {
			t.Fatalf("%s: metrics are not JSON: %v\n%s", size, err, a["--metrics"])
		}
		if len(metrics) != 1 || !metrics[0].ReturnData || metrics[0].Expression != "" {
			t.Fatalf("%s: want the one metric alone, returned: %s", size, a["--metrics"])
		}
		m := metrics[0].MetricStat
		d := m.Metric.Dimensions
		if m.Metric.Namespace != "superci-pull" || m.Metric.MetricName != "IdentitiesOnline" || len(d) != 1 || d[0].Name != "Set" || d[0].Value != "superci-"+size || m.Period != 60 || m.Stat != "Minimum" {
			t.Errorf("%s: reads %+v, want IdentitiesOnline of the Set superci-%s, Minimum over 60 s", size, m, size)
		}
		if got := fmt.Sprint(alarms["superci-pull-identity-starved-"+size]["--threshold"]); got != "70" {
			t.Errorf("%s: the 70%% alarm changed: threshold %s", size, got)
		}
	}
}

// A canary rolls back on the wait and the stall of the sets its job ran on, so each size has its own alarm of each, on the Set
// dimension, with the listener-wide alarm's threshold, periods and missing-data rule (which still pages, over every set).
func TestEachSizeHasItsOwnOldestJobWaitAndJobsStalledAlarmOnTheSetDimension(t *testing.T) {
	alarms := starvedAlarms(t)
	for _, c := range []struct{ alarm, metric string }{{"oldest-job-wait", "OldestWaitingJobAgeS"}, {"jobs-stalled", "StalledSets"}} {
		global := alarms["superci-pull-"+c.alarm]
		if global == nil || global["--dimensions"] != "" || global["--metric-name"] != c.metric {
			t.Fatalf("the listener-wide %s must stay, on %s with no dimension: %v", c.alarm, c.metric, global)
		}
		for _, size := range []string{"2cpu", "4cpu", "8cpu", "16cpu", "32cpu"} {
			a := alarms["superci-pull-"+c.alarm+"-"+size]
			if a == nil {
				t.Errorf("no %s alarm for superci-%s", c.alarm, size)
				continue
			}
			if a["--metric-name"] != c.metric || a["--dimensions"] != "Name=Set,Value=superci-"+size || a["--namespace"] != "superci-pull" {
				t.Errorf("%s-%s reads %s %s in %s, want %s of the Set superci-%s", c.alarm, size, a["--metric-name"], a["--dimensions"], a["--namespace"], c.metric, size)
			}
			for _, f := range []string{"--statistic", "--comparison-operator", "--threshold", "--period", "--evaluation-periods", "--datapoints-to-alarm", "--treat-missing-data", "--alarm-actions", "--ok-actions"} {
				if a[f] != global[f] {
					t.Errorf("%s-%s %s is %q, the listener-wide alarm's is %q", c.alarm, size, f, a[f], global[f])
				}
			}
		}
	}
}
