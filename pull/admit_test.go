package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// refusingGitHub is the fake registrar that refuses its first refusals registrations the way GitHub's cap does.
type refusingGitHub struct {
	fakeRegistrar
	mu       sync.Mutex
	refusals int
}

func (g *refusingGitHub) GenerateJitRunnerConfig(ctx context.Context, in *scaleset.RunnerScaleSetJitRunnerSetting, id int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	g.mu.Lock()
	refuse := g.refusals > 0
	if refuse {
		g.refusals--
	}
	g.mu.Unlock()
	if refuse {
		return nil, errors.New("400 Bad Request: ConfigureTooManyRunnersException: too many runners configured in the last 5 minutes")
	}
	return g.fakeRegistrar.GenerateJitRunnerConfig(ctx, in, id)
}

// builtIn is the registration budget the listener ships with.
func builtIn(t *testing.T) RegConfig {
	t.Helper()
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	return c.Registration
}

// burstProcess is a listener whose one set has n machines booted and asking, for n waiting jobs.
func burstProcess(gh registrarFunc, b *Budget, n int) *Server {
	now := time.Now()
	st := NewState("set", 0, 100, []string{"us-east-1"}, now.Add(-time.Hour))
	st.Observe(stats(n, 0), now)
	st.Seen("us-east-1", machines(n, now.Add(-10*time.Minute)), now.Add(-time.Minute), now)
	return &Server{sets: map[string]*Set{"set": {spec: SetSpec{Name: "set"}, id: 1, state: st}}, budget: b, verifier: trusting{}, client: gh,
		refresh: func(context.Context, string) {}, claim: (&Claims{store: &fakeS3{objects: map[string]string{}}, bucket: "claims"}).Take,
		issue: func(context.Context, *Machine) error { return nil }, slots: make(chan struct{}, 16), maxAge: func() time.Duration { return 8 * time.Hour }}
}

type registrarFunc interface {
	GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
}

// The canary of Nov 26 waited 482 to 898 s while GitHub refused no one: the budget's estimate of the organization's
// rate turned every ask away before GitHub was asked. With the estimate reading 1,704 in the window, above GitHub's
// documented 1,500, as it did at 03:15Z that night, a burst of ten machines still gets ten runners at once.
func TestWithoutARefusalNoMachineWaitsWhateverTheEstimateSays(t *testing.T) {
	now := time.Now()
	b := NewBudget(builtIn(t), now.Add(-time.Minute))
	for s := 0; s <= 360; s += 15 {
		b.Head(now.Add(time.Duration(s-360)*time.Second), 8_000_000+int64(5.68*float64(s)))
	}
	if e := b.Estimate(now); !e.Known || e.WindowUse < 1700 {
		t.Fatalf("the estimate reads %+v, want the 1,704 of 03:15Z", e)
	}
	gh := &fakeRegistrar{runners: map[string]bool{}}
	h := burstProcess(gh, b, 10)
	for i := range 10 {
		id := fmt.Sprintf("i-%04d", i)
		if code, body := jitAsk(h, id); code != http.StatusOK || !strings.HasPrefix(body, "config-of-") {
			t.Fatalf("%s asked once and got %d %q: a machine waited with GitHub never asked", id, code, body)
		}
	}
	if gh.made != 10 || b.Snapshot(time.Now()).Refused != 0 {
		t.Fatalf("GitHub registered %d of 10", gh.made)
	}
}

// GitHub's refusal is the one hard signal. It halves the admitted rate and holds every ask for 5 s; the rate climbs
// back to the ceiling while GitHub accepts, and a second refusal in a row halves it again and holds twice as long.
func TestARefusalBacksOffThenTheRateRecovers(t *testing.T) {
	c := builtIn(t)
	now := time.Now()
	b := NewBudget(c, now.Add(-time.Minute))
	gh := &refusingGitHub{fakeRegistrar: fakeRegistrar{runners: map[string]bool{}}, refusals: 1}
	h := burstProcess(gh, b, 3)
	if code, _ := jitAsk(h, "i-0000"); code != http.StatusTooManyRequests {
		t.Fatalf("a refused registration answered %d, want 429 so the machine asks again", code)
	}
	at := time.Now()
	s := b.Snapshot(at)
	if s.Refused != 1 || !s.Throttled || s.Rate > c.CeilingRate/2+0.01 {
		t.Fatalf("after GitHub's refusal: %+v, want it counted, every ask held and the rate halved from %v", s, c.CeilingRate)
	}
	if code, _ := jitAsk(h, "i-0001"); code != http.StatusTooManyRequests || gh.made != 0 {
		t.Fatalf("an ask inside the hold answered %d with %d registered: GitHub must not be asked again yet", code, gh.made)
	}
	if ok, why := b.Try(at.Add(4*time.Second), Asker{P: ForJob}); ok || why != DeniedThrottled {
		t.Fatalf("4 s after the refusal: %v %v, want held", ok, why)
	}
	if ok, _ := b.Try(at.Add(6*time.Second), Asker{P: ForJob}); !ok {
		t.Fatal("6 s after the refusal the hold is over and tokens have saved up: still turned away")
	}
	b.Done(at.Add(6*time.Second), Made, 0)
	if r := b.Rate(); r >= c.CeilingRate {
		t.Fatalf("the rate is back at the ceiling 6 s after a refusal: %v", r)
	}
	back := at.Add(time.Duration(c.RecoverS/2+5) * time.Second)
	b.Try(back, Asker{P: ForJob})
	b.Done(back, Made, 0)
	if r := b.Rate(); r != c.CeilingRate {
		t.Fatalf("%s after a refusal with none since, the rate is %v, want the ceiling %v", back.Sub(at), r, c.CeilingRate)
	}
	b.Try(back, Asker{P: ForJob})
	b.Done(back, Refused, 0)
	b.Try(back, Asker{P: ForJob})
	b.Done(back, Refused, 0)
	if r, s := b.Rate(), b.Snapshot(back.Add(9*time.Second)); r > c.CeilingRate/4+0.01 || !s.Throttled || s.Refused != 3 {
		t.Fatalf("two refusals in a row: rate %v and %+v, want a quarter of the ceiling and a hold of 10 s", r, s)
	}
}

// When tokens are short (a refusal halved the rate, or a burst passes the ceiling), the machines that wait are served in
// the order they launched, whatever order they ask in: here all ten first ask on an empty bucket, and the newest asks
// first in every round.
func TestABurstOfTenIsServedInLaunchOrder(t *testing.T) {
	c := reg()
	c.CeilingRate, c.CeilingBurst = 1, 1
	at := t0.Add(6 * time.Minute)
	b := NewBudget(c, at)
	s := newState(0, 100)
	waitingJobs(s, at, "pull_request", "pull_request", "pull_request", "pull_request", "pull_request",
		"pull_request", "pull_request", "pull_request", "pull_request", "pull_request")
	ms := machines(10, at)
	byLaunch := make([]*Machine, 10)
	for i, place := range []int{3, 7, 0, 9, 1, 5, 8, 2, 6, 4} { // launch order unlike the ids' order
		ms[i].Launched = at.Add(time.Duration(place-10) * time.Second)
		byLaunch[place] = ms[i]
	}
	launch(s, ms, at)
	var order []string
	for round := 0; round < 12 && len(order) < 10; round++ {
		now := at.Add(time.Duration(3*round) * time.Second)
		for i := 9; i >= 0; i-- {
			if m := byLaunch[i]; s.Ask(m.ID, now, b, 8*time.Hour) == Grant {
				order = append(order, m.ID)
				b.Done(now, Made, 0)
			}
		}
	}
	want := ids(byLaunch)
	if strings.Join(order, " ") != strings.Join(want, " ") {
		t.Fatalf("granted in the order %v, want the launch order %v", order, want)
	}
}

// Our own registrations never pass the ceiling, whatever GitHub answers and however many machines ask: the built-in
// ceiling alone keeps us inside GitHub's documented cap, so a fault of ours cannot take the organization's.
func TestTheCeilingHoldsWhenGitHubRefusesNothing(t *testing.T) {
	c := builtIn(t)
	if c.CeilingRate*float64(c.WindowS) > float64(c.Cap) || c.CeilingBurst > c.Cap/10 {
		t.Fatalf("ceiling %v/s and burst %d: alone they could pass the documented %d in %d s", c.CeilingRate, c.CeilingBurst, c.Cap, c.WindowS)
	}
	at := t0.Add(6 * time.Minute)
	b := NewBudget(c, at.Add(-time.Hour)) // a full bucket
	made, d := 0, 10*time.Minute
	for ms := 0; ms < int(d.Milliseconds()); ms += 10 { // a hundred asks a second
		now := at.Add(time.Duration(ms) * time.Millisecond)
		if b.Take(now, ForJob) {
			made++
			b.Done(now, Made, 0)
		}
	}
	most := int(c.CeilingRate*d.Seconds()) + c.CeilingBurst
	if made > most || made < most-2 {
		t.Fatalf("registered %d in %s against a ceiling of %v/s and a burst of %d: want %d", made, d, c.CeilingRate, c.CeilingBurst, most)
	}
}
