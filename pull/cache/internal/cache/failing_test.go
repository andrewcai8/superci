package cache_test

import (
	"context"
	"errors"
	"io"

	"superci-pull-cache/internal/cache"
)

type failing struct{}

var errDown = errors.New("the bucket is down")

func (failing) Head(context.Context, string) (int64, bool, error)         { return 0, false, errDown }
func (failing) List(context.Context, string, int) ([]cache.Object, error) { return nil, errDown }
func (failing) Put(context.Context, string, []byte) error                 { return errDown }
func (failing) Assemble(context.Context, string, []cache.Part) error      { return errDown }
func (failing) Open(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return nil, errDown
}
func (failing) Delete(context.Context, []string) error     { return errDown }
func (failing) DeletePrefix(context.Context, string) error { return errDown }
