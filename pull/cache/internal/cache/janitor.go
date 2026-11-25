package cache

import (
	"context"
	"log"
	"sort"
	"strings"
)

// Evict names the entries to delete so that those kept hold at most capBytes: every other scope's entries first
// (the oldest first), then the default branch's (the oldest first). A pull request's cache is the one that is
// saved most and read least, so it goes before a cache every job reads.
func Evict(objs []Object, capBytes int64) []string {
	var total int64
	for _, o := range objs {
		total += o.Size
	}
	if total <= capBytes {
		return nil
	}
	rank := func(o Object) int {
		if strings.HasPrefix(o.Key, EntryRoot+"d/") {
			return 1
		}
		return 0
	}
	order := append([]Object(nil), objs...)
	sort.SliceStable(order, func(i, j int) bool {
		if ri, rj := rank(order[i]), rank(order[j]); ri != rj {
			return ri < rj
		}
		return order[i].Modified.Before(order[j].Modified)
	})
	var gone []string
	for _, o := range order {
		if total <= capBytes {
			break
		}
		gone = append(gone, o.Key)
		total -= o.Size
	}
	return gone
}

// Janitor deletes entries until the bucket holds at most capBytes (the bucket's lifecycle rules take the old ones
// by age; this is the limit on size). With dry, it only says what it would delete.
func Janitor(ctx context.Context, st Store, capBytes int64, dry bool, lg *log.Logger) (n int, freed int64, err error) {
	objs, err := st.List(ctx, EntryRoot, 5_000_000)
	if err != nil {
		return 0, 0, err
	}
	gone := Evict(objs, capBytes)
	size := map[string]int64{}
	for _, o := range objs {
		size[o.Key] = o.Size
	}
	for _, k := range gone {
		freed += size[k]
	}
	lg.Printf("janitor: %d entries, %d MB held, cap %d MB: %d to delete (%d MB)", len(objs), sum(objs)>>20, capBytes>>20, len(gone), freed>>20)
	if dry || len(gone) == 0 {
		return len(gone), freed, nil
	}
	return len(gone), freed, st.Delete(ctx, gone)
}

func sum(objs []Object) (n int64) {
	for _, o := range objs {
		n += o.Size
	}
	return n
}
