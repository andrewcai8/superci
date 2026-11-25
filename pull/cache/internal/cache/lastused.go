package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"strings"
	"sync"
	"time"
)

// MarkRoot is where the markers of use are: a/<sha256 of the entry's object name>/<yyyymmdd>, an empty object that
// says the entry was restored on that UTC day. The hash comes first so that the hot entries' markers spread over
// the bucket's partitions instead of piling up under one day's prefix.
//
// A marker is a hint the janitor ranks by, never a fact a job relies on: a marker lost, late or written by a job
// that should not have changes what is evicted first and nothing a restore returns. That is why the daemon may
// write it with the machine's own role (nothing a job's step takes from it can harm an entry).
const MarkRoot = "a/"

const markDay = "20060102"

// MarkKey is the marker of the entry whose object name is entryKey, for the UTC day of at.
func MarkKey(entryKey string, at time.Time) string {
	return MarkRoot + markHash(entryKey) + "/" + at.UTC().Format(markDay)
}

func markHash(entryKey string) string {
	h := sha256.Sum256([]byte(entryKey))
	return hex.EncodeToString(h[:])
}

// Marks is what the markers say: for each entry (by the hash of its object name) the end of the latest UTC day it
// was restored on. A day is the finest a marker says, and a marker never says an earlier time than the truth: the
// entry counts as used until the day's end.
type Marks map[string]time.Time

// ReadMarks is the markers among objs, the listing of MarkRoot, as of now. A marker is empty and dated no later than
// the UTC day after now's (a day of slack for a machine whose clock runs ahead): the machines' role can write under
// MarkRoot, and a marker dated far ahead would keep an old entry "used" for ever and push main's newest out first.
// Anything else there is junk, returned for the janitor to delete. A forged marker of today still counts as a use of
// that day; that bounds what a job can do to one day at a time and never changes what a restore returns.
func ReadMarks(objs []Object, now time.Time) (Marks, []Object) {
	m := Marks{}
	var junk []Object
	latest := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	for _, o := range objs {
		rest, ok := strings.CutPrefix(o.Key, MarkRoot)
		hash, day, ok2 := strings.Cut(rest, "/")
		at, err := time.Parse(markDay, day)
		if !ok || !ok2 || len(hash) != sha256.Size*2 || err != nil || o.Size != 0 || at.After(latest) {
			junk = append(junk, o)
			continue
		}
		if end := at.Add(24 * time.Hour); end.After(m[hash]) {
			m[hash] = end
		}
	}
	return m, junk
}

// LastUsed is when the entry was last restored, or saved if that is later.
func (m Marks) LastUsed(o Object) time.Time {
	if t := m[markHash(o.Key)]; t.After(o.SaveTime()) {
		return t
	}
	return o.SaveTime()
}

// MarkStore is the bucket as the daemon marks with: the machine's own role.
type MarkStore interface {
	Head(ctx context.Context, key string) (Object, bool, error)
	// CreateMark writes the empty marker unless it exists (nil when it did, too).
	CreateMark(ctx context.Context, key string) error
}

// Uses records which entries a restore returned, one marker per entry per UTC day however many jobs and machines
// restore it: a machine asks once per entry per day, and asks the bucket (a HEAD, a tenth of a write's price)
// before it writes, so the day's markers of a hot entry are about one write and a few reads whatever its hits.
// Marking happens beside the restore and never delays or fails it.
type Uses struct {
	Store MarkStore
	Now   func() time.Time
	Log   *log.Logger

	mu   sync.Mutex
	day  string
	seen map[string]struct{}
	wg   sync.WaitGroup
}

func (u *Uses) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

// Hit notes that the entry with this object name was restored now. Repeating it the same day does nothing.
func (u *Uses) Hit(entryKey string) {
	at := u.now()
	mk := MarkKey(entryKey, at)
	u.mu.Lock()
	if d := at.UTC().Format(markDay); d != u.day || u.seen == nil {
		u.day, u.seen = d, map[string]struct{}{}
	}
	if _, ok := u.seen[mk]; ok {
		u.mu.Unlock()
		return
	}
	u.seen[mk] = struct{}{}
	u.mu.Unlock()
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := u.mark(ctx, mk); err != nil {
			u.mu.Lock()
			delete(u.seen, mk)
			u.mu.Unlock()
			if u.Log != nil {
				u.Log.Printf("mark %s: not written, the next restore of the entry tries again: %v", mk, err)
			}
		}
	}()
}

func (u *Uses) mark(ctx context.Context, mk string) error {
	if _, there, err := u.Store.Head(ctx, mk); err != nil || there {
		return err
	}
	return u.Store.CreateMark(ctx, mk)
}

// Wait returns when the markers begun so far are written or given up.
func (u *Uses) Wait() { u.wg.Wait() }
