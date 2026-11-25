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
	s := NewState("set", 2, 1200)
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
	s.Seen("us-east-1", got[:400], t0) // a listing shows part of the round before its call answers
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
	s := NewState("set", 0, 300)
	s.Observe(stats(1000, 0), t0)
	if _, n := s.Deficit(t0, 100000); n != 300 {
		t.Fatalf("deficit %d, want the cap", n)
	}
}

func TestAMachineIsToldToEndOnceTheQueueNoLongerNeedsIt(t *testing.T) {
	s := NewState("set", 1, 1200)
	s.Observe(stats(3, 0), t0)
	launch(s, machines(4, t0), t0)
	for i := 0; i < 4; i++ {
		if v := ask(s, fmt.Sprintf("i-%04d", i), t0); v != Grant {
			t.Fatalf("machine %d: %v, want a runner while all four are needed", i, v)
		}
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
	s := NewState("set", 0, 1200)
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
	s := NewState("set", 0, 1200)
	s.Observe(stats(4, 0), t0)
	launch(s, machines(4, t0), t0)
	issued := t0.Add(30 * time.Second)
	busy := &Runner{ID: 1, Name: "set-i-0001-aa", Issued: issued, Busy: true}
	listed := &Runner{ID: 2, Name: "set-i-0002-bb", Issued: issued}
	gone := &Runner{ID: 3, Name: "set-i-0003-cc", Issued: issued, Busy: true, Done: true}
	give(s, "i-0001", busy)
	give(s, "i-0002", listed)
	give(s, "i-0003", gone)

	later := t0.Add(10 * time.Minute)
	if ended, unlisted := s.Stuck(later, 5*time.Minute, 3*time.Minute); len(ended)+len(unlisted) != 0 {
		t.Fatalf("before any listing, nothing is stuck: %v %v", ids(ended), ids(unlisted))
	}
	for i := 0; i < 3; i++ {
		s.Listed(map[string]ListedRunner{"set-i-0002-bb": {ID: 2, Name: "set-i-0002-bb"}}, later.Add(time.Duration(i)*time.Minute))
	}
	ended, unlisted := s.Stuck(later.Add(3*time.Minute), 5*time.Minute, 3*time.Minute)
	if len(ended) != 1 || ended[0].ID != "i-0000" {
		t.Fatalf("ended %v: want only the machine with no runner that three listings showed none of", ids(ended))
	}
	if len(unlisted) != 1 || unlisted[0].ID != "i-0003" {
		t.Fatalf("to check %v: want only the machine whose finished runner the listings do not show (a busy one and a listed one stay)", ids(unlisted))
	}
	s.Gone(unlisted[0], unlisted[0].Runner, false)
	if snap := s.Snapshot(later); snap.Ending != 1 {
		t.Fatalf("a runner GitHub still has keeps its machine: %+v", snap)
	}
	s.Gone(unlisted[0], unlisted[0].Runner, true)
	if snap := s.Snapshot(later); snap.Ending != 2 {
		t.Fatalf("a runner GitHub no longer has ends its machine: %+v", snap)
	}
}

func TestOneListingThatMissesAnIdleRunnerEndsNothing(t *testing.T) {
	s := NewState("set", 1, 10)
	s.Observe(stats(0, 0), t0)
	launch(s, machines(1, t0), t0)
	give(s, "i-0000", &Runner{ID: 9, Name: "set-i-0000-aa", Issued: t0})
	s.Listed(map[string]ListedRunner{}, t0.Add(10*time.Minute))
	ended, unlisted := s.Stuck(t0.Add(10*time.Minute), 5*time.Minute, 3*time.Minute)
	if len(ended) != 0 || len(unlisted) != 1 {
		t.Fatalf("ended %v, to check %v: an idle pool runner one listing missed is checked, not ended", ids(ended), ids(unlisted))
	}
	if ended, unlisted := s.Stuck(t0.Add(11*time.Minute), 5*time.Minute, 3*time.Minute); len(ended)+len(unlisted) != 0 {
		t.Fatalf("checked twice at once: %v %v", ids(ended), ids(unlisted))
	}
}

func TestAMachineThatAskedLatelyIsNotStuck(t *testing.T) {
	s := NewState("set", 0, 1200)
	s.Observe(stats(1, 0), t0)
	launch(s, machines(1, t0), t0)
	ask(s, "i-0000", t0.Add(9*time.Minute))
	if ended, unlisted := s.Stuck(t0.Add(10*time.Minute), 5*time.Minute, 3*time.Minute); len(ended)+len(unlisted) != 0 {
		t.Fatalf("stuck %v %v", ids(ended), ids(unlisted))
	}
}

func TestSurplusTakesOnlyIdleRunnersAndOnlyAfterTheHold(t *testing.T) {
	s := NewState("set", 0, 1200)
	s.Observe(stats(3, 0), t0)
	launch(s, machines(3, t0), t0)
	give(s, "i-0000", &Runner{ID: 10, Name: "set-i-0000-aa", Issued: t0})
	give(s, "i-0001", &Runner{ID: 11, Name: "set-i-0001-bb", Issued: t0, Busy: true})
	give(s, "i-0002", &Runner{ID: 12, Name: "set-i-0002-cc", Issued: t0})
	s.Observe(stats(1, 0), t0)
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
	s := NewState("set", 0, 1200)
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
	s := NewState("set", 0, 1200)
	s.Observe(stats(1, 0), t0)
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
	s := NewState("set", 0, 1200)
	s.Seen("us-east-1", machines(2, t0), t0.Add(time.Hour))
	s.Listed(map[string]ListedRunner{"set-i-0000-aa": {ID: 7, Name: "set-i-0000-aa", Busy: true}}, t0.Add(time.Hour))
	if snap := s.Snapshot(t0.Add(time.Hour)); snap.Machines != 2 || snap.Busy != 1 || snap.Booting != 1 {
		t.Fatalf("adopted %+v", snap)
	}
	if ended, unlisted := s.Stuck(t0.Add(time.Hour+2*time.Minute), 5*time.Minute, 3*time.Minute); len(ended)+len(unlisted) != 0 {
		t.Fatalf("adopted machines are given the quiet grace from adoption: %v %v", ids(ended), ids(unlisted))
	}
}

func TestASpotNoticeReplacesTheMachineAndFencesOnlyAnIdleRunner(t *testing.T) {
	s := NewState("set", 0, 1200)
	s.Observe(stats(3, 0), t0)
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
	if _, n := s.Deficit(t0, 100000); n != 3 {
		t.Fatalf("launched %d, want 3: no noticed machine is supply", n)
	}
	end := s.ToEnd(t0)
	if len(end) != 1 || end[0].ID != "i-0000" {
		t.Fatalf("ending %v: only the machine with no runner ends at once", ids(end))
	}
}

// open is a budget that never refuses.
func open() *Budget {
	return NewBudget(RegConfig{Cap: 1e9, WindowS: 300, Margin: 0.2, PoolRate: 1e6, PoolShare: 1, BlindRate: 1e6}, t0.Add(-time.Hour))
}

func ask(s *State, id string, at time.Time) Verdict {
	v, _ := s.Ask(id, at, open(), 8*time.Hour)
	return v
}

func give(s *State, id string, r *Runner) {
	s.mu.Lock()
	s.machines[id].Runner = r
	s.mu.Unlock()
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
