// Package processortest checks that a Processor meets the contract described
// on ingest.Processor. Every storage implementation runs it.
package processortest

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/go-fynx/reporter/internal/event"
	"github.com/go-fynx/reporter/internal/ingest"
)

// CountFunc returns how many events p has stored for project.
type CountFunc func(t *testing.T, project string) int

// NewEvent returns a valid event for project, as ingest would pass it on.
func NewEvent(project string, severity event.Severity) event.Event {
	now := time.Now().UTC()
	return event.Event{
		EventID:    uuid.NewV7().String(),
		Project:    project,
		Source:     event.Source{Service: "booking-api", Environment: "test"},
		Timestamp:  now.Add(-time.Second),
		ReceivedAt: now,
		Severity:   severity,
		Message:    "something happened",
	}
}

// Run checks p. Each subtest uses its own project, so p may be shared with
// other tests.
func Run(t *testing.T, p ingest.Processor, count CountFunc) {
	t.Run("stores events", func(t *testing.T) {
		project := uniqueProject()
		events := []event.Event{NewEvent(project, event.SeverityInfo), NewEvent(project, event.SeverityError)}
		if err := p.Process(t.Context(), events); err != nil {
			t.Fatalf("Process: %v", err)
		}
		if n := count(t, project); n != 2 {
			t.Errorf("stored %d events, want 2", n)
		}
	})

	t.Run("ignores events already stored", func(t *testing.T) {
		project := uniqueProject()
		first := NewEvent(project, event.SeverityError)
		if err := p.Process(t.Context(), []event.Event{first}); err != nil {
			t.Fatalf("first Process: %v", err)
		}
		second := NewEvent(project, event.SeverityError)
		if err := p.Process(t.Context(), []event.Event{first, second}); err != nil {
			t.Fatalf("Process with a duplicate: %v", err)
		}
		if n := count(t, project); n != 2 {
			t.Errorf("stored %d events, want 2", n)
		}
	})

	t.Run("same event id in another project is a different event", func(t *testing.T) {
		a, b := uniqueProject(), uniqueProject()
		ea := NewEvent(a, event.SeverityWarning)
		eb := ea
		eb.Project = b
		if err := p.Process(t.Context(), []event.Event{ea, eb}); err != nil {
			t.Fatalf("Process: %v", err)
		}
		if count(t, a) != 1 || count(t, b) != 1 {
			t.Errorf("stored %d and %d events, want 1 each", count(t, a), count(t, b))
		}
	})

	t.Run("fails when the context is cancelled", func(t *testing.T) {
		project := uniqueProject()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := p.Process(ctx, []event.Event{NewEvent(project, event.SeverityInfo)}); err == nil {
			t.Error("Process with a cancelled context succeeded")
		}
	})
}

func uniqueProject() string {
	return "contract-" + uuid.NewV4().String()[:8]
}
