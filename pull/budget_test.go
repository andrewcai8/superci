package main

import (
	"math"
	"testing"
	"time"
)

func reg() RegConfig {
	return RegConfig{Cap: 1500, WindowS: 300, Margin: 0.2, PoolRate: 0.5, BlindRate: 0.3}
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
	if !e.Known || !near(e.OrgRate, 3) || !near(e.OthersRate, 3) || !near(e.SpareRate, 1) || !near(e.WindowUse, 900) {
		t.Fatalf("estimate %+v: want 3/s by others, leaving 1/s of the 4/s the margin allows", e)
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
			t.Fatalf("registration %d refused with 2/s spare", i)
		}
		id += 3
		b.Done(at, Made, id)
	}
	e := b.Estimate(at)
	if !near(e.OthersRate, 2) || !near(e.OursRate, 60.0/300) {
		t.Fatalf("estimate %+v: want others at 2/s and ours at 60 in the window", e)
	}
}

func TestNoRegistrationWhileTheOrganizationIsNearTheCap(t *testing.T) {
	b := NewBudget(reg(), t0.Add(-time.Hour))
	heads(b, 4.2, 6*time.Minute)
	if b.Take(t0.Add(6*time.Minute), ForJob) {
		t.Fatal("registered while the rest of the organization uses more than the 80% the margin leaves it: production would wait for us")
	}
}

func TestTheSpareRateBoundsRegistrations(t *testing.T) {
	b := NewBudget(reg(), t0.Add(6*time.Minute))
	heads(b, 3, 6*time.Minute)
	start := t0.Add(6 * time.Minute)
	made := 0
	for ms := 0; ms < 60_000; ms += 100 {
		at := start.Add(time.Duration(ms) * time.Millisecond)
		b.Head(at, 7_000_000+int64(3*at.Sub(t0).Seconds())+int64(made))
		if b.Take(at, ForJob) {
			made++
			b.Done(at, Made, 0)
		}
	}
	if made < 50 || made > 63 {
		t.Fatalf("made %d in a minute with 1/s spare: want about 60 (and the burst of 10 at most)", made)
	}
}

func TestUnknownRateRegistersAtTheBlindRate(t *testing.T) {
	b := NewBudget(reg(), t0.Add(-time.Hour))
	if e := b.Estimate(t0); e.Known || e.SpareRate != 0.3 {
		t.Fatalf("estimate %+v with no runner id seen", e)
	}
	made := 0
	for s := 0; s < 300; s++ {
		at := t0.Add(time.Duration(s) * time.Second)
		if b.Take(at, ForJob) {
			made++
			b.Done(at, Made, 0)
		}
	}
	if made > 93 {
		t.Fatalf("made %d in five minutes blind, want at most 0.3/s and its burst", made)
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
	b := NewBudget(reg(), t0.Add(6*time.Minute))
	heads(b, 2, 6*time.Minute) // 2/s spare: a burst of 20
	at := t0.Add(6*time.Minute + 6*time.Second)
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
	if pool > 2 || jobs < 9 {
		t.Fatalf("pool took %d and jobs %d of a 12-token save: the pool stops at half the burst of 20 and leaves the rest to jobs", pool, jobs)
	}
}
