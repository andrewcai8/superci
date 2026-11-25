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

func machines(n int, at time.Time) []*Machine {
	out := make([]*Machine, n)
	for i := range out {
		out[i] = &Machine{ID: fmt.Sprintf("i-%04d", i), Region: "us-east-1", IP: fmt.Sprintf("10.0.%d.%d", i/250, i%250), Lifecycle: "spot", Launched: at, Asked: at}
	}
	return out
}

func TestABurstIsLaunchedOnceWhileItsCallsAreInFlight(t *testing.T) {
	s := NewState("set", 2, 1200, 1400)
	s.Observe(stats(0, 1000))
	round, n := s.Deficit(t0)
	if n != 1002 {
		t.Fatalf("deficit %d, want the burst plus the pool", n)
	}
	if _, n := s.Deficit(t0); n != 0 {
		t.Fatalf("asked again for capacity already in flight: %d", n)
	}
	got := machines(900, t0)
	for _, m := range got {
		m.Round = round
	}
	s.Seen("us-east-1", got[:400], t0) // a listing shows part of the round before its call answers
	if live := s.Snapshot(t0).Live; live != 1002 {
		t.Fatalf("live %d while the call is unanswered: a machine seen early is counted twice or not at all", live)
	}
	if v := s.Ask(got[0].ID, t0); v != Grant {
		t.Fatalf("a machine of the round that boots before its call answers: %v, want Grant", v)
	}
	s.Launched(round, got, t0)
	if _, n := s.Deficit(t0); n != 102 {
		t.Fatalf("a short launch leaves %d to ask again, want 102", n)
	}
}

func TestTheCapBoundsTheSupply(t *testing.T) {
	s := NewState("set", 0, 300, 1400)
	s.Observe(stats(1000, 0))
	if _, n := s.Deficit(t0); n != 300 {
		t.Fatalf("deficit %d, want the cap", n)
	}
}

func TestAMachineIsToldToEndOnceTheQueueNoLongerNeedsIt(t *testing.T) {
	s := NewState("set", 1, 1200, 1400)
	s.Observe(stats(3, 0))
	launch(s, machines(4, t0), t0)
	for i := 0; i < 4; i++ {
		if v := s.Ask(fmt.Sprintf("i-%04d", i), t0); v != Grant {
			t.Fatalf("machine %d: %v, want a runner while all four are needed", i, v)
		}
	}
	s.Observe(stats(1, 0)) // two jobs completed
	if v := s.Ask("i-0000", t0); v != End {
		t.Fatalf("got %v, want End: four machines for one job and a pool of one", v)
	}
	if v := s.Ask("i-0001", t0); v != End {
		t.Fatalf("got %v, want End", v)
	}
	if v := s.Ask("i-0002", t0); v != Grant {
		t.Fatalf("got %v, want Grant: two machines left for one job and a pool of one", v)
	}
	if v := s.Ask("i-9999", t0); v != Unknown {
		t.Fatalf("a machine not ours: %v", v)
	}
}

func TestRegistrationsKeepTheBudgetInAnyFiveMinutes(t *testing.T) {
	s := NewState("set", 0, 1200, 3)
	s.Observe(stats(10, 0))
	launch(s, machines(10, t0), t0)
	ask := func(i int, at time.Time) Verdict { return s.Ask(fmt.Sprintf("i-%04d", i), at) }
	for i := 0; i < 2; i++ {
		if v := ask(i, t0); v != Grant {
			t.Fatalf("ask %d: %v", i, v)
		}
		s.Registered(fmt.Sprintf("i-%04d", i), &Runner{ID: int64(i + 1)}, Made, t0.Add(time.Duration(i)*time.Second))
	}
	if v := ask(2, t0.Add(2*time.Second)); v != Grant {
		t.Fatalf("third: %v", v)
	}
	if v := ask(3, t0.Add(3*time.Second)); v != Wait {
		t.Fatalf("a fourth while the third is unanswered: %v, want Wait (asked counts against the budget)", v)
	}
	s.Registered("i-0002", nil, Failed, t0.Add(3*time.Second))
	if v := ask(3, t0.Add(4*time.Second)); v != Grant {
		t.Fatalf("after a failed one, which GitHub did not count: %v, want Grant", v)
	}
	s.Registered("i-0003", &Runner{ID: 4}, Made, t0.Add(4*time.Second))
	if v := ask(4, t0.Add(4*time.Minute)); v != Wait {
		t.Fatalf("fourth made in the window: %v, want Wait", v)
	}
	if v := ask(4, t0.Add(5*time.Minute+time.Second)); v != Grant {
		t.Fatalf("after the first left the window: %v, want Grant", v)
	}
	if snap := s.Snapshot(t0.Add(5*time.Minute + time.Second)); snap.MaxRegWindow != 3 || snap.TotalRegs != 3 || snap.RegErrors != 1 {
		t.Fatalf("counted %+v", snap)
	}
}

func TestARefusalStopsRegistrationsForAWhileAndLongerEachTime(t *testing.T) {
	s := NewState("set", 0, 1200, 1400)
	s.Observe(stats(10, 0))
	launch(s, machines(10, t0), t0)
	s.Ask("i-0000", t0)
	s.Registered("i-0000", nil, Refused, t0)
	if v := s.Ask("i-0001", t0.Add(4*time.Second)); v != Wait {
		t.Fatalf("4 s after a refusal: %v, want Wait", v)
	}
	if v := s.Ask("i-0001", t0.Add(6*time.Second)); v != Grant {
		t.Fatalf("6 s after: %v, want Grant", v)
	}
	s.Registered("i-0001", nil, Refused, t0.Add(6*time.Second))
	if v := s.Ask("i-0002", t0.Add(15*time.Second)); v != Wait {
		t.Fatalf("9 s after a second refusal: %v, want Wait (the wait doubled)", v)
	}
	s.Observe(stats(100, 0))
	if _, n := s.Deficit(t0.Add(20 * time.Second)); n != 40 {
		t.Fatalf("launched %d of 90 more while GitHub refuses: want 40, so 50 at most wait for runners", n)
	}
	if snap := s.Snapshot(t0); snap.Refused != 2 || snap.TotalRegs != 0 {
		t.Fatalf("counted %+v", snap)
	}
}

func TestPacedRegistrationsLaunchOnlyWhatCanSoonBeRegistered(t *testing.T) {
	s := NewState("set", 0, 1200, 1400)
	s.Pace(1, 2)
	s.Observe(stats(0, 0))
	s.SetPool(1000)
	_, n := s.Deficit(t0)
	if n != 90 {
		t.Fatalf("launched %d for a pool of 1000 at one registration a second, want 90", n)
	}
	launch(s, machines(3, t0), t0)
	if s.Ask("i-0000", t0) != Grant || s.Ask("i-0001", t0) != Grant || s.Ask("i-0002", t0) != Wait {
		t.Fatal("want the burst of two, then Wait")
	}
	if v := s.Ask("i-0002", t0.Add(1100*time.Millisecond)); v != Grant {
		t.Fatalf("a second on: %v, want Grant", v)
	}
}

func TestWaitingJobsThatIdleRunnersIgnoreAreStale(t *testing.T) {
	s := NewState("set", 0, 1200, 1400)
	s.lastActivity = t0
	s.Observe(&scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 3, TotalIdleRunners: 3}})
	if stale := s.MarkStale(t0.Add(2*time.Minute), 3*time.Minute); stale != 0 {
		t.Fatalf("stale %d too soon", stale)
	}
	if stale := s.MarkStale(t0.Add(4*time.Minute), 3*time.Minute); stale != 3 || s.Snapshot(t0).Desired != 0 {
		t.Fatalf("stale %d, desired %d: want 3 and 0", stale, s.Snapshot(t0).Desired)
	}
	s.Observe(&scaleset.RunnerScaleSetMessage{JobAssignedMessages: []*scaleset.JobAssigned{{}},
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 4, TotalIdleRunners: 3}})
	if d := s.Snapshot(t0).Desired; d != 4 {
		t.Fatalf("a new assignment makes the count trusted again: desired %d, want 4", d)
	}
	s.lastActivity = t0
	s.Observe(stats(5, 0)) // five waiting, no idle runner: real demand however old
	if stale := s.MarkStale(t0.Add(10*time.Minute), 3*time.Minute); stale != 0 {
		t.Fatalf("stale %d with no idle runner to take them", stale)
	}
}

func TestOnlyAMachineThatCanHoldNoJobIsStuck(t *testing.T) {
	s := NewState("set", 0, 1200, 1400)
	s.Observe(stats(4, 0))
	launch(s, machines(4, t0), t0)
	issued := t0.Add(30 * time.Second)
	busy := &Runner{ID: 1, Name: "set-i-0001-aa", Issued: issued, Busy: true}
	listed := &Runner{ID: 2, Name: "set-i-0002-bb", Issued: issued}
	gone := &Runner{ID: 3, Name: "set-i-0003-cc", Issued: issued, Busy: true, Done: true}
	give(s, "i-0001", busy)
	give(s, "i-0002", listed)
	give(s, "i-0003", gone)

	later := t0.Add(10 * time.Minute)
	if got := s.Stuck(later, 5*time.Minute, 3*time.Minute); len(got) != 1 || got[0].ID != "i-0000" {
		t.Fatalf("before any listing after the runners were issued, only the machine with no runner is stuck: %v", ids(got))
	}
	s.Listed(map[string]ListedRunner{"set-i-0002-bb": {ID: 2, Name: "set-i-0002-bb"}}, later)
	got := s.Stuck(later, 5*time.Minute, 3*time.Minute)
	if len(got) != 1 || got[0].ID != "i-0003" {
		t.Fatalf("stuck %v: want only the machine whose runner finished and is gone (a busy one and a listed one stay)", ids(got))
	}
}

func TestAMachineThatAskedLatelyIsNotStuck(t *testing.T) {
	s := NewState("set", 0, 1200, 1400)
	s.Observe(stats(1, 0))
	launch(s, machines(1, t0), t0)
	s.Ask("i-0000", t0.Add(9*time.Minute))
	if got := s.Stuck(t0.Add(10*time.Minute), 5*time.Minute, 3*time.Minute); len(got) != 0 {
		t.Fatalf("stuck %v", ids(got))
	}
}

func TestSurplusTakesOnlyIdleRunnersAndOnlyAfterTheHold(t *testing.T) {
	s := NewState("set", 0, 1200, 1400)
	s.Observe(stats(3, 0))
	launch(s, machines(3, t0), t0)
	give(s, "i-0000", &Runner{ID: 10, Name: "set-i-0000-aa", Issued: t0})
	give(s, "i-0001", &Runner{ID: 11, Name: "set-i-0001-bb", Issued: t0, Busy: true})
	give(s, "i-0002", &Runner{ID: 12, Name: "set-i-0002-cc", Issued: t0})
	s.Observe(stats(1, 0))
	now := t0.Add(time.Minute)
	if got := s.Surplus(now, 20*time.Second, 100); len(got) != 0 {
		t.Fatalf("removed %v before the surplus lasted", ids(got))
	}
	got := s.Surplus(now.Add(21*time.Second), 20*time.Second, 100)
	if len(got) != 2 {
		t.Fatalf("surplus %v: want both idle runners", ids(got))
	}
	for _, m := range got {
		if m.ID == "i-0001" {
			t.Fatal("a busy runner was taken")
		}
	}
	s.Removed(got[0], false) // GitHub refused: it took a job meanwhile
	s.Removed(got[1], true)
	if snap := s.Snapshot(now); snap.Ending != 1 || snap.Busy != 2 {
		t.Fatalf("after one refusal and one removal: %+v", snap)
	}
}

func TestOnDemandStaysUnderItsShareOfLaunches(t *testing.T) {
	s := NewState("set", 0, 1200, 1400)
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
	s := NewState("set", 0, 1200, 1400)
	s.Observe(stats(1, 0))
	launch(s, machines(1, t0), t0)
	give(s, "i-0000", &Runner{ID: 2, Name: "set-i-0000-new", Issued: t0})
	orphans := s.Listed(map[string]ListedRunner{
		"set-i-0000-new": {ID: 2, Name: "set-i-0000-new"},
		"set-i-0000-old": {ID: 1, Name: "set-i-0000-old"},
		"set-i-gone-zz":  {ID: 3, Name: "set-i-gone-zz"},
	}, t0)
	if len(orphans) != 2 {
		t.Fatalf("orphans %v: want the machine's old runner and the one of a machine not live", orphans)
	}
}

func TestARestartAdoptsMachinesAndTheirRunners(t *testing.T) {
	s := NewState("set", 0, 1200, 1400)
	s.Seen("us-east-1", machines(2, t0), t0.Add(time.Hour))
	s.Listed(map[string]ListedRunner{"set-i-0000-aa": {ID: 7, Name: "set-i-0000-aa", Busy: true}}, t0.Add(time.Hour))
	if snap := s.Snapshot(t0.Add(time.Hour)); snap.Machines != 2 || snap.Busy != 1 || snap.Booting != 1 {
		t.Fatalf("adopted %+v", snap)
	}
	if got := s.Stuck(t0.Add(time.Hour+2*time.Minute), 5*time.Minute, 3*time.Minute); len(got) != 0 {
		t.Fatalf("adopted machines are given the quiet grace from adoption: %v", ids(got))
	}
}

func TestASpotNoticeReplacesTheMachineAndFencesOnlyAnIdleRunner(t *testing.T) {
	s := NewState("set", 0, 1200, 1400)
	s.Observe(stats(3, 0))
	launch(s, machines(3, t0), t0)
	give(s, "i-0001", &Runner{ID: 11, Name: "set-i-0001-bb", Issued: t0})
	give(s, "i-0002", &Runner{ID: 12, Name: "set-i-0002-cc", Issued: t0, Busy: true})
	if _, r := s.Noticed("i-0000"); r != nil {
		t.Fatal("a machine with no runner has nothing to fence")
	}
	if _, r := s.Noticed("i-0001"); r == nil || r.ID != 11 {
		t.Fatalf("idle runner to remove: %v", r)
	}
	if _, r := s.Noticed("i-0002"); r != nil {
		t.Fatal("a busy runner is not removed under its job")
	}
	if _, n := s.Deficit(t0); n != 3 {
		t.Fatalf("launched %d, want 3: no noticed machine is supply", n)
	}
	end := s.ToEnd(t0)
	if len(end) != 1 || end[0].ID != "i-0000" {
		t.Fatalf("ending %v: only the machine with no runner ends at once", ids(end))
	}
}

func give(s *State, id string, r *Runner) {
	s.mu.Lock()
	s.machines[id].Runner = r
	s.mu.Unlock()
}

func launch(s *State, ms []*Machine, at time.Time) {
	round, _ := s.Deficit(t0)
	s.Launched(round, ms, at)
}

func ids(ms []*Machine) []string {
	out := []string{}
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}
