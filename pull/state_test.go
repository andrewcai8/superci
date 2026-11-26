package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

var t0 = time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)

func stats(assigned, available int) *scaleset.RunnerScaleSetMessage {
	return &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: assigned, TotalAvailableJobs: available}}
}

// newState is a set of one region whose last listing began well after the process started: grantable once the
// queue is known.
func newState(pool, max int) *State {
	s := NewState("set", pool, max, []string{"us-east-1"}, t0.Add(-2*time.Hour))
	s.listings["us-east-1"] = t0.Add(-time.Hour)
	return s
}

func machines(n int, at time.Time) []*Machine {
	out := make([]*Machine, n)
	for i := range out {
		out[i] = &Machine{ID: fmt.Sprintf("i-%04d", i), Region: "us-east-1", IP: fmt.Sprintf("10.0.%d.%d", i/250, i%250), Lifecycle: "spot", Launched: at, Asked: at}
	}
	return out
}

func TestABurstIsLaunchedOnceWhileItsCallsAreInFlight(t *testing.T) {
	s := newState(2, 1200)
	s.Observe(stats(0, 1000), t0)
	round, n := s.Deficit(t0, 100000)
	if n != 1002 {
		t.Fatalf("deficit %d, want the burst plus the pool", n)
	}
	if _, n := s.Deficit(t0, 100000); n != 0 {
		t.Fatalf("asked again for capacity already in flight: %d", n)
	}
	got := machines(900, t0)
	for _, m := range got {
		m.Round = round
	}
	s.Seen("us-east-1", got[:400], t0, t0) // a listing shows part of the round before its call answers
	if live := s.Snapshot(t0).Live; live != 1002 {
		t.Fatalf("live %d while the call is unanswered: a machine seen early is counted twice or not at all", live)
	}
	if v := ask(s, got[0].ID, t0); v != Grant {
		t.Fatalf("a machine of the round that boots before its call answers: %v, want Grant", v)
	}
	s.Launched(round, got, t0)
	if _, n := s.Deficit(t0, 100000); n != 102 {
		t.Fatalf("a short launch leaves %d to ask again, want 102", n)
	}
}

func TestTheCapBoundsTheSupply(t *testing.T) {
	s := newState(0, 300)
	s.Observe(stats(1000, 0), t0)
	if _, n := s.Deficit(t0, 100000); n != 300 {
		t.Fatalf("deficit %d, want the cap", n)
	}
}

func TestAMachineIsToldToEndOnceTheQueueNoLongerNeedsIt(t *testing.T) {
	s := newState(1, 1200)
	s.Observe(stats(3, 0), t0)
	launch(s, machines(4, t0), t0)
	for i := 0; i < 4; i++ {
		if v := ask(s, fmt.Sprintf("i-%04d", i), t0); v != Grant {
			t.Fatalf("machine %d: %v, want a runner while all four are needed", i, v)
		}
		s.Unissued(s.Machine(fmt.Sprintf("i-%04d", i)))
	}
	s.Observe(stats(1, 0), t0) // two jobs completed
	if v := ask(s, "i-0000", t0); v != End {
		t.Fatalf("got %v, want End: four machines for one job and a pool of one", v)
	}
	if v := ask(s, "i-0001", t0); v != End {
		t.Fatalf("got %v, want End", v)
	}
	if v := ask(s, "i-0002", t0); v != Grant {
		t.Fatalf("got %v, want Grant: two machines left for one job and a pool of one", v)
	}
	if v := ask(s, "i-9999", t0); v != Unknown {
		t.Fatalf("a machine not ours: %v", v)
	}
}

func TestWaitingJobsThatIdleRunnersIgnoreAreStale(t *testing.T) {
	s := newState(0, 1200)
	s.lastActivity = t0
	s.Observe(&scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 3, TotalIdleRunners: 3}}, t0)
	if stale := s.MarkStale(t0.Add(2*time.Minute), 3*time.Minute); stale != 0 {
		t.Fatalf("stale %d too soon", stale)
	}
	if stale := s.MarkStale(t0.Add(4*time.Minute), 3*time.Minute); stale != 3 || s.Snapshot(t0).Desired != 0 {
		t.Fatalf("stale %d, desired %d: want 3 and 0", stale, s.Snapshot(t0).Desired)
	}
	s.Observe(&scaleset.RunnerScaleSetMessage{JobAssignedMessages: []*scaleset.JobAssigned{{}},
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 4, TotalIdleRunners: 3}}, t0)
	if d := s.Snapshot(t0).Desired; d != 4 {
		t.Fatalf("a new assignment makes the count trusted again: desired %d, want 4", d)
	}
	s.lastActivity = t0
	s.Observe(stats(5, 0), t0) // five waiting, no idle runner: real demand however old
	if stale := s.MarkStale(t0.Add(10*time.Minute), 3*time.Minute); stale != 0 {
		t.Fatalf("stale %d with no idle runner to take them", stale)
	}
}

func TestOnlyAMachineThatCanHoldNoJobIsStuck(t *testing.T) {
	s := newState(0, 1200)
	s.Observe(stats(4, 0), t0)
	launch(s, machines(4, t0), t0)
	issued := t0.Add(30 * time.Second)
	give(s, "i-0001", &Runner{ID: 1, Name: "set-i-0001", Issued: issued, Busy: true})
	give(s, "i-0002", &Runner{ID: 2, Name: "set-i-0002", Issued: issued})
	give(s, "i-0003", &Runner{ID: 3, Name: "set-i-0003", Issued: issued})

	later := t0.Add(10 * time.Minute)
	if ended, unlisted := s.Stuck(later, 5*time.Minute, 3*time.Minute); len(ended)+len(unlisted) != 0 {
		t.Fatalf("before any listing, nothing is stuck: %v %v", ids(ended), ids(unlisted))
	}
	for i := 0; i < 3; i++ {
		s.Listed(map[string]ListedRunner{"set-i-0002": {ID: 2, Name: "set-i-0002"}}, later.Add(time.Duration(i)*time.Minute))
	}
	ended, unlisted := s.Stuck(later.Add(3*time.Minute), 5*time.Minute, 3*time.Minute)
	if len(ended) != 1 || ended[0].ID != "i-0000" {
		t.Fatalf("ended %v: want only the machine never issued a runner that three listings showed none of", ids(ended))
	}
	if got := ids(unlisted); len(got) != 2 || !(got[0] == "i-0001" && got[1] == "i-0003" || got[0] == "i-0003" && got[1] == "i-0001") {
		t.Fatalf("to check %v: want both runners the listings do not show, busy or not, and not the listed one", got)
	}
	s.Checked(s.Machine("i-0001"), Present, nil)
	if snap := s.Snapshot(later); snap.Ending != 1 {
		t.Fatalf("a runner GitHub still has keeps its machine: %+v", snap)
	}
	s.Checked(s.Machine("i-0003"), Absent, nil)
	if snap := s.Snapshot(later); snap.Ending != 2 {
		t.Fatalf("a runner GitHub no longer has ends its machine: %+v", snap)
	}
	s.Checked(s.Machine("i-0002"), Unsure, nil)
	if snap := s.Snapshot(later); snap.Ending != 2 {
		t.Fatalf("a failed check ends nothing: %+v", snap)
	}
}

func TestOneListingThatMissesAnIdleRunnerEndsNothing(t *testing.T) {
	s := newState(1, 10)
	s.Observe(stats(0, 0), t0)
	launch(s, machines(1, t0), t0)
	give(s, "i-0000", &Runner{ID: 9, Name: "set-i-0000", Issued: t0})
	s.Listed(map[string]ListedRunner{}, t0.Add(10*time.Minute))
	ended, unlisted := s.Stuck(t0.Add(10*time.Minute), 5*time.Minute, 3*time.Minute)
	if len(ended) != 0 || len(unlisted) != 1 {
		t.Fatalf("ended %v, to check %v: an idle pool runner one listing missed is checked, not ended", ids(ended), ids(unlisted))
	}
	if ended, unlisted := s.Stuck(t0.Add(11*time.Minute), 5*time.Minute, 3*time.Minute); len(ended)+len(unlisted) != 0 {
		t.Fatalf("checked twice at once: %v %v", ids(ended), ids(unlisted))
	}
}

func TestAMachineNeverIssuedThatAskedLatelyIsNotStuck(t *testing.T) {
	s := newState(0, 1200)
	s.Observe(stats(1, 0), t0)
	launch(s, machines(1, t0), t0)
	for i := 0; i < 3; i++ {
		s.Listed(map[string]ListedRunner{}, t0.Add(time.Duration(2+i)*time.Minute))
	}
	if v := s.Ask("i-0000", t0.Add(9*time.Minute), closed(), 8*time.Hour); v != Wait {
		t.Fatalf("an ask the budget cannot serve: %v, want Wait", v)
	}
	if ended, unlisted := s.Stuck(t0.Add(10*time.Minute), 5*time.Minute, 3*time.Minute); len(ended)+len(unlisted) != 0 {
		t.Fatalf("stuck %v %v", ids(ended), ids(unlisted))
	}
}

func TestSurplusRetiresOnlyIdleRunnersAndOnlyAfterTheHold(t *testing.T) {
	s := newState(0, 1200)
	s.Observe(stats(3, 0), t0)
	launch(s, machines(3, t0), t0)
	give(s, "i-0000", &Runner{ID: 10, Name: "set-i-0000", Issued: t0})
	give(s, "i-0001", &Runner{ID: 11, Name: "set-i-0001", Issued: t0, Busy: true})
	give(s, "i-0002", &Runner{ID: 12, Name: "set-i-0002", Issued: t0})
	s.Observe(stats(1, 0), t0)
	now := t0.Add(time.Minute)
	if n := s.Surplus(now, 20*time.Second, 10*time.Minute, 100); n != 0 {
		t.Fatalf("retired %d before the surplus lasted", n)
	}
	if n := s.Surplus(now.Add(21*time.Second), 20*time.Second, 10*time.Minute, 100); n != 2 {
		t.Fatalf("retired %d: want both idle runners", n)
	}
	got := s.ToFence(t0, 100)
	if len(got) != 2 {
		t.Fatalf("to fence %v: want both idle runners", ids(got))
	}
	for _, m := range got {
		if m.ID == "i-0001" {
			t.Fatal("a busy runner was taken")
		}
	}
	if again := s.ToFence(t0, 100); len(again) != 0 {
		t.Fatalf("fenced twice at once: %v", ids(again))
	}
	s.Fenced(got[0], HeldJob, t0) // GitHub refused: it took a job meanwhile
	s.Fenced(got[1], Removed, t0)
	if snap := s.Snapshot(now); snap.Ending != 1 || snap.Busy != 2 || snap.Retiring != 0 {
		t.Fatalf("after one refusal and one removal: %+v", snap)
	}
}

func TestOnDemandStaysUnderItsShareOfLaunches(t *testing.T) {
	s := newState(0, 1200)
	if room := s.OnDemandRoom(0.1, 100, t0); room != 10 {
		t.Fatalf("room %d of 100, want 10", room)
	}
	od := machines(10, t0)
	for _, m := range od {
		m.Lifecycle = "on-demand"
	}
	s.Launched("r", od, t0)
	if room := s.OnDemandRoom(0.1, 90, t0); room != 0 {
		t.Fatalf("room %d with 10 of 100 on-demand already", room)
	}
	if room := s.OnDemandRoom(0.1, 0, t0.Add(31*time.Minute)); room != 0 {
		t.Fatalf("room %d with nothing to launch", room)
	}
}

func TestStaleAndForeignRunnersAreOrphans(t *testing.T) {
	s := newState(0, 1200)
	s.Observe(stats(1, 0), t0)
	launch(s, machines(1, t0), t0)
	give(s, "i-0000", &Runner{ID: 2, Name: "set-i-0000", Issued: t0})
	orphans := s.Listed(map[string]ListedRunner{
		"set-i-0000":    {ID: 2, Name: "set-i-0000"},
		"set-i-0000-zz": {ID: 1, Name: "set-i-0000-zz"},
		"set-i-gone":    {ID: 3, Name: "set-i-gone"},
	}, t0)
	if len(orphans) != 2 {
		t.Fatalf("orphans %v: want a second runner of the machine and the one of a machine not live", orphans)
	}
}

func TestARestartAdoptsMachinesAndTheirRunners(t *testing.T) {
	s := newState(0, 1200)
	s.Seen("us-east-1", machines(2, t0), t0.Add(time.Hour), t0.Add(time.Hour))
	s.Listed(map[string]ListedRunner{"set-i-0000": {ID: 7, Name: "set-i-0000", Busy: true}}, t0.Add(time.Hour))
	if snap := s.Snapshot(t0.Add(time.Hour)); snap.Machines != 2 || snap.Busy != 1 || snap.Booting != 1 || snap.Issued != 1 {
		t.Fatalf("adopted %+v", snap)
	}
}

func TestASpotNoticeReplacesTheMachineAndRetiresOnlyAnIdleRunner(t *testing.T) {
	s := newState(0, 1200)
	s.Observe(stats(3, 0), t0)
	launch(s, machines(3, t0), t0)
	give(s, "i-0001", &Runner{ID: 11, Name: "set-i-0001", Issued: t0})
	give(s, "i-0002", &Runner{ID: 12, Name: "set-i-0002", Issued: t0, Busy: true})
	for _, id := range []string{"i-0000", "i-0001", "i-0002"} {
		s.Noticed(id)
	}
	if fence := s.ToFence(t0, 100); len(fence) != 1 || fence[0].ID != "i-0001" {
		t.Fatalf("to fence %v: only the idle runner is removed, not the busy one under its job", ids(fence))
	}
	if _, n := s.Deficit(t0, 100000); n != 3 {
		t.Fatalf("launched %d, want 3: no noticed machine is supply", n)
	}
	end := s.ToEnd(t0)
	if len(end) != 1 || end[0].ID != "i-0000" {
		t.Fatalf("ending %v: only the machine with no runner ends at once", ids(end))
	}
}

func TestJobsThatWaitWithNoneStartedAreStalled(t *testing.T) {
	s := newState(0, 10)
	s.Observe(stats(2, 0), t0)
	if s.Stalled(t0.Add(9*time.Minute), 10*time.Minute) {
		t.Fatal("stalled after 9 minutes")
	}
	if !s.Stalled(t0.Add(11*time.Minute), 10*time.Minute) {
		t.Fatal("two jobs waited 11 minutes with none started: not stalled")
	}
	s.Observe(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{RunnerName: "set-i-none"}},
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2, TotalRunningJobs: 1}}, t0.Add(12*time.Minute))
	if s.Stalled(t0.Add(13*time.Minute), 10*time.Minute) {
		t.Fatal("a job started a minute ago: not stalled")
	}
	s.Observe(stats(0, 0), t0.Add(30*time.Minute))
	if s.Stalled(t0.Add(45*time.Minute), 10*time.Minute) {
		t.Fatal("nothing waits: not stalled")
	}
}

// open is a budget that never refuses.
func open() *Budget {
	return NewBudget(RegConfig{Cap: 1e9, WindowS: 300, PoolRate: 1e6, PoolShare: 1, CeilingRate: 1e6, CeilingBurst: 1e6, RecoverS: 1}, t0.Add(-time.Hour))
}

// closed is a budget that registers nothing.
func closed() *Budget {
	return NewBudget(RegConfig{Cap: 1, WindowS: 300, CeilingRate: 0, CeilingBurst: 1}, t0.Add(-time.Hour))
}

func ask(s *State, id string, at time.Time) Verdict {
	return s.Ask(id, at, open(), 8*time.Hour)
}

// give records a runner issued to a machine, as a registration whose tag EC2 accepted does.
func give(s *State, id string, r *Runner) {
	s.Issue(s.Machine(id), r)
}

func launch(s *State, ms []*Machine, at time.Time) {
	round, _ := s.Deficit(t0, 100000)
	s.Launched(round, ms, at)
}

func ids(ms []*Machine) []string {
	out := []string{}
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

// GitHub assigns these sets' jobs with JobAssigned and no JobAvailable, so arrivals and queue waits counted only from
// JobAvailable read as none: the pool kept to its floor and no queue wait was published.
func TestAJobAssignedWithNoJobAvailableArrivesAndIsTimed(t *testing.T) {
	s := NewState("set", 0, 10, []string{"us-east-1"}, t0.Add(-time.Hour))
	queued := t0.Add(-4 * time.Second)
	for i := 0; i < 60; i++ {
		base := scaleset.JobMessageBase{RunnerRequestID: int64(i + 1), QueueTime: queued}
		s.Observe(&scaleset.RunnerScaleSetMessage{JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: base}, {JobMessageBase: base}}}, t0)
	}
	if n := s.Burst(t0.Add(time.Minute), testRule); n != 49 {
		t.Fatalf("burst %d, want 49: 60 jobs at once (each assigned twice, counted once), the 80th percentile's 49th", n)
	}
	s.Observe(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 7}}}}, t0.Add(2*time.Second))
	if w := s.TakeWaits(); len(w) != 1 || w[0] != 6 {
		t.Fatalf("waits %v, want one of 6 s: from when GitHub queued the job", w)
	}
}

// GitHub's JobAssigned carries a job id, no runner request id (0) and no queue time: keyed by the runner request id,
// every job after the first read as one already seen, and the pool kept to its floor.
func TestAJobAssignedWithoutAnOfferSizesThePool(t *testing.T) {
	s := NewState("set", 0, 100, []string{"us-east-1"}, t0.Add(-time.Hour))
	msg := &scaleset.RunnerScaleSetMessage{}
	for i := 0; i < 30; i++ {
		msg.JobAssignedMessages = append(msg.JobAssignedMessages, &scaleset.JobAssigned{JobMessageBase: scaleset.JobMessageBase{
			JobID: fmt.Sprintf("job-%d", i), ScaleSetAssignTime: t0}})
	}
	s.Observe(msg, t0.Add(time.Second))
	if n := s.Burst(t0.Add(time.Minute), testRule); n != 25 {
		t.Fatalf("30 jobs assigned at once: burst %d, want 25 (the 80th percentile job is the 25th)", n)
	}
	s.Observe(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{JobMessageBase: scaleset.JobMessageBase{JobID: "job-3"}}}}, t0.Add(5*time.Second))
	if w := s.TakeWaits(); len(w) != 1 || w[0] != 5 {
		t.Fatalf("waits %v, want one of 5 s: from when GitHub assigned the job to the set", w)
	}
}

// A backlog GitHub redelivers after a restart arrives in one message; dated by its assignments, the rate covers them.
func TestARedeliveredBacklogKeepsItsOwnTimes(t *testing.T) {
	s := NewState("set", 0, 100, []string{"us-east-1"}, t0.Add(-time.Hour))
	msg := &scaleset.RunnerScaleSetMessage{}
	for i := 0; i < 30; i++ {
		msg.JobAssignedMessages = append(msg.JobAssignedMessages, &scaleset.JobAssigned{JobMessageBase: scaleset.JobMessageBase{
			JobID: fmt.Sprintf("job-%d", i), ScaleSetAssignTime: t0.Add(-time.Duration(30-i) * 20 * time.Second)}})
	}
	s.Observe(msg, t0)
	if n := s.Burst(t0, testRule); n != 2 {
		t.Fatalf("30 jobs assigned 20 s apart, delivered at once: burst %d, want 2 (a job and the one 20 s before, within 22 s)", n)
	}
}

func TestAJobWithNeitherIDCountsAndIsNeverMergedWithAnother(t *testing.T) {
	s := NewState("set", 0, 100, []string{"us-east-1"}, t0.Add(-time.Hour))
	for i := 0; i < 3; i++ {
		s.Observe(&scaleset.RunnerScaleSetMessage{JobAssignedMessages: []*scaleset.JobAssigned{{}}}, t0)
	}
	if n := s.Burst(t0.Add(time.Minute), testRule); n != 3 {
		t.Fatalf("three jobs with no job id and no runner request id: burst %d, want 3", n)
	}
}

var testRule = PoolRule{Quantile: 0.8, WindowMinutes: 30, HoldMinutes: 5, ReadySeconds: 22}

// assigned is a message of JobAssigned as GitHub sends them: a job id, no runner request id, no queue time.
func assigned(s *State, at time.Time, ids ...int64) {
	msg := &scaleset.RunnerScaleSetMessage{}
	for _, id := range ids {
		msg.JobAssignedMessages = append(msg.JobAssignedMessages, &scaleset.JobAssigned{JobMessageBase: scaleset.JobMessageBase{JobID: fmt.Sprintf("job-%d", id)}})
	}
	s.Observe(msg, at)
}

func TestThePoolIsSizedForTheSetsOwnLaunchTime(t *testing.T) {
	s := NewState("set", 0, 100, []string{"us-east-1"}, t0)
	for i := 0; i < 60; i++ {
		assigned(s, t0.Add(time.Duration(i)*5*time.Second), int64(i))
	}
	now := t0.Add(5 * time.Minute)
	if n := s.Burst(now, testRule); n != 5 {
		t.Fatalf("a job every 5 s, no launch measured: burst %d, want 5 (those within ready_seconds, 22)", n)
	}
	for i := 0; i < 8; i++ {
		m := &Machine{ID: fmt.Sprintf("i-%04d", i), Set: "set", Region: "us-east-1"}
		s.Launched(fmt.Sprintf("r-%d", i), []*Machine{m}, now)
		s.Issue(m, &Runner{ID: int64(i + 1), Name: runnerName("set", m.ID), Issued: now.Add(6 * time.Second)})
	}
	held := PoolRule{Quantile: 0.8, WindowMinutes: 30, HoldMinutes: 0, ReadySeconds: 22}
	if n := s.Burst(now.Add(time.Second), held); n != 2 {
		t.Fatalf("launches measured at 6 s: burst %d, want 2 (a job and the one 5 s before)", n)
	}
}

func TestThePoolShrinksOnlyAfterTheHold(t *testing.T) {
	s := NewState("set", 0, 100, []string{"us-east-1"}, t0)
	assigned(s, t0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	rule := PoolRule{Quantile: 0.8, WindowMinutes: 1, HoldMinutes: 5, ReadySeconds: 22}
	if n := s.Burst(t0.Add(time.Second), rule); n != 9 {
		t.Fatalf("10 jobs at once: burst %d, want 9 (the 80th percentile job is the 9th)", n)
	}
	if n := s.Burst(t0.Add(3*time.Minute), rule); n != 9 {
		t.Fatalf("3 minutes on, the jobs out of the window: burst %d, want the 9 held", n)
	}
	if n := s.Burst(t0.Add(7*time.Minute), rule); n != 0 {
		t.Fatalf("past the hold: burst %d, want 0", n)
	}
}

// F9: a restart lost the bursts and launch times, so the pool fell to its floor for a window.
func TestARestartStartsFromTheBurstTheLastListenerCarried(t *testing.T) {
	old := NewState("set", 0, 100, []string{"us-east-1"}, t0)
	ids := make([]int64, 30)
	for i := range ids {
		ids[i] = int64(i)
	}
	assigned(old, t0, ids...)
	for i := 0; i < 8; i++ {
		m := &Machine{ID: fmt.Sprintf("i-%04d", i), Set: "set", Region: "us-east-1"}
		old.Launched(fmt.Sprintf("r-%d", i), []*Machine{m}, t0)
		old.Issue(m, &Runner{ID: int64(i + 1), Name: runnerName("set", m.ID), Issued: t0.Add(30 * time.Second)})
	}
	old.Burst(t0.Add(time.Minute), testRule)
	c := old.Carry(t0.Add(time.Minute), testRule)
	restart := t0.Add(2 * time.Minute)
	s := NewState("set", 0, 100, []string{"us-east-1"}, restart)
	s.Resume(c)
	if n := s.Burst(restart.Add(time.Minute), testRule); n != 25 {
		t.Fatalf("a minute after the restart, no jobs yet: burst %d, want the 25 carried", n)
	}
	if got := s.Snapshot(restart).LaunchS; got != 30 {
		t.Fatalf("launch time %v s after the restart, want the 30 s carried", got)
	}
	if n := s.Burst(restart.Add(31*time.Minute), testRule); n != 0 {
		t.Fatalf("a window after the restart: burst %d, want 0", n)
	}
}

// The budget's wait is not a launch time: counting it grew the pool, whose registrations grew the wait (dev, Nov 25).
func TestTheLaunchTimeEndsAtTheMachinesFirstAsk(t *testing.T) {
	s := newState(0, 100)
	s.Observe(stats(0, 0), t0)
	for i := 0; i < 8; i++ {
		m := &Machine{ID: fmt.Sprintf("i-%04d", i), Set: "set", Region: "us-east-1", Launched: t0}
		s.Launched(fmt.Sprintf("r-%d", i), []*Machine{m}, t0)
		s.Ask(m.ID, t0.Add(20*time.Second), closed(), 8*time.Hour)
		s.Issue(m, &Runner{ID: int64(i + 1), Name: runnerName("set", m.ID), Issued: t0.Add(2 * time.Minute)})
	}
	if got := s.Snapshot(t0).LaunchS; got != 20 {
		t.Fatalf("machines that asked 20 s after their launch and waited for the budget until 2 min: launch time %v s, want 20", got)
	}
}

func TestAReleaseThatChangesTheBootReplacesTheIdleMachinesOfTheOldOneAFewAtATime(t *testing.T) {
	s := newState(0, 1200)
	s.Observe(stats(1, 0), t0)
	ms := machines(6, t0)
	for i, m := range ms {
		m.Template = "35"
		if i == 5 {
			m.Template = "36"
		}
	}
	launch(s, ms, t0)
	for i := range 4 {
		give(s, fmt.Sprintf("i-%04d", i), &Runner{ID: int64(i + 1), Name: fmt.Sprintf("set-i-%04d", i), Issued: t0})
	}
	s.Machine("i-0003").Runner.Busy = true // its job finishes on the old boot
	// i-0004 has no runner yet; i-0005 booted from the current version.
	current := map[string]string{"us-east-1": "36"}
	first := s.Recycle(current, 2)
	second := s.Recycle(current, 2)
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("recycled %v then %v: want 2 a pass of the 4 idle or unregistered machines of version 35", ids(first), ids(second))
	}
	for _, id := range []string{"i-0000", "i-0001", "i-0002"} {
		if m := s.Machine(id); !m.Retire {
			t.Errorf("%s: an idle runner of the old boot retires (removed first)", id)
		}
	}
	if m := s.Machine("i-0004"); !m.Ending {
		t.Error("a machine of the old boot with no runner yet ends")
	}
	if m := s.Machine("i-0003"); m.Retire || m.Ending {
		t.Error("a busy machine of the old boot is left to finish its job")
	}
	if m := s.Machine("i-0005"); m.Retire || m.Ending {
		t.Error("a machine of the current boot stays")
	}
	if again := s.Recycle(current, 2); len(again) != 0 {
		t.Fatalf("recycled again %v", ids(again))
	}
}
