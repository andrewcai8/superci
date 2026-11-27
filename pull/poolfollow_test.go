package main

import (
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

// A set nothing is routed to holds no idle machine, at any hour, whatever the shipped floors are: Nov 26 23:57Z the
// floors held 70, 100 and 9 idle runners of 4, 8 and 2 vCPU, 43.8 USD an hour, with SUPERCI_JOBS unset.
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

// The floor still serves the busy hours: once jobs are routed to a set the pool is the hour's floor, so the first
// merge-queue wave of the peak finds ready runners; and 30 minutes after the last job it is 0 again.
func TestAFloorHoldsWhileJobsArriveAndReleasesAWindowAfterTheLastOne(t *testing.T) {
	c := shippedConfig(t)
	var spec SetSpec
	for _, s := range shippedSets(c) {
		if s.Name == "superci-4cpu" {
			spec = s
		}
	}
	floor, _ := c.PoolBounds(spec, time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC))
	st := NewState(spec.Name, 0, 1200, []string{"us-east-1"}, t0)
	st.listings["us-east-1"] = t0
	at := time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)
	st.Observe(&scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
		JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: scaleset.JobMessageBase{JobID: "g1", ScaleSetAssignTime: at}}}}, at)
	if n := c.PoolTarget(spec, at, st.Burst(at, c.Pool)); n != floor || floor == 0 {
		t.Fatalf("a job arrived at 15:00Z: pool %d, want the hour's floor %d (a shipped floor above 0)", n, floor)
	}
	later := at.Add(c.Pool.window() + c.Pool.hold() + time.Minute)
	if n := c.PoolTarget(spec, later, st.Burst(later, c.Pool)); n != 0 {
		t.Fatalf("%v after the last job: pool %d, want 0", later.Sub(at), n)
	}
}

// A dispatch size follows the same rule as a scale set: its arrivals are the jobs its identities' shims bring. With
// none routed it sits at 0 at every hour; one job brings the hour's floor back, so the first wave finds warm machines;
// and a window plus the hold after the last job it is 0 again.
func TestADispatchSetKeepsItsFloorOnlyWhileJobsAreRoutedToIt(t *testing.T) {
	c := shippedConfig(t)
	var spec SetSpec
	for _, s := range shippedSets(c) {
		if s.Name == "superci-4cpu" {
			spec = s
		}
	}
	spec.Dispatch = true
	idle := NewDispatchSet(spec, nil, []string{"us-east-1"}, time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC))
	for h := 0; h < 24; h++ {
		at := time.Date(2026, 11, 26, h, 5, 0, 0, time.UTC)
		if n := c.PoolTarget(spec, at, idle.Burst(at, c.Pool)); n != 0 {
			t.Errorf("at %02d:05Z with no jobs: pool %d, want 0", h, n)
		}
	}
	at := time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)
	floor, _ := c.PoolBounds(spec, at)
	busy := NewDispatchSet(spec, nil, []string{"us-east-1"}, at.Add(-time.Hour))
	busy.arrive(at)
	if n := c.PoolTarget(spec, at, busy.Burst(at, c.Pool)); n != floor || floor == 0 {
		t.Fatalf("a job arrived at 15:00Z: pool %d, want the hour's floor %d (a shipped floor above 0)", n, floor)
	}
	later := at.Add(c.Pool.window() + c.Pool.hold() + time.Minute)
	if n := c.PoolTarget(spec, later, busy.Burst(later, c.Pool)); n != 0 {
		t.Fatalf("%v after the last job: pool %d, want 0", later.Sub(at), n)
	}
}
