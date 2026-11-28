package main

import (
	"context"
	"strings"
	"testing"
)

// unit-tests' sizes ask on demand first (Nov 28). On Nov 27 23:57Z (F2, unit-tests' volume) AWS took 27 of 28 us-east-1
// m8azn.xlarge spot machines within 18 minutes, and 31 jobs with them (their spot requests read
// instance-terminated-no-capacity, stamped before the notices reached the listener); the 146 on demand lost none.

// Each type is asked on demand alone, in the size's order and in us-east-1 before us-east-2, before any spot: one on-demand
// ask over several types would weigh their prices, not the order. The fake EC2 refuses a ClientToken sent again with other
// overrides, as EC2 does (IdempotentParameterMismatch): the asks of one round sharing a token never reached m8a.
func TestTheUnitTestsSizesAskEachTypeOnDemandBeforeAnySpot(t *testing.T) {
	for _, c := range []struct{ label, fast, slow string }{{"superci-8cpu", "m8azn.3xlarge", "m8a.2xlarge"}, {"superci-4cpu", "m8azn.xlarge", "m8a.xlarge"}} {
		east := map[string]int{c.fast + "/on-demand": 2, c.slow + "/on-demand": 2, c.fast + "/spot": 100, c.slow + "/spot": 100}
		west := map[string]int{c.fast + "/on-demand": 1, c.slow + "/on-demand": 2, c.fast + "/spot": 100, c.slow + "/spot": 100}
		sf := launchSet(t, c.label, east, west)
		// the on-demand share is spent (room 0): it bounds none of these
		got := shapes(sf.Launch(context.Background(), "r1", 8, 0))
		if want := map[string]int{c.fast + "/on-demand": 3, c.slow + "/on-demand": 4, c.fast + "/spot": 1}; !sameShapes(got, want) {
			t.Fatalf("%s: 8 machines started as %v, want %v", c.label, got, want)
		}
		var order []string
		for _, r := range sf.fleet.regions {
			for _, call := range r.fleets.(*capacity).asked {
				order = append(order, r.Name+" "+call)
				if strings.HasPrefix(call, "on-demand") && len(strings.Fields(call)) != 2 {
					t.Fatalf("%s asked several types in one on-demand fleet: %q", c.label, call)
				}
			}
		}
		if len(order) < 4 || order[0] != "us-east-1 on-demand "+c.fast || order[1] != "us-east-1 on-demand "+c.slow {
			t.Fatalf("%s: us-east-1 was asked %q, want %s on demand, then %s on demand, then spot", c.label, order, c.fast, c.slow)
		}
		for _, r := range sf.fleet.regions {
			calls := r.fleets.(*capacity).asked
			for i, call := range calls {
				if strings.HasPrefix(call, "spot") {
					for _, later := range calls[i:] {
						if strings.HasPrefix(later, "on-demand") {
							t.Fatalf("%s: %s asked on demand after spot: %q", c.label, r.Name, calls)
						}
					}
				}
			}
		}
	}
}

// With every type's on demand out in both regions, the machines still start, on spot.
func TestAnOnDemandSizeTakesSpotWhenOnDemandIsOutEverywhere(t *testing.T) {
	spot := map[string]int{"m8a.2xlarge/spot": 100}
	if got, want := shapes(launchSet(t, "superci-8cpu", spot, maps(spot)).Launch(context.Background(), "r1", 5, 0)), map[string]int{"m8a.2xlarge/spot": 5}; !sameShapes(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// The account's on-demand vCPU quota for standard instances (Nov 28): us-east-1 5,000, us-east-2 4,000 (raised from 1,152).
// The sizes that ask on demand first fill it whatever the share, so at their max, with the biggest of their types, they
// leave room for what the share lets the other sizes ask on demand (on_demand_share of each one's max), and for the
// listener and the two identity hosts.
const onDemandQuotaVCPU = 5000 + 4000

var vcpus = map[string]int{"m8azn.xlarge": 4, "m8a.xlarge": 4, "r8a.xlarge": 4, "m8azn.3xlarge": 12, "m8a.2xlarge": 8}

func TestTheOnDemandSizesFitTheAccountsOnDemandQuota(t *testing.T) {
	c := shippedConfig(t)
	first, share := 0, 0.0
	for label, s := range c.Sizes {
		biggest := s.CPU
		for _, typ := range s.Types {
			biggest = max(biggest, vcpus[typ])
		}
		if label == "superci-4cpu" || label == "superci-8cpu" {
			first += s.Max * biggest
		} else {
			share += c.OnDemand * float64(s.Max*biggest)
		}
	}
	const planes = 2 + 2*16 // the listener (c7g.large) and two r7i.4xlarge identity hosts (identity.sh's default type)
	// Machines that are ending hold their vCPUs until EC2 has terminated them; while jobs queue behind max, each job's
	// machine ends as the next launches: room for 30 of the biggest type (m8azn.3xlarge) ending at once.
	const ending = 30 * 12
	if total := float64(first+planes+ending) + share; total > onDemandQuotaVCPU {
		t.Fatalf("on demand at the sizes' max: %d vCPU first, %.0f by the share, %d the plane's own, %d ending: %.0f against the quota's %d", first, share, planes, ending, total, onDemandQuotaVCPU)
	}
	if c.Sizes["superci-8cpu"].Max < 285+250 {
		t.Fatalf("superci-8cpu's max %d holds fewer than the F2 trace's 285 machines at once and the 250 of its pool cap", c.Sizes["superci-8cpu"].Max)
	}
	if c.Sizes["superci-4cpu"].Max < 122+200 {
		t.Fatalf("superci-4cpu's max %d holds fewer than the F2 trace's 122 legs at once and the 200 of its pool cap", c.Sizes["superci-4cpu"].Max)
	}
}

// On demand, unit-tests' day costs more than its spot did; the caps leave it room (Blacksmith's unit-tests were about
// 1,343 USD a day, which this replaces): a cap reached stops every superci job until the day turns.
func TestTheCapsLeaveUnitTestsOnDemandRoom(t *testing.T) {
	c := shippedConfig(t)
	if c.DailyUSD != 3000 || c.MonthlyUSD != 45000 {
		t.Fatalf("caps %v a day and %v a month, want 3000 and 45000", c.DailyUSD, c.MonthlyUSD)
	}
}
