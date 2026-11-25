package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// params is SSM as the pools see it: named string values, a missing name an error as SSM's.
type params struct {
	mu      sync.Mutex
	values  map[string]string
	putErr  error
	getErr  error
	puts    int
	lastPut string
}

func (p *params) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.getErr != nil {
		return nil, p.getErr
	}
	v, ok := p.values[aws.ToString(in.Name)]
	if !ok {
		return nil, &ssmtypes.ParameterNotFound{}
	}
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Value: aws.String(v)}}, nil
}

func (p *params) PutParameter(_ context.Context, in *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.putErr != nil {
		return nil, p.putErr
	}
	if p.values == nil {
		p.values = map[string]string{}
	}
	p.values[aws.ToString(in.Name)] = aws.ToString(in.Value)
	p.puts++
	p.lastPut = aws.ToString(in.Name)
	return &ssm.PutParameterOutput{}, nil
}

// poolSet is a set started at start that resumed from a burst of resumed, as a listener's first minutes leave it.
func poolSet(name string, start time.Time, resumed int) *Set {
	s := &Set{spec: SetSpec{Name: name}, state: NewState(name, 0, 100, []string{"us-east-1"}, start)}
	if resumed > 0 {
		s.state.Resume(Carried{Burst: resumed, Launches: []int64{30000}})
	}
	return s
}

// F1: Carry took the largest burst of the held ones only, so a start with no arrivals yet saved its raw 0 over the
// burst it had carried, and the saved burst survived one restart.
func TestACarriedBurstIsSavedAgainWhileItHolds(t *testing.T) {
	s := NewState("set", 0, 100, []string{"us-east-1"}, t0)
	s.Resume(Carried{Burst: 56})
	for at := t0; at.Before(t0.Add(testRule.window())); at = at.Add(15 * time.Second) { // setPools' tick, no job in sight
		s.Burst(at, testRule)
		if got := s.Carry(at, testRule).Burst; got != 56 {
			t.Fatalf("%v after the start, no arrivals: carries %d, want the 56 it started from", at.Sub(t0), got)
		}
	}
	after := t0.Add(testRule.window() + time.Minute)
	s.Burst(after, testRule)
	if got := s.Carry(after, testRule).Burst; got != 0 {
		t.Fatalf("past the window, no arrivals: carries %d, want 0 (the pool is what the jobs ask for)", got)
	}
}

func TestACarriedBurstGivesWayToALargerOneTheJobsMake(t *testing.T) {
	s := NewState("set", 0, 100, []string{"us-east-1"}, t0)
	s.Resume(Carried{Burst: 3})
	assigned(s, t0.Add(time.Second), 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	at := t0.Add(2 * time.Second)
	s.Burst(at, testRule)
	if got := s.Carry(at, testRule).Burst; got != 9 {
		t.Fatalf("10 jobs at once over a carried 3: carries %d, want 9", got)
	}
}

// The restart that follows saves reads back what the one before it saved, however many restarts there are.
func TestABurstSurvivesRestartsThroughTheSavedParameter(t *testing.T) {
	store := &params{}
	const name = "/superci-pull/pool/prod"
	first := poolSet("set", t0, 0)
	assigned(first.state, t0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	at := t0.Add(time.Second)
	first.state.Burst(at, testRule)
	if err := savePools(context.Background(), store, name, []*Set{first}, at, testRule); err != nil {
		t.Fatal(err)
	}
	for restart := 1; restart <= 3; restart++ {
		at = at.Add(5 * time.Minute)
		next := poolSet("set", at, 0)
		if err := loadPools(context.Background(), store, name, []*Set{next}, testRule.window(), at); err != nil {
			t.Fatal(err)
		}
		for tick := time.Duration(0); tick < time.Minute; tick += 15 * time.Second { // a minute of no arrivals, saved at the end
			next.state.Burst(at.Add(tick), testRule)
		}
		at = at.Add(time.Minute)
		if err := savePools(context.Background(), store, name, []*Set{next}, at, testRule); err != nil {
			t.Fatal(err)
		}
		if got := next.state.Burst(at, testRule); got != 9 {
			t.Fatalf("restart %d: burst %d, want the 9 saved", restart, got)
		}
	}
}

// Two deployments (production and one that shares its prefix) save each its own parameter: neither erases the other's sets.
func TestTwoDeploymentsSavingDoNotEraseEachOther(t *testing.T) {
	store := &params{}
	prod, dev := poolParamName("/superci-pull/pool", "superci-pull"), poolParamName("/superci-pull/pool", "superci-pull-wip")
	p := []*Set{poolSet("superci-96cpu", t0, 56), poolSet("superci-8cpu", t0, 12)}
	d := []*Set{poolSet("superci-96cpu", t0, 2)}
	ctx := context.Background()
	for _, save := range []struct {
		name string
		sets []*Set
	}{{prod, p}, {dev, d}, {prod, p}, {dev, d}} { // interleaved, and each twice
		if err := savePools(ctx, store, save.name, save.sets, t0.Add(time.Minute), testRule); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []struct {
		name  string
		burst map[string]int
	}{{prod, map[string]int{"superci-96cpu": 56, "superci-8cpu": 12}}, {dev, map[string]int{"superci-96cpu": 2, "superci-8cpu": 0}}} {
		restarted := []*Set{poolSet("superci-96cpu", t0.Add(2*time.Minute), 0), poolSet("superci-8cpu", t0.Add(2*time.Minute), 0)}
		if err := loadPools(ctx, store, want.name, restarted, testRule.window(), t0.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
		for _, s := range restarted {
			if got := s.state.Burst(t0.Add(2*time.Minute), testRule); got != want.burst[s.spec.Name] {
				t.Errorf("%s of %s restarted: burst %d, want %d", s.spec.Name, want.name, got, want.burst[s.spec.Name])
			}
		}
	}
}

func TestEachDeploymentHasItsOwnPoolParameter(t *testing.T) {
	a, b := poolParamName("/superci-pull/pool", "superci-pull"), poolParamName("/superci-pull/pool", "superci-pull-wip")
	if a == b {
		t.Fatalf("two deployments under one prefix both save to %s", a)
	}

	for in, want := range map[[2]string]string{
		{"/superci-pull/pool", "superci-pull"}:        "/superci-pull/pool/superci-pull",
		{"/superci-pull-dev/pool/", "superci-pull-x"}: "/superci-pull-dev/pool/superci-pull-x",
	} {
		if got := poolParamName(in[0], in[1]); got != want {
			t.Errorf("%v: %s, want %s", in, got, want)
		}
	}
}

func TestSavingTwiceLeavesTheSameValueAsOnce(t *testing.T) {
	store := &params{}
	sets := []*Set{poolSet("a", t0, 7), poolSet("b", t0, 0)}
	at := t0.Add(time.Minute)
	if err := savePools(context.Background(), store, "p", sets, at, testRule); err != nil {
		t.Fatal(err)
	}
	once := store.values["p"]
	if err := savePools(context.Background(), store, "p", sets, at, testRule); err != nil {
		t.Fatal(err)
	}
	if store.values["p"] != once {
		t.Fatalf("a second save at the same time changed %s to %s", once, store.values["p"])
	}
}

func TestASaveThatFailsReportsAndWritesNothing(t *testing.T) {
	store := &params{putErr: errors.New("throttled")}
	if err := savePools(context.Background(), store, "p", []*Set{poolSet("a", t0, 7)}, t0, testRule); err == nil {
		t.Fatal("a refused save returned no error: the listener would think its pools are kept")
	}
	if len(store.values) != 0 {
		t.Fatalf("a refused save left %v", store.values)
	}
}

func TestLoadingResumesOnlyWhatWasSavedWithinTheWindow(t *testing.T) {
	ctx := context.Background()
	store := &params{}
	saver := []*Set{poolSet("a", t0, 20), poolSet("b", t0, 30)}
	if err := savePools(ctx, store, "p", saver, t0, testRule); err != nil {
		t.Fatal(err)
	}
	burst := func(s *Set, at time.Time) int { return s.state.Burst(at, testRule) }

	fresh := t0.Add(testRule.window() - time.Minute)
	a, b, c := poolSet("a", fresh, 0), poolSet("b", fresh, 0), poolSet("c", fresh, 0)
	if err := loadPools(ctx, store, "p", []*Set{a, b, c}, testRule.window(), fresh); err != nil {
		t.Fatal(err)
	}
	if got := []int{burst(a, fresh), burst(b, fresh), burst(c, fresh)}; got[0] != 20 || got[1] != 30 || got[2] != 0 {
		t.Fatalf("a save %v old resumed bursts %v, want 20, 30 and 0 for the set it never saw", fresh.Sub(t0), got)
	}

	late := t0.Add(testRule.window() + time.Minute)
	l := poolSet("a", late, 0)
	if err := loadPools(ctx, store, "p", []*Set{l}, testRule.window(), late); err != nil {
		t.Fatal(err)
	}
	if got := burst(l, late); got != 0 {
		t.Fatalf("a save older than the window resumed burst %d, want 0: it says nothing about the jobs now", got)
	}
}

func TestLoadingWithNothingSavedStartsFromTheFloor(t *testing.T) {
	s := poolSet("a", t0, 0)
	if err := loadPools(context.Background(), &params{}, "p", []*Set{s}, testRule.window(), t0); err != nil {
		t.Fatalf("a missing parameter is a first start, not an error: %v", err)
	}
	if got := s.state.Burst(t0, testRule); got != 0 {
		t.Fatalf("burst %d with nothing saved", got)
	}
}

func TestLoadingFailsOnAnUnreadableValueOrAFailedRead(t *testing.T) {
	for name, store := range map[string]*params{
		"garbage": {values: map[string]string{"p": "not json"}},
		"denied":  {getErr: errors.New("AccessDenied")},
	} {
		s := poolSet("a", t0, 0)
		err := loadPools(context.Background(), store, "p", []*Set{s}, testRule.window(), t0)
		if err == nil || strings.Contains(err.Error(), "ParameterNotFound") {
			t.Errorf("%s: error %v, want one the caller logs", name, err)
		}
		if got := s.state.Burst(t0, testRule); got != 0 {
			t.Errorf("%s: resumed burst %d", name, got)
		}
	}
}
