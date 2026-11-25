package cache

import (
	"bytes"
	"context"
	"io"
	"sync"
)

const (
	// A read of more than one chunk is made of reads of this size, several at a time: one read of the bucket goes at
	// about 100 MB/s, and actions/cache asks for a whole entry in one GET.
	defaultChunk = 16 << 20
	// Chunks read ahead of the one being sent, at most (memory: chunk size times this, for each download).
	defaultAhead = 12
)

// stream sends bytes start..end of an object to w, in order, reading the chunks of it concurrently.
func (s *Server) stream(ctx context.Context, w io.Writer, key string, start, end int64) (int64, error) {
	chunk, ahead := s.ReadChunk, s.ReadAhead
	if chunk <= 0 {
		chunk = defaultChunk
	}
	if ahead <= 0 {
		ahead = defaultAhead
	}
	if end-start < chunk {
		body, err := s.Store.Open(ctx, key, start, end)
		if err != nil {
			return 0, err
		}
		defer body.Close()
		return io.Copy(w, body)
	}
	// (Deferred in this order, cancel runs first: the readers stop, and only then is waited for them.)
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type slot struct {
		buf  []byte
		err  error
		done chan struct{}
	}
	n := int((end-start)/chunk) + 1
	slots := make([]*slot, n)
	for i := range slots {
		slots[i] = &slot{done: make(chan struct{})}
	}
	room := make(chan struct{}, ahead)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i, sl := range slots {
			select {
			case room <- struct{}{}:
			case <-ctx.Done():
				for _, rest := range slots[i:] {
					rest.err = ctx.Err()
					close(rest.done)
				}
				return
			}
			from := start + int64(i)*chunk
			to := min(from+chunk-1, end)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer close(sl.done)
				body, err := s.Store.Open(ctx, key, from, to)
				if err != nil {
					sl.err = err
					return
				}
				defer body.Close()
				sl.buf = make([]byte, to-from+1)
				_, sl.err = io.ReadFull(body, sl.buf)
			}()
		}
	}()
	var sent int64
	for _, sl := range slots {
		<-sl.done
		if sl.err != nil {
			return sent, sl.err
		}
		m, err := w.Write(sl.buf)
		sent += int64(m)
		sl.buf = nil
		<-room
		if err != nil {
			return sent, err
		}
	}
	return sent, nil
}

// Stream and StreamTo are for tests: the bytes stream sends.
func (s *Server) Stream(ctx context.Context, key string, start, end int64) []byte {
	var b bytes.Buffer
	_, _ = s.stream(ctx, &b, key, start, end)
	return b.Bytes()
}

func (s *Server) StreamTo(ctx context.Context, w io.Writer, key string, start, end int64) (int64, error) {
	return s.stream(ctx, w, key, start, end)
}
