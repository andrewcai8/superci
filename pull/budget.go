package main

import (
	"sort"
	"sync"
	"time"
)

// Budget keeps this listener's runner registrations inside what GitHub leaves the organization. GitHub refuses past
// Cap runners configured in any WindowS seconds, counting every registrant of the organization (Blacksmith's runners
// register here too), so the budget measures the organization's live rate and registers only into what is spare,
// leaving Margin of the cap to everyone else. It is shared by every scale set of the listener.
//
// Runner ids are one sequence for the organization: the advance of the highest id GitHub has issued, less our own
// registrations, is everyone else's rate. The highest id is learned from each of our registrations (exact) and, when
// we register rarely, from a listing of the organization's runners (a lower bound by seconds).
type Budget struct {
	mu             sync.Mutex
	c              RegConfig
	heads          []head      // highest runner id issued by a time, increasing in both
	ours           []time.Time // our registrations, oldest first
	pending        int
	tokens         float64
	poolTokens     float64
	refilled       time.Time
	throttledUntil time.Time
	throttleWait   time.Duration
	sampleWait     time.Duration // after a failed sample of the organization's runner ids, doubling from 1 to 10 minutes
	sampleNext     time.Time     // no sample before this
	made, refused  int
	failed         int
	denied         [denialKinds]int // asks the budget turned away, by why
	criticalWait   time.Time        // when a critical ask was last turned away: batch and pool give way to it for a moment
	leases         map[string]lease // budget leased to a lane that registers runners outside the listener
}

type head struct {
	at time.Time
	id int64
}

// Priority is the class of a registration. The budget is one allocator for the organization's cap, and a class is what
// a registration may take of it:
//
//   - ForJob is critical: a waiting job of the required check needs it. It has the reserve, and is served first.
//   - ForBatch is a job that can wait (a cache-warm cron, a load branch). It borrows only what the reserve leaves, and
//     gives way while a critical ask is waiting.
//   - ForPool only fills the hot pool: it leaves half the saved-up burst to jobs, keeps its own pace, and gives way to
//     a waiting critical ask too.
type Priority int

const (
	ForJob Priority = iota
	ForPool
	ForBatch
)

// Denial is why the budget turned a registration away. A denial is GitHub never being asked, so none of it reaches
// Refused: without its own count a starved budget (Take false at every ask) reads as a quiet one.
type Denial int

const (
	Allowed         Denial = iota
	DeniedWindow           // the organization's use of the window leaves no room under the cap
	DeniedTokens           // the spare rate has saved up no token
	DeniedThrottled        // GitHub refused a registration a moment ago
	DeniedBlind            // the organization's rate is unknown and the blind rate is spent
	DeniedHeld             // a critical ask was turned away a moment ago: batch and pool give way
	denialKinds
)

var denialNames = [denialKinds]string{"", "window", "tokens", "throttled", "blind", "held"}

func (d Denial) String() string { return denialNames[d] }

// criticalHold is how long batch and pool give way after a critical ask was turned away: two of a machine's polls.
const criticalHold = 6 * time.Second

// Outcome is what GitHub answered to a registration.
type Outcome int

const (
	Made    Outcome = iota
	Refused         // GitHub's limit on runners configured in 5 minutes
	Failed
	Unused // the grant registered nothing: its machine's claim was not its own
)

func NewBudget(c RegConfig, now time.Time) *Budget { return &Budget{c: c, refilled: now} }

func (b *Budget) SetConfig(c RegConfig) { b.mu.Lock(); b.c = c; b.mu.Unlock() }

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

// Estimate is the budget's view of the organization's registrations, each a rate a second over the last window.
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
	e := Estimate{OursRate: float64(ours) / w.Seconds(), SpareRate: b.c.BlindRate, WindowUse: float64(ours)}
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
	e.SpareRate = max(0, float64(b.c.Cap)*(1-b.c.Margin)/w.Seconds()-e.OthersRate)
	return e
}

func (b *Budget) Estimate(now time.Time) Estimate {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.estimate(now)
}

// Take reserves one registration now, or says no.
func (b *Budget) Take(now time.Time, p Priority) bool {
	ok, _ := b.Try(now, p)
	return ok
}

// Try is Take with the reason of a no. A registration is allowed when the organization's use of the window leaves room
// under Cap less half the margin, and the spare rate has saved up a token. A pool registration also leaves half the
// saved-up burst to jobs, and keeps to its own pace: PoolRate, and PoolShare of the spare rate. While the
// organization's rate is unknown, the listener registers at BlindRate only. After a refusal by GitHub, no one
// registers for a while. Every no is counted by its reason.
func (b *Budget) Try(now time.Time, p Priority) (bool, Denial) {
	b.mu.Lock()
	defer b.mu.Unlock()
	why := b.take(now, p)
	return why == Allowed, why
}

func (b *Budget) take(now time.Time, p Priority) Denial {
	b.expire(now)
	why := b.admit(now, p)
	if why != Allowed {
		b.denied[why]++
	}
	switch {
	case p == ForJob && why != Allowed:
		b.criticalWait = now
	case p == ForJob:
		b.criticalWait = time.Time{}
	}
	return why
}

func (b *Budget) admit(now time.Time, p Priority) Denial {
	if now.Before(b.throttledUntil) {
		return DeniedThrottled
	}
	if p != ForJob && now.Sub(b.criticalWait) < criticalHold {
		return DeniedHeld
	}
	e := b.estimate(now)
	w := b.window().Seconds()
	if e.Known && e.WindowUse+float64(b.pending)+1 > float64(b.c.Cap)*(1-b.c.Margin/2) {
		return DeniedWindow
	}
	if !e.Known && float64(countBetween(b.ours, now.Add(-b.window()), now)+b.pending+1) > b.c.BlindRate*w {
		return DeniedBlind
	}
	// The saved-up burst spans a launch and a margin (30 s): what the spare rate refills while a burst's machines boot
	// is theirs when they ask, rather than lost above a shallower bucket. The window guard above bounds the whole.
	burst := max(3, e.SpareRate*30)
	poolRate := min(b.c.PoolRate, e.SpareRate*b.c.PoolShare)
	dt := max(0, now.Sub(b.refilled).Seconds())
	b.refilled = now
	b.tokens = min(burst, b.tokens+dt*e.SpareRate)
	b.poolTokens = min(max(1, poolRate*10), b.poolTokens+dt*poolRate)
	// The critical class keeps a reserve of tokens: a batch registration needs it saved up besides its own, so a
	// critical ask finds a token when one arrives, not only when batch has left one.
	reserve := min(float64(b.c.Reserve), burst-1)
	if b.tokens < 1 || (p == ForBatch && b.tokens < 1+reserve) || (p == ForPool && (b.tokens < 1+burst/2 || b.poolTokens < 1)) {
		return DeniedTokens
	}
	if p == ForPool {
		b.poolTokens--
	}
	b.tokens--
	b.pending++
	return Allowed
}

// Done settles a reservation with GitHub's answer. A runner GitHub made is also the organization's newest id.
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
		b.refused++
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
	// Denied counts the asks turned away since the start, by reason (window, tokens, throttled, blind, held).
	Denied map[string]int `json:"denied"`
	Leased int            `json:"leased"` // registrations held for lanes outside the listener, in flight
}

func (b *Budget) Snapshot(now time.Time) BudgetSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	denied := make(map[string]int, denialKinds-1)
	for d := DeniedWindow; d < denialKinds; d++ {
		denied[d.String()] = b.denied[d]
	}
	return BudgetSnapshot{Estimate: b.estimate(now), Pending: b.pending, Made: b.made, Refused: b.refused, Failed: b.failed,
		Throttled: now.Before(b.throttledUntil), Denied: denied, Leased: b.leased()}
}

// countBetween counts the times in (from, to] of a sorted slice.
func countBetween(ts []time.Time, from, to time.Time) int {
	after := func(t time.Time) int { return sort.Search(len(ts), func(i int) bool { return ts[i].After(t) }) }
	return max(0, after(to)-after(from))
}
