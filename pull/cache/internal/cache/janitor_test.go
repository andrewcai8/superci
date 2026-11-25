package cache_test

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"superci-pull-cache/internal/cache"
	"superci-pull-cache/internal/cachetest"
)

var (
	t0  = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	gib = int64(1 << 30)
)

// entry is the object a save of name in scope leaves, saved at the given time.
func entry(scope, name string, size int64, saved time.Time) cache.Object {
	return cache.Object{Key: cache.ObjectKey("1", cache.Scope{Name: scope}, "v1", name, "main"), Size: size, Modified: saved, ETag: fmt.Sprintf(`"%s-%d"`, name, saved.Unix())}
}

const (
	main = "refs/heads/main"
	pr7  = "refs/pull/7/merge"
	pr8  = "refs/pull/8/merge"
)

func ago(d time.Duration) time.Time { return t0.Add(-d) }

const (
	hour = time.Hour
	day  = 24 * time.Hour
)

// marked is the markers of the entries used on the given days (each a time of that day).
func marked(uses map[cache.Object][]time.Time) cache.Marks {
	var objs []cache.Object
	for o, days := range uses {
		for _, d := range days {
			objs = append(objs, cache.Object{Key: cache.MarkKey(o.Key, d)})
		}
	}
	marks, _ := cache.ReadMarks(objs, t0)
	return marks
}

func names(plan []cache.Victim) []string {
	var out []string
	for _, v := range plan {
		e, _ := cache.ParseEntry(v.Object)
		out = append(out, e.Name)
	}
	return out
}

func reasons(plan []cache.Victim) []cache.Reason {
	var out []cache.Reason
	for _, v := range plan {
		out = append(out, v.Reason)
	}
	return out
}

// The names are the ones in the production buckets on Nov 25 (the hashes shortened where a test does not care).
func TestFamilyIsTheKeyWithoutItsTrailingHashes(t *testing.T) {
	h64 := strings.Repeat("ab12", 16)
	for key, want := range map[string]string{
		"git-seed-v1-Linux-X64-45662624410":                                  "git-seed-v1-Linux-X64",
		"acid-vp-deps-v1-Linux-X64-2503440cae4c07a2-" + h64:                  "acid-vp-deps-v1-Linux-X64",
		"acid-vp-tool-v1-Linux-X64-" + h64:                                   "acid-vp-tool-v1-Linux-X64",
		"bun-modules-v1-Linux-X64-bun1.4.3-aura-hono-api-scripts-on-" + h64:  "bun-modules-v1-Linux-X64-bun1.4.3-aura-hono-api-scripts-on",
		"bun-modules-v1-Linux-X64-bun1.4.3-aura-hono-api-scripts-off-" + h64: "bun-modules-v1-Linux-X64-bun1.4.3-aura-hono-api-scripts-off",
		"bun-Linux-mind-tool-" + h64:                                         "bun-Linux-mind-tool",
		"bun-+NUKu9E/U4apAe78eSCZvQ/GSdc=":                                   "bun",
		"setup-go-Linux-x64-ubuntu24-go-1.26.11-" + h64:                      "setup-go-Linux-x64-ubuntu24-go-1.26.11",
		"mind-python-v1-Linux-X64-py3.13.18-uv0.13.9-" + h64:                 "mind-python-v1-Linux-X64-py3.13.18-uv0.13.9",
		"vite-plus-Linux-x64-pnpm-" + h64:                                    "vite-plus-Linux-x64-pnpm",
		"patchcli-linux-X64-" + h64:                                          "patchcli-linux-X64",
		"uv-toolcache-v1-Linux-X64-uv0.13.9":                                 "uv-toolcache-v1-Linux-X64-uv0.13.9",
		"apt-Linux-X64-playwright-chromium-2026-W48":                         "apt-Linux-X64-playwright-chromium-2026-W48",
		"playwright-chromium-shell-Linux-X64-" + h64:                         "playwright-chromium-shell-Linux-X64",
		"Linux-cache-20261125":                                               "Linux-cache",
		"deadbeefdeadbeef":                                                   "deadbeefdeadbeef",
		"a-1b2c3d4e5f6a-" + h64:                                              "a",
		"configurationconfiguration-" + h64 + "-notahash":                    "configurationconfiguration-" + h64 + "-notahash",
	} {
		if got := cache.Family(key); got != want {
			t.Errorf("Family(%q) = %q, want %q", key, got, want)
		}
	}
	// Two saves of one cache are of one family and two caches are not: what the restore keys of the real
	// workflows already rely on.
	a, b := cache.Family("git-seed-v1-Linux-X64-45662624410"), cache.Family("git-seed-v1-Linux-X64-45664552088")
	if a != b {
		t.Errorf("two git seeds: %q and %q", a, b)
	}
	if cache.Family("bun-Linux-mind-"+h64) == cache.Family("bun-Linux-mind-tool-"+h64) {
		t.Error("mind and mind-tool share a family")
	}
	if cache.Family("bun-modules-v1-Linux-X64-bun1.4.3-aura-hono-api-scripts-on-"+h64) == cache.Family("bun-modules-v1-Linux-X64-bun1.4.3-aura-hono-api-scripts-off-"+h64) {
		t.Error("scripts-on and scripts-off share a family")
	}
}

func TestParseEntryReadsWhereAnEntryLives(t *testing.T) {
	o := entry("refs/pull/7/merge", "bun-+NUKu9E/U4apAe78eSCZvQ/GSdc=", 1, t0)
	e, ok := cache.ParseEntry(o)
	if !ok || e.Class != "r" || e.Repo != "1" || e.Scope != "refs/pull/7/merge" || e.Version != "v1" || e.Name != "bun-+NUKu9E/U4apAe78eSCZvQ/GSdc=" {
		t.Fatalf("ParseEntry(%q) = %+v, %v", o.Key, e, ok)
	}
	for _, k := range []string{"a/abc/20261125", "v1/d/1/main", "bin/pullcache", "v1/d/1/main/v1/"} {
		if _, ok := cache.ParseEntry(cache.Object{Key: k}); ok {
			t.Errorf("%q is not an entry", k)
		}
	}
}

func TestNothingGoesUnderTheCap(t *testing.T) {
	objs := []cache.Object{entry(main, "git-seed-v1-X-111111111", gib, ago(2*day)), entry(main, "git-seed-v1-X-222222222", gib, ago(day))}
	if plan := cache.Plan(objs, nil, cache.Policy{CapBytes: 2 * gib, MaxIdle: 30 * day}, t0); plan != nil {
		t.Fatalf("a bucket at its cap loses %v", names(plan))
	}
}

func TestSupersededEntriesGoBeforeEverythingElse(t *testing.T) {
	seed := func(run string, saved time.Time) cache.Object {
		return entry(main, "git-seed-v1-Linux-X64-"+run, gib, saved)
	}
	oldest, mid, newest := seed("10000000001", ago(30*hour)), seed("10000000002", ago(20*hour)), seed("10000000003", ago(hour))
	// A pull request's entry that is older than all of them, and the default branch's cold entry.
	pull := entry(pr7, "go-build-Linux-"+strings.Repeat("c", 16), gib, ago(40*hour))
	cold := entry(main, "uv-toolcache-v1-uv0.13.9", gib, ago(45*hour))
	objs := []cache.Object{newest, pull, cold, oldest, mid}

	plan := cache.Plan(objs, nil, cache.Policy{CapBytes: 3 * gib, MaxIdle: 30 * day}, t0)
	if got, want := names(plan), []string{"git-seed-v1-Linux-X64-10000000001", "git-seed-v1-Linux-X64-10000000002"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("over by two: %v, want the two older seeds, oldest first", got)
	}
	if !slices.Equal(reasons(plan), []cache.Reason{cache.Superseded, cache.Superseded}) {
		t.Fatalf("reasons %v", reasons(plan))
	}

	// Past the superseded ones: other scopes before the default branch's, the least recently used first.
	plan = cache.Plan(objs, nil, cache.Policy{CapBytes: 1 * gib, MaxIdle: 30 * day}, t0)
	if got, want := names(plan), []string{"git-seed-v1-Linux-X64-10000000001", "git-seed-v1-Linux-X64-10000000002", pull2name(pull), "uv-toolcache-v1-uv0.13.9"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("over by four: %v, want %v", got, want)
	}
	if !slices.Equal(reasons(plan), []cache.Reason{cache.Superseded, cache.Superseded, cache.Cold, cache.Cold}) {
		t.Fatalf("reasons %v", reasons(plan))
	}
}

func pull2name(o cache.Object) string { e, _ := cache.ParseEntry(o); return e.Name }

func TestAnOlderEntryRestoredAfterTheNewerWasSavedIsNotSuperseded(t *testing.T) {
	h := strings.Repeat("a", 16)
	// Two live caches that share a name (two lockfiles): both are restored every day, the older one saved first.
	older := entry(main, "bun-Linux-"+h+"1", gib, ago(10*day))
	newer := entry(main, "bun-Linux-"+h+"2", gib, ago(9*day))
	dead := entry(main, "bun-Linux-"+h+"0", gib, ago(12*day))
	filler := entry(pr7, "x-"+strings.Repeat("e", 16), gib, ago(2*day))
	marks := marked(map[cache.Object][]time.Time{older: {ago(day)}, newer: {ago(day)}})
	plan := cache.Plan([]cache.Object{older, newer, dead, filler}, marks, cache.Policy{CapBytes: 3 * gib, MaxIdle: 30 * day}, t0)
	if got := names(plan); !reflect.DeepEqual(got, []string{"bun-Linux-" + h + "0"}) {
		t.Fatalf("lost %v: only the entry nobody restored after a newer one was saved is superseded", got)
	}
	// Over the cap by more: the superseded one first, then by class (the pull request's), the used ones last.
	plan = cache.Plan([]cache.Object{older, newer, dead, filler}, marks, cache.Policy{CapBytes: 2 * gib, MaxIdle: 30 * day}, t0)
	if got := names(plan); !reflect.DeepEqual(got, []string{"bun-Linux-" + h + "0", pull2name(filler)}) {
		t.Fatalf("lost %v", got)
	}
}

func TestTheLeastRecentlyRestoredGoesFirstAndAMarkerSavesAnEntry(t *testing.T) {
	a := entry(main, "tool-a", gib, ago(20*day)) // oldest save, restored yesterday
	b := entry(main, "tool-b", gib, ago(10*day)) // saved later, never restored
	c := entry(main, "tool-c", gib, ago(5*day))
	marks := marked(map[cache.Object][]time.Time{a: {ago(3 * day), ago(day)}})
	plan := cache.Plan([]cache.Object{a, b, c}, marks, cache.Policy{CapBytes: 2 * gib}, t0)
	if got := names(plan); !reflect.DeepEqual(got, []string{"tool-b"}) {
		t.Fatalf("lost %v: b is the one nobody restored since it was saved, a was restored yesterday", got)
	}
	plan = cache.Plan([]cache.Object{a, b, c}, nil, cache.Policy{CapBytes: 2 * gib}, t0)
	if got := names(plan); !reflect.DeepEqual(got, []string{"tool-a"}) {
		t.Fatalf("without markers the order is the save age: lost %v", got)
	}
}

func TestAMarkerDatedAheadCountsForNothingAndIsDeleted(t *testing.T) {
	m := cachetest.NewMem()
	oldSeed := plant(m, entry(main, "git-seed-v1-Linux-X64-10000000001", gib, ago(20*day)))
	newSeed := plant(m, entry(main, "git-seed-v1-Linux-X64-10000000002", gib, ago(hour)))
	hot := plant(m, entry(main, "tool-hot", gib, ago(5*day)))
	m.Set(cache.MarkKey(hot.Key, ago(hour)), nil, ago(hour))
	forged := cache.MarkKey(oldSeed.Key, time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC))
	m.Set(forged, nil, ago(hour))
	stuffed := cache.MarkKey(newSeed.Key, ago(hour))
	m.Set(stuffed, make([]byte, 64), ago(hour))
	m.Set(cache.MarkRoot+"not-a-marker", make([]byte, 64), ago(hour))

	rep, err := cache.Janitor(context.Background(), sized{m, gib}, cache.Policy{CapBytes: 2 * gib, MaxIdle: 30 * day}, t0, false, log.New(&tape{}, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if left := m.Keys("v1/"); !slices.Contains(left, newSeed.Key) || slices.Contains(left, oldSeed.Key) {
		t.Fatalf("a marker dated ahead kept the old seed and cost main its newest: left %v (report %+v)", left, rep)
	}
	if marks := m.Keys(cache.MarkRoot); !reflect.DeepEqual(marks, []string{cache.MarkKey(hot.Key, ago(hour))}) {
		t.Fatalf("only real markers outlive a janitor run: %v", marks)
	}
}

func TestAnEntryInUseOutlivesTheOldThirtyDays(t *testing.T) {
	hot := entry(main, "uv-toolcache-v1-uv0.13.9", gib, ago(45*day))
	idle := entry(main, "uv-toolcache-v1-uv0.12.0", gib, ago(31*day))
	young := entry(main, "other", gib, ago(2*day))
	marks := marked(map[cache.Object][]time.Time{hot: {ago(day)}})
	plan := cache.Plan([]cache.Object{hot, idle, young}, marks, cache.Policy{CapBytes: 100 * gib, MaxIdle: 30 * day}, t0)
	if got := names(plan); !reflect.DeepEqual(got, []string{"uv-toolcache-v1-uv0.12.0"}) || plan[0].Reason != cache.Idle {
		t.Fatalf("plan %v %v: a 45-day-old entry restored yesterday stays, a 31-day-old one nobody restored goes whatever the size", names(plan), reasons(plan))
	}
}

func TestAMarkerOfTodayCountsToTheEndOfTheDay(t *testing.T) {
	// A day is the finest a marker says. The old seed was restored some time on Nov 25, the newer seed saved at 03:00
	// the same day: the restore may have been after the save, so the old seed is not called superseded.
	oldSeed := entry(main, "git-seed-100000001", gib, t0.Add(-40*hour))
	newSeed := entry(main, "git-seed-100000002", gib, time.Date(2026, 11, 25, 3, 0, 0, 0, time.UTC))
	marks := marked(map[cache.Object][]time.Time{oldSeed: {time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)}})
	plan := cache.Plan([]cache.Object{oldSeed, newSeed}, marks, cache.Policy{CapBytes: gib}, t0)
	if len(plan) != 1 || plan[0].Reason != cache.Cold {
		t.Fatalf("plan %v %v", names(plan), reasons(plan))
	}
	// Restored on Nov 23 only, it is superseded by the seed saved on the 25th.
	marks = marked(map[cache.Object][]time.Time{oldSeed: {time.Date(2026, 11, 23, 1, 0, 0, 0, time.UTC)}})
	plan = cache.Plan([]cache.Object{oldSeed, newSeed}, marks, cache.Policy{CapBytes: gib}, t0)
	if len(plan) != 1 || plan[0].Reason != cache.Superseded || names(plan)[0] != "git-seed-100000001" {
		t.Fatalf("plan %v %v", names(plan), reasons(plan))
	}
}

func TestPlanIsAFunctionOfItsInputsAndConverges(t *testing.T) {
	var objs []cache.Object
	for i := range 40 {
		scope := main
		if i%3 == 0 {
			scope = fmt.Sprintf("refs/pull/%d/merge", i)
		}
		objs = append(objs, entry(scope, fmt.Sprintf("fam%d-%016x", i%5, i*7919), int64(1+i%4)*gib, ago(time.Duration(i)*hour)))
	}
	p := cache.Policy{CapBytes: 30 * gib, MaxIdle: 30 * day}
	first := cache.Plan(objs, nil, p, t0)
	shuffled := slices.Clone(objs)
	slices.Reverse(shuffled)
	if second := cache.Plan(shuffled, nil, p, t0); !reflect.DeepEqual(first, second) {
		t.Fatal("the plan depends on the order of the listing")
	}
	gone := map[string]bool{}
	var freed int64
	for _, v := range first {
		gone[v.Key] = true
		freed += v.Size
	}
	var left []cache.Object
	var total int64
	for _, o := range objs {
		if !gone[o.Key] {
			left = append(left, o)
			total += o.Size
		}
	}
	if total > p.CapBytes {
		t.Fatalf("%d GB left over a cap of %d", total/gib, p.CapBytes/gib)
	}
	if again := cache.Plan(left, nil, p, t0); again != nil {
		t.Fatalf("a second run over what is left deletes %v", names(again))
	}
}

func TestEntriesTheJanitorDoesNotKnowAreStillRanked(t *testing.T) {
	odd := cache.Object{Key: "v1/d/odd", Size: gib, Modified: ago(day)}
	good := entry(main, "k-"+strings.Repeat("d", 16), gib, ago(2*day))
	plan := cache.Plan([]cache.Object{odd, good}, nil, cache.Policy{CapBytes: gib}, t0)
	if len(plan) != 1 || plan[0].Key != good.Key {
		t.Fatalf("plan %v: the older of the two goes, parsed or not", plan)
	}
}

func listing(m *cachetest.Mem, prefix string) []cache.Object {
	objs, _ := m.List(context.Background(), prefix, 1<<20)
	return objs
}

func plant(m *cachetest.Mem, o cache.Object) cache.Object {
	m.Set(o.Key, make([]byte, 1), o.Modified)
	got, _, _ := m.Head(context.Background(), o.Key)
	got.Size = o.Size
	return got
}

type tape struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (t *tape) Write(p []byte) (int, error) { t.mu.Lock(); defer t.mu.Unlock(); return t.b.Write(p) }

func TestJanitorDeletesWhatThePlanNamesAndNothingElse(t *testing.T) {
	m := cachetest.NewMem()
	oldSeed := plant(m, entry(main, "git-seed-v1-Linux-X64-10000000001", gib, ago(30*hour)))
	newSeed := plant(m, entry(main, "git-seed-v1-Linux-X64-10000000002", gib, ago(hour)))
	cold := plant(m, entry(pr7, "go-build-"+strings.Repeat("f", 16), gib, ago(2*day)))
	m.Set(cache.MarkKey(newSeed.Key, ago(hour)), nil, ago(hour))
	lg := log.New(&tape{}, "", 0)
	// The cache is told its sizes by the listing; the memory bucket holds one byte each, so a cap of 0 stands for "all of it".
	rep, err := cache.Janitor(context.Background(), sized{m, gib}, cache.Policy{CapBytes: 1 * gib}, t0, false, lg)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Deleted != 2 || rep.Planned != 2 || rep.Freed != 2*gib {
		t.Fatalf("report %+v", rep)
	}
	if left := m.Keys("v1/"); !reflect.DeepEqual(left, []string{newSeed.Key}) {
		t.Fatalf("left %v (cold %s old %s)", left, cold.Key, oldSeed.Key)
	}
	if marks := m.Keys("a/"); len(marks) != 1 {
		t.Fatalf("the janitor deletes no marker: %v", marks)
	}
	// Run again, as the next schedule does: nothing to do.
	rep, err = cache.Janitor(context.Background(), sized{m, gib}, cache.Policy{CapBytes: 1 * gib}, t0, false, lg)
	if err != nil || rep.Planned != 0 {
		t.Fatalf("second run %+v, %v", rep, err)
	}
}

// sized says each object is size bytes, as the real bucket would say of the entries the test planted one byte of.
type sized struct {
	cache.Pruner
	size int64
}

func (s sized) List(ctx context.Context, prefix string, max int) ([]cache.Object, error) {
	objs, err := s.Pruner.List(ctx, prefix, max)
	if strings.HasPrefix(prefix, cache.EntryRoot) {
		for i := range objs {
			objs[i].Size = s.size
		}
	}
	return objs, err
}

func TestDryRunDeletesNothingAndSaysWhy(t *testing.T) {
	m := cachetest.NewMem()
	plant(m, entry(main, "git-seed-v1-Linux-X64-10000000001", gib, ago(30*hour)))
	plant(m, entry(main, "git-seed-v1-Linux-X64-10000000002", gib, ago(hour)))
	var out tape
	rep, err := cache.Janitor(context.Background(), sized{m, gib}, cache.Policy{CapBytes: gib}, t0, true, log.New(&out, "", 0))
	if err != nil || rep.Planned != 1 || rep.Deleted != 0 || len(m.Keys("v1/")) != 2 {
		t.Fatalf("report %+v, %v, left %v", rep, err, m.Keys("v1/"))
	}
	if s := out.b.String(); !strings.Contains(s, "would delete") || !strings.Contains(s, "superseded") || !strings.Contains(s, "10000000001") {
		t.Fatalf("log:\n%s", s)
	}
}

// resave puts the entry back as another version after the janitor's listing, as a job that misses it and saves it
// again does.
type resave struct {
	*cachetest.Mem
	on  string
	act func()
	do  sync.Once
}

func (r *resave) List(ctx context.Context, prefix string, max int) ([]cache.Object, error) {
	objs, err := r.Mem.List(ctx, prefix, max)
	if prefix == cache.MarkRoot {
		r.do.Do(r.act)
	}
	return objs, err
}

func TestAnEntrySavedAgainAfterTheListingIsNotDeleted(t *testing.T) {
	m := cachetest.NewMem()
	victim := plant(m, entry(main, "git-seed-v1-Linux-X64-10000000001", gib, ago(30*hour)))
	plant(m, entry(main, "git-seed-v1-Linux-X64-10000000002", gib, ago(hour)))
	r := &resave{Mem: m, act: func() { m.Set(victim.Key, []byte("the new archive"), t0) }}
	rep, err := cache.Janitor(context.Background(), sized{r, gib}, cache.Policy{CapBytes: gib}, t0, false, log.New(&tape{}, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Deleted != 0 || rep.Changed != 1 {
		t.Fatalf("report %+v", rep)
	}
	if b, ok := m.Get(victim.Key); !ok || string(b) != "the new archive" {
		t.Fatalf("the new save was deleted: %q %v", b, ok)
	}
}

func TestJanitorsRunningAtOnceAgreeAndNeverFail(t *testing.T) {
	m := cachetest.NewMem()
	for i := range 12 {
		plant(m, entry(main, fmt.Sprintf("git-seed-v1-Linux-X64-%011d", i), gib, ago(time.Duration(100-i)*hour)))
	}
	// Both list before either deletes, so each plans the same eleven deletions and the second finds them gone.
	listed := make(chan struct{}, 2)
	release := make(chan struct{})
	go func() { <-listed; <-listed; close(release) }()
	var wg sync.WaitGroup
	reps := make([]cache.Report, 2)
	errs := make([]error, 2)
	for i := range reps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reps[i], errs[i] = cache.Janitor(context.Background(), &gate{sized{m, gib}, listed, release}, cache.Policy{CapBytes: gib}, t0, false, log.New(&tape{}, "", 0))
		}()
	}
	wg.Wait()
	for i, r := range reps {
		if errs[i] != nil || r.Planned != 11 || r.Deleted+r.Gone != 11 || r.Changed != 0 {
			t.Fatalf("janitor %d: %+v, %v", i, r, errs[i])
		}
	}
	if reps[0].Deleted+reps[1].Deleted != 11 {
		t.Fatalf("each entry is deleted once: %d and %d", reps[0].Deleted, reps[1].Deleted)
	}
	if left := m.Keys("v1/"); len(left) != 1 || !strings.HasSuffix(left[0], "-00000000011") {
		t.Fatalf("left %v", left)
	}
}

type gate struct {
	sized
	listed  chan struct{}
	release chan struct{}
}

func (g *gate) List(ctx context.Context, prefix string, max int) ([]cache.Object, error) {
	objs, err := g.sized.List(ctx, prefix, max)
	if prefix == cache.MarkRoot {
		g.listed <- struct{}{}
		<-g.release
	}
	return objs, err
}

func TestSupersededEntriesNobodyRestoredGoWhateverTheSize(t *testing.T) {
	seed := func(run string, saved time.Time) cache.Object {
		return entry(main, "git-seed-v1-Linux-X64-"+run, gib, saved)
	}
	live := seed("10000000009", ago(hour))
	recent := seed("10000000008", ago(30*hour)) // superseded for a day: inside the grace
	dead := seed("10000000007", ago(5*day))     // superseded for days, never restored
	used := seed("10000000006", ago(6*day))     // restored yesterday: someone still wants it
	marks := marked(map[cache.Object][]time.Time{used: {ago(day)}})
	p := cache.Policy{CapBytes: 100 * gib, MaxIdle: 30 * day, SupersededGrace: 2 * day}
	plan := cache.Plan([]cache.Object{live, recent, dead, used}, marks, p, t0)
	if got := names(plan); !reflect.DeepEqual(got, []string{"git-seed-v1-Linux-X64-10000000007"}) || plan[0].Reason != cache.Superseded {
		t.Fatalf("plan %v %v: of four seeds in a bucket far under its cap, only the one superseded and unrestored for days goes", names(plan), reasons(plan))
	}
}

// heads counts the HEADs the janitor makes.
type heads struct {
	*cachetest.Mem
	mu sync.Mutex
	n  int
}

func (h *heads) Head(ctx context.Context, key string) (cache.Object, bool, error) {
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
	return h.Mem.Head(ctx, key)
}

func TestACopyIsRankedByTheSaveTimeItKeepsNotByWhenItWasCopied(t *testing.T) {
	// The state of us-east-1 on Nov 25: run ...410 was saved at 17:15 in the other region and copied by hand at
	// 18:23, after run ...088 was saved here at 17:45. By LastModified the copy is the newest seed; by its save time
	// it is the older one, and the one to delete.
	m := cachetest.NewMem()
	sevenFifteen := time.Date(2026, 11, 25, 17, 15, 0, 0, time.UTC)
	seed := func(run string, saved time.Time) string {
		return entry(main, "git-seed-v1-Linux-X64-"+run, gib, saved).Key
	}
	older, newer := seed("45662624410", sevenFifteen), seed("45664552088", time.Date(2026, 11, 25, 17, 45, 15, 0, time.UTC))
	m.Set(newer, []byte("x"), time.Date(2026, 11, 25, 17, 45, 15, 0, time.UTC))
	m.SetCopy(older, []byte("x"), sevenFifteen, time.Date(2026, 11, 25, 18, 23, 54, 0, time.UTC))
	h := &heads{Mem: m}
	now := time.Date(2026, 11, 25, 19, 0, 0, 0, time.UTC)
	rep, err := cache.Janitor(context.Background(), sized{h, gib}, cache.Policy{CapBytes: gib}, now, false, log.New(&tape{}, "", 0))
	if err != nil || rep.Deleted != 1 {
		t.Fatalf("report %+v, %v", rep, err)
	}
	if left := m.Keys("v1/"); !reflect.DeepEqual(left, []string{newer}) {
		t.Fatalf("left %v: the seed saved last is the one to keep", left)
	}
	if h.n != 2 {
		t.Fatalf("%d HEADs, want one for each of the two entries of the family", h.n)
	}
}

func TestNothingToDeleteCostsNoHeads(t *testing.T) {
	m := cachetest.NewMem()
	for i := range 20 {
		plant(m, entry(main, fmt.Sprintf("git-seed-v1-Linux-X64-%011d", i), gib, ago(time.Duration(i)*hour)))
	}
	h := &heads{Mem: m}
	rep, err := cache.Janitor(context.Background(), sized{h, gib}, cache.Policy{CapBytes: 100 * gib, MaxIdle: 30 * day, SupersededGrace: 2 * day}, t0, false, log.New(&tape{}, "", 0))
	if err != nil || rep.Planned != 0 || h.n != 0 {
		t.Fatalf("report %+v, %v, %d HEADs", rep, err, h.n)
	}
}
