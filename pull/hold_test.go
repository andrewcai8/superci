package main

import (
	"fmt"
	"testing"
	"time"
)

// A hold is GitHub told us to stop: nothing saves up while it lasts. Refilling before the hold check let the rate climb
// back and the bucket fill during a hold of 60 s, so the instant it ended about 90 registrations went out at once,
// into the cap GitHub had just refused.
func TestAHoldSavesNoTokensAndLetsTheRateNotClimb(t *testing.T) {
	for _, polling := range []bool{true, false} {
		for _, inflight := range []int{1, 5, 64} {
			t.Run(fmt.Sprintf("polling=%v/%d refusals in flight", polling, inflight), func(t *testing.T) {
				c := reg()
				c.CeilingRate, c.CeilingBurst = 5, 150
				at := t0.Add(time.Hour)
				b := NewBudget(c, at.Add(-time.Hour))
				for range inflight {
					b.Try(at, Asker{P: ForJob})
				}
				held := b.Snapshot(at).Pending
				if held != inflight {
					t.Fatalf("%d in flight, want %d", held, inflight)
				}
				for range inflight {
					b.Done(at, Refused, 0)
				}
				before, rateBefore := b.tokens, b.Rate()
				hold := b.throttledUntil.Sub(at)
				if inflight == 64 && hold < time.Minute {
					t.Fatalf("a hold of %s after 64 refusals, want 60 s", hold)
				}
				if polling {
					for now := at; now.Before(at.Add(hold)); now = now.Add(3 * time.Second) {
						if ok, why := b.Try(now, Asker{ID: "i-1", Launched: at, P: ForJob}); ok || why != DeniedThrottled {
							t.Fatalf("an ask %s into a hold of %s: %v %v", now.Sub(at), hold, ok, why)
						}
					}
				}
				end := b.throttledUntil.Add(10 * time.Millisecond)
				granted := 0
				for b.Take(end, ForJob) {
					granted++
					b.Done(end, Made, 0)
				}
				if float64(granted) > before+1 {
					t.Fatalf("%d registrations the instant a hold of %s ended, with %.0f tokens held when it began", granted, hold, before)
				}
				if r := b.Rate(); r > rateBefore+0.01 {
					t.Fatalf("the rate climbed from %.2f to %.2f during the hold", rateBefore, r)
				}
			})
		}
	}
}
