package main

import (
	"math"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

func jobBase(guid string, queued time.Time) scaleset.JobMessageBase {
	return scaleset.JobMessageBase{JobID: guid, WorkflowRunID: 9, JobDisplayName: "unit (3)", OwnerName: "o", RepositoryName: "r",
		JobWorkflowRef: "o/r/.github/workflows/ci.yml@refs/heads/main", EventName: "push", RequestLabels: []string{"superci-4cpu"}, QueueTime: queued}
}

// jobRan is a set whose machine i-0000 (spot, launched at launched, $0.40 an hour) took one job queued at queued, started
// at start and finished at finish. The started message carries the job's times; a completed one carries only its finish.
func jobRan(t *testing.T, launched, queued, start, finish time.Time, result string) (*State, []JobRecord) {
	t.Helper()
	s := newState(0, 10)
	s.SetPricer(func(Machine) float64 { return 0.40 })
	m := machines(1, launched)[0]
	m.Set, m.Type, m.AZ = "set", "m8azn.xlarge", "us-east-1b"
	s.Seen("us-east-1", []*Machine{m}, t0.Add(-time.Hour), t0.Add(-time.Hour))
	base := jobBase("guid-1", queued)
	started := base
	started.RunnerAssignTime = start
	s.Observe(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{RunnerID: 3, RunnerName: "set-i-0000", JobMessageBase: started}}}, start)
	done := scaleset.JobMessageBase{JobID: "guid-1", WorkflowRunID: 9, JobDisplayName: "unit (3)", OwnerName: "o", RepositoryName: "r",
		JobWorkflowRef: base.JobWorkflowRef, EventName: "push", FinishTime: finish}
	rec := s.Observe(&scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{Result: result, RunnerID: 3, RunnerName: "set-i-0000", JobMessageBase: done}}}, finish)
	return s, rec
}

// The record of a finished job holds what the gate and the cost report read: its queue wait and wall time, its
// outcome in GitHub's REST word, the workflow and branch, the machine that ran it and what the machine cost, idle
// split from busy. A completed message carries neither the queue time nor the start, so they come from the started one.
func TestAFinishedJobsRecordHoldsWhatTheGateAndTheCostReportRead(t *testing.T) {
	queued := t0.Add(-70 * time.Second)
	start := t0
	finish := t0.Add(5 * time.Minute)
	_, recs := jobRan(t, t0.Add(-100*time.Second), queued, start, finish, "Failed")
	if len(recs) != 1 {
		t.Fatalf("%d records", len(recs))
	}
	r := recs[0]
	if r.Conclusion() != "failure" || r.QueueMS() != 70000 || r.WallMS() != 300000 {
		t.Errorf("conclusion %s, queue %d ms, wall %d ms", r.Conclusion(), r.QueueMS(), r.WallMS())
	}
	if r.WorkflowRef != "o/r/.github/workflows/ci.yml@refs/heads/main" || r.Name != "unit (3)" || r.Repo != "o/r" || r.Inst != "i-0000" || r.Set != "set" {
		t.Errorf("identity: %+v", r)
	}
	if r.Machine == nil || r.Machine.Type != "m8azn.xlarge" || r.Machine.AZ != "us-east-1b" || r.Machine.Lifecycle != "spot" {
		t.Fatalf("machine: %+v", r.Machine)
	}
	// 100 s of boot and idle, then 300 s of job, at $0.40 an hour
	if want := 0.40 * 400 / 3600; math.Abs(r.USD-want) > 1e-9 {
		t.Errorf("usd %.6f, want %.6f", r.USD, want)
	}
	if want := 0.40 * 300 / 3600; math.Abs(r.BusyUSD-want) > 1e-9 || math.Abs(r.IdleUSD()-0.40*100/3600) > 1e-9 {
		t.Errorf("busy %.6f idle %.6f", r.BusyUSD, r.IdleUSD())
	}
	lines := capture(t, func() { tlJobRecord(r, finish) })
	if len(lines) != 1 || lines[0]["ev"] != "job.record" || lines[0]["conclusion"] != "failure" || lines[0]["queue_ms"] != float64(70000) ||
		lines[0]["wall_ms"] != float64(300000) || lines[0]["type"] != "m8azn.xlarge" || lines[0]["warm"] != true || lines[0]["t"] != tlTime(finish) {
		t.Errorf("event: %v", lines)
	}
}

// GitHub redelivers a completion until the listener acknowledges it; the job has one record, so the metrics and the
// cost report count it once.
func TestARedeliveredCompletionMakesNoSecondRecord(t *testing.T) {
	s, recs := jobRan(t, t0.Add(-time.Minute), t0, t0, t0.Add(time.Minute), "Succeeded")
	if len(recs) != 1 || recs[0].Conclusion() != "success" {
		t.Fatalf("first: %+v", recs)
	}
	again := &scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{Result: "Succeeded", RunnerName: "set-i-0000", JobMessageBase: jobBase("guid-1", time.Time{})}}}
	if got := s.Observe(again, t0.Add(2*time.Minute)); len(got) != 0 {
		t.Errorf("a redelivery made %d records", len(got))
	}
	if n := len(s.TakeJobs()); n != 1 {
		t.Errorf("the metrics take %d records, want 1", n)
	}
	if n := len(s.TakeJobs()); n != 0 {
		t.Errorf("taken twice: %d", n)
	}
}

// A job whose machine the listener no longer holds (it restarted, and EC2 stopped listing the machine) still has a
// record, with no machine and no cost: the gate needs its times, not its price.
func TestAJobWithNoMachineStillHasItsRecord(t *testing.T) {
	s := newState(0, 10)
	rec := s.Observe(&scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{Result: "Canceled", RunnerName: "set-i-9999",
		JobMessageBase: scaleset.JobMessageBase{JobID: "g", QueueTime: t0, RunnerAssignTime: t0.Add(time.Second), FinishTime: t0.Add(4 * time.Second)}}}}, t0.Add(5*time.Second))
	if len(rec) != 1 || rec[0].Machine != nil || rec[0].USD != 0 || rec[0].Conclusion() != "cancelled" || rec[0].WallMS() != 3000 {
		t.Fatalf("%+v", rec)
	}
	lines := capture(t, func() { tlJobRecord(rec[0], t0) })
	if _, has := lines[0]["type"]; has || lines[0]["wall_ms"] != float64(3000) {
		t.Errorf("event: %v", lines[0])
	}
}

func TestResultsMapToGitHubsConclusions(t *testing.T) {
	for result, want := range map[string]string{"Succeeded": "success", "Failed": "failure", "Canceled": "cancelled", "Abandoned": "cancelled", "": "failure", "SucceededWithIssues": "failure"} {
		if got := (JobRecord{Result: result}).Conclusion(); got != want {
			t.Errorf("%q: %s, want %s", result, got, want)
		}
	}
}
