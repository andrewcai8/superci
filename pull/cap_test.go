package main

import (
	"fmt"
	"testing"
	"time"
)

// At the spend cap a dispatch set keeps no pool, but a job superci already holds still gets its machine: the canary's
// spend-capped rollback sends the jobs after it to Blacksmith, and a job left with no machine would wait out its timeout.
// (Nov 28: at the cap the set launched nothing, so every held job would have.)
func TestAtTheSpendCapADispatchSetStillGivesItsWaitingJobsAMachineButKeepsNoPool(t *testing.T) {
	now := time.Now()
	st := NewDispatchSet(SetSpec{Name: "superci-8cpu", Max: 50, Dispatch: true}, nil, []string{"us-east-1"}, now.Add(-time.Hour))
	st.Seen("us-east-1", nil, now, now)
	st.SetPool(10)
	st.SetCapped(true)
	for i := range 3 {
		st.enqueue(&session{set: st, arrived: now}, jobHead{ID: fmt.Sprintf("job-%d", i)}, now)
	}
	if _, n := st.Deficit(now); n != 3 {
		t.Fatalf("at the cap with 3 jobs waiting and a pool of 10, launched %d, want 3: one a held job, no pool", n)
	}
}

// At capOverrunPercent of the cap the spend stops: nothing launches, and a job still waiting for a machine is failed at once
// (its shim exits 1), never left to wait out its timeout. A job on a machine runs on.
func TestPastTheHardStopNothingLaunchesAndAWaitingJobsShimExitsAtOnce(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1"))
	_, running, _ := handOne(t, p, "i-0m1", jobA)
	waiting := p.shim(t)
	waiting.sendJob(jobMessage("6a1d4c63-0a9e-4d73-9f42-2b3c4d5e6f70", 8, "unit"))
	waitUntil(t, "the second job waits for a machine", func() bool { p.st.mu.Lock(); defer p.st.mu.Unlock(); return len(p.st.queue) == 1 })
	p.st.SetStopped(true)
	if code := exitCode(waiting.expect(frameExit)); code != 1 {
		t.Fatalf("the waiting job's shim got exit %d, want 1 at once", code)
	}
	if err := running.quiet(300 * time.Millisecond); err != nil {
		t.Fatalf("the job on a machine was touched by the stop: %v", err)
	}
	if _, n := p.st.Deficit(time.Now()); n != 0 {
		t.Fatalf("launched %d past the hard stop", n)
	}
	recs := p.st.TakeJobs()
	if len(recs) != 1 || recs[0].Result != "Failed" {
		t.Fatalf("records %+v, want the refused job's one, failed", recs)
	}
	again := p.shim(t)
	again.sendJob(jobMessage("7b2e5d74-1b0f-4e84-8a53-3c4d5e6f7081", 9, "unit"))
	waitUntil(t, "a job that comes after waits", func() bool { p.st.mu.Lock(); defer p.st.mu.Unlock(); return len(p.st.queue) == 1 })
	p.st.SetStopped(true) // the next tick
	if code := exitCode(again.expect(frameExit)); code != 1 {
		t.Fatalf("a job that came after the stop got exit %d at the next tick, want 1", code)
	}
}

func TestTheHardStopIsTheCapPlusItsOverrun(t *testing.T) {
	sp := NewSpend(100, 1000, nil, t0)
	for _, c := range []struct {
		day, month    float64
		over, stopped bool
	}{{99, 99, false, false}, {100, 100, true, false}, {109.9, 109.9, true, false}, {110, 110, true, true}, {50, 1100, true, true}} {
		sp.DayUSD, sp.MonthUSD = c.day, c.month
		if sp.Over() != c.over || sp.Stopped() != c.stopped {
			t.Errorf("day %v month %v: over %v stopped %v, want %v %v", c.day, c.month, sp.Over(), sp.Stopped(), c.over, c.stopped)
		}
	}
}
