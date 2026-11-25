package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

func TestASecondAskWhileAJobRunsGetsNoRunner(t *testing.T) {
	s := NewState("set", 0, 10)
	s.Observe(stats(1, 0), t0)
	launch(s, machines(1, t0), t0)
	if v := ask(s, "i-0000", t0); v != Grant {
		t.Fatalf("first ask: %v", v)
	}
	s.Registered("i-0000", &Runner{ID: 5, Name: "set-i-0000-aa", Issued: t0})
	s.Observe(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{RunnerName: "set-i-0000-aa"}},
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2, TotalRunningJobs: 1}}, t0)
	if v := ask(s, "i-0000", t0.Add(time.Minute)); v != Wait {
		t.Fatalf("an ask from the machine while its runner holds a job: %v, want Wait", v)
	}
	s.Observe(&scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "set-i-0000-aa"}},
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}, t0)
	if v := ask(s, "i-0000", t0.Add(2*time.Minute)); v != End {
		t.Fatalf("an ask after its job completed: %v, want End (a machine runs one job, though a job still waits)", v)
	}
}

func TestARunnerThatNeverStartedIsRemovedBeforeAnother(t *testing.T) {
	s := NewState("set", 0, 10)
	s.Observe(stats(1, 0), t0)
	launch(s, machines(1, t0), t0)
	ask(s, "i-0000", t0)
	old := &Runner{ID: 5, Name: "set-i-0000-aa", Issued: t0}
	s.Registered("i-0000", old)
	v, r := s.Ask("i-0000", t0.Add(time.Second), open(), 8*time.Hour)
	if v != Replace || r != old {
		t.Fatalf("ask again with an unstarted runner: %v %v, want Replace of it", v, r)
	}
	if v := ask(s, "i-0000", t0.Add(2*time.Second)); v != Wait {
		t.Fatalf("while its removal is in flight: %v, want Wait", v)
	}
	s.Replaced(s.Machine("i-0000"), old, false)
	if v := ask(s, "i-0000", t0.Add(3*time.Second)); v != Wait {
		t.Fatalf("GitHub refused the removal (it took a job): %v, want Wait", v)
	}
	s2 := NewState("set", 0, 10)
	s2.Observe(stats(1, 0), t0)
	launch(s2, machines(1, t0), t0)
	s2.Registered("i-0000", &Runner{ID: 6, Name: "set-i-0000-bb", Issued: t0})
	v, r = s2.Ask("i-0000", t0, open(), 8*time.Hour)
	if v != Replace {
		t.Fatalf("ask with an unstarted runner: %v, want Replace", v)
	}
	s2.Replaced(s2.Machine("i-0000"), r, true)
	if v := ask(s2, "i-0000", t0.Add(time.Second)); v != Grant {
		t.Fatalf("after GitHub removed it: %v, want Grant", v)
	}
}

func TestABusyRunnerGitHubNoLongerListsEndsItsMachine(t *testing.T) {
	s := NewState("set", 0, 10)
	s.Observe(stats(1, 0), t0)
	launch(s, machines(1, t0), t0)
	s.Registered("i-0000", &Runner{ID: 5, Name: "set-i-0000-aa", Issued: t0, Busy: true})
	s.Listed(map[string]ListedRunner{}, t0.Add(30*time.Second))
	if v := ask(s, "i-0000", t0.Add(40*time.Second)); v != Wait {
		t.Fatalf("a listing too soon after it was issued proves nothing: %v", v)
	}
	s.Listed(map[string]ListedRunner{}, t0.Add(2*time.Minute))
	if v := ask(s, "i-0000", t0.Add(2*time.Minute)); v != End {
		t.Fatalf("its job's completion was missed, and GitHub has deleted the runner: %v, want End (its job is over)", v)
	}
}

func TestARunnerThatNeverStartedAndIsGoneLeavesTheMachineFree(t *testing.T) {
	s := NewState("set", 0, 10)
	s.Observe(stats(1, 0), t0)
	launch(s, machines(1, t0), t0)
	s.Registered("i-0000", &Runner{ID: 5, Name: "set-i-0000-aa", Issued: t0})
	s.Listed(map[string]ListedRunner{}, t0.Add(2*time.Minute))
	if v := ask(s, "i-0000", t0.Add(2*time.Minute)); v != Grant {
		t.Fatalf("a runner that never took a job and GitHub no longer lists: %v, want Grant", v)
	}
}

func TestAMachinePastItsAgeIsToldToEnd(t *testing.T) {
	s := NewState("set", 5, 10)
	s.Observe(stats(0, 0), t0)
	launch(s, machines(1, t0), t0)
	if v, _ := s.Ask("i-0000", t0.Add(9*time.Hour), open(), 8*time.Hour); v != End {
		t.Fatalf("an 9-hour-old machine: %v, want End", v)
	}
}

func TestAMachineLostMidJobIsReportedForARerun(t *testing.T) {
	s := NewState("set", 0, 10)
	ms := machines(2, t0)
	s.Seen("us-east-1", ms, t0)
	s.Registered("i-0000", &Runner{ID: 5, Name: "set-i-0000-aa", Issued: t0})
	s.Registered("i-0001", &Runner{ID: 6, Name: "set-i-0001-bb", Issued: t0})
	s.Observe(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{
		{RunnerName: "set-i-0000-aa", JobMessageBase: scaleset.JobMessageBase{OwnerName: "o", RepositoryName: "r", WorkflowRunID: 9, JobDisplayName: "a"}},
		{RunnerName: "set-i-0001-bb", JobMessageBase: scaleset.JobMessageBase{OwnerName: "o", RepositoryName: "r", WorkflowRunID: 9, JobDisplayName: "b"}},
	}}, t0)
	s.Observe(&scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "set-i-0001-bb"}}}, t0)
	lost := s.Seen("us-east-1", nil, t0.Add(5*time.Minute))
	if len(lost) != 1 || lost[0].Name != "a" || lost[0].RunID != 9 || lost[0].Runner != "set-i-0000-aa" {
		t.Fatalf("lost %+v: want only the job whose runner vanished before completing it", lost)
	}
}

func TestTheSpendCapStopsLaunchesAndThePool(t *testing.T) {
	s := NewState("set", 5, 100)
	s.Observe(stats(3, 0), t0)
	sp := NewSpend(1, 0, []SetSpec{{Name: "set", CPU: 4, DiskGB: 60}}, t0)
	ms := []Machine{}
	for i := 0; i < 10; i++ {
		ms = append(ms, Machine{Set: "set", Lifecycle: "on-demand"})
	}
	sp.Accrue(t0.Add(time.Minute), ms)
	if sp.Over() {
		t.Fatalf("over after a minute of 10 machines: %+v", sp.Snapshot())
	}
	for i := 2; i <= 30; i++ {
		sp.Accrue(t0.Add(time.Duration(i)*time.Minute), ms)
	}
	if !sp.Over() {
		t.Fatalf("not over a $1 cap after half an hour of 10 on-demand 4-vCPU machines: %+v", sp.Snapshot())
	}
	s.SetCapped(sp.Over())
	if _, n := s.Deficit(t0, 1000); n != 0 {
		t.Fatalf("launched %d while capped", n)
	}
	if d := s.Snapshot(t0).Desired; d != 3 {
		t.Fatalf("desired %d while capped: want the 3 jobs and no pool", d)
	}
}

func TestSpendRollsOverAtMidnight(t *testing.T) {
	day := time.Date(2026, 11, 25, 23, 58, 0, 0, time.UTC)
	sp := NewSpend(100, 1000, []SetSpec{{Name: "set", CPU: 2, DiskGB: 40}}, day)
	ms := []Machine{{Set: "set", Lifecycle: "on-demand"}}
	for i := 1; i <= 3; i++ {
		sp.Accrue(day.Add(time.Duration(i)*time.Minute), ms)
	}
	if snap := sp.Snapshot(); snap.Day != "2026-11-26" || snap.DayUSD >= snap.MonthUSD {
		t.Fatalf("after midnight: %+v, want a new day with less than the month", snap)
	}
}

func TestUntilTheQueueIsKnownMachinesWaitAndNothingEndsOrLaunches(t *testing.T) {
	s := NewState("set", 0, 10)
	s.Seen("us-east-1", machines(3, t0), t0)
	s.Registered("i-0002", &Runner{ID: 7, Name: "set-i-0002-cc", Issued: t0.Add(-time.Hour)})
	if v := ask(s, "i-0000", t0); v != Wait {
		t.Fatalf("an ask before the session's statistics: %v, want Wait (the queue may hold its job)", v)
	}
	if got := s.Surplus(t0.Add(time.Hour), 20*time.Second, 100); len(got) != 0 {
		t.Fatalf("surplus %v before the statistics", ids(got))
	}
	if _, n := s.Deficit(t0, 100); n != 0 {
		t.Fatalf("launched %d before the statistics", n)
	}
	s.Observe(stats(3, 0), t0)
	if v := ask(s, "i-0000", t0); v != Grant {
		t.Fatalf("after the statistics (three jobs for three machines): %v, want Grant", v)
	}
}

func TestShedEndsOnlyMachinesLongWaitingWithNoRunner(t *testing.T) {
	s := NewState("set", 0, 10)
	old, fresh := machines(3, t0), machines(1, t0.Add(9*time.Minute))
	fresh[0].ID = "i-new"
	s.Seen("us-east-1", append(old, fresh...), t0)
	s.Registered("i-0001", &Runner{ID: 5, Name: "set-i-0001-aa", Issued: t0})
	got := s.Shed(t0.Add(10*time.Minute), 5)
	if len(got) != 2 || got[0].ID == "i-0001" || got[1].ID == "i-0001" {
		t.Fatalf("shed %v: want the two old machines with no runner, not the one with a runner nor the new one", ids(got))
	}
	if snap := s.Snapshot(t0.Add(10 * time.Minute)); snap.Ending != 2 {
		t.Fatalf("ending %d", snap.Ending)
	}
}

func TestAJobThatEndsOnAMachineBeingTakenBackIsLost(t *testing.T) {
	s := NewState("set", 0, 10)
	s.Seen("us-east-1", machines(2, t0), t0)
	for i, name := range []string{"set-i-0000-aa", "set-i-0001-bb"} {
		s.Registered(fmt.Sprintf("i-%04d", i), &Runner{ID: int64(i + 1), Name: name, Issued: t0})
	}
	s.Observe(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{
		{RunnerName: "set-i-0000-aa", JobMessageBase: scaleset.JobMessageBase{WorkflowRunID: 9, JobDisplayName: "a"}},
		{RunnerName: "set-i-0001-bb", JobMessageBase: scaleset.JobMessageBase{WorkflowRunID: 9, JobDisplayName: "b"}},
	}}, t0)
	s.Noticed("i-0000")
	s.Observe(&scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "set-i-0000-aa"}, {RunnerName: "set-i-0001-bb"}}}, t0)
	if lost := s.TakeLost(); len(lost) != 1 || lost[0].Name != "a" {
		t.Fatalf("lost %+v: want the job of the noticed machine only", lost)
	}
	if lost := s.TakeLost(); len(lost) != 0 {
		t.Fatalf("handed over twice: %+v", lost)
	}
}

func TestAStartedJobsQueueWaitRunsFromWhenItBecameAvailable(t *testing.T) {
	s := NewState("set", 0, 10)
	s.Observe(&scaleset.RunnerScaleSetMessage{JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 7}}}}, t0)
	s.Observe(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 7}}}}, t0.Add(6*time.Second))
	if w := s.TakeWaits(); len(w) != 1 || w[0] != 6 {
		t.Fatalf("waits %v, want one of 6 s", w)
	}
}
