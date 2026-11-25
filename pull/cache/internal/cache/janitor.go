package cache

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
)

// Why an entry is deleted.
type Reason string

const (
	// Idle: nothing restored it for longer than the policy's MaxIdle. Deleted whatever the size.
	Idle Reason = "idle"
	// Superseded: a newer entry of its family was saved after the last time it was restored, so the restore key
	// that finds the family returns the newer one. First to go when the bucket is over its cap.
	Superseded Reason = "superseded"
	// Cold: the least recently restored of what is left, a pull request's entries before the default branch's.
	Cold Reason = "least recently used"
)

// Victim is an entry the janitor means to delete, as it was listed (the delete is of this version only).
type Victim struct {
	Object
	Reason   Reason
	LastUsed time.Time
}

// Policy is what the janitor holds the bucket to.
type Policy struct {
	// CapBytes is the most the entries may hold.
	CapBytes int64
	// MaxIdle is how long an entry may go without a restore, whatever the size; zero is no limit.
	MaxIdle time.Duration
}

// DefaultMaxIdle is the longest an entry nobody restores is kept, by use and not by save age: an entry that is read
// stays however old it is.
const DefaultMaxIdle = 30 * 24 * time.Hour

// Plan names the entries to delete, in the order to delete them. It is a function of the listing, the markers, the
// policy and the clock alone, so running it twice over one listing, or in two places at once, gives one answer.
//
// Idle entries go whatever the size. If what is left is still over the cap, entries go in this order until it is
// not:
//
//  1. Superseded entries: those a newer entry of the same (class, repository, scope, version, family) replaced and
//     nobody restored since that entry was saved. Old lockfile hashes, old git seeds. The entry kept is the newest
//     by save time (the object's LastModified, which is when the save ended; the mirror's copy to the other region
//     is a few seconds after it, so both regions agree on which is newest). An older entry that was restored after
//     the newest was saved is not superseded: a family too wide for its name (two live caches that share one) loses
//     nothing it still uses.
//  2. Other scopes' entries (pull requests, queue batches), then the default branch's: a pull request's entry is
//     saved most and read least, and a default-branch entry is what every job falls back to.
//
// Within each, the least recently restored first, then the oldest saved. An entry never restored counts from its
// save.
func Plan(objs []Object, marks Marks, p Policy, now time.Time) []Victim {
	type item struct {
		Victim
		tier int
	}
	newest := map[string]Object{}
	family := func(o Object) string {
		e, ok := ParseEntry(o)
		if !ok {
			return o.Key
		}
		return strings.Join([]string{e.Class, e.Repo, e.Scope, e.Version, Family(e.Name)}, "/")
	}
	newer := func(a, b Object) bool { // a was saved after b
		return cmp.Or(a.Modified.Compare(b.Modified), cmp.Compare(a.Key, b.Key)) > 0
	}
	for _, o := range objs {
		if f := family(o); newest[f].Key == "" || newer(o, newest[f]) {
			newest[f] = o
		}
	}

	var out []Victim
	var kept []item
	var total int64
	for _, o := range objs {
		used := marks.LastUsed(o)
		if p.MaxIdle > 0 && now.Sub(used) > p.MaxIdle {
			out = append(out, Victim{Object: o, Reason: Idle, LastUsed: used})
			continue
		}
		it := item{Victim: Victim{Object: o, Reason: Cold, LastUsed: used}, tier: 1}
		if e, ok := ParseEntry(o); ok && e.Class == "d" || !ok && strings.HasPrefix(o.Key, EntryRoot+"d/") {
			it.tier = 2
		}
		if top := newest[family(o)]; top.Key != o.Key && !used.After(top.Modified) {
			it.Reason, it.tier = Superseded, 0
		}
		kept = append(kept, it)
		total += o.Size
	}
	slices.SortFunc(out, func(a, b Victim) int { return cmp.Or(a.LastUsed.Compare(b.LastUsed), cmp.Compare(a.Key, b.Key)) })
	if total > p.CapBytes {
		slices.SortFunc(kept, func(a, b item) int {
			return cmp.Or(cmp.Compare(a.tier, b.tier), a.LastUsed.Compare(b.LastUsed), a.Modified.Compare(b.Modified), cmp.Compare(a.Key, b.Key))
		})
		for _, it := range kept {
			if total <= p.CapBytes {
				break
			}
			out = append(out, it.Victim)
			total -= it.Size
		}
	}
	return out
}

// Removal is what a delete of one listed version did.
type Removal int

const (
	// Deleted: the version listed was deleted.
	Deleted Removal = iota
	// Gone: there was no object by that name (another janitor deleted it first).
	Gone
	// Changed: an object by that name is no longer the version listed (a job saved it again after the janitor
	// listed it), and it was left alone.
	Changed
)

// Pruner is the bucket as the janitor's own role reaches it: it lists entries and markers, and deletes entries, and
// nothing else can delete one.
type Pruner interface {
	List(ctx context.Context, prefix string, max int) ([]Object, error)
	// Delete deletes each object only if it is still the version it was listed as (its ETag), and says what it did
	// to each, in order.
	Delete(ctx context.Context, objs []Object) ([]Removal, error)
}

// Report is what a janitor run found and did.
type Report struct {
	Entries, HeldMB int   // entries listed, and what they held
	Planned         int   // entries the plan names
	Deleted         int   // of those, deleted
	Gone, Changed   int   // of those, already gone, or saved again since the listing
	Freed           int64 // bytes of the entries deleted
	Dry             bool
}

func (r Report) String() string {
	if r.Dry {
		return fmt.Sprintf("dry run, nothing deleted: %d entries, %d MB held, %d would go", r.Entries, r.HeldMB, r.Planned)
	}
	return fmt.Sprintf("%d entries, %d MB held: %d planned, %d deleted (%d MB freed), %d already gone, %d saved again since the listing", r.Entries, r.HeldMB, r.Planned, r.Deleted, r.Freed>>20, r.Gone, r.Changed)
}

// Janitor holds the bucket to the policy. Any number of janitors may run at once, or one again after a failed run:
// the plan is a function of the listing, and a delete takes only the version listed, so what one run did another
// finds gone or changed and leaves. With dry, it only says what it would delete, one line each. (The bucket's
// lifecycle rule expires the markers, which are hints and cost nothing to keep a few weeks.)
func Janitor(ctx context.Context, st Pruner, p Policy, now time.Time, dry bool, lg *log.Logger) (Report, error) {
	objs, err := st.List(ctx, EntryRoot, 5_000_000)
	if err != nil {
		return Report{}, err
	}
	mobjs, err := st.List(ctx, MarkRoot, 5_000_000)
	if err != nil {
		return Report{}, err
	}
	plan := Plan(objs, ReadMarks(mobjs), p, now)
	rep := Report{Entries: len(objs), HeldMB: int(sum(objs) >> 20), Planned: len(plan), Dry: dry}
	lg.Printf("janitor: %d entries, %d MB held, %d markers, cap %d MB, idle limit %s: %d to delete", rep.Entries, rep.HeldMB, len(mobjs), p.CapBytes>>20, p.MaxIdle, len(plan))
	if dry {
		for _, v := range plan {
			lg.Printf("janitor: would delete %s (%d MB, saved %s, last used %s): %s", v.Key, v.Size>>20, v.Modified.UTC().Format(time.RFC3339), v.LastUsed.UTC().Format(time.RFC3339), v.Reason)
		}
		return rep, nil
	}
	for len(plan) > 0 {
		n := min(len(plan), 1000)
		batch := plan[:n]
		objs := make([]Object, n)
		for i, v := range batch {
			objs[i] = v.Object
		}
		got, err := st.Delete(ctx, objs)
		for i, r := range got {
			switch r {
			case Deleted:
				rep.Deleted++
				rep.Freed += batch[i].Size
				lg.Printf("janitor: deleted %s (%d MB): %s", batch[i].Key, batch[i].Size>>20, batch[i].Reason)
			case Gone:
				rep.Gone++
			case Changed:
				rep.Changed++
				lg.Printf("janitor: left %s, saved again since the listing", batch[i].Key)
			}
		}
		if err != nil {
			return rep, err
		}
		plan = plan[n:]
	}
	return rep, nil
}

func sum(objs []Object) (n int64) {
	for _, o := range objs {
		n += o.Size
	}
	return n
}
