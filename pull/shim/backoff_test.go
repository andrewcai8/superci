package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// A fakeClock stands in for the wall clock: a sleep only moves its time forward, so these tests wait on nothing.
type fakeClock struct {
	t      time.Time
	start  time.Time
	sleeps []time.Duration
	dials  []time.Time
}

func newFakeClock() *fakeClock {
	t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &fakeClock{t: t, start: t}
}

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) sleep(_ context.Context, d time.Duration) bool {
	c.sleeps = append(c.sleeps, d)
	c.t = c.t.Add(d)
	return true
}

// shimOn is a shim whose every dial is refused with err, on the fake clock, drawing draw() for its jitter.
func (c *fakeClock) shimOn(dial func(ctx context.Context, resume string) (net.Conn, *bufio.Reader, error), draw func() float64) *shim {
	return &shim{
		dial: func(ctx context.Context, resume string) (net.Conn, *bufio.Reader, error) {
			c.dials = append(c.dials, c.t)
			return dial(ctx, resume)
		},
		ob: newOutbox(maxBuffered), stdout: io.Discard, stderr: io.Discard,
		pingEvery: time.Hour, silence: time.Hour, window: resumeWindow, retry: retryMin, retryMax: retryMax,
		now: c.now, sleep: c.sleep, jitter: draw,
	}
}

func refused(context.Context, string) (net.Conn, *bufio.Reader, error) {
	return nil, nil, errors.New("connection refused")
}

func seconds(ds []time.Duration) []float64 {
	out := make([]float64, len(ds))
	for i, d := range ds {
		out[i] = d.Seconds()
	}
	return out
}

func TestBackoffCeilingDoublesFromOneSecondToFifteenAndTheDrawIsUniformBelowIt(t *testing.T) {
	cases := []struct {
		attempt int
		draw    float64
		want    time.Duration
	}{
		{0, 1, time.Second}, {1, 1, 2 * time.Second}, {2, 1, 4 * time.Second}, {3, 1, 8 * time.Second},
		{4, 1, 15 * time.Second}, {5, 1, 15 * time.Second}, {40, 1, 15 * time.Second}, {63, 1, 15 * time.Second},
		{3, 0.5, 4 * time.Second}, {3, 0, 0}, {0, 0.25, 250 * time.Millisecond},
	}
	for _, c := range cases {
		if got := backoff(c.attempt, retryMin, retryMax, c.draw); got != c.want {
			t.Errorf("backoff(attempt %d, draw %v) = %v, want %v", c.attempt, c.draw, got, c.want)
		}
	}
}

func TestAThousandShimsAtTheSameAttemptReturnSpreadOverTheWholeCeilingNotTogether(t *testing.T) {
	const shims = 1400
	buckets := make(map[time.Duration]int)
	for i := 0; i < shims; i++ {
		d := backoff(4, retryMin, retryMax, float64(i)/shims)
		buckets[d.Truncate(time.Second)]++
	}
	most := 0
	for _, n := range buckets {
		most = max(most, n)
	}
	if len(buckets) != 15 || most > shims/15+2 {
		t.Errorf("%d one-second buckets, fullest holds %d of %d: want 15 buckets of about %d", len(buckets), most, shims, shims/15)
	}
}

func TestFailedReconnectsWaitLongerEachTimeUpToFifteenSecondsAndTheJobIsLostAtTheWindow(t *testing.T) {
	c := newFakeClock()
	s := c.shimOn(refused, func() float64 { return 1 })
	if code := s.run(context.Background(), strings.NewReader("")); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	got := seconds(c.sleeps)
	want := []float64{1, 2, 4, 8, 15, 15, 15}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Fatalf("waits %v, want them to begin %v", got, want)
		}
	}
	for _, w := range got {
		if w > 15 {
			t.Errorf("a wait of %vs is past the 15 s cap", w)
		}
	}
}

func TestNoReconnectStartsAfterTheResumeWindowEnds(t *testing.T) {
	for _, window := range []time.Duration{20 * time.Second, 3 * time.Minute, 181 * time.Second} {
		c := newFakeClock()
		s := c.shimOn(refused, func() float64 { return 1 })
		s.window = window
		if code := s.run(context.Background(), strings.NewReader("")); code != 1 {
			t.Fatalf("window %v: exit %d, want 1", window, code)
		}
		end := c.start.Add(window)
		for i, at := range c.dials {
			if at.After(end) {
				t.Errorf("window %v: dial %d started %v after the window ended", window, i, at.Sub(end))
			}
		}
		if last := c.dials[len(c.dials)-1]; last.Before(end) {
			t.Errorf("window %v: the last dial was %v before the window ended: the window was not used", window, end.Sub(last))
		}
	}
}

func TestAConnectionThatSentAFrameStartsTheBackoffOver(t *testing.T) {
	c := newFakeClock()
	calls := 0
	dial := func(ctx context.Context, resume string) (net.Conn, *bufio.Reader, error) {
		calls++
		if calls != 4 {
			return refused(ctx, resume)
		}
		a, b := net.Pipe()
		go func() {
			writeFrame(b, typePing, nil)
			b.Close()
		}()
		return a, bufio.NewReader(a), nil
	}
	s := c.shimOn(dial, func() float64 { return 1 })
	s.window = 40 * time.Second
	s.run(context.Background(), strings.NewReader(""))
	got := seconds(c.sleeps)
	want := []float64{1, 2, 4, 1, 2, 4, 8}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Fatalf("waits %v, want them to begin %v: a frame on the fourth connection restarts the ladder at 1 s", got, want)
		}
	}
}

func TestACancelledContextEndsTheWaitWithoutAnotherDial(t *testing.T) {
	c := newFakeClock()
	s := c.shimOn(refused, func() float64 { return 1 })
	s.sleep = func(context.Context, time.Duration) bool { return false }
	if code := s.run(context.Background(), strings.NewReader("")); code != 1 || len(c.dials) != 1 {
		t.Errorf("exit %d after %d dials, want 1 after 1", code, len(c.dials))
	}
}
