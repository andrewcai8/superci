package cache

import (
	"context"
	"errors"
	"io"
	"time"
)

// Object is what a listing says of one object.
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
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
)

// Store is the bucket, as the cache needs it.
type Store interface {
	// Head: the object's size, whether it is there.
	Head(ctx context.Context, key string) (int64, bool, error)
	// List: the objects whose names start with prefix, at most max.
	List(ctx context.Context, prefix string, max int) ([]Object, error)
	Put(ctx context.Context, key string, body []byte) error
	// Assemble joins the parts, in order, into one object that must not exist yet (ErrExists when it does).
	Assemble(ctx context.Context, final string, parts []Part) error
	// Open reads the bytes start..end (inclusive) of an object.
	Open(ctx context.Context, key string, start, end int64) (io.ReadCloser, error)
	Delete(ctx context.Context, keys []string) error
	DeletePrefix(ctx context.Context, prefix string) error
}
