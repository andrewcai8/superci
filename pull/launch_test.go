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
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(c.Sets(), func(s SetSpec) bool { return s.Name == label })
	spec := c.Sets()[i]
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

func TestSuperci4cpuAsksM8aznOnDemandBeforeASlowerSpotType(t *testing.T) {
	thin := map[string]int{"m8azn.xlarge/spot": 1, "m8azn.xlarge/on-demand": 100, "m8a.xlarge/spot": 100, "r8a.xlarge/spot": 100}
	got := shapes(launchSet(t, "superci-4cpu", thin, maps(thin)).Launch(context.Background(), "r1", 6, 0))
	if want := map[string]int{"m8azn.xlarge/spot": 2, "m8azn.xlarge/on-demand": 4}; !sameShapes(got, want) {
		t.Fatalf("with m8azn spot thin and the on-demand share spent, 6 machines started as %v, want %v", got, want)
	}
}

func TestSuperci4cpuTakesM8aznSpotWhenItHasCapacity(t *testing.T) {
	plenty := map[string]int{"m8azn.xlarge/spot": 100, "m8azn.xlarge/on-demand": 100, "m8a.xlarge/spot": 100}
	launched := launchSet(t, "superci-4cpu", plenty, maps(plenty)).Launch(context.Background(), "r1", 6, 6)
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
	got := shapes(launchSet(t, "superci-4cpu", out, maps(out)).Launch(context.Background(), "r1", 6, 0))
	if want := map[string]int{"m8a.xlarge/spot": 4, "r8a.xlarge/spot": 2}; !sameShapes(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTheShareBoundedAskLeavesOutTheTypesAlreadyAskedOnDemand(t *testing.T) {
	east := &capacity{left: map[string]int{"m8azn.xlarge/on-demand": 2, "m8a.xlarge/on-demand": 100}}
	sf := launchSet(t, "superci-4cpu", map[string]int{}, map[string]int{})
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
	s.OnDemandFirst = len(s.Types) + 1
	c.Sizes["superci-4cpu"] = s
	if err := c.validate(); err == nil {
		t.Fatal("on_demand_first past the size's types passed validation")
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
