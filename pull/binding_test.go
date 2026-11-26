package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// hostile asks for a runner from machine id every 3 s from at for d, as a job that keeps its machine alive and keeps
// asking would, and returns how many asks were granted.
func hostile(s *State, id string, at time.Time, d time.Duration) (grants int) {
	for t := at; t.Before(at.Add(d)); t = t.Add(3 * time.Second) {
		if ask(s, id, t) == Grant {
			grants++
			s.Unissued(s.Machine(id))
		}
	}
	return grants
}

func started(name string, run int64) *scaleset.RunnerScaleSetMessage {
	return &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{RunnerName: name,
		JobMessageBase: scaleset.JobMessageBase{OwnerName: "o", RepositoryName: "r", WorkflowRunID: run, JobDisplayName: name}}},
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1, TotalRunningJobs: 1}}
}

func completed(name string) *scaleset.RunnerScaleSetMessage {
	return &scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: name}},
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}
}

// SEC-F1: a restart forgot which machines held a runner, so a machine whose runner had finished its job got another.
func TestARestartNeverGivesAMachineASecondRunner(t *testing.T) {
	s := newState(0, 10)
	s.Observe(stats(1, 0), t0)
	launch(s, machines(1, t0), t0)
	if v := ask(s, "i-0000", t0); v != Grant {
		t.Fatalf("first ask: %v", v)
	}
	give(s, "i-0000", &Runner{ID: 5, Name: "set-i-0000", Issued: t0})
	s.Observe(started("set-i-0000", 9), t0.Add(time.Minute))
	s.Observe(completed("set-i-0000"), t0.Add(2*time.Minute))
	if end := s.ToEnd(t0.Add(2 * time.Minute)); len(end) != 1 {
		t.Fatalf("its job completed: ending %v, want the machine ended by the listener at once", ids(end))
	}

	// The process restarts while the machine, whose job stopped its power-off, keeps asking. EC2 lists it with its
	// issued tag; GitHub has deleted the ephemeral runner, so no listing shows one; the queue wants a machine.
	after := t0.Add(5 * time.Minute)
	r := NewState("set", 0, 10, []string{"us-east-1"}, after)
	tagged := machines(1, t0)
	tagged[0].Issued, tagged[0].Tagged = true, true
	r.Observe(stats(1, 0), after)
	if g := hostile(r, "i-0000", after, 20*time.Second); g != 0 {
		t.Fatalf("granted %d before any listing", g)
	}
	r.Seen("us-east-1", tagged, after.Add(time.Minute), after.Add(time.Minute))
	r.Listed(map[string]ListedRunner{}, after.Add(time.Minute))
	if g := hostile(r, "i-0000", after.Add(time.Minute), 10*time.Minute); g != 0 {
		t.Fatalf("granted %d runners to a machine EC2 says was issued one", g)
	}
	_, unlisted := r.Stuck(after.Add(11*time.Minute), 5*time.Minute, 3*time.Minute)
	if len(unlisted) != 1 {
		t.Fatalf("to check %v: an issued machine with no runner listed is checked with GitHub, however lately it asked", ids(unlisted))
	}
	r.Checked(unlisted[0], Absent, nil)
	if end := r.ToEnd(after.Add(11 * time.Minute)); len(end) != 1 || end[0].ID != "i-0000" {
		t.Fatalf("GitHub has no runner of it: ending %v, want the machine", ids(end))
	}
}

// SEC-F1: a job message for a machine whose runner a restart left unknown is that runner's.
func TestAJobCompletedOnAMachineAdoptedAfterARestartEndsIt(t *testing.T) {
	s := newState(0, 10)
	s.Seen("us-east-1", machines(1, t0), t0, t0)
	s.Observe(completed("set-i-0000"), t0)
	if end := s.ToEnd(t0); len(end) != 1 {
		t.Fatalf("ending %v: a job completed on it, so it ends", ids(end))
	}
	if v := ask(s, "i-0000", t0); v != End {
		t.Fatalf("its ask: %v, want End", v)
	}
}

// SEC-F1: a machine GitHub lists a runner of is issued, whatever its tags say, and its tag is written.
func TestAMachineWithAListedRunnerIsIssuedAndTagged(t *testing.T) {
	s := newState(0, 10)
	s.Observe(stats(1, 0), t0)
	s.Seen("us-east-1", machines(1, t0), t0, t0)
	s.Listed(map[string]ListedRunner{"set-i-0000-ab": {ID: 3, Name: "set-i-0000-ab"}}, t0)
	if untagged := s.Untagged(); len(untagged) != 1 {
		t.Fatalf("untagged %v: want the machine whose runner the listing showed", ids(untagged))
	}
	if again := s.Untagged(); len(again) != 0 {
		t.Fatalf("tagged twice at once: %v", ids(again))
	}
	if v := ask(s, "i-0000", t0); v != End {
		t.Fatalf("its ask: %v, want End", v)
	}
}

// SEC-F2: a runner that never started was replaced by another, so a job that finished before its JobStarted was
// read got a second runner.
func TestARunnerThatNeverStartedIsNeverReplaced(t *testing.T) {
	s := newState(0, 10)
	s.Observe(stats(1, 0), t0)
	launch(s, machines(1, t0), t0)
	if v := ask(s, "i-0000", t0); v != Grant {
		t.Fatalf("first ask: %v", v)
	}
	if v := ask(s, "i-0000", t0.Add(time.Second)); v != Wait {
		t.Fatalf("an ask while its registration is in flight: %v, want Wait", v)
	}
	give(s, "i-0000", &Runner{ID: 5, Name: "set-i-0000", Issued: t0})
	if g := hostile(s, "i-0000", t0.Add(2*time.Second), 5*time.Minute); g != 0 {
		t.Fatalf("granted %d more runners to a machine that has one", g)
	}
	fence := s.ToFence(t0, 10)
	if len(fence) != 1 {
		t.Fatalf("to fence %v: a machine that asks again does not run its idle runner, which is removed", ids(fence))
	}
	s.Fenced(fence[0], Removed, t0)
	if end := s.ToEnd(t0.Add(6 * time.Minute)); len(end) != 1 {
		t.Fatalf("ending %v: once its runner is removed, the machine ends", ids(end))
	}
	if g := hostile(s, "i-0000", t0.Add(6*time.Minute), time.Minute); g != 0 {
		t.Fatalf("granted %d after its runner was removed", g)
	}
}

func TestASecondAskWhileAJobRunsGetsNoRunnerAndEndsNothing(t *testing.T) {
	s := newState(0, 10)
	s.Observe(stats(1, 0), t0)
	launch(s, machines(1, t0), t0)
	ask(s, "i-0000", t0)
	give(s, "i-0000", &Runner{ID: 5, Name: "set-i-0000", Issued: t0})
	s.Observe(started("set-i-0000", 9), t0)
	if g := hostile(s, "i-0000", t0.Add(time.Minute), 5*time.Minute); g != 0 {
		t.Fatalf("granted %d while its runner holds a job", g)
	}
	if end, fence := s.ToEnd(t0.Add(6*time.Minute)), s.ToFence(t0, 10); len(end)+len(fence) != 0 {
		t.Fatalf("ending %v, fencing %v: an ask is no proof the job is over", ids(end), ids(fence))
	}
}

func TestARegistrationWhoseConfigNeverLeftEndsTheMachine(t *testing.T) {
	s := newState(0, 10)
	s.Observe(stats(2, 0), t0)
	launch(s, machines(2, t0), t0)
	ask(s, "i-0000", t0)
	s.Withheld(s.Machine("i-0000"), &Runner{ID: 5, Name: "set-i-0000", Issued: t0}) // its tag could not be written
	ask(s, "i-0001", t0)
	s.Withheld(s.Machine("i-0001"), nil) // GitHub already had a runner of its name
	if v := ask(s, "i-0000", t0.Add(time.Second)); v != End {
		t.Fatalf("after a withheld config: %v, want End", v)
	}
	if fence := s.ToFence(t0, 10); len(fence) != 1 || fence[0].ID != "i-0000" {
		t.Fatalf("to fence %v: the registered runner is removed before its machine ends", ids(fence))
	}
	if end := s.ToEnd(t0); len(end) != 1 || end[0].ID != "i-0001" {
		t.Fatalf("ending %v: the machine whose runner is not known ends, its runner removed as an orphan", ids(end))
	}
}

// OPS-5: an empty machine map after a failed first listing read as no machines.
func TestNothingIsDecidedUntilEveryRegionIsListed(t *testing.T) {
	s := NewState("set", 0, 10, []string{"us-east-1", "us-east-2"}, t0.Add(-time.Hour))
	s.Observe(stats(3, 0), t0)
	east2 := machines(3, t0.Add(-30*time.Minute))
	for _, m := range east2 {
		m.Region = "us-east-2"
	}
	s.Seen("us-east-1", nil, t0, t0)
	if _, n := s.Deficit(t0, 100); n != 0 {
		t.Fatalf("launched %d before us-east-2 was listed", n)
	}
	if orphans := s.Listed(map[string]ListedRunner{"set-i-0000": {ID: 1, Name: "set-i-0000"}}, t0); len(orphans) != 0 {
		t.Fatalf("orphans %v: a runner of a machine in a region not listed yet", orphans)
	}
	if ended, unlisted := s.Stuck(t0.Add(time.Hour), 5*time.Minute, 3*time.Minute); len(ended)+len(unlisted) != 0 {
		t.Fatalf("stuck %v %v before every region was listed", ids(ended), ids(unlisted))
	}
	s.Seen("us-east-2", east2, t0, t0)
	if _, n := s.Deficit(t0, 100); n != 0 {
		t.Fatalf("launched %d with the three machines us-east-2 holds", n)
	}
	if orphans := s.Listed(map[string]ListedRunner{"set-i-0000": {ID: 1, Name: "set-i-0000"}, "set-i-0009": {ID: 2, Name: "set-i-0009"}}, t0); len(orphans) != 1 {
		t.Fatalf("orphans %v: want only the runner of no machine", orphans)
	}
}

// OPS-3: arrivals were divided by the whole window, so a restart read as no arrivals and the pool fell to its floor.
// The pool now counts the jobs that arrived within a launch time of each, which a restart cannot dilute.
func TestARestartDoesNotReadAsNoArrivals(t *testing.T) {
	s := NewState("set", 0, 10, []string{"us-east-1"}, t0)
	for i := 0; i < 300; i++ {
		s.Observe(&scaleset.RunnerScaleSetMessage{JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: scaleset.JobMessageBase{JobID: fmt.Sprint(i)}}}},
			t0.Add(time.Duration(i)*200*time.Millisecond))
	}
	if n := s.Burst(t0.Add(time.Minute), testRule); n != 110 {
		t.Fatalf("burst %d a minute after a start, want 110 (5 jobs a second for the 22 s a launch takes)", n)
	}
}

// OPS-3: surplus pool runners were trimmed in the first window after a start, while the pool read as its floor.
func TestNoPoolRunnerIsTrimmedInTheFirstWindowAfterAStart(t *testing.T) {
	s := NewState("set", 0, 10, []string{"us-east-1"}, t0)
	pool := machines(3, t0.Add(-time.Hour))
	for _, m := range pool {
		m.Issued, m.Tagged = true, true
	}
	s.Seen("us-east-1", pool, t0.Add(time.Minute), t0.Add(time.Minute))
	for _, m := range pool {
		s.Listed(map[string]ListedRunner{runnerName("set", m.ID): {ID: 1, Name: runnerName("set", m.ID)}}, t0.Add(time.Minute))
	}
	s.Observe(stats(0, 0), t0.Add(time.Minute))
	s.Surplus(t0.Add(2*time.Minute), 20*time.Second, 10*time.Minute, 100)
	s.Observe(stats(0, 0), t0.Add(3*time.Minute))
	if n := s.Surplus(t0.Add(3*time.Minute), 20*time.Second, 10*time.Minute, 100); n != 0 {
		t.Fatalf("retired %d pool runners 3 minutes after a start", n)
	}
	s.Observe(stats(0, 0), t0.Add(11*time.Minute))
	s.Surplus(t0.Add(11*time.Minute), 20*time.Second, 10*time.Minute, 100)
	if n := s.Surplus(t0.Add(12*time.Minute), 20*time.Second, 10*time.Minute, 100); n == 0 {
		t.Fatal("no surplus retired once the window has passed")
	}
}

// OPS-8: any removal error was taken for a job, which kept an idle runner busy for ever.
func TestARemovalThatFailsIsTriedAgainNotTakenForAJob(t *testing.T) {
	cases := []struct {
		err  error
		want Removal
	}{
		{nil, Removed},
		{fmt.Errorf("delete: %w", scaleset.RunnerNotFoundError), Removed},
		{fmt.Errorf("delete: %w", scaleset.NotFoundError), Removed},
		{fmt.Errorf("delete: %w: %w", scaleset.BadRequestError, scaleset.JobStillRunningError), HeldJob},
		{fmt.Errorf("delete: %w", scaleset.BadRequestError), NotSure},
		{errors.New("dial tcp: i/o timeout"), NotSure},
	}
	for _, c := range cases {
		if got := removal(c.err); got != c.want {
			t.Fatalf("removal(%v) = %v, want %v", c.err, got, c.want)
		}
	}
	s := newState(0, 10)
	s.Observe(stats(0, 0), t0)
	launch(s, machines(1, t0), t0)
	give(s, "i-0000", &Runner{ID: 5, Name: "set-i-0000", Issued: t0})
	s.Noticed("i-0000")
	m := s.ToFence(t0, 10)[0]
	s.Fenced(m, NotSure, t0)
	if snap := s.Snapshot(t0); snap.Busy != 0 || snap.Retiring != 1 {
		t.Fatalf("after a failed removal: %+v, want the runner idle and still retiring", snap)
	}
	if again := s.ToFence(t0.Add(fenceBackoff), 10); len(again) != 1 {
		t.Fatalf("to fence %v: a failed removal is tried again once its backoff is over", ids(again))
	}
}

// OPS-13: a runner that finished its job was checked with GitHub as stuck, again and again.
func TestAFinishedRunnerEndsItsMachineAndIsNeverChecked(t *testing.T) {
	s := newState(0, 10)
	s.Observe(stats(1, 0), t0)
	launch(s, machines(1, t0), t0)
	give(s, "i-0000", &Runner{ID: 5, Name: "set-i-0000", Issued: t0})
	s.Observe(started("set-i-0000", 9), t0)
	s.Observe(completed("set-i-0000"), t0.Add(time.Minute))
	s.Listed(map[string]ListedRunner{}, t0.Add(10*time.Minute))
	if ended, unlisted := s.Stuck(t0.Add(10*time.Minute), 5*time.Minute, 3*time.Minute); len(ended)+len(unlisted) != 0 {
		t.Fatalf("stuck %v, to check %v: a finished runner's machine is already ending", ids(ended), ids(unlisted))
	}
}

// OPS-6: the machine bounded a job from its runner's start, which killed a pool runner (and its job) 6.5 h after
// it registered; the listener now bounds a job from when it started, and an idle runner by its machine's age.
func TestJobsAreBoundedFromTheirStartAndIdleRunnersByAge(t *testing.T) {
	s := newState(0, 10)
	s.Observe(stats(3, 0), t0)
	launch(s, machines(3, t0), t0)
	for _, id := range []string{"i-0000", "i-0001", "i-0002"} {
		give(s, id, &Runner{ID: 1, Name: runnerName("set", id), Issued: t0})
	}
	job, idle, life := 390*time.Minute, 8*time.Hour, 8*time.Hour+390*time.Minute
	at := t0.Add(7 * time.Hour)
	s.Observe(started("set-i-0000", 1), at) // a pool runner takes a job after 7 h
	if ended := s.Overdue(at.Add(2*time.Hour), job, idle, life); len(ended) != 0 {
		t.Fatalf("ended %v: a job 2 h in, on a 9 h-old machine, is within its bound", ids(ended))
	}
	if fence := s.ToFence(t0, 10); len(fence) != 2 {
		t.Fatalf("to fence %v: the idle runners of 9 h-old machines retire", ids(fence))
	}
	if ended := s.Overdue(at.Add(job+time.Minute), job, idle, life); len(ended) != 1 || ended[0].ID != "i-0000" {
		t.Fatalf("ended %v: a job past its bound from its start ends", ids(ended))
	}
}

func TestAMachinePastItsAgeIsToldToEnd(t *testing.T) {
	s := newState(5, 10)
	s.Observe(stats(0, 0), t0)
	launch(s, machines(1, t0), t0)
	if v := ask(s, "i-0000", t0.Add(9*time.Hour)); v != End {
		t.Fatalf("an 9-hour-old machine: %v, want End", v)
	}
}

func TestAMachineLostMidJobIsReportedForARerun(t *testing.T) {
	s := newState(0, 10)
	s.Seen("us-east-1", machines(2, t0), t0, t0)
	give(s, "i-0000", &Runner{ID: 5, Name: "set-i-0000", Issued: t0})
	give(s, "i-0001", &Runner{ID: 6, Name: "set-i-0001", Issued: t0})
	s.Observe(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{
		{RunnerName: "set-i-0000", JobMessageBase: scaleset.JobMessageBase{OwnerName: "o", RepositoryName: "r", WorkflowRunID: 9, JobDisplayName: "a"}},
		{RunnerName: "set-i-0001", JobMessageBase: scaleset.JobMessageBase{OwnerName: "o", RepositoryName: "r", WorkflowRunID: 9, JobDisplayName: "b"}},
	}}, t0)
	s.Observe(&scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "set-i-0001"}}}, t0)
	lost := s.Seen("us-east-1", nil, t0.Add(5*time.Minute), t0.Add(5*time.Minute))
	if len(lost) != 1 || lost[0].Name != "a" || lost[0].RunID != 9 || lost[0].Runner != "set-i-0000" {
		t.Fatalf("lost %+v: want only the job whose runner vanished before completing it", lost)
	}
}

func TestTheSpendCapStopsLaunchesAndThePool(t *testing.T) {
	s := newState(5, 100)
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

// OPS-11: the estimate left out each machine's public IPv4 address.
func TestAMachinesPriceCountsItsAddress(t *testing.T) {
	sp := NewSpend(0, 0, []SetSpec{{Name: "set", CPU: 2, DiskGB: 40}}, t0)
	sp.SetPrices(map[string]float64{"us-east-1a/c7a.large": 0.03})
	got := sp.price(Machine{Set: "set", Lifecycle: "spot", AZ: "us-east-1a", Type: "c7a.large"})
	if want := 0.03 + 40*gp3PerGBHour + gp3ExtraPerHour + 0.005; got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("price %v, want %v (spot, gp3 and the address)", got, want)
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
	s := newState(0, 10)
	s.Seen("us-east-1", machines(3, t0), t0, t0)
	give(s, "i-0002", &Runner{ID: 7, Name: "set-i-0002", Issued: t0.Add(-time.Hour)})
	if v := ask(s, "i-0000", t0); v != Wait {
		t.Fatalf("an ask before the session's statistics: %v, want Wait (the queue may hold its job)", v)
	}
	if n := s.Surplus(t0.Add(time.Hour), 20*time.Second, 10*time.Minute, 100); n != 0 {
		t.Fatalf("surplus %d before the statistics", n)
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
	s := newState(0, 10)
	old, fresh := machines(4, t0), machines(1, t0.Add(9*time.Minute))
	fresh[0].ID = "i-new"
	old[3].Issued = true // issued before a restart, its runner not known yet
	s.Seen("us-east-1", append(old, fresh...), t0, t0)
	give(s, "i-0001", &Runner{ID: 5, Name: "set-i-0001", Issued: t0})
	s.Observe(stats(5, 0), t0.Add(10*time.Minute))
	s.Listed(map[string]ListedRunner{}, t0.Add(10*time.Minute))
	got := s.Shed(t0.Add(10*time.Minute), 5)
	if len(got) != 2 || got[0].ID == "i-0001" || got[1].ID == "i-0001" || got[0].ID == "i-0003" || got[1].ID == "i-0003" {
		t.Fatalf("shed %v: want the two old machines never issued a runner, not the one with a runner, the issued one nor the new one", ids(got))
	}
	if snap := s.Snapshot(t0.Add(10 * time.Minute)); snap.Ending != 2 {
		t.Fatalf("ending %d", snap.Ending)
	}
}

func TestAJobThatEndsOnAMachineBeingTakenBackIsLost(t *testing.T) {
	s := newState(0, 10)
	s.Seen("us-east-1", machines(2, t0), t0, t0)
	for i, name := range []string{"set-i-0000", "set-i-0001"} {
		give(s, fmt.Sprintf("i-%04d", i), &Runner{ID: int64(i + 1), Name: name, Issued: t0})
	}
	s.Observe(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{
		{RunnerName: "set-i-0000", JobMessageBase: scaleset.JobMessageBase{WorkflowRunID: 9, JobDisplayName: "a"}},
		{RunnerName: "set-i-0001", JobMessageBase: scaleset.JobMessageBase{WorkflowRunID: 9, JobDisplayName: "b"}},
	}}, t0)
	s.Noticed("i-0000")
	s.Observe(&scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "set-i-0000"}, {RunnerName: "set-i-0001"}}}, t0)
	if lost := s.TakeLost(); len(lost) != 1 || lost[0].Name != "a" {
		t.Fatalf("lost %+v: want the job of the noticed machine only", lost)
	}
	if lost := s.TakeLost(); len(lost) != 0 {
		t.Fatalf("handed over twice: %+v", lost)
	}
}

func TestAStartedJobsQueueWaitRunsFromWhenItBecameAvailable(t *testing.T) {
	s := newState(0, 10)
	s.Observe(&scaleset.RunnerScaleSetMessage{JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 7}}}}, t0)
	s.Observe(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 7}}}}, t0.Add(6*time.Second))
	if w := s.TakeWaits(); len(w) != 1 || w[0] != 6 {
		t.Fatalf("waits %v, want one of 6 s", w)
	}
}

// OPS-12: the list of lost jobs was in memory only; a noticed machine's job is now kept on EC2.
func TestASpotNoticeRecordsTheJobAndARestartReadsItBack(t *testing.T) {
	s := newState(0, 10)
	s.Observe(stats(1, 0), t0)
	launch(s, machines(1, t0), t0)
	give(s, "i-0000", &Runner{ID: 5, Name: "set-i-0000", Issued: t0})
	s.Observe(started("set-i-0000", 42), t0)
	j := s.Noticed("i-0000")
	if j == nil || j.RunID != 42 || j.Owner != "o" || j.Repo != "r" {
		t.Fatalf("noticed job %+v: want run 42 of o/r, for the machine's notice tag", j)
	}
	got, ok := noticedJob("set", "i-0000", fmt.Sprintf("%s/%s/%d", j.Owner, j.Repo, j.RunID))
	if !ok || got.RunID != 42 || got.Runner != "set-i-0000" {
		t.Fatalf("the tag read back: %+v %v", got, ok)
	}
	for _, bad := range []string{"", "o/r", "o/r/x", "/r/1", "o/r/1/2"} {
		if _, ok := noticedJob("set", "i-0000", bad); ok {
			t.Fatalf("tag %q read as a job", bad)
		}
	}
	r := newState(0, 10)
	r.Observe(stats(1, 0), t0)
	noticed := machines(1, t0)
	noticed[0].Issued, noticed[0].Notice = true, true
	r.Seen("us-east-1", noticed, t0, t0)
	if live := r.Snapshot(t0).Live; live != 0 {
		t.Fatalf("live %d: a machine EC2 lists as noticed is no supply after a restart", live)
	}
}
