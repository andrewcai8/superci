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
	mu    sync.Mutex
	queue map[string][]tagWaiter
	kick  map[string]chan struct{}
}

type tagWaiter struct {
	id   string
	done chan error
}

func NewTagger(ctx context.Context, f *Fleet) *Tagger {
	t := &Tagger{fleet: f, queue: map[string][]tagWaiter{}, kick: map[string]chan struct{}{}}
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
			ids := make([]string, len(part))
			for i, w := range part {
				ids[i] = w.id
			}
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			err := t.fleet.TagIssued(cctx, r, ids, time.Now())
			for _, w := range part {
				// One id EC2 refuses fails the whole call, so on a failure each is tried alone.
				if err != nil && len(part) > 1 {
					w.done <- t.fleet.TagIssued(cctx, r, []string{w.id}, time.Now())
				} else {
					w.done <- err
				}
			}
			cancel()
		}
	}
}
