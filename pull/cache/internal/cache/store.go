package cache

import (
	"context"
	"errors"
	"io"
	"sort"
	"time"
)

// Object is one version of an object, as a listing or a HEAD says it.
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
	ETag     string
	// Saved is when a job saved the entry, as a copy in the other region keeps it (SavedMetadata): the copy's own
	// Modified is when it was copied. Zero on an original, whose Modified is its save, and in a listing, which
	// carries no metadata.
	Saved time.Time
}

// SavedMetadata names the object metadata (x-amz-meta-saved) that holds a copy's Saved, in RFC 3339 with
// nanoseconds.
const SavedMetadata = "saved"

// SaveTime is when the entry was saved. Which of several matches is the newest, and how old an entry is, go by it.
func (o Object) SaveTime() time.Time {
	if !o.Saved.IsZero() {
		return o.Saved
	}
	return o.Modified
}

// Newest is the match saved last, as a HEAD of each says (a listing has no save time). A copy is made after its save,
// so its Modified is never before its SaveTime: matches are read from the latest Modified down, and the reading stops
// at the first that cannot be newer than the best so far. Ties go to the larger key.
func Newest(ctx context.Context, r Reader, objs []Object) (Object, bool, error) {
	order := append([]Object(nil), objs...)
	sort.Slice(order, func(i, j int) bool {
		if !order[i].Modified.Equal(order[j].Modified) {
			return order[i].Modified.After(order[j].Modified)
		}
		return order[i].Key > order[j].Key
	})
	var best Object
	found := false
	for _, c := range order {
		if found && c.Modified.Before(best.SaveTime()) {
			break
		}
		o, ok, err := r.Head(ctx, c.Key)
		if err != nil {
			return Object{}, false, err
		}
		if !ok {
			continue
		}
		if t, b := o.SaveTime(), best.SaveTime(); !found || t.After(b) || t.Equal(b) && o.Key > best.Key {
			best, found = o, true
		}
	}
	return best, found, nil
}

// Part is one staged block an entry is assembled from.
type Part struct {
	Key  string
	Size int64
}

var (
	// ErrExists: the entry's object was there already (an entry is written once; the first save wins).
	ErrExists = errors.New("the entry exists")
	// ErrPartsTooSmall: S3 joins parts of at least 5 MiB, the last one apart.
	ErrPartsTooSmall = errors.New("a block before the last is under 5 MiB")
	// ErrChanged: the object is no longer the version a read began with (replaced, or deleted and saved again).
	ErrChanged = errors.New("the entry changed while it was read")
)

// Reader is the bucket as every job on the machine may read it, with the machine's own role.
type Reader interface {
	Head(ctx context.Context, key string) (Object, bool, error)
	// List: the objects whose names start with prefix, at most max.
	List(ctx context.Context, prefix string, max int) ([]Object, error)
	// Open reads the bytes start..end (inclusive) of the version of key whose ETag is etag (ErrChanged when that
	// version is gone).
	Open(ctx context.Context, key, etag string, start, end int64) (io.ReadCloser, error)
}

// Writer is the bucket as one job may write it: only inside its Grant.
type Writer interface {
	// Put keeps one of an upload's blocks, in the stage bucket.
	Put(ctx context.Context, key string, body []byte) error
	// Assemble joins the parts, in order, into one object that must not exist yet (ErrExists when it does).
	Assemble(ctx context.Context, final string, parts []Part) error
	// Delete deletes an upload's blocks.
	Delete(ctx context.Context, keys []string) error
}

// Store is the bucket, as the cache needs it.
type Store interface {
	Reader
	// Writer is how the job whose token this is writes, inside g (the grant the server worked out from the token).
	Writer(ctx context.Context, token string, g Grant) (Writer, error)
}
