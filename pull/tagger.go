package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Tagger writes machines' issued tags. The machines of a region issued at once share one CreateTags call: EC2 allows
// only a few mutating calls a second, and a burst issues hundreds of runners.
type Tagger struct {
	fleet *Fleet
	write func(ctx context.Context, r *Region, ids []string, when time.Time) error // Fleet.TagIssued
	mu    sync.Mutex
	queue map[string][]tagWaiter
	kick  map[string]chan struct{}
}

type tagWaiter struct {
	id   string
	done chan error
}

func NewTagger(ctx context.Context, f *Fleet) *Tagger {
	t := &Tagger{fleet: f, write: f.TagIssued, queue: map[string][]tagWaiter{}, kick: map[string]chan struct{}{}}
	for _, r := range f.regions {
		t.kick[r.Name] = make(chan struct{}, 1)
	}
	for _, r := range f.regions {
		go t.run(ctx, r)
	}
	return t
}

// Issue writes a machine's issued tag and returns once EC2 has accepted it, or it failed, or ctx ended.
func (t *Tagger) Issue(ctx context.Context, m *Machine) error {
	kick, ok := t.kick[m.Region]
	if !ok {
		return fmt.Errorf("no region %q", m.Region)
	}
	w := tagWaiter{id: m.ID, done: make(chan error, 1)}
	t.mu.Lock()
	t.queue[m.Region] = append(t.queue[m.Region], w)
	t.mu.Unlock()
	select {
	case kick <- struct{}{}:
	default:
	}
	select {
	case err := <-w.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *Tagger) run(ctx context.Context, r *Region) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.kick[r.Name]:
		}
		time.Sleep(20 * time.Millisecond)
		t.mu.Lock()
		batch := t.queue[r.Name]
		t.queue[r.Name] = nil
		t.mu.Unlock()
		for len(batch) > 0 {
			part := batch[:min(len(batch), 500)]
			batch = batch[len(part):]
			t.tag(ctx, r, part)
		}
	}
}

// tag writes the issued tag of a batch in one call. One id EC2 refuses fails the whole call, so a failed batch is
// tried again as two halves: a bad id costs about two calls per halving, not one call for every id of the batch.
func (t *Tagger) tag(ctx context.Context, r *Region, part []tagWaiter) {
	ids := make([]string, len(part))
	for i, w := range part {
		ids[i] = w.id
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	err := t.write(cctx, r, ids, time.Now())
	cancel()
	if err == nil || len(part) == 1 {
		for _, w := range part {
			w.done <- err
		}
		return
	}
	t.tag(ctx, r, part[:len(part)/2])
	t.tag(ctx, r, part[len(part)/2:])
}
