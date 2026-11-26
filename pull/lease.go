package main

import "time"

// A lane that registers runners outside the listener (a test of another runner plane) takes from the same organization
// cap, and the budget only sees it afterwards, as others' registrations that starve the listener's own. It asks for a
// lease first: n registrations at batch priority, prepaid out of the budget. They count as ours in flight (pending)
// until the lane settles the lease with how many it made, when each becomes one of ours, or until the lease expires and
// they are released. A lane never gets more than what the reserve and the critical class leave.
type lease struct {
	n       int
	expires time.Time
}

// Lease is what a lane is granted.
type Lease struct {
	ID      string    `json:"id"`
	Granted int       `json:"granted"`
	Expires time.Time `json:"expires"`
}

const (
	maxLease    = 1000
	maxLeaseTTL = 10 * time.Minute
)

// Lease takes up to n registrations for a lane at batch priority, held for ttl. It grants as many as the budget allows
// now (none when the cap leaves nothing or a critical ask is waiting) and says how many.
func (b *Budget) Lease(now time.Time, n int, ttl time.Duration) Lease {
	b.mu.Lock()
	defer b.mu.Unlock()
	got := 0
	for ; got < min(max(n, 0), maxLease); got++ {
		if b.take(now, ForBatch) != Allowed {
			break
		}
	}
	if got == 0 {
		return Lease{}
	}
	if b.leases == nil {
		b.leases = map[string]lease{}
	}
	id, exp := newToken(), now.Add(min(max(ttl, time.Second), maxLeaseTTL))
	b.leases[id] = lease{got, exp}
	return Lease{ID: id, Granted: got, Expires: exp}
}

// Settle ends a lease with the number of registrations the lane made under it, each one of ours from now. It says
// whether the lease was still held: an expired lease was released already, and its registrations are the lane's own risk.
func (b *Budget) Settle(now time.Time, id string, made int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expire(now)
	l, ok := b.leases[id]
	if !ok {
		return false
	}
	delete(b.leases, id)
	b.pending -= l.n
	for i := 0; i < min(max(made, 0), l.n); i++ {
		b.made1(now)
	}
	return true
}

// expire releases the leases whose time is past.
func (b *Budget) expire(now time.Time) {
	for id, l := range b.leases {
		if !now.Before(l.expires) {
			b.pending -= l.n
			delete(b.leases, id)
		}
	}
}

// leased is what the leases hold in flight now.
func (b *Budget) leased() int {
	n := 0
	for _, l := range b.leases {
		n += l.n
	}
	return n
}
