package main

import (
	"math"
	"testing"
	"time"
)

func reg() RegConfig {
	return RegConfig{Cap: 1500, WindowS: 300, PoolRate: 0.5, PoolShare: 0.5, CeilingRate: 1, CeilingBurst: 30, RecoverS: 120}
}

// heads feeds the budget the organization's highest runner id advancing at rate a second from t0 for d.
func heads(b *Budget, rate float64, d time.Duration) {
	for s := 0; s <= int(d.Seconds()); s += 15 {
		b.Head(t0.Add(time.Duration(s)*time.Second), 7_000_000+int64(rate*float64(s)))
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.05 }

func TestTheOrganizationsRateIsTheRunnerIdAdvance(t *testing.T) {
	b := NewBudget(reg(), t0)
	heads(b, 3, 6*time.Minute)
	e := b.Estimate(t0.Add(6 * time.Minute))
	if !e.Known || !near(e.OrgRate, 3) || !near(e.OthersRate, 3) || !near(e.SpareRate, 2) || !near(e.WindowUse, 900) {
		t.Fatalf("estimate %+v: want 3/s by others, leaving 2/s of the documented 5/s", e)
	}
}

func TestOurOwnRegistrationsAreNotCountedAsOthers(t *testing.T) {
	b := NewBudget(reg(), t0.Add(-time.Hour))
	heads(b, 2, 2*time.Minute)
	at := t0.Add(2 * time.Minute)
	id := int64(7_000_240)
	for i := 0; i < 60; i++ { // a second of ours each, while the others keep their 2/s
		at = at.Add(time.Second)
		if !b.Take(at, ForJob) {
			t.Fatalf("registration %d refused at one a second, the ceiling", i)
		}
		id += 3
		b.Done(at, Made, id)
	}
	e := b.Estimate(at)
	if !near(e.OthersRate, 2) || !near(e.OursRate, 60.0/300) {
		t.Fatalf("estimate %+v: want others at 2/s and ours at 60 in the window", e)
	}
}

func TestARefusalStopsRegistrationsForAWhileAndLongerEachTime(t *testing.T) {
	b := NewBudget(reg(), t0.Add(-time.Hour))
	if !b.Take(t0, ForJob) {
		t.Fatal("first")
	}
	b.Done(t0, Refused, 0)
	if b.Take(t0.Add(4*time.Second), ForJob) {
		t.Fatal("4 s after a refusal")
	}
	if !b.Take(t0.Add(16*time.Second), ForJob) {
		t.Fatal("16 s after a refusal, with tokens saved again")
	}
	b.Done(t0.Add(16*time.Second), Refused, 0)
	if b.Take(t0.Add(25*time.Second), ForJob) {
		t.Fatal("9 s after a second refusal: the wait doubled to 10 s")
	}
	if s := b.Snapshot(t0.Add(25 * time.Second)); s.Refused != 2 || s.Made != 0 || !s.Throttled {
		t.Fatalf("counted %+v", s)
	}
}

func TestThePoolLeavesHalfTheBurstToJobs(t *testing.T) {
	c := reg()
	c.CeilingRate, c.CeilingBurst, c.PoolRate, c.PoolShare = 10, 60, 1e6, 1 // the pool's own pace never binds here
	at := t0.Add(6 * time.Minute)
	b := NewBudget(c, at.Add(-time.Minute)) // a full bucket of 60
	pool := 0
	for b.Take(at, ForPool) {
		pool++
		b.Done(at, Made, 0)
	}
	jobs := 0
	for b.Take(at, ForJob) {
		jobs++
		b.Done(at, Made, 0)
	}
	if pool != 30 || jobs != 30 {
		t.Fatalf("pool took %d and jobs %d of a full bucket of 60: the pool stops at half the burst and leaves the rest to jobs", pool, jobs)
	}
}

// OPS-4: the second sample of the organization's runner ids came 100 s after the first, so the budget was blind for
// that long after every start.
func TestTheSecondSampleComesAMinuteAfterTheFirst(t *testing.T) {
	b := NewBudget(reg(), t0)
	if !b.SampleDue(t0) {
		t.Fatal("no sample due at the start")
	}
	b.Head(t0, 7_000_000)
	if b.SampleDue(t0.Add(60 * time.Second)) {
		t.Fatal("due a minute after the first: two heads must be 60 s apart")
	}
	if !b.SampleDue(t0.Add(61 * time.Second)) {
		t.Fatal("not due 61 s after the first sample, while the rate is unknown")
	}
	b.Head(t0.Add(61*time.Second), 7_000_200)
	if !b.Estimate(t0.Add(61 * time.Second)).Known {
		t.Fatal("two heads 61 s apart do not make the rate known")
	}
	if b.SampleDue(t0.Add(130 * time.Second)) {
		t.Fatal("due 69 s after the last sample with the rate known: want 100 s")
	}
	if !b.SampleDue(t0.Add(162 * time.Second)) {
		t.Fatal("not due 101 s after the last sample")
	}
}

// A failed sample was retried every 5 s tick: on Nov 25 from 20:12 to 20:27Z, 180 listings of up to 41 pages each, on
// the App's rate limit that the push plane and every other listener share.
func TestAFailedSampleIsNotRetriedEveryTick(t *testing.T) {
	b := NewBudget(reg(), t0)
	if !b.SampleDue(t0) {
		t.Fatal("no sample due at the start")
	}
	b.SampleFailed(t0)
	if b.SampleDue(t0.Add(5 * time.Second)) {
		t.Fatal("due 5 s after a failed sample")
	}
	if !b.SampleDue(t0.Add(time.Minute)) {
		t.Fatal("not due a minute after one failed sample")
	}
	b.SampleFailed(t0.Add(time.Minute))
	if b.SampleDue(t0.Add(2*time.Minute)) || !b.SampleDue(t0.Add(3*time.Minute)) {
		t.Fatal("a second failure in a row does not wait two minutes")
	}
	for i := 0; i < 10; i++ {
		b.SampleFailed(t0)
	}
	if b.SampleDue(t0.Add(9*time.Minute)) || !b.SampleDue(t0.Add(10*time.Minute)) {
		t.Fatal("the wait does not stop doubling at ten minutes")
	}
	b.Head(t0.Add(10*time.Minute), 7_000_000)
	b.Head(t0.Add(11*time.Minute+time.Second), 7_000_100)
	if !b.SampleDue(t0.Add(13 * time.Minute)) {
		t.Fatal("a sample that succeeded does not end the backoff")
	}
}

func TestCountBetweenCountsTheHalfOpenWindow(t *testing.T) {
	var ts []time.Time
	for i := 1; i <= 5; i++ {
		ts = append(ts, t0.Add(time.Duration(i)*time.Second))
	}
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	for _, tc := range []struct{ from, to, want int }{{1, 4, 3}, {0, 5, 5}, {5, 9, 0}, {-3, 0, 0}, {2, 2, 0}, {4, 1, 0}} {
		if n := countBetween(ts, at(tc.from), at(tc.to)); n != tc.want {
			t.Fatalf("(%d s, %d s]: %d, want %d", tc.from, tc.to, n, tc.want)
		}
	}
}
