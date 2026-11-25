package cache

import (
	"context"
	"errors"
	"io"
	"time"
)

// Object is one version of an object, as a listing or a HEAD says it.
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
	ETag     string
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
