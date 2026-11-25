package main

import (
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
	made, refused  int
	failed         int
}

type head struct {
	at time.Time
	id int64
}

// Priority orders a registration: one a waiting job needs comes before one that only fills the hot pool.
type Priority int

const (
	ForJob Priority = iota
	ForPool
)

// Outcome is what GitHub answered to a registration.
type Outcome int

const (
	Made    Outcome = iota
	Refused         // GitHub's limit on runners configured in 5 minutes
	Failed
)

func NewBudget(c RegConfig, now time.Time) *Budget { return &Budget{c: c, refilled: now} }

func (b *Budget) SetConfig(c RegConfig) { b.mu.Lock(); b.c = c; b.mu.Unlock() }

func (b *Budget) window() time.Duration { return time.Duration(b.c.WindowS) * time.Second }

// Head records that the organization's highest runner id was at least id at a time.
func (b *Budget) Head(at time.Time, id int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
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
	if span < 60 || now.Sub(last.at) > 3*time.Minute {
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

// Take reserves one registration now, or says no. A registration is allowed when the organization's use of the
// window leaves room under Cap less half the margin, and the spare rate has saved up a token (a pool registration
// also needs half the saved-up burst left for jobs, and its own slower pace). While the organization's rate is
// unknown, the listener registers at BlindRate only. After a refusal by GitHub, no one registers for a while.
func (b *Budget) Take(now time.Time, p Priority) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Before(b.throttledUntil) {
		return false
	}
	e := b.estimate(now)
	w := b.window().Seconds()
	if e.Known && e.WindowUse+float64(b.pending)+1 > float64(b.c.Cap)*(1-b.c.Margin/2) {
		return false
	}
	if !e.Known && float64(countBetween(b.ours, now.Add(-b.window()), now)+b.pending+1) > b.c.BlindRate*w {
		return false
	}
	burst := max(3, e.SpareRate*10)
	poolRate := min(b.c.PoolRate, e.SpareRate/2)
	dt := max(0, now.Sub(b.refilled).Seconds())
	b.refilled = now
	b.tokens = min(burst, b.tokens+dt*e.SpareRate)
	b.poolTokens = min(max(1, poolRate*10), b.poolTokens+dt*poolRate)
	if b.tokens < 1 || (p == ForPool && (b.tokens < 1+burst/2 || b.poolTokens < 1)) {
		return false
	}
	if p == ForPool {
		b.poolTokens--
	}
	b.tokens--
	b.pending++
	return true
}

// Done settles a reservation with GitHub's answer. A runner GitHub made is also the organization's newest id.
func (b *Budget) Done(now time.Time, outcome Outcome, runnerID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending--
	switch outcome {
	case Made:
		cut := now.Add(-2 * b.window())
		i := 0
		for i < len(b.ours) && b.ours[i].Before(cut) {
			i++
		}
		b.ours = append(b.ours[i:], now)
		b.made++
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

// BudgetSnapshot is the budget as the admin endpoint and the metrics report it.
type BudgetSnapshot struct {
	Estimate
	Pending   int  `json:"pending"`
	Made      int  `json:"made"`
	Refused   int  `json:"refused"`
	Failed    int  `json:"failed"`
	Throttled bool `json:"throttled"`
}

func (b *Budget) Snapshot(now time.Time) BudgetSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return BudgetSnapshot{Estimate: b.estimate(now), Pending: b.pending, Made: b.made, Refused: b.refused, Failed: b.failed,
		Throttled: now.Before(b.throttledUntil)}
}

// countBetween counts the times in (from, to] of a sorted slice.
func countBetween(ts []time.Time, from, to time.Time) int {
	n := 0
	for _, t := range ts {
		if t.After(from) && !t.After(to) {
			n++
		}
	}
	return n
}
