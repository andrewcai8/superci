package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// putAlarm is one put-metric-alarm call of alarms.sh, with its flags by name.
type putAlarm map[string]string

// starvedAlarms runs alarms.sh up to its budget with every AZ it sums over publishing IdentitiesPolling, and fails if it refuses.
func starvedAlarms(t *testing.T) map[string]putAlarm {
	t.Helper()
	azs := os.Getenv("IDENTITY_AZS")
	if azs == "" {
		azs = "us-east-1a us-east-1b"
	}
	alarms, out, err := runAlarms(t, strings.Join(strings.Fields(azs), "\t"))
	if err != nil {
		t.Fatalf("alarms.sh: %v\n%s", err, out)
	}
	return alarms
}

// runAlarms runs alarms.sh up to its budget, with an aws that records each put-metric-alarm instead of calling AWS, and
// answers list-metrics for IdentitiesPolling with the AZs listed (tab separated, as --output text prints them; "FAIL" makes it fail).
func runAlarms(t *testing.T, listed string, env ...string) (map[string]putAlarm, string, error) {
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
  *"cloudwatch list-metrics"*"--metric-name IdentitiesPolling"*) [ "$LISTED" = FAIL ] && exit 255; printf '%s\n' "$LISTED" ;;
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
	cmd.Env = append(append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "CALLS="+out, "LISTED="+listed), env...)
	b, runErr := cmd.CombinedOutput()
	calls, _ := os.ReadFile(out) // none when it refused before any alarm
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
	return alarms, string(b), runErr
}

// alarmMetric is one entry of an alarm's --metrics.
type alarmMetric struct {
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

func alarmMetrics(t *testing.T, name string, a putAlarm) []alarmMetric {
	t.Helper()
	var metrics []alarmMetric
	if err := json.Unmarshal([]byte(a["--metrics"]), &metrics); err != nil {
		t.Fatalf("%s: metrics are not JSON: %v\n%s", name, err, a["--metrics"])
	}
	return metrics
}

// The listener counts identities from GitHub's runner list, which said "offline" for up to 620 of 620 whose listeners were taking
// jobs (Nov 28 from 03:05Z: IdentitiesOnline read 0 and an operator rolled a canary back on it). The two alarms on identities read
// what the hosts publish instead: IdentitiesPolling, the Runner.Listeners holding a session with GitHub's broker, one series a host
// (Set and AZ), summed over the hosts' AZs.
func TestEachDispatchSizeHasAnAlarmOnItsRunningAndWaitingJobsAgainstItsPollingIdentities(t *testing.T) {
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
		seen := map[string]string{}
		returned := 0
		for _, m := range alarmMetrics(t, size, a) {
			if m.ReturnData {
				returned++
			}
			if m.Expression != "" {
				if strings.Contains(m.Expression, "IdentitiesOnline") {
					t.Errorf("%s: %s reads GitHub's REST status: %s", size, m.ID, m.Expression)
				}
				continue
			}
			d := m.MetricStat.Metric.Dimensions
			if m.MetricStat.Period != 60 || len(d) < 1 || d[0].Name != "Set" || d[0].Value != "superci-"+size {
				t.Errorf("%s: %s reads %+v, want the Set superci-%s at 60 s", size, m.ID, m.MetricStat, size)
			}
			if m.MetricStat.Metric.MetricName == "IdentitiesLabeled" && (len(d) != 1 || m.MetricStat.Stat != "Minimum") {
				t.Errorf("%s: %s reads %+v, want IdentitiesLabeled of the Set alone, Minimum", size, m.ID, m.MetricStat)
			}
			if m.MetricStat.Metric.MetricName == "IdentitiesPolling" {
				if len(d) != 2 || d[1].Name != "AZ" || m.MetricStat.Stat != "Minimum" {
					t.Errorf("%s: %s reads %+v, want IdentitiesPolling of a Set and an AZ, Minimum", size, m.ID, m.MetricStat)
				}
				seen[m.MetricStat.Metric.MetricName+" "+d[1].Value] = m.ID
				continue
			}
			seen[m.ID] = m.MetricStat.Metric.MetricName
		}
		if fmt.Sprint(seen) != "map[IdentitiesPolling us-east-1a:p0 IdentitiesPolling us-east-1b:p1 labeled:IdentitiesLabeled running:DispatchRunning waiting:JobsWaiting]" || returned != 1 {
			t.Errorf("%s: reads %v, returns %d series", size, seen, returned)
		}
	}
}

func TestEachDispatchSizeHasAnAlarmForHavingNoIdentityPolling(t *testing.T) {
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
		var polled []string
		returned, labeled := 0, 0
		var result alarmMetric
		var expressions string
		for _, m := range alarmMetrics(t, size, a) {
			expressions += m.Expression + "\n"
			if m.MetricStat.Metric.MetricName == "IdentitiesLabeled" && len(m.MetricStat.Metric.Dimensions) == 1 && m.MetricStat.Stat == "Minimum" {
				labeled++
			}
			if m.ReturnData {
				returned++
				result = m
			}
			if m.MetricStat.Metric.MetricName == "IdentitiesOnline" || strings.Contains(m.Expression, "IdentitiesOnline") {
				t.Errorf("%s: the alarm reads GitHub's REST status (%s)", size, m.ID)
			}
			if m.MetricStat.Metric.MetricName != "IdentitiesPolling" {
				continue
			}
			s, d := m.MetricStat, m.MetricStat.Metric.Dimensions
			if s.Metric.Namespace != "superci-pull" || len(d) != 2 || d[0].Name != "Set" || d[0].Value != "superci-"+size || d[1].Name != "AZ" || s.Period != 60 || s.Stat != "Minimum" || m.ReturnData {
				t.Errorf("%s: reads %+v, want IdentitiesPolling of the Set superci-%s and an AZ, Minimum over 60 s", size, s, size)
			}
			polled = append(polled, d[1].Value)
		}
		if fmt.Sprint(polled) != "[us-east-1a us-east-1b]" || returned != 1 || result.Expression == "" {
			t.Errorf("%s: reads the hosts of %v and returns %d series (%q), want the sum over both AZs as the one", size, polled, returned, result.Expression)
		}
		// A size that is not dispatched has no host row and must stay OK: the sum only counts where the listener publishes the size's DispatchRunning.
		if !strings.Contains(expressions, "FILL(p0, 0) + FILL(p1, 0)") || !strings.Contains(result.Expression, "running >= 0") {
			t.Errorf("%s: the expressions are %q, want the AZs' polling, gaps as 0, only for a dispatched size", size, expressions)
		}
		if labeled != 1 {
			t.Errorf("%s: reads IdentitiesLabeled %d times, want once: a size whose identities are all parked has hosts that still poll", size, labeled)
		}
		if got := fmt.Sprint(alarms["superci-pull-identity-starved-"+size]["--threshold"]); got != "70" {
			t.Errorf("%s: the 70%% alarm changed: threshold %s", size, got)
		}
	}
}

// A third host AZ is read when alarms.sh is told of it, as identity.sh is.
func TestTheIdentityAlarmsReadEachHostAZTheyAreToldOf(t *testing.T) {
	t.Setenv("IDENTITY_AZS", "us-east-1a us-east-1b us-east-1c")
	a := starvedAlarms(t)["superci-pull-identity-none-4cpu"]
	if a == nil {
		t.Fatal("no identity-none-4cpu alarm")
	}
	var azs []string
	for _, m := range alarmMetrics(t, "4cpu", a) {
		if d := m.MetricStat.Metric.Dimensions; m.MetricStat.Metric.MetricName == "IdentitiesPolling" && len(d) == 2 {
			azs = append(azs, d[1].Value)
		}
	}
	if fmt.Sprint(azs) != "[us-east-1a us-east-1b us-east-1c]" {
		t.Errorf("reads the hosts of %v", azs)
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

// What the two alarms read, at one minute, over a world of series: the hosts count a parked identity's listener as polling (they
// cannot see a label), so each alarm takes the lower of that and the identities that carry the size's label.
func TestTheIdentityAlarmsReadTheLowerOfTheLabeledAndThePollingCount(t *testing.T) {
	alarms := starvedAlarms(t)
	none, starved := alarms["superci-pull-identity-none-4cpu"], alarms["superci-pull-identity-starved-4cpu"]
	const set = "superci-4cpu"
	world := func(labeled float64, polledA, polledB float64, running, waiting float64) map[string]float64 {
		w := map[string]float64{
			seriesKey("DispatchRunning", set, ""):             running,
			seriesKey("JobsWaiting", set, ""):                 waiting,
			seriesKey("IdentitiesPolling", set, "us-east-1a"): polledA,
			seriesKey("IdentitiesPolling", set, "us-east-1b"): polledB,
		}
		if labeled >= 0 {
			w[seriesKey("IdentitiesLabeled", set, "")] = labeled
		}
		return w
	}
	cases := []struct {
		name         string
		world        map[string]float64
		noneValue    float64
		noneFires    bool
		starvedValue float64 // NaN: no data
		starvedFires bool
	}{
		{"healthy: 310 labeled, 300 polling, 100 running", world(310, 150, 150, 100, 0), 300, false, 100 * 100.0 / 300, false},
		{"every identity parked: the hosts still poll 310, none carries the label", world(0, 155, 155, 5, 0), 0, true, 1000, true},
		{"half relabelled: 100 carry the label, the hosts poll all 300, 80 jobs", world(100, 150, 150, 60, 20), 100, false, 80, true},
		{"the same jobs with the labels all back are no breach", world(300, 150, 150, 60, 20), 300, false, 100 * 80.0 / 300, false},
		{"a host AZ that publishes nothing reads 0, as a missing series does (FILL)", func() map[string]float64 {
			w := world(310, 155, 0, 100, 0)
			delete(w, seriesKey("IdentitiesPolling", set, "us-east-1b"))
			return w
		}(), 155, false, 100 * 100.0 / 155, false},
		{"both hosts down: nothing polls, though 310 carry the label", func() map[string]float64 {
			w := world(310, 0, 0, 5, 0)
			delete(w, seriesKey("IdentitiesPolling", set, "us-east-1a"))
			delete(w, seriesKey("IdentitiesPolling", set, "us-east-1b"))
			return w
		}(), 0, true, 1000, true},
		{"the listing is stale (no IdentitiesLabeled for 15 minutes): no data, no breach", world(-1, 150, 150, 5, 0), math.NaN(), false, math.NaN(), false},
	}
	for _, c := range cases {
		got, fires := alarmValue(t, none, c.world)
		if !(got == c.noneValue || (math.IsNaN(got) && math.IsNaN(c.noneValue))) || fires != c.noneFires {
			t.Errorf("identity-none, %s: reads %v and fires %v, want %v and %v", c.name, got, fires, c.noneValue, c.noneFires)
		}
		got, fires = alarmValue(t, starved, c.world)
		if !(math.Abs(got-c.starvedValue) < 1e-9 || (math.IsNaN(got) && math.IsNaN(c.starvedValue))) || fires != c.starvedFires {
			t.Errorf("identity-starved, %s: reads %v and fires %v, want %v and %v", c.name, got, fires, c.starvedValue, c.starvedFires)
		}
	}
	// A size the listener does not dispatch publishes no DispatchRunning: its alarm stays OK whatever the hosts and the listing say.
	w := world(0, 0, 0, 0, 0)
	delete(w, seriesKey("DispatchRunning", set, ""))
	if got, fires := alarmValue(t, none, w); fires || !math.IsNaN(got) {
		t.Errorf("identity-none of a size that is not dispatched reads %v and fires %v, want no data", got, fires)
	}
}

// identity-none and identity-starved read IdentitiesPolling, which no host publishes until it runs the new identity-host.sh: an alarm made
// first reads no data and stays OK for ever. alarms.sh refuses to write anything unless every AZ it sums over has a series.
func TestAlarmsRefusesTheIdentityAlarmsUntilEveryHostAZPublishesPolling(t *testing.T) {
	for _, c := range []struct{ name, listed string }{
		{"one of the two AZs", "us-east-1a"},
		{"neither", ""},
		{"an AZ that is not one of the hosts'", "us-east-1a\tus-east-1c"},
		{"a listing that fails", "FAIL"},
	} {
		alarms, out, err := runAlarms(t, c.listed)
		if err == nil || len(alarms) != 0 {
			t.Errorf("%s listed: alarms.sh ran (%v) and made %d alarms, want a refusal before any: %s", c.name, err, len(alarms), out)
		}
		if c.listed != "FAIL" && !strings.Contains(out, "IDENTITY_ALARMS_UNPUBLISHED=1") {
			t.Errorf("%s listed: the refusal does not say how to override it for a first install: %s", c.name, out)
		}
	}
	if _, out, err := runAlarms(t, "us-east-1a"); err == nil || !strings.Contains(out, "us-east-1b") || strings.Contains(out, "for us-east-1a") {
		t.Errorf("the refusal must name the AZ without a series (us-east-1b), not the one with: %v %s", err, out)
	}
	alarms, out, err := runAlarms(t, "us-east-1a\tus-east-1b")
	if err != nil || alarms["superci-pull-identity-none-4cpu"] == nil {
		t.Errorf("with both AZs publishing: %v, %d alarms: %s", err, len(alarms), out)
	}
	alarms, out, err = runAlarms(t, "us-east-1a\tus-east-1b\tus-east-1c", "IDENTITY_AZS=us-east-1a us-east-1b us-east-1c")
	if err != nil || alarms["superci-pull-identity-none-4cpu"] == nil {
		t.Errorf("with a third AZ told of and publishing: %v: %s", err, out)
	}
	_, out, err = runAlarms(t, "us-east-1a\tus-east-1b", "IDENTITY_AZS=us-east-1a us-east-1b us-east-1c")
	if err == nil || !strings.Contains(out, "us-east-1c") {
		t.Errorf("a third AZ told of and not publishing: %v: %s", err, out)
	}
	alarms, out, err = runAlarms(t, "", "IDENTITY_ALARMS_UNPUBLISHED=1")
	if err != nil || alarms["superci-pull-identity-none-4cpu"] == nil || alarms["superci-pull-identity-starved-4cpu"] == nil {
		t.Errorf("a first install with IDENTITY_ALARMS_UNPUBLISHED=1: %v, %d alarms: %s", err, len(alarms), out)
	}
}
