// Package memory provides an in-memory Processor for tests.
package memory

import (
	"context"
	"slices"
	"sync"

	"github.com/go-fynx/reporter/internal/event"
)

type Store struct {
	mu     sync.Mutex
	events []event.Event
	seen   map[string]bool
	err    error
}

func New() *Store {
	return &Store{seen: make(map[string]bool)}
}

func (s *Store) Process(ctx context.Context, events []event.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	for _, e := range events {
		id := e.Project + "/" + e.EventID
		if s.seen[id] {
			continue
		}
		s.seen[id] = true
		s.events = append(s.events, e)
	}
	return nil
}

// Events returns a copy of everything stored so far.
func (s *Store) Events() []event.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events)
}

// Count returns how many events are stored for project.
func (s *Store) Count(project string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.events {
		if e.Project == project {
			n++
		}
	}
	return n
}

// FailWith makes every later Process call return err; nil restores normal
// behaviour.
func (s *Store) FailWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *Store) Ready(context.Context) error { return nil }
func (s *Store) Close(context.Context) error { return nil }
