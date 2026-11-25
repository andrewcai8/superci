package cache_test

import (
	"context"
	"errors"
	"log"
	"sync"
	"testing"
	"time"

	"superci-pull-cache/internal/cache"
	"superci-pull-cache/internal/cachetest"
)

var usesDay = time.Date(2026, 11, 25, 9, 30, 0, 0, time.UTC)

// counted is a bucket that counts the marker writes it is asked for.
type counted struct {
	*cachetest.Mem
	mu     sync.Mutex
	writes int
	heads  int
	fail   error
}

func (c *counted) Head(ctx context.Context, key string) (cache.Object, bool, error) {
	c.mu.Lock()
	c.heads++
	c.mu.Unlock()
	return c.Mem.Head(ctx, key)
}

func (c *counted) CreateMark(ctx context.Context, key string) error {
	c.mu.Lock()
	c.writes++
	err := c.fail
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return c.Mem.CreateMark(ctx, key)
}

func TestARestoreMarksTheEntryOncePerDayHoweverOftenItIsHit(t *testing.T) {
	b := &counted{Mem: cachetest.NewMem()}
	now := usesDay
	u := &cache.Uses{Store: b, Now: func() time.Time { return now }}
	for range 500 {
		u.Hit("v1/d/1/main/v1/hot")
	}
	u.Hit("v1/d/1/main/v1/other")
	u.Wait()
	if b.writes != 2 || len(b.Keys("a/")) != 2 {
		t.Fatalf("%d marker writes for 501 hits of two entries: %v", b.writes, b.Keys("a/"))
	}

	// Another machine hits the same entry the same day: it asks the bucket, finds the marker, and writes nothing.
	other := &cache.Uses{Store: b, Now: func() time.Time { return now }}
	other.Hit("v1/d/1/main/v1/hot")
	other.Wait()
	if b.writes != 2 {
		t.Fatalf("a second machine wrote again: %d writes", b.writes)
	}

	// The next day is a new marker; the day before's is kept for the janitor to read.
	now = now.Add(24 * time.Hour)
	u.Hit("v1/d/1/main/v1/hot")
	u.Wait()
	if b.writes != 3 || len(b.Keys("a/")) != 3 {
		t.Fatalf("after a day: %d writes, markers %v", b.writes, b.Keys("a/"))
	}
}

func TestAMarkIsWhatTheJanitorReadsBack(t *testing.T) {
	b := &counted{Mem: cachetest.NewMem()}
	u := &cache.Uses{Store: b, Now: func() time.Time { return usesDay }}
	o := cache.Object{Key: "v1/d/1/main/v1/hot", Modified: usesDay.Add(-72 * time.Hour)}
	u.Hit(o.Key)
	u.Wait()
	got := marksAt(b.Mem, usesDay).LastUsed(o)
	if want := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("last used %s, want the end of the day %s", got, want)
	}
	if other := marksAt(b.Mem, usesDay).LastUsed(cache.Object{Key: "v1/d/1/main/v1/cold", Modified: usesDay.Add(-time.Hour)}); !other.Equal(usesDay.Add(-time.Hour)) {
		t.Fatalf("an entry with no marker was last used when it was saved, not %s", other)
	}
}

func TestAMarkThatFailsIsTriedAgainAndNeverMattersToTheRestore(t *testing.T) {
	var out tape
	b := &counted{Mem: cachetest.NewMem(), fail: errors.New("AccessDenied")}
	u := &cache.Uses{Store: b, Now: func() time.Time { return usesDay }, Log: log.New(&out, "", 0)}
	u.Hit("v1/d/1/main/v1/hot")
	u.Wait()
	if len(b.Keys("a/")) != 0 || out.b.Len() == 0 {
		t.Fatalf("markers %v, log %q", b.Keys("a/"), out.b.String())
	}
	b.mu.Lock()
	b.fail = nil
	b.mu.Unlock()
	u.Hit("v1/d/1/main/v1/hot")
	u.Wait()
	if len(b.Keys("a/")) != 1 {
		t.Fatalf("the next hit does not retry: %v", b.Keys("a/"))
	}
}

func TestAHitRestoreMarksTheEntryItReturnedAndAMissMarksNothing(t *testing.T) {
	r := newRig(t)
	main := r.token("77", "refs/heads/main:rw")
	pr := r.token("77", "refs/pull/7/merge:rw", "refs/heads/main:r")
	if !r.save(main, "Linux-go-aaaa1111", [][]byte{[]byte("main")}, true) {
		t.Fatal("not saved")
	}
	if got := r.restore(pr, "Linux-go-zzzz9999", "Linux-go-"); !got.hit {
		t.Fatalf("restore = %+v", got)
	}
	r.uses.Wait()
	entry := "v1/d/77/refs%2Fheads%2Fmain/" + version + "/Linux-go-aaaa1111"
	if keys := r.mem.Keys("a/"); len(keys) != 1 || keys[0] != cache.MarkKey(entry, usesDay) {
		t.Fatalf("markers %v, want the marker of main's entry the pull request restored", keys)
	}
	if got := r.restore(pr, "Linux-rust-1", "Linux-zig-"); got.hit {
		t.Fatal("a miss")
	}
	r.uses.Wait()
	if keys := r.mem.Keys("a/"); len(keys) != 1 {
		t.Fatalf("a miss left markers %v", keys)
	}
}

func marksAt(m *cachetest.Mem, now time.Time) cache.Marks {
	marks, _ := cache.ReadMarks(listing(m, cache.MarkRoot), now)
	return marks
}
