package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// starved is a budget whose organization's rate is known and above what the margin leaves: others register 4.3/s of
// the 4/s the margin allows, as in the canary of Nov 26 2026. Its use of the window (1,290) is under the guard, so
// every ask is turned away for want of a token.
func starved(at time.Time) *Budget {
	b := NewBudget(reg(), at)
	heads(b, 4.3, 6*time.Minute)
	return b
}

func offered(job string, queued time.Time, waiting int) *scaleset.RunnerScaleSetMessage {
	return &scaleset.RunnerScaleSetMessage{
		Statistics:           &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: waiting},
		JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{JobID: job, QueueTime: queued}}},
	}
}

func TestAStarvedBudgetLeavesAJobUnstartedAndItsWaitIsVisibleWhileItLasts(t *testing.T) {
	now := t0.Add(6 * time.Minute)
	b := starved(t0)
	s := newState(0, 10)
	s.Observe(offered("j1", now, 1), now)
	launch(s, machines(1, now), now)
	asks := 0
	for at := now; !at.After(now.Add(90 * time.Second)); at = at.Add(3 * time.Second) {
		s.Observe(nil, at)
		v, why := s.AskWhy("i-0000", at, b, 8*time.Hour)
		if v != Wait || why != "tokens" {
			t.Fatalf("ask at +%s: %v for %q, want Wait for tokens", at.Sub(now), v, why)
		}
		asks++
	}
	end := now.Add(90 * time.Second)
	snap := s.Snapshot(end)
	if snap.OldestJobS != 90 {
		t.Errorf("the job has waited 90 s unstarted: gauge reads %v", snap.OldestJobS)
	}
	if snap.OldestUnregisteredS != 90 {
		t.Errorf("the machine has had no runner for 90 s: gauge reads %v", snap.OldestUnregisteredS)
	}
	if got := b.Snapshot(end).Denied; got["tokens"] != asks || got["window"]+got["throttled"]+got["blind"] != 0 {
		t.Errorf("denied %v, want %d for tokens and nothing else: GitHub was never asked, so refused stays %d", got, asks, b.Snapshot(end).Refused)
	}
	if b.Snapshot(end).Refused != 0 || b.Snapshot(end).Throttled {
		t.Errorf("a denial is no refusal by GitHub: %+v", b.Snapshot(end))
	}
}

func TestEveryReasonTheBudgetTurnsAwayIsCountedUnderItsOwnName(t *testing.T) {
	window := NewBudget(reg(), t0)
	heads(window, 4.9, 6*time.Minute) // 1,470 of the window, over the 1,350 guard
	tokens := starved(t0)
	throttled := NewBudget(reg(), t0)
	throttled.Done(t0, Refused, 0)
	blind := NewBudget(RegConfig{Cap: 1500, WindowS: 300, Margin: 0.2, BlindRate: 0.01}, t0.Add(-time.Hour))
	for i := 0; i < 3; i++ { // the 3 a window the blind rate allows
		if !blind.Take(t0, ForJob) {
			t.Fatalf("blind registration %d turned away", i)
		}
	}
	at := t0.Add(6 * time.Minute)
	for _, c := range []struct {
		name   string
		b      *Budget
		at     time.Time
		reason Denial
	}{{"window", window, at, DeniedWindow}, {"tokens", tokens, at, DeniedTokens}, {"throttled", throttled, t0.Add(time.Second), DeniedThrottled}, {"blind", blind, t0, DeniedBlind}} {
		ok, why := c.b.Try(c.at, ForJob)
		if ok || why != c.reason {
			t.Errorf("%s: Try says %v for %v, want a no for %v", c.name, ok, why, c.reason)
		}
		if got := c.b.Snapshot(c.at).Denied[c.name]; got != 1 {
			t.Errorf("%s: denied %v, want 1", c.name, c.b.Snapshot(c.at).Denied)
		}
	}
	open := NewBudget(reg(), t0)
	heads(open, 1, 6*time.Minute)
	if ok, why := open.Try(at, ForJob); !ok || why != Allowed || open.Snapshot(at).Denied["tokens"] != 0 {
		t.Errorf("a registration the budget allows is no denial: %v %v %v", ok, why, open.Snapshot(at).Denied)
	}
}

func TestTheOldestWaitingJobIsBoundedByWhatTheQueueSaysWaits(t *testing.T) {
	now := t0.Add(10 * time.Minute)
	s := newState(0, 10)
	s.Observe(offered("gone", now.Add(-9*time.Minute), 1), now.Add(-9*time.Minute)) // cancelled in the queue: no start ever comes
	s.Observe(offered("live", now.Add(-40*time.Second), 1), now.Add(-40*time.Second))
	if got := s.Snapshot(now).OldestJobS; got != 40 {
		t.Errorf("one job waits by the statistics, and it is the newer: oldest %v, want 40", got)
	}
	s.Observe(stats(0, 0), now)
	if got := s.Snapshot(now).OldestJobS; got != 0 {
		t.Errorf("nothing waits by the statistics: oldest %v, want 0", got)
	}
	s.Observe(&scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1, TotalRunningJobs: 1},
		JobStartedMessages: []*scaleset.JobStarted{{JobMessageBase: scaleset.JobMessageBase{JobID: "live", QueueTime: now.Add(-40 * time.Second)}}}}, now)
	if got := s.Snapshot(now).OldestJobS; got != 0 {
		t.Errorf("the job started: oldest %v, want 0", got)
	}
}

func TestAMachineStillToldToWaitWritesJitWaitEvery30SecondsWithItsReason(t *testing.T) {
	now := t0
	lines := capture(t, func() {
		for at := now; !at.After(now.Add(100 * time.Second)); at = at.Add(3 * time.Second) {
			tlAsk("superci-4cpu", "i-wait", Wait, "tokens", at)
		}
	})
	var waits []map[string]any
	for _, l := range lines {
		if l["ev"] == "jit.wait" {
			waits = append(waits, l)
		}
	}
	if len(waits) != 3 {
		t.Fatalf("100 s of waiting: %d jit.wait lines, want one at 30, 60 and 90 s: %v", len(waits), lines)
	}
	for i, w := range waits {
		if w["why"] != "tokens" || w["inst"] != "i-wait" || w["set"] != "superci-4cpu" {
			t.Errorf("jit.wait %d: %v", i, w)
		}
		if got := w["waited_ms"].(float64); got < float64(30000*(i+1)) || got > float64(30000*(i+1)+3000) {
			t.Errorf("jit.wait %d at waited_ms %v, want about %d", i, got, 30000*(i+1))
		}
	}
	if lines[0]["ev"] != "jit.ask" {
		t.Errorf("the first ask still writes jit.ask: %v", lines[0])
	}
}

func TestThePublishedMetricsCarryTheDenialsAndTheWaitsInProgress(t *testing.T) {
	m := Metrics{Denied: map[string]int{"window": 1, "tokens": 20, "throttled": 3, "blind": 0},
		Sets: []Snapshot{{Set: "superci-4cpu", OldestJobS: 480, OldestUnregisteredS: 500}, {Set: "superci-2cpu", OldestJobS: 7, OldestUnregisteredS: 9}}}
	got := map[string]float64{}
	for _, d := range metricData(m, t0) {
		key := *d.MetricName
		for _, dim := range d.Dimensions {
			key += "/" + *dim.Name + "=" + *dim.Value
		}
		got[key] = *d.Value
	}
	want := map[string]float64{
		"BudgetDenied/Reason=window": 1, "BudgetDenied/Reason=tokens": 20, "BudgetDenied/Reason=throttled": 3, "BudgetDenied/Reason=blind": 0,
		"OldestWaitingJobAgeS/Set=superci-4cpu": 480, "OldestWaitingJobAgeS/Set=superci-2cpu": 7, "OldestWaitingJobAgeS": 480,
		"OldestUnregisteredMachineAgeS/Set=superci-4cpu": 500, "OldestUnregisteredMachineAgeS/Set=superci-2cpu": 9, "OldestUnregisteredMachineAgeS": 500,
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s: published %v (present %v), want %v", k, g, ok, v)
		}
	}
}

func TestTheBudgetsDenialsOfAMinuteAreTheDifferenceOfTwoSnapshots(t *testing.T) {
	got := deniedSince(map[string]int{"tokens": 5, "window": 2}, map[string]int{"tokens": 25, "window": 2, "blind": 4})
	if got["tokens"] != 20 || got["window"] != 0 || got["blind"] != 4 {
		t.Errorf("deniedSince %v", got)
	}
	if got := deniedSince(nil, map[string]int{"tokens": 7}); got["tokens"] != 7 {
		t.Errorf("the first minute counts everything denied since the start: %v", got)
	}
}

// canaryJobs reads the test-go jobs of the Nov 26 canary: when GitHub queued each and when a runner started it.
func canaryJobs(t *testing.T) (queued, started map[string]time.Time) {
	t.Helper()
	f, err := os.Open("testdata/canary-0024-4cpu-jobs.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	queued, started = map[string]time.Time{}, map[string]time.Time{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "#") {
			continue
		}
		c := strings.Split(sc.Text(), "\t")
		q, err1 := time.Parse(time.RFC3339, c[2])
		st, err2 := time.Parse(time.RFC3339, c[3])
		if len(c) != 4 || err1 != nil || err2 != nil {
			t.Fatalf("bad row %q", sc.Text())
		}
		queued[c[0]], started[c[0]] = q, st
	}
	return queued, started
}

// alarmRule reads the oldest-job-wait alarm out of deploy/alarms.sh: threshold, period (s), evaluation periods and
// datapoints to alarm.
func alarmRule(t *testing.T) (threshold, period float64, evals, datapoints int) {
	t.Helper()
	raw, err := os.ReadFile("deploy/alarms.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`alarm oldest-job-wait [^\n]*\\\n\s+OldestWaitingJobAgeS Maximum GreaterThanThreshold (\d+) (\d+) (\d+) (\d+) `).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("deploy/alarms.sh has no alarm oldest-job-wait on OldestWaitingJobAgeS, Maximum, GreaterThanThreshold")
	}
	n := func(s string) float64 { v, _ := strconv.Atoi(s); return float64(v) }
	return n(m[1]), n(m[2]), int(n(m[3])), int(n(m[4]))
}

// alarms is CloudWatch's rule for one alarm over a series of datapoints a period apart: it goes to ALARM at the first
// datapoint that makes datapoints of the last evals breach.
func alarms(series []float64, threshold float64, evals, datapoints int) (first int) {
	for i := range series {
		breaching := 0
		for _, v := range series[max(0, i-evals+1) : i+1] {
			if v > threshold {
				breaching++
			}
		}
		if breaching >= datapoints {
			return i
		}
	}
	return -1
}

// The canary's jobs, replayed through the listener's own state a minute at a time, the way the metrics publish it:
// the oldest waiting job's age, from 00:22Z when the budget starved until the last job started at 00:46Z.
func TestTheOldestJobAlarmFiresWithinTwoMinutesOfTheCanarysStall(t *testing.T) {
	queued, started := canaryJobs(t)
	threshold, period, evals, datapoints := alarmRule(t)
	if period != 60 {
		t.Fatalf("the replay samples a minute at a time, the alarm's period is %v s", period)
	}
	type event struct {
		at    time.Time
		job   string
		start bool
	}
	var events []event
	for j := range queued {
		events = append(events, event{queued[j], j, false}, event{started[j], j, true})
	}
	slices.SortStableFunc(events, func(a, b event) int { return a.at.Compare(b.at) })
	s := NewState("superci-4cpu", 0, 100, []string{"us-east-1"}, time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC))
	s.listings["us-east-1"] = time.Date(2026, 11, 26, 0, 1, 0, 0, time.UTC)
	waiting, running := 0, 0
	var ticks []time.Time
	var series []float64
	next := 0
	for at := time.Date(2026, 11, 26, 0, 23, 0, 0, time.UTC); !at.After(time.Date(2026, 11, 26, 0, 48, 0, 0, time.UTC)); at = at.Add(time.Minute) {
		for ; next < len(events) && !events[next].at.After(at); next++ {
			e := events[next]
			msg := &scaleset.RunnerScaleSetMessage{}
			base := scaleset.JobMessageBase{JobID: e.job, QueueTime: queued[e.job]}
			if e.start {
				waiting, running = waiting-1, running+1
				msg.JobStartedMessages = []*scaleset.JobStarted{{JobMessageBase: base}}
			} else {
				waiting++
				msg.JobAvailableMessages = []*scaleset.JobAvailable{{JobMessageBase: base}}
			}
			msg.Statistics = &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: waiting + running, TotalRunningJobs: running}
			s.Observe(msg, e.at)
		}
		ticks = append(ticks, at)
		series = append(series, s.Snapshot(at).OldestJobS)
	}
	crossed := slices.IndexFunc(series, func(v float64) bool { return v > threshold })
	fired := alarms(series, threshold, evals, datapoints)
	if crossed < 0 || fired < 0 {
		t.Fatalf("the canary's stall never read above %v s or never alarmed: %v", threshold, series)
	}
	stall := time.Date(2026, 11, 26, 0, 24, 20, 0, time.UTC).Add(time.Duration(threshold) * time.Second) // the first job to wait over the threshold
	if ticks[fired].Sub(stall) > 2*time.Minute || ticks[fired].Before(stall) {
		t.Errorf("the alarm went off at %s, the first job passed %v s at %s: want within 2 minutes after it", ticks[fired].Format("15:04:05"), threshold, stall.Format("15:04:05"))
	}
	if p90 := time.Date(2026, 11, 26, 0, 48, 24, 0, time.UTC); p90.Sub(ticks[fired]) < 20*time.Minute {
		t.Errorf("the p90 alarm of the day went off at 00:48:24, after the rollback; this one at %s is no earlier by 20 minutes", ticks[fired].Format("15:04:05"))
	}
	if series[0] > threshold || series[1] > threshold {
		t.Errorf("a job started in 2 s at 00:23:58 and the first sample reads %v", series[:2])
	}
	if end := series[len(series)-1]; end != 0 {
		t.Errorf("every job has started by 00:48: the gauge reads %v", end)
	}
	if fmt.Sprint(alarms(series[len(series)-2:], threshold, evals, datapoints)) != "-1" {
		t.Errorf("the alarm is still breaching after the last job started: %v", series[len(series)-3:])
	}
}
