package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/aws/aws-sdk-go-v2/aws"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

func rec(set, result string, wallS int, usd, busy float64) JobRecord {
	start := t0
	r := JobRecord{Set: set, Result: result, Queued: start.Add(-5 * time.Second), Started: start, Finished: start.Add(time.Duration(wallS) * time.Second), USD: usd, BusyUSD: busy}
	return r
}

func datum(data []cwtypes.MetricDatum, name string, dims ...string) *cwtypes.MetricDatum {
	for i, d := range data {
		if aws.ToString(d.MetricName) != name || len(d.Dimensions) != len(dims)/2 {
			continue
		}
		ok := true
		for j, dim := range d.Dimensions {
			ok = ok && aws.ToString(dim.Name) == dims[2*j] && aws.ToString(dim.Value) == dims[2*j+1]
		}
		if ok {
			return &data[i]
		}
	}
	return nil
}

func histogramCount(d *cwtypes.MetricDatum) (n, sum float64) {
	for i, v := range d.Values {
		n += d.Counts[i]
		sum += v * d.Counts[i]
	}
	return
}

// A minute's finished jobs become counts by result, the wall time of the successes and the cost with its idle part, for
// every set together and for each set: the totals the alarms read and the per-set lines the dashboard draws.
func TestFinishedJobsBecomeMetricsByResultAndSet(t *testing.T) {
	partial := JobRecord{Set: "superci-4cpu", Result: "Succeeded", Finished: t0, USD: 0.30}
	jobs := []JobRecord{rec("superci-4cpu", "succeeded", 60, 0.10, 0.06), rec("superci-4cpu", "succeeded", 120, 0.20, 0.15), rec("superci-8cpu", "succeeded", 90, 0.30, 0.20),
		rec("superci-4cpu", "failed", 30, 0.05, 0.02), rec("superci-8cpu", "canceled", 10, 0.04, 0.01), partial}
	at := t0
	data := jobData(jobs, &at)
	count := func(want float64, dims ...string) {
		t.Helper()
		d := datum(data, "JobsCompleted", dims...)
		if d == nil || aws.ToFloat64(d.Value) != want {
			t.Errorf("JobsCompleted %v: %v, want %v", dims, d, want)
		}
	}
	count(4, "Result", "success")
	count(1, "Result", "failure")
	count(1, "Result", "cancelled")
	count(3, "Set", "superci-4cpu", "Result", "success")
	count(1, "Set", "superci-4cpu", "Result", "failure")
	count(0, "Set", "superci-4cpu", "Result", "cancelled")
	count(1, "Set", "superci-8cpu", "Result", "success")
	if n, sum := histogramCount(datum(data, "JobWall")); n != 3 || sum != 270 {
		t.Errorf("JobWall of the successes with a known start: %v samples summing %v, want 3 and 270 (the partial one has no wall time)", n, sum)
	}
	if n, sum := histogramCount(datum(data, "JobWall", "Set", "superci-4cpu")); n != 2 || sum != 180 {
		t.Errorf("superci-4cpu JobWall: %v samples summing %v", n, sum)
	}
	cost, idle := datum(data, "JobCostUSD"), datum(data, "JobIdleCostUSD")
	if math.Abs(aws.ToFloat64(cost.Value)-0.99) > 1e-9 || math.Abs(aws.ToFloat64(idle.Value)-(0.04+0.05+0.10+0.03+0.03)) > 1e-9 {
		t.Errorf("cost %v idle %v: a partial record's cost counts and none of it is idle", aws.ToFloat64(cost.Value), aws.ToFloat64(idle.Value))
	}
	if datum(data, "JobCostUSD", "Set", "superci-8cpu") == nil || jobData(nil, &at) != nil {
		t.Errorf("per-set cost missing, or a quiet minute publishes job metrics")
	}
}

// The spend an hour is split by whether a machine's runner holds a job: a machine booting or waiting for work is the idle
// part, the one the alarm on spend with nothing routed reads.
func TestSpendPerHourSplitsBusyFromIdle(t *testing.T) {
	sp := NewSpend(0, 0, []SetSpec{{Name: "set", CPU: 4, DiskGB: 100}}, t0)
	busy := &Machine{ID: "i-1", Set: "set", Lifecycle: "on-demand", Runner: &Runner{Busy: true}}
	idle := &Machine{ID: "i-2", Set: "set", Lifecycle: "on-demand", Runner: &Runner{}}
	booting := &Machine{ID: "i-3", Set: "set", Lifecycle: "on-demand"}
	sp.Accrue(t0.Add(time.Minute), []Machine{*busy, *idle, *booting})
	one := sp.Price(*busy)
	snap := sp.Snapshot()
	if math.Abs(snap.PerHour-3*one) > 1e-9 || math.Abs(snap.BusyPerHour-one) > 1e-9 || math.Abs(snap.IdlePerHour-2*one) > 1e-9 {
		t.Errorf("per hour %v busy %v idle %v of machines priced %v each", snap.PerHour, snap.BusyPerHour, snap.IdlePerHour, one)
	}
	names := map[string]float64{}
	for _, d := range metricData(Metrics{Spend: snap}, t0) {
		names[aws.ToString(d.MetricName)] = aws.ToFloat64(d.Value)
	}
	if names["SpendBusyPerHourUSD"] != snap.BusyPerHour || names["SpendIdlePerHourUSD"] != snap.IdlePerHour {
		t.Errorf("published %v", names)
	}
}

// A dashboard widget or an alarm on a metric nothing publishes is blank or never fires, and nothing says so. Every metric
// name deploy/dashboard.json and deploy/alarms.sh read is one the listener publishes.
func TestEveryMetricTheDashboardAndAlarmsReadIsPublished(t *testing.T) {
	at := t0
	published := map[string]bool{}
	m := Metrics{Sets: []Snapshot{{Set: "superci-4cpu"}}, Waits: []float64{1}, SetWaits: map[string][]float64{"superci-4cpu": {1}}, Denied: map[string]int{}, Jobs: []JobRecord{rec("superci-4cpu", "succeeded", 5, 0.1, 0.1)}}
	m.Budget.Known = true
	for _, d := range metricData(m, at) {
		published[aws.ToString(d.MetricName)] = true
	}
	raw, err := os.ReadFile("deploy/dashboard.json")
	if err != nil {
		t.Fatal(err)
	}
	var dash struct {
		Widgets []struct {
			Type       string `json:"type"`
			Properties struct {
				Metrics [][]any `json:"metrics"`
			} `json:"properties"`
		} `json:"widgets"`
	}
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatalf("dashboard.json: %v", err)
	}
	read := map[string]string{}
	search := regexp.MustCompile(`MetricName=\\?"(\w+)\\?"`)
	for _, w := range dash.Widgets {
		for _, row := range w.Properties.Metrics {
			for i, cell := range row {
				s, _ := cell.(string)
				switch {
				case i == 1 && len(row) > 1 && (row[0] == Namespace || row[0] == ".") && s != "." && s != "...":
					read[s] = "dashboard"
				case i == 0:
				}
				if e, ok := cell.(map[string]any); ok {
					if expr, _ := e["expression"].(string); expr != "" {
						for _, g := range search.FindAllStringSubmatch(expr, -1) {
							read[g[1]] = "dashboard search"
						}
					}
				}
			}
		}
	}
	script, err := os.ReadFile("deploy/alarms.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range regexp.MustCompile(`"MetricName":"(\w+)"`).FindAllStringSubmatch(string(script), -1) {
		read[g[1]] = "alarm"
	}
	for _, g := range regexp.MustCompile(`(?m)^\s+(\w+) (?:Sum|Maximum|Minimum|Average|SampleCount|p\d+) (?:Greater|Less)`).FindAllStringSubmatch(string(script), -1) {
		read[g[1]] = "alarm"
	}
	if len(read) < 25 {
		t.Fatalf("read only %d metric names from the dashboard and alarms: the parse is broken: %v", len(read), read)
	}
	for name, where := range read {
		if !published[name] && !strings.HasPrefix(name, "AWS/") {
			t.Errorf("%s reads %s, which no metricData publishes", where, name)
		}
	}
	for _, want := range []string{"JobsCompleted", "JobWall", "JobCostUSD", "SpendIdlePerHourUSD"} {
		if read[want] == "" {
			t.Errorf("the dashboard or alarms do not read %s", want)
		}
	}
}

// Every alarm alarms.sh makes is on the dashboard's alarm panel, pages the one topic the existing alarms use, and the
// script names no person: the topic's destination is given by the one who runs it, never kept here.
func TestAlarmsAreOnTheDashboardPageOneTopicAndNameNoPerson(t *testing.T) {
	script, err := os.ReadFile("deploy/alarms.sh")
	if err != nil {
		t.Fatal(err)
	}
	made := map[string]bool{}
	for _, g := range regexp.MustCompile(`(?m)^(?:alarm|math) (\S+) `).FindAllStringSubmatch(string(script), -1) {
		made["superci-pull-"+g[1]] = true
	}
	for _, want := range []string{"superci-pull-job-failure-rate", "superci-pull-idle-spend-unrouted", "superci-pull-on-demand-share", "superci-pull-oldest-job-wait"} {
		if !made[want] {
			t.Errorf("alarms.sh does not make %s", want)
		}
	}
	dash, err := os.ReadFile("deploy/dashboard.json")
	if err != nil {
		t.Fatal(err)
	}
	for name := range made {
		if !strings.Contains(string(dash), ":alarm:"+name+`"`) {
			t.Errorf("%s is not on the dashboard's alarm panel", name)
		}
	}
	if n := strings.Count(string(script), "create-topic"); n != 1 || !strings.Contains(string(script), "--name superci-pull-alarms") {
		t.Errorf("the alarms page one topic, superci-pull-alarms: %d create-topic calls", n)
	}
	if strings.Contains(string(script), "--alarm-actions") && strings.Count(string(script), `--alarm-actions "$TOPIC"`) != 2 {
		t.Errorf("both alarm kinds must act on $TOPIC")
	}
	if m := regexp.MustCompile(`[A-Za-z0-9._+-]+@[A-Za-z0-9-]+\.[A-Za-z]{2,}`).FindString(string(script)); m != "" {
		t.Errorf("alarms.sh holds an address: %s", m)
	}
	for _, line := range strings.Split(string(script), "\n") {
		if strings.Contains(line, "sns subscribe") && !strings.HasPrefix(strings.TrimSpace(line), "echo") {
			t.Errorf("alarms.sh subscribes someone: %s", line)
		}
		if strings.Contains(line, "SubscriptionType=EMAIL") || strings.Contains(line, `"EMAIL"`) {
			t.Errorf("the budget notifies a person, not the topic: %s", line)
		}
	}
}

// The failure alarm reads a rate only from a period with enough jobs, and the idle alarm only while no job started.
func TestHarmAlarmsReadTheJobMetricsTheyNameWithTheirThresholds(t *testing.T) {
	script, _ := os.ReadFile("deploy/alarms.sh")
	s := string(script)
	for _, want := range []string{`IF(machines >= 10, share, 0)`, `GreaterThanThreshold 0.4 6 6 notBreaching`, `"MetricName":"OnDemandShare"`, `IF(failed + passed >= 20, 100 * failed / (failed + passed), 0)`, `GreaterThanThreshold 20 3 2 notBreaching`,
		`IF(FILL(started, 0) < 1 AND idle > 8, idle, 0)`, `GreaterThanThreshold 8 6 6 notBreaching`, `"MetricName":"JobsCompleted"`, `"MetricName":"SpendIdlePerHourUSD"`,
		`"BudgetName": "superci-pull-daily"`, `"TimeUnit": "DAILY"`, `for t in (80, 100)`} {
		if !strings.Contains(s, want) {
			t.Errorf("alarms.sh lacks %q", want)
		}
	}
}

// The minute's metrics take the jobs the message loop finished: a completion reaches metricData through Observe and
// TakeJobs (the two lines that joined them were lost once and every test still passed), a second take is empty, and a
// redelivered completion is not counted twice.
func TestAJobObservedFinishedIsPublishedOnceByTheNextMinuteOfMetrics(t *testing.T) {
	s := newState(0, 10)
	s.SetPricer(func(Machine) float64 { return 0.40 })
	m := machines(1, t0.Add(-time.Minute))[0]
	m.Set = "set"
	s.Seen("us-east-1", []*Machine{m}, t0.Add(-time.Hour), t0.Add(-time.Hour))
	done := &scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{Result: "failed", RunnerName: "set-i-0000",
		JobMessageBase: scaleset.JobMessageBase{JobID: "g1", QueueTime: t0, RunnerAssignTime: t0, FinishTime: t0.Add(30 * time.Second)}}}}
	s.Observe(done, t0.Add(30*time.Second))
	s.Observe(done, t0.Add(31*time.Second))
	jobs := s.TakeJobs()
	if len(jobs) != 1 {
		t.Fatalf("TakeJobs hands over %d records, want the one job's", len(jobs))
	}
	at := t0
	d := datum(jobData(jobs, &at), "JobsCompleted", "Result", "failure")
	if d == nil || aws.ToFloat64(d.Value) != 1 {
		t.Errorf("the failed job is not counted: %v", d)
	}
	if len(s.TakeJobs()) != 0 {
		t.Errorf("a second take hands the same records over")
	}
}

// Spend reads a snapshot of each machine's runner: State.Running copies it, so the busy flag the message loop writes under
// the State's lock is not read through a shared pointer.
func TestRunningCopiesEachMachinesRunner(t *testing.T) {
	s := newState(0, 10)
	m := machines(1, t0)[0]
	m.Runner = &Runner{Name: "r"}
	s.Seen("us-east-1", []*Machine{m}, t0.Add(-time.Hour), t0.Add(-time.Hour))
	got := s.Running()
	if len(got) != 1 || got[0].Runner == nil {
		t.Fatalf("%+v", got)
	}
	if kept := s.machines[m.ID].Runner; got[0].Runner == kept {
		t.Errorf("Running hands out the runner the State goes on writing")
	}
}

func TestTheRecordsWaitingForTheMetricsAreBounded(t *testing.T) {
	s := newState(0, 10)
	for i := 0; i < maxJobBuffer+50; i++ {
		s.Observe(&scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{Result: "succeeded", RunnerName: "set-i-9",
			JobMessageBase: scaleset.JobMessageBase{JobID: fmt.Sprint("g", i)}}}}, t0)
	}
	if n := len(s.TakeJobs()); n != maxJobBuffer {
		t.Errorf("%d records kept, want %d", n, maxJobBuffer)
	}
}
