// Package devlog provides a Processor that writes events to the log instead of
// storing them. It is for local development only; the configuration refuses
// it in production because accepted events would not be kept.
package devlog

import (
	"context"
	"log/slog"

	"github.com/go-fynx/reporter/internal/event"
)

type Processor struct {
	logger *slog.Logger
}

func New(logger *slog.Logger) *Processor {
	return &Processor{logger: logger}
}

func (p *Processor) Process(ctx context.Context, events []event.Event) error {
	for _, e := range events {
		p.logger.InfoContext(ctx, "event",
			"event_id", e.EventID,
			"project", e.Project,
			"service", e.Source.Service,
			"environment", e.Source.Environment,
			"severity", e.Severity.String(),
			"unhandled", e.Unhandled,
			"error_type", e.ErrorType,
			"message", e.Message,
			"frames", len(e.Stack),
			"causes", len(e.Causes),
			"attributes", len(e.Attributes),
		)
	}
	return nil
}

func (p *Processor) Ready(context.Context) error { return nil }
func (p *Processor) Close(context.Context) error { return nil }
