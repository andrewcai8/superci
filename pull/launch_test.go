package main

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// capacity stands in for one region's EC2: how many machines of each (type, spot or on-demand) it will still start. A
// fleet takes its overrides in priority order, as capacity-optimized-prioritized and prioritized do when capacity allows.
type capacity struct {
	mu    sync.Mutex
	left  map[string]int // "<type>/<spot|on-demand>"
	n     int
	asked []string // "<kind> <types...>" of each call
}

func (c *capacity) CreateFleet(_ context.Context, in *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	kind := "spot"
	if in.TargetCapacitySpecification.DefaultTargetCapacityType == ec2types.DefaultTargetCapacityTypeOnDemand {
		kind = "on-demand"
	}
	pools := slices.Clone(in.LaunchTemplateConfigs[0].Overrides)
	call := kind
	for _, p := range pools {
		call += " " + string(p.InstanceType)
	}
	c.asked = append(c.asked, call)
	sort.SliceStable(pools, func(i, j int) bool { return aws.ToFloat64(pools[i].Priority) < aws.ToFloat64(pools[j].Priority) })
	want := int(aws.ToInt32(in.TargetCapacitySpecification.TotalTargetCapacity))
	out := &ec2.CreateFleetOutput{}
	for _, p := range pools {
		key := string(p.InstanceType) + "/" + kind
		k := min(want, c.left[key])
		if k == 0 {
			continue
		}
		c.left[key] -= k
		want -= k
		var ids []string
		for range k {
			c.n++
			ids = append(ids, fmt.Sprintf("i-%04d", c.n))
		}
		out.Instances = append(out.Instances, ec2types.CreateFleetInstance{InstanceType: p.InstanceType, InstanceIds: ids})
	}
	if want > 0 {
		out.Errors = append(out.Errors, ec2types.CreateFleetError{ErrorCode: aws.String("InsufficientInstanceCapacity")})
	}
	return out, nil
}

// launchSet is the built-in config's set of label, launching into one region per capacity map (us-east-1 weight 2,
// us-east-2 weight 1), every type offered in one zone of each.
func launchSet(t *testing.T, label string, regions ...map[string]int) *SetFleet {
	t.Helper()
	return launchMix(t, label, nil, regions...)
}

// launchMix is launchSet with the set's spec changed by mix first (nil: as built in).
func launchMix(t *testing.T, label string, mix func(*SetSpec), regions ...map[string]int) *SetFleet {
	t.Helper()
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(c.Sets(), func(s SetSpec) bool { return s.Name == label })
	spec := c.Sets()[i]
	if mix != nil {
		mix(&spec)
	}
	sf := &SetFleet{fleet: &Fleet{}, spec: spec, regions: map[string]*setRegion{}}
	for k, left := range regions {
		name := []string{"us-east-1", "us-east-2"}[k]
		sf.fleet.regions = append(sf.fleet.regions, &Region{Name: name, Weight: 2 - k, fleets: &capacity{left: left}})
		var offers []ec2types.InstanceTypeOffering
		for _, typ := range spec.Types {
			offers = append(offers, ec2types.InstanceTypeOffering{InstanceType: ec2types.InstanceType(typ), Location: aws.String(name + "a")})
		}
		sf.regions[name] = &setRegion{template: "lt", version: "1", pools: rankedPools(offers, map[string]string{name + "a": "subnet-" + name}, spec.Types)}
	}
	return sf
}

func shapes(ms []*Machine) map[string]int {
	out := map[string]int{}
	for _, m := range ms {
		out[m.Type+"/"+m.Lifecycle]++
	}
	return out
}

// The mixes superci-4cpu and superci-8cpu shipped until Nov 28, when they began to ask on demand first: on_demand_first
// and spot_first still mean what they did, for the spot that comes back later.
func onDemandFirstMix(s *SetSpec) { s.OnDemand, s.OnDemandFirst, s.SpotFirst = false, 1, 0 }
func spotFirstMix(s *SetSpec)     { s.OnDemand, s.OnDemandFirst, s.SpotFirst = false, 0, 1 }

func TestSuperci4cpuAsksM8aznOnDemandBeforeASlowerSpotType(t *testing.T) {
	thin := map[string]int{"m8azn.xlarge/spot": 1, "m8azn.xlarge/on-demand": 100, "m8a.xlarge/spot": 100, "r8a.xlarge/spot": 100}
	got := shapes(launchMix(t, "superci-4cpu", onDemandFirstMix, thin, maps(thin)).Launch(context.Background(), "r1", 6, 0))
	if want := map[string]int{"m8azn.xlarge/spot": 2, "m8azn.xlarge/on-demand": 4}; !sameShapes(got, want) {
		t.Fatalf("with m8azn spot thin and the on-demand share spent, 6 machines started as %v, want %v", got, want)
	}
}

func TestSuperci4cpuTakesM8aznSpotWhenItHasCapacity(t *testing.T) {
	plenty := map[string]int{"m8azn.xlarge/spot": 100, "m8azn.xlarge/on-demand": 100, "m8a.xlarge/spot": 100}
	launched := launchMix(t, "superci-4cpu", onDemandFirstMix, plenty, maps(plenty)).Launch(context.Background(), "r1", 6, 6)
	got := shapes(launched)
	if want := map[string]int{"m8azn.xlarge/spot": 6}; !sameShapes(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, m := range launched {
		if m.Template != "1" {
			t.Fatalf("%s booted from template version %q, want its region's current 1: Recycle would replace it at once", m.ID, m.Template)
		}
	}
}

func TestSuperci4cpuFallsToSlowerSpotOnlyWhenM8aznIsOutEntirely(t *testing.T) {
	out := map[string]int{"m8a.xlarge/spot": 2, "r8a.xlarge/spot": 100, "m8a.xlarge/on-demand": 100}
	got := shapes(launchMix(t, "superci-4cpu", onDemandFirstMix, out, maps(out)).Launch(context.Background(), "r1", 6, 0))
	if want := map[string]int{"m8a.xlarge/spot": 4, "r8a.xlarge/spot": 2}; !sameShapes(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// superci-8cpu carries unit-tests' 8 vCPU legs (microvm-design-v5): m8azn.3xlarge ran their tests at 0.75x Blacksmith's
// time and m8a.2xlarge at 1.03x, and per leg they cost 1.01x and 0.40x of Blacksmith on spot. m8azn.3xlarge on demand
// costs 2.40x, so it is asked spot alone first and never on demand; m8a.2xlarge's spot comes next, and only its on-demand
// is left to on_demand_share.
func TestSuperci8cpuAsksM8aznSpotThenM8aSpotAndNeverM8aznOnDemand(t *testing.T) {
	thin := map[string]int{"m8azn.3xlarge/spot": 1, "m8azn.3xlarge/on-demand": 100, "m8a.2xlarge/spot": 100, "m8a.2xlarge/on-demand": 100}
	east, west := thin, maps(thin)
	sf := launchMix(t, "superci-8cpu", spotFirstMix, east, west)
	got := shapes(sf.Launch(context.Background(), "r1", 6, 6))
	if want := map[string]int{"m8azn.3xlarge/spot": 2, "m8a.2xlarge/spot": 4}; !sameShapes(got, want) {
		t.Fatalf("with m8azn spot thin, 6 machines started as %v, want %v", got, want)
	}
	plenty := map[string]int{"m8azn.3xlarge/spot": 100, "m8azn.3xlarge/on-demand": 100, "m8a.2xlarge/spot": 100}
	if got, want := shapes(launchMix(t, "superci-8cpu", spotFirstMix, plenty, maps(plenty)).Launch(context.Background(), "r1", 6, 6)), map[string]int{"m8azn.3xlarge/spot": 6}; !sameShapes(got, want) {
		t.Fatalf("with m8azn spot to spare: got %v, want %v", got, want)
	}
	out := map[string]int{"m8azn.3xlarge/on-demand": 100, "m8a.2xlarge/spot": 2, "m8a.2xlarge/on-demand": 100}
	east, west = out, maps(out)
	sf = launchMix(t, "superci-8cpu", spotFirstMix, east, west)
	if got, want := shapes(sf.Launch(context.Background(), "r1", 6, 1)), map[string]int{"m8a.2xlarge/spot": 4, "m8a.2xlarge/on-demand": 1}; !sameShapes(got, want) {
		t.Fatalf("with no m8azn spot and m8a's spot short: got %v, want %v", got, want)
	}
	for _, r := range sf.fleet.regions {
		for _, call := range r.fleets.(*capacity).asked {
			if strings.HasPrefix(call, "on-demand") && strings.Contains(call, "m8azn") {
				t.Fatalf("superci-8cpu asked m8azn on demand: %q", call)
			}
		}
	}
}

// The shipped superci-8cpu, as the listener loads it (deploy/override.json over config.json): unit-tests' types, m8azn
// first (0.75x Blacksmith's test time against m8a's 1.03x), asked on demand first, a dispatch size, and a pool cap that
// holds the warm-first pool v5 sized at 242 for the Nov 26 peak. The built-in config says the same.
func TestTheShippedSuperci8cpuIsTheUnitTestsMix(t *testing.T) {
	builtin, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	c := shippedConfig(t)
	s := c.Sizes["superci-8cpu"]
	if want := []string{"m8azn.3xlarge", "m8a.2xlarge"}; !slices.Equal(s.Types, want) || !s.OnDemand || s.SpotFirst != 0 || s.OnDemandFirst != 0 || !s.Dispatch {
		t.Fatalf("superci-8cpu: types %v on_demand %v spot_first %d on_demand_first %d dispatch %v; want types %v, on_demand, dispatch",
			s.Types, s.OnDemand, s.SpotFirst, s.OnDemandFirst, s.Dispatch, want)
	}
	for _, l := range []string{"superci-4cpu", "superci-8cpu"} {
		if b, o := builtin.Sizes[l], c.Sizes[l]; b.OnDemand != o.OnDemand || b.Max != o.Max || !slices.Equal(b.Types, o.Types) {
			t.Fatalf("%s: built in on_demand %v max %d types %v, shipped on_demand %v max %d types %v", l, b.OnDemand, b.Max, b.Types, o.OnDemand, o.Max, o.Types)
		}
	}
	for h, cap := range c.Pools["superci-8cpu"].Cap {
		if cap != 250 {
			t.Fatalf("superci-8cpu's pool cap at %02dZ is %d, want 250", h, cap)
		}
	}
}

// A size whose first type is an m8azn asks it first, alone (spot_first) or spot then on demand (on_demand_first): asked
// with slower types in one spot fleet, 73% of superci-8cpu's machines landed on the slower ones (Nov 26-27).
func TestEverySizeThatLeadsWithM8aznAsksItFirst(t *testing.T) {
	c := shippedConfig(t)
	seen := 0
	for label, s := range c.Sizes {
		if len(s.Types) > 0 && strings.HasPrefix(s.Types[0], "m8azn.") {
			seen++
			if s.OnDemandFirst < 1 && s.SpotFirst < 1 && !s.OnDemand {
				t.Errorf("%s leads with %s and asks it with the slower types: on_demand_first %d, spot_first %d, on_demand %v", label, s.Types[0], s.OnDemandFirst, s.SpotFirst, s.OnDemand)
			}
		}
	}
	if seen < 2 {
		t.Fatalf("only %d sizes lead with m8azn: superci-4cpu and superci-8cpu do", seen)
	}
}

func TestTheShareBoundedAskLeavesOutTheTypesAlreadyAskedOnDemand(t *testing.T) {
	east := &capacity{left: map[string]int{"m8azn.xlarge/on-demand": 2, "m8a.xlarge/on-demand": 100}}
	sf := launchMix(t, "superci-4cpu", onDemandFirstMix, map[string]int{}, map[string]int{})
	sf.fleet.regions[0].fleets, sf.fleet.regions[1].fleets = east, &capacity{left: map[string]int{}}
	got := shapes(sf.Launch(context.Background(), "r1", 6, 3))
	if want := map[string]int{"m8azn.xlarge/on-demand": 2, "m8a.xlarge/on-demand": 3}; !sameShapes(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, call := range east.asked {
		if strings.HasPrefix(call, "on-demand") && strings.Contains(call, "m8a.xlarge") && strings.Contains(call, "m8azn") {
			t.Fatalf("the share-bounded ask named m8azn again: %q (calls %q)", call, east.asked)
		}
	}
}

func TestASizeWithoutOnDemandFirstKeepsOnDemandUnderItsShare(t *testing.T) {
	none := map[string]int{"c8a.large/on-demand": 100}
	got := shapes(launchSet(t, "superci-2cpu", none, maps(none)).Launch(context.Background(), "r1", 6, 1))
	if want := map[string]int{"c8a.large/on-demand": 1}; !sameShapes(got, want) {
		t.Fatalf("got %v, want %v: on_demand_share bounds a size that asks no type on demand first", got, want)
	}
}

func TestOnDemandFirstPastTheSizesTypesIsRefused(t *testing.T) {
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	s := c.Sizes["superci-4cpu"]
	s.OnDemand, s.OnDemandFirst = false, len(s.Types)+1
	c.Sizes["superci-4cpu"] = s
	if err := c.validate(); err == nil {
		t.Fatal("on_demand_first past the size's types passed validation")
	}
}

func TestSpotFirstPastTheTypesOrBesideOnDemandFirstIsRefused(t *testing.T) {
	for name, set := range map[string]func(*SizeConfig){
		"past its types":           func(s *SizeConfig) { s.SpotFirst = len(s.Types) + 1 },
		"with on_demand_first too": func(s *SizeConfig) { s.SpotFirst, s.OnDemandFirst = 1, 1 },
	} {
		c, err := LoadConfig("")
		if err != nil {
			t.Fatal(err)
		}
		s := c.Sizes["superci-8cpu"]
		s.OnDemand = false
		set(&s)
		c.Sizes["superci-8cpu"] = s
		if err := c.validate(); err == nil {
			t.Errorf("spot_first %s passed validation", name)
		}
	}
}

func sameShapes(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestOnDemandBesideOnDemandFirstOrSpotFirstIsRefused(t *testing.T) {
	for name, set := range map[string]func(*SizeConfig){
		"with on_demand_first": func(s *SizeConfig) { s.OnDemand, s.OnDemandFirst = true, 1 },
		"with spot_first":      func(s *SizeConfig) { s.OnDemand, s.SpotFirst = true, 1 },
	} {
		c, err := LoadConfig("")
		if err != nil {
			t.Fatal(err)
		}
		s := c.Sizes["superci-8cpu"]
		set(&s)
		c.Sizes["superci-8cpu"] = s
		if err := c.validate(); err == nil || !strings.Contains(err.Error(), "on_demand") {
			t.Errorf("on_demand %s passed validation (%v)", name, err)
		}
	}
}
