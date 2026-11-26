package main

import (
	"sort"
	"sync"
	"time"
)

// Budget admits this listener's runner registrations. GitHub's answer is the only limit it obeys: a registration
// GitHub refuses (its cap on runners configured in 5 minutes) halves the rate the budget admits at and holds every ask
// for a while, and the rate climbs back while GitHub accepts. Below that rate the budget never makes a machine wait.
// The rate never passes a ceiling of our own (CeilingRate a second, CeilingBurst at once), so a fault of ours cannot
// flood the organization's cap. When tokens are short, machines are served in order: critical before batch before
// pool, and in each class the machine launched first.
//
// The organization's rate is measured as a gauge only. Runner ids are one sequence for the organization: the advance
// of the highest id GitHub has issued, less our own registrations, is everyone else's rate. The highest id is learned
// from each of our registrations (exact) and, when we register rarely, from a probe or a listing of the organization's
// runners. It decides nothing: the canary of Nov 26 waited 12 to 19 minutes on this estimate while GitHub refused no
// one, and the estimate read above GitHub's documented cap with no refusal on 14 minutes of Nov 25-26.
type Budget struct {
	mu             sync.Mutex
	c              RegConfig
	heads          []head      // highest runner id issued by a time, increasing in both
	ours           []time.Time // our registrations, oldest first
	pending        int
	rate           float64 // the admitted rate a second: halved on each refusal, climbing back to CeilingRate
	tokens         float64
	poolTokens     float64
	refilled       time.Time
	throttledUntil time.Time
	throttleWait   time.Duration
	sampleWait     time.Duration // after a failed sample of the organization's runner ids, doubling from 1 to 10 minutes
	sampleNext     time.Time     // no sample before this
	made, refused  int
	failed         int
	denied         [denialKinds]int  // asks the budget turned away, by why
	queue          map[string]waiter // machines turned away, so the next tokens are theirs in order
	leases         map[string]lease  // budget leased to a lane that registers runners outside the listener
}

// Asker is who asks for a registration: a machine (its id and launch time) in a class. An asker with no id (a probe,
// a lease) has no place in the queue and comes after every machine that waits in its class.
type Asker struct {
	ID       string
	Launched time.Time
	P        Priority
}

type waiter struct {
	p        Priority
	launched time.Time
	asked    time.Time
}

// queueHold is how long a machine keeps its place after its last ask: two of its polls. One that stopped asking (it
// ended, or got its runner elsewhere) holds nothing after it.
const queueHold = 6 * time.Second

type head struct {
	at time.Time
	id int64
}

// servedBefore says whether waiter id is served ahead of a: a higher class first, then the earlier launch.
func servedBefore(id string, w waiter, a Asker) bool {
	if r, o := rank(w.p), rank(a.P); r != o {
		return r < o
	}
	if a.ID == "" {
		return true
	}
	if !w.launched.Equal(a.Launched) {
		return w.launched.Before(a.Launched)
	}
	return id < a.ID
}

// Priority is the class of a registration. The budget is one allocator for the organization's cap, and a class is what
// a registration may take of it:
//
//   - ForJob is critical: a waiting job of the required check needs it. It has the reserve, and is served first.
//   - ForBatch is a job that can wait (a cache-warm cron, a load branch). It borrows only what the reserve leaves, and
//     comes after every critical machine that waits.
//   - ForPool only fills the hot pool: it leaves half the saved-up burst to jobs, keeps its own pace, and comes last.
type Priority int

const (
	ForJob Priority = iota
	ForPool
	ForBatch
)

func rank(p Priority) int { return [...]int{ForJob: 0, ForBatch: 1, ForPool: 2}[p] }

// Denial is why the budget turned a registration away. A denial is GitHub never being asked, so none of it reaches
// Refused: without its own count a starved budget reads as a quiet one.
type Denial int

const (
	Allowed         Denial = iota
	DeniedTokens           // the admitted rate has saved up no token: our ceiling, or the rate a refusal halved
	DeniedThrottled        // GitHub refused a registration a moment ago
	DeniedQueued           // a token is there, but a machine ahead in the queue waits for it
	denialKinds
)

var denialNames = [denialKinds]string{"", "tokens", "throttled", "queued"}

func (d Denial) String() string { return denialNames[d] }

// Outcome is what GitHub answered to a registration.
type Outcome int

const (
	Made    Outcome = iota
	Refused         // GitHub's limit on runners configured in 5 minutes
	Failed
	Unused // the grant registered nothing: its machine's claim was not its own
)

func NewBudget(c RegConfig, now time.Time) *Budget {
	return &Budget{c: c, refilled: now, rate: c.CeilingRate}
}

func (b *Budget) SetConfig(c RegConfig) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.c = c
	b.rate = min(b.rate, c.CeilingRate)
}

func (b *Budget) window() time.Duration { return time.Duration(b.c.WindowS) * time.Second }

// Head records that the organization's highest runner id was at least id at a time.
func (b *Budget) Head(at time.Time, id int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sampleWait, b.sampleNext = 0, time.Time{}
	b.head(at, id)
}

func (b *Budget) head(at time.Time, id int64) {
	if n := len(b.heads); n > 0 {
		last := b.heads[n-1]
		if id <= last.id {
			return
		}
		if !at.After(last.at) {
			at = last.at.Add(time.Millisecond)
		}
	}
	b.heads = append(b.heads, head{at, id})
	cut := at.Add(-2 * b.window())
	i := 0
	for i < len(b.heads)-2 && b.heads[i+1].at.Before(cut) {
		i++
	}
	b.heads = b.heads[i:]
}

// LastHead is when the highest runner id was last learned.
func (b *Budget) LastHead() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.heads) == 0 {
		return time.Time{}
	}
	return b.heads[len(b.heads)-1].at
}

// SampleDue says whether to learn the organization's highest runner id from a listing now: at once after a start, a
// minute later (the rate needs two heads 60 s apart, so the budget is blind for about a minute, not two), and then
// whenever neither a listing nor our own registrations have told it for 100 s.
func (b *Budget) SampleDue(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Before(b.sampleNext) {
		return false
	}
	n := len(b.heads)
	if n == 0 {
		return true
	}
	wait := 100 * time.Second
	if !b.estimate(now).Known {
		wait = 61 * time.Second
	}
	return now.Sub(b.heads[n-1].at) >= wait
}

// SampleFailed holds off the next sample after one failed: a minute, doubling to ten while they keep failing. A sample
// reads every page of the organization's runners (44 at 4,300 runners), and the App's rate limit is shared with the
// push plane and every other listener, so a failure retried every tick would spend it.
func (b *Budget) SampleFailed(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sampleWait = min(10*time.Minute, max(time.Minute, 2*b.sampleWait))
	b.sampleNext = now.Add(b.sampleWait)
}

// Estimate is the gauge of the organization's registrations, each a rate a second over the last window. SpareRate is
// what GitHub's documented cap leaves past everyone's rate. None of it admits or refuses a registration.
type Estimate struct {
	Known      bool    `json:"known"`
	OrgRate    float64 `json:"org_rate"`
	OthersRate float64 `json:"others_rate"`
	OursRate   float64 `json:"ours_rate"`
	WindowUse  float64 `json:"window_use"`
	SpareRate  float64 `json:"spare_rate"`
}

func (b *Budget) estimate(now time.Time) Estimate {
	w := b.window()
	ours := countBetween(b.ours, now.Add(-w), now)
	e := Estimate{OursRate: float64(ours) / w.Seconds(), WindowUse: float64(ours)}
	n := len(b.heads)
	if n < 2 {
		return e
	}
	last := b.heads[n-1]
	from := last.at.Add(-w)
	at, id := b.heads[0].at, float64(b.heads[0].id)
	for i := 0; i+1 < n; i++ {
		a, c := b.heads[i], b.heads[i+1]
		if !a.at.After(from) && c.at.After(from) {
			f := from.Sub(a.at).Seconds() / c.at.Sub(a.at).Seconds()
			at, id = from, float64(a.id)+f*float64(c.id-a.id)
			break
		}
	}
	span := last.at.Sub(at).Seconds()
	if span < 60 || now.Sub(last.at) > 5*time.Minute {
		return e
	}
	advance := float64(last.id) - id
	e.Known = true
	e.OrgRate = advance / span
	e.OthersRate = max(0, (advance-float64(countBetween(b.ours, at, last.at)))/span)
	e.WindowUse = e.OthersRate*w.Seconds() + float64(ours)
	e.SpareRate = max(0, float64(b.c.Cap)/w.Seconds()-e.OrgRate)
	return e
}

func (b *Budget) Estimate(now time.Time) Estimate {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.estimate(now)
}

// Take reserves one registration now for an asker with no place in the queue, or says no.
func (b *Budget) Take(now time.Time, p Priority) bool {
	ok, _ := b.Try(now, Asker{P: p})
	return ok
}

// Try reserves one registration now, or says why not. A registration is allowed when the admitted rate has saved up a
// token for it and for every machine served before it that still waits. A batch registration also leaves the critical
// reserve, and a pool registration half the saved-up burst, at its own pace (PoolRate, and PoolShare of the rate). A
// machine turned away keeps its place while it keeps asking. Every no is counted by its reason.
func (b *Budget) Try(now time.Time, a Asker) (bool, Denial) {
	b.mu.Lock()
	defer b.mu.Unlock()
	why := b.take(now, a)
	return why == Allowed, why
}

func (b *Budget) take(now time.Time, a Asker) Denial {
	b.expire(now)
	why := b.admit(now, a)
	switch {
	case why != Allowed:
		b.denied[why]++
		if a.ID != "" {
			if b.queue == nil {
				b.queue = map[string]waiter{}
			}
			b.queue[a.ID] = waiter{a.P, a.Launched, now}
		}
	case a.ID != "":
		delete(b.queue, a.ID)
	}
	return why
}

func (b *Budget) admit(now time.Time, a Asker) Denial {
	b.refill(now)
	if now.Before(b.throttledUntil) {
		return DeniedThrottled
	}
	burst := b.burst()
	need := 1.0
	switch a.P {
	case ForBatch:
		need += min(float64(b.c.Reserve), burst-1)
	case ForPool:
		need = max(need, 1+burst/2)
	}
	if b.tokens < need || a.P == ForPool && b.poolTokens < 1 {
		return DeniedTokens
	}
	if b.tokens < need+float64(b.ahead(now, a)) {
		return DeniedQueued
	}
	if a.P == ForPool {
		b.poolTokens--
	}
	b.tokens--
	b.pending++
	return Allowed
}

// burst is how many registrations the budget admits at once: its ceiling's burst, never below one.
func (b *Budget) burst() float64 { return max(1, float64(b.c.CeilingBurst)) }

// refill adds the tokens the admitted rate saved since the last ask, and climbs the rate back toward the ceiling: from
// nothing to the ceiling in RecoverS seconds while GitHub refuses nothing. A hold saves nothing and climbs nothing:
// time counts from the end of the hold, whether or not anyone asked during it.
func (b *Budget) refill(now time.Time) {
	from := b.refilled
	if b.throttledUntil.After(from) {
		from = b.throttledUntil
	}
	dt := max(0, now.Sub(from).Seconds())
	b.refilled = now
	if b.c.RecoverS > 0 {
		b.rate = min(b.c.CeilingRate, b.rate+b.c.CeilingRate*dt/float64(b.c.RecoverS))
	}
	b.tokens = min(b.burst(), b.tokens+dt*b.rate)
	poolRate := min(b.c.PoolRate, b.rate*b.c.PoolShare)
	b.poolTokens = min(max(1, poolRate*10), b.poolTokens+dt*poolRate)
}

// ahead counts the machines served before a that still wait, and forgets the ones that stopped asking.
func (b *Budget) ahead(now time.Time, a Asker) int {
	b.waiting(now)
	n := 0
	for id, w := range b.queue {
		if id != a.ID && servedBefore(id, w, a) {
			n++
		}
	}
	return n
}

// waiting counts the machines in the queue that still ask.
func (b *Budget) waiting(now time.Time) int {
	for id, w := range b.queue {
		if now.Sub(w.asked) > queueHold {
			delete(b.queue, id)
		}
	}
	return len(b.queue)
}

// Rate is the rate a second the budget admits at now.
func (b *Budget) Rate() float64 { b.mu.Lock(); defer b.mu.Unlock(); return b.rate }

// minRate is the least a run of refusals halves the admitted rate to, so the budget still asks GitHub now and then.
const minRate = 0.1

// Done settles a reservation with GitHub's answer. A runner GitHub made is also the organization's newest id. A
// refusal is the one signal the budget backs off on: the rate halves, the saved tokens go, and every ask waits a while,
// longer each refusal in a row.
func (b *Budget) Done(now time.Time, outcome Outcome, runnerID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending--
	switch outcome {
	case Made:
		b.made1(now)
		b.throttleWait = 0
		if runnerID > 0 {
			b.head(now, runnerID)
		}
	case Refused:
		b.refill(now)
		b.refused++
		b.rate = max(min(minRate, b.c.CeilingRate), b.rate/2)
		b.tokens = 0
		b.throttleWait = min(time.Minute, max(5*time.Second, 2*b.throttleWait))
		b.throttledUntil = now.Add(b.throttleWait)
	case Failed:
		b.failed++
	}
}

// made1 records one registration of ours GitHub made.
func (b *Budget) made1(now time.Time) {
	cut := now.Add(-2 * b.window())
	i := 0
	for i < len(b.ours) && b.ours[i].Before(cut) {
		i++
	}
	// Kept sorted for countBetween: registrations that ran at once report their times in any order.
	b.ours = append(b.ours[i:], now)
	for j := len(b.ours) - 1; j > 0 && b.ours[j].Before(b.ours[j-1]); j-- {
		b.ours[j], b.ours[j-1] = b.ours[j-1], b.ours[j]
	}
	b.made++
}

// Lost counts a registration GitHub made whose config was withheld (its issued tag could not be written): an error
// like a failed registration, though it took the organization's registration.
func (b *Budget) Lost() { b.mu.Lock(); b.failed++; b.mu.Unlock() }

// BudgetSnapshot is the budget as the admin endpoint and the metrics report it.
type BudgetSnapshot struct {
	Estimate
	Pending   int  `json:"pending"`
	Made      int  `json:"made"`
	Refused   int  `json:"refused"`
	Failed    int  `json:"failed"`
	Throttled bool `json:"throttled"`
	// Denied counts the asks turned away since the start, by reason (tokens, throttled, queued).
	Denied map[string]int `json:"denied"`
	Rate   float64        `json:"rate"`   // the admitted rate a second
	Queued int            `json:"queued"` // machines that wait in the queue now
	Leased int            `json:"leased"` // registrations held for lanes outside the listener, in flight
}

func (b *Budget) Snapshot(now time.Time) BudgetSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	denied := make(map[string]int, denialKinds-1)
	for d := DeniedTokens; d < denialKinds; d++ {
		denied[d.String()] = b.denied[d]
	}
	return BudgetSnapshot{Estimate: b.estimate(now), Pending: b.pending, Made: b.made, Refused: b.refused, Failed: b.failed,
		Throttled: now.Before(b.throttledUntil), Denied: denied, Leased: b.leased(), Rate: b.rate, Queued: b.waiting(now)}
}

// countBetween counts the times in (from, to] of a sorted slice.
func countBetween(ts []time.Time, from, to time.Time) int {
	after := func(t time.Time) int { return sort.Search(len(ts), func(i int) bool { return ts[i].After(t) }) }
	return max(0, after(to)-after(from))
}
