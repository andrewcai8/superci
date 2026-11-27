package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// shippedConfig is the config a listener runs: the built-in one with deploy/override.json over it, as a release installs it.
func shippedConfig(t *testing.T) Config {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("deploy", "override.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("deploy/override.json over the built-in config: %v", err)
	}
	return c
}

func shippedSets(c Config) []SetSpec { return c.Sets() }

const (
	pushRef      = "Authentic-Intelligence/megpt-mono/.github/workflows/lint-test-backend.yml@refs/heads/main"
	cacheWarmRef = "Authentic-Intelligence/megpt-mono/.github/workflows/ci-cache-warm.yml@refs/heads/main"
)

func peak() time.Time { return time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC) }

func setOf(t *testing.T, c Config, name string) SetSpec {
	t.Helper()
	for _, s := range shippedSets(c) {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no set %s", name)
	return SetSpec{}
}

// assign is a scale set's JobAssigned for n jobs of one event and workflow, all assigned at at.
func assign(st *State, n int, event, ref string, at time.Time) {
	var msgs []*scaleset.JobAssigned
	for i := 0; i < n; i++ {
		msgs = append(msgs, &scaleset.JobAssigned{JobMessageBase: scaleset.JobMessageBase{JobID: fmt.Sprintf("%s-%d", event, i), EventName: event, JobWorkflowRef: ref, ScaleSetAssignTime: at}})
	}
	st.Observe(&scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: n}, JobAssignedMessages: msgs}, at)
}

func shippedState(c Config, name string, started time.Time) *State {
	st := NewState(name, 0, 1200, []string{"us-east-1"}, started)
	st.listings["us-east-1"] = started
	st.SetBatch(c.Batch)
	return st
}

// No hour of the day has a hand floor in the shipped config: a floor is the size of a pool no arrival asked for. A
// single arrival at 15:00Z (the peak) gave 70 idle 4 vCPU and 100 idle 8 vCPU runners (Nov 27 02:48Z, about 171
// registrations and 20 USD for one maintenance job).
func TestTheShippedConfigHasNoHourlyFloor(t *testing.T) {
	c := shippedConfig(t)
	for _, spec := range shippedSets(c) {
		for h := 0; h < 24; h++ {
			if floor, _ := c.PoolBounds(spec, time.Date(2026, 11, 26, h, 5, 0, 0, time.UTC)); floor != 0 {
				t.Errorf("%s at %02d:05Z: floor %d, want 0", spec.Name, h, floor)
			}
		}
	}
}

// One routed job warms the pool by its own burst and no more, at the peak hour, for every size and on both planes.
func TestOneRoutedJobWarmsOnlyItsOwnBurst(t *testing.T) {
	c := shippedConfig(t)
	at := peak()
	for _, name := range []string{"superci-2cpu", "superci-4cpu", "superci-8cpu"} {
		spec := setOf(t, c, name)
		st := shippedState(c, name, at.Add(-time.Hour))
		assign(st, 1, "push", pushRef, at)
		if n := c.PoolTarget(spec, at, st.Burst(at, c.Pool)); n != 1 {
			t.Errorf("%s: one push job at the peak made pool %d, want its burst of 1", name, n)
		}
		spec.Dispatch = true
		ds := NewDispatchSet(spec, nil, []string{"us-east-1"}, at.Add(-time.Hour))
		ds.SetBatch(c.Batch)
		ds.enqueue(&session{set: ds, arrived: at}, jobHead{ID: "j", Event: "push", WorkflowRef: pushRef}, at)
		if n := c.PoolTarget(spec, at, ds.Burst(at, c.Pool)); n != 1 {
			t.Errorf("%s dispatch: one push job made pool %d, want 1", name, n)
		}
	}
}

// The pool follows the volume of routed arrivals: 40 jobs at once ask for the quantile of the burst (the 88th percentile of
// the jobs' own counts, 36), which is the pool and nothing more or less; no hand floor above it.
func TestThePoolEqualsTheMeasuredBurstOfRoutedJobs(t *testing.T) {
	c := shippedConfig(t)
	spec := setOf(t, c, "superci-4cpu")
	at := peak()
	st := shippedState(c, spec.Name, at.Add(-time.Hour))
	assign(st, 40, "push", pushRef, at)
	burst := st.Burst(at, c.Pool)
	if n := c.PoolTarget(spec, at, burst); n != burst || n < 30 || n > 40 {
		t.Fatalf("a burst of 40 jobs at once: pool %d for a burst of %d, want the burst, between 30 and 40", n, burst)
	}
}

// superci's own jobs (the cache-warm and hydration runs, by schedule or dispatch) are not routed demand: 9 of them size
// no pool on any size, at the peak hour, on a set that just started (a restart: GitHub redelivers the backlog), and on
// the dispatch plane; a push job beside them counts for itself alone.
func TestSupercisOwnMaintenanceJobsSizeNoPool(t *testing.T) {
	c := shippedConfig(t)
	at := peak()
	for _, name := range []string{"superci-2cpu", "superci-4cpu", "superci-8cpu"} {
		spec := setOf(t, c, name)
		for _, how := range []string{"fresh set", "restarted set"} {
			started := at.Add(-time.Hour)
			if how == "restarted set" {
				started = at.Add(-time.Minute)
			}
			st := shippedState(c, name, started)
			assign(st, 9, "schedule", cacheWarmRef, at)
			assign(st, 9, "workflow_dispatch", cacheWarmRef, at)
			if n := c.PoolTarget(spec, at, st.Burst(at, c.Pool)); n != 0 {
				t.Errorf("%s (%s): 18 cache-warm jobs made pool %d, want 0", name, how, n)
			}
			assign(st, 1, "push", pushRef, at)
			if n := c.PoolTarget(spec, at, st.Burst(at, c.Pool)); n != 1 {
				t.Errorf("%s (%s): a push job beside them made pool %d, want 1", name, how, n)
			}
		}
		spec.Dispatch = true
		ds := NewDispatchSet(spec, nil, []string{"us-east-1"}, at.Add(-time.Hour))
		ds.SetBatch(c.Batch)
		for i := 0; i < 9; i++ {
			ds.enqueue(&session{set: ds, arrived: at}, jobHead{ID: fmt.Sprintf("w%d", i), Event: "schedule", WorkflowRef: cacheWarmRef}, at)
		}
		if n := c.PoolTarget(spec, at, ds.Burst(at, c.Pool)); n != 0 {
			t.Errorf("%s dispatch: 9 cache-warm jobs made pool %d, want 0", name, n)
		}
	}
}

// A quiet set is restarted with no pin: a listener that saw only maintenance jobs saved a burst of 0, and the next one
// starts at 0 and stays there. (Pins do not survive a restart; with no hand floor and no maintenance demand there is nothing to pin.)
func TestARestartKeepsAQuietSetAtZeroWithNoPin(t *testing.T) {
	c := shippedConfig(t)
	spec := setOf(t, c, "superci-8cpu")
	at := peak()
	old := shippedState(c, spec.Name, at.Add(-2*time.Hour))
	assign(old, 9, "schedule", cacheWarmRef, at.Add(-10*time.Minute))
	old.Burst(at.Add(-5*time.Minute), c.Pool)
	saved := old.Carry(at, c.Pool)
	next := shippedState(c, spec.Name, at.Add(-time.Minute))
	next.Resume(saved)
	if n := c.PoolTarget(spec, at, next.Burst(at, c.Pool)); n != 0 {
		t.Fatalf("a restart of a set that saw only maintenance jobs: pool %d, want 0 (saved %+v)", n, saved)
	}
}

// A set nothing is routed to holds no idle machine, at any hour.
func TestASetWithNoJobsRoutedToItSitsAtZeroAtEveryHourUnderTheShippedConfig(t *testing.T) {
	c := shippedConfig(t)
	idle := NewState("superci-4cpu", 0, 100, []string{"us-east-1"}, t0.Add(-3*time.Hour))
	for _, spec := range shippedSets(c) {
		for h := 0; h < 24; h++ {
			at := time.Date(2026, 11, 26, h, 5, 0, 0, time.UTC)
			if n := c.PoolTarget(spec, at, idle.Burst(at, c.Pool)); n != 0 {
				t.Errorf("%s at %02d:05Z with no jobs: pool %d, want 0", spec.Name, h, n)
			}
		}
	}
}
