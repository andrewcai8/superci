package cache_test

import (
	"context"
	"errors"
	"io"

	"superci-pull-cache/internal/cache"
)

type failing struct{}

var errDown = errors.New("the bucket is down")

func (failing) Head(context.Context, string) (cache.Object, bool, error) {
	return cache.Object{}, false, errDown
}
func (failing) List(context.Context, string, int) ([]cache.Object, error) { return nil, errDown }
func (failing) Open(context.Context, string, string, int64, int64) (io.ReadCloser, error) {
	return nil, errDown
}
func (failing) Writer(context.Context, string, cache.Grant) (cache.Writer, error) {
	return nil, errDown
}
