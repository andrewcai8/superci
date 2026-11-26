package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// registry is GitHub's runner list as a probe sees it: a registration is made by the generate call and ends only by a
// removal that reaches GitHub (a removal whose context is done never does).
type registry struct {
	mu          sync.Mutex
	runners     map[int64]string
	next        int64
	generateErr error
	removeErr   error
	onGenerate  func() // runs inside the generate call, after it registered
	setID       int
}

func (r *registry) GenerateJitRunnerConfig(_ context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, setID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	r.mu.Lock()
	if r.generateErr != nil {
		r.mu.Unlock()
		return nil, r.generateErr
	}
	r.next++
	if r.runners == nil {
		r.runners = map[int64]string{}
	}
	r.runners[r.next] = s.Name
	r.setID = setID
	id, hook := r.next, r.onGenerate
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	return &scaleset.RunnerScaleSetJitRunnerConfig{Runner: &scaleset.RunnerReference{ID: int(id), Name: s.Name}}, nil
}

func (r *registry) RemoveRunner(ctx context.Context, id int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.removeErr != nil {
		return r.removeErr
	}
	delete(r.runners, id)
	return nil
}

func (r *registry) listing() map[string]ListedRunner {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]ListedRunner{}
	for id, name := range r.runners {
		out[name] = ListedRunner{ID: id, Name: name}
	}
	return out
}

func (r *registry) held() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runners)
}

// probeBudget is a budget with room for one pool registration.
func probeBudget(now time.Time) *Budget {
	c := reg()
	c.CeilingRate, c.CeilingBurst, c.PoolRate = 5, 30, 5
	return NewBudget(c, now.Add(-time.Hour))
}

func TestAProbeIsRemovedOnceItGaveTheHead(t *testing.T) {
	g := &registry{next: 7_000_000}
	b := probeBudget(t0)
	if !probeOrgHead(context.Background(), g, b, 12, t0) {
		t.Fatal("a probe that registered says it learned nothing")
	}
	if g.held() != 0 {
		t.Fatalf("%d runners still registered after the probe", g.held())
	}
	if g.setID != 12 {
		t.Fatalf("probe registered in scale set %d, want 12", g.setID)
	}
	if snap := b.Snapshot(t0); snap.Made != 1 {
		t.Fatalf("the probe is a registration of ours: made %d, want 1", snap.Made)
	}
	if b.SampleDue(t0.Add(time.Second)) {
		t.Fatal("the probe's id did not count as the head: another sample is due a second on")
	}
}

func TestAProbeThatCouldNotRegisterLeavesNothingAndLearnsNothing(t *testing.T) {
	g := &registry{generateErr: errors.New("boom")}
	b := probeBudget(t0)
	if probeOrgHead(context.Background(), g, b, 1, t0) {
		t.Fatal("a failed registration says it learned the head")
	}
	if g.held() != 0 || b.Snapshot(t0).Failed != 1 {
		t.Fatalf("held %d, failed %d: want nothing held and the failure counted", g.held(), b.Snapshot(t0).Failed)
	}
}

// A shutdown during the registration used to cancel the removal that followed: the probe stayed registered.
func TestAProbeIsRemovedEvenWhenTheListenerIsStopping(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	g := &registry{onGenerate: stop}
	if !probeOrgHead(ctx, g, probeBudget(t0), 1, t0) {
		t.Fatal("the probe's head was lost")
	}
	if g.held() != 0 {
		t.Fatalf("a probe registered as the listener stopped was left registered")
	}
}

func TestAProbeWhoseRemovalFailedIsSweptOnceItIsOldEnough(t *testing.T) {
	ctx := context.Background()
	g := &registry{removeErr: errors.New("502")}
	if !probeOrgHead(ctx, g, probeBudget(t0), 1, t0) {
		t.Fatal("the head is learned even when the removal fails")
	}
	if g.held() != 1 {
		t.Fatalf("setup: %d probes held, want the one whose removal failed", g.held())
	}
	g.removeErr = nil
	if n := sweepProbes(ctx, g, g.listing(), t0.Add(probeGrace-time.Second), probeGrace); n != 0 || g.held() != 1 {
		t.Fatalf("a probe inside its grace was swept: found %d, held %d (it may be mid-registration)", n, g.held())
	}
	if n := sweepProbes(ctx, g, g.listing(), t0.Add(probeGrace+time.Second), probeGrace); n != 1 || g.held() != 0 {
		t.Fatalf("an old probe: found %d, held %d, want it counted and removed", n, g.held())
	}
	if n := sweepProbes(ctx, g, g.listing(), t0.Add(time.Hour), probeGrace); n != 0 {
		t.Fatalf("a second sweep found %d", n)
	}
}

func TestTheSweepKeepsWhatIsNotAnOldProbe(t *testing.T) {
	ctx := context.Background()
	old := fmt.Sprintf("%s%d", probePrefix, t0.UnixNano())
	g := &registry{runners: map[int64]string{1: "superci-8cpu-i-0abc-x", 2: probePrefix + "soon", 3: old, 4: "superci-probe"}}
	listed := g.listing()
	busy := listed[old]
	busy.Busy = true
	listed[old] = busy
	if n := sweepProbes(ctx, g, listed, t0.Add(time.Hour), probeGrace); n != 0 || g.held() != 4 {
		t.Fatalf("swept %d, held %d: a machine's runner, a name that is not a probe's date and a busy runner stay", n, g.held())
	}
	listed[old] = ListedRunner{ID: 3, Name: old}
	if n := sweepProbes(ctx, g, listed, t0.Add(time.Hour), probeGrace); n != 1 || g.held() != 3 {
		t.Fatalf("swept %d, held %d, want only the old probe gone", n, g.held())
	}
}
