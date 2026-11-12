// Package ingest implements the ReporterService RPC. It turns requests into
// internal events, validates each one and hands the accepted events to a
// Processor. It contains no storage code.
package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/go-fynx/reporter/internal/auth"
	"github.com/go-fynx/reporter/internal/event"
	reporterv1 "github.com/go-fynx/reporter/reporter-go/proto/reporter/v1"
	"github.com/go-fynx/reporter/reporter-go/proto/reporter/v1/reporterv1connect"
)

// Processor stores accepted events. Process must return nil only once the
// events are stored, and must treat an event_id it has already stored for
// the same project as success without storing it again.
type Processor interface {
	Process(ctx context.Context, events []event.Event) error
}

// defaultEnvironment is used when a client leaves source.environment empty,
// matching Bugsnag's default release stage.
const defaultEnvironment = "production"

// maxSourceFieldLength bounds the free-text fields of a request's source.
const maxSourceFieldLength = 256

type Options struct {
	Limits              event.Limits
	MaxEventsPerRequest int
	ProcessingTimeout   time.Duration
	Redactor            *event.Redactor
	RateLimiter         *auth.RateLimiter
	Logger              *slog.Logger
	// Now returns the current time; it defaults to time.Now.
	Now func() time.Time
}

type Service struct {
	reporterv1connect.UnimplementedReporterServiceHandler

	processor Processor
	opts      Options
}

func NewService(processor Processor, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Redactor == nil {
		opts.Redactor = event.NewRedactor(nil)
	}
	return &Service{processor: processor, opts: opts}
}

func (s *Service) Notify(ctx context.Context, req *reporterv1.NotifyRequest) (*reporterv1.NotifyResponse, error) {
	key, ok := auth.FromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, "missing credentials")
	}
	source, err := convertSource(req.GetSource())
	if err != nil {
		return nil, err
	}

	n := len(req.GetEvents())
	switch {
	case n == 0:
		return nil, connect.NewError(connect.CodeInvalidArgument, "request has no events")
	case n > s.opts.MaxEventsPerRequest:
		return nil, connect.Errorf(connect.CodeInvalidArgument,
			"request has %d events, the limit is %d", n, s.opts.MaxEventsPerRequest)
	}
	if !s.opts.RateLimiter.Allow(key.Project, n) {
		s.opts.Logger.Warn("rate limit exceeded", "project", key.Project, "service", source.Service, "events", n)
		return nil, connect.Errorf(connect.CodeResourceExhausted, "rate limit exceeded for project %q", key.Project)
	}

	now := s.opts.Now().UTC()
	accepted := make([]event.Event, 0, n)
	var rejections []*reporterv1.Rejection
	seen := make(map[string]bool, n)
	for i, pe := range req.GetEvents() {
		ev, problem := convertEvent(pe)
		if problem == nil {
			problem = event.Validate(&ev, s.opts.Limits, now)
		}
		if problem == nil && seen[ev.EventID] {
			problem = &event.Problem{Reason: event.ReasonDuplicateEventID, Detail: "event_id appears more than once in this request"}
		}
		if problem != nil {
			rejections = append(rejections, &reporterv1.Rejection{
				Index:   int32(i),
				EventId: pe.GetEventId(),
				Reason:  protoReason(problem.Reason),
				Detail:  problem.Detail,
			})
			continue
		}
		seen[ev.EventID] = true
		ev.Project = key.Project
		ev.Source = source
		ev.ReceivedAt = now
		s.opts.Redactor.Apply(ev.Attributes)
		accepted = append(accepted, ev)
	}

	if len(accepted) > 0 {
		pctx, cancel := context.WithTimeout(ctx, s.opts.ProcessingTimeout)
		err := s.processor.Process(pctx, accepted)
		cancel()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				// The client gave up; connect reports this as Canceled or
				// DeadlineExceeded.
				return nil, ctxErr
			}
			s.opts.Logger.Error("storing events failed",
				"project", key.Project, "service", source.Service, "events", len(accepted), "error", err)
			return nil, connect.NewError(connect.CodeUnavailable, "events could not be stored").WithCause(err)
		}
	}

	if len(rejections) > 0 {
		s.opts.Logger.Warn("events rejected",
			"project", key.Project, "service", source.Service,
			"rejected", len(rejections), "accepted", len(accepted),
			"first_reason", rejections[0].GetReason().String(), "first_detail", rejections[0].GetDetail())
	}
	s.opts.Logger.Debug("events accepted",
		"project", key.Project, "service", source.Service, "environment", source.Environment, "accepted", len(accepted))

	return &reporterv1.NotifyResponse{AcceptedCount: int32(len(accepted)), Rejections: rejections}, nil
}

func convertSource(src *reporterv1.Source) (event.Source, error) {
	s := event.Source{
		Service:        strings.TrimSpace(src.GetService()),
		Environment:    strings.ToLower(strings.TrimSpace(src.GetEnvironment())),
		AppVersion:     src.GetAppVersion(),
		Hostname:       src.GetHostname(),
		OS:             src.GetOs(),
		RuntimeVersion: src.GetRuntimeVersion(),
		SDKName:        src.GetSdkName(),
		SDKVersion:     src.GetSdkVersion(),
	}
	if s.Environment == "" {
		s.Environment = defaultEnvironment
	}
	if !event.ValidName(s.Service) {
		return s, connect.NewError(connect.CodeInvalidArgument,
			"source.service is required: 1 to 100 letters, digits, '.', '_' or '-'")
	}
	if !event.ValidName(s.Environment) {
		return s, connect.NewError(connect.CodeInvalidArgument,
			"source.environment must be 1 to 100 letters, digits, '.', '_' or '-'")
	}
	for _, f := range []struct{ name, value string }{
		{"app_version", s.AppVersion},
		{"hostname", s.Hostname},
		{"os", s.OS},
		{"runtime_version", s.RuntimeVersion},
		{"sdk_name", s.SDKName},
		{"sdk_version", s.SDKVersion},
	} {
		if len(f.value) > maxSourceFieldLength {
			return s, connect.Errorf(connect.CodeInvalidArgument,
				"source.%s is longer than %d bytes", f.name, maxSourceFieldLength)
		}
	}
	return s, nil
}

// convertEvent copies a wire event into the internal form. Problems that only
// exist on the wire, such as a missing oneof value, are reported here; the
// remaining rules live in event.Validate.
func convertEvent(pe *reporterv1.Event) (event.Event, *event.Problem) {
	ev := event.Event{
		EventID:   pe.GetEventId(),
		Unhandled: pe.GetUnhandled(),
		Message:   pe.GetMessage(),
		ErrorType: pe.GetErrorType(),
		Stack:     convertFrames(pe.GetStack()),
	}

	ts := pe.GetTimestamp()
	if ts == nil {
		return ev, &event.Problem{Reason: event.ReasonInvalidTimestamp, Detail: "timestamp is required"}
	}
	if err := ts.CheckValid(); err != nil {
		return ev, &event.Problem{Reason: event.ReasonInvalidTimestamp, Detail: "timestamp is out of range"}
	}
	ev.Timestamp = ts.AsTime()

	switch pe.GetSeverity() {
	case reporterv1.Severity_SEVERITY_INFO:
		ev.Severity = event.SeverityInfo
	case reporterv1.Severity_SEVERITY_WARNING:
		ev.Severity = event.SeverityWarning
	case reporterv1.Severity_SEVERITY_ERROR:
		ev.Severity = event.SeverityError
	}

	if causes := pe.GetCauses(); len(causes) > 0 {
		ev.Causes = make([]event.Cause, len(causes))
		for i, c := range causes {
			ev.Causes[i] = event.Cause{ErrorType: c.GetErrorType(), Message: c.GetMessage(), Stack: convertFrames(c.GetStack())}
		}
	}

	if attrs := pe.GetAttributes(); len(attrs) > 0 {
		ev.Attributes = make([]event.Attribute, 0, len(attrs))
		for k, v := range attrs {
			ev.Attributes = append(ev.Attributes, event.Attribute{Key: k, Value: attributeValue(v)})
		}
		slices.SortFunc(ev.Attributes, func(a, b event.Attribute) int { return strings.Compare(a.Key, b.Key) })
	}
	return ev, nil
}

func convertFrames(frames []*reporterv1.StackFrame) []event.Frame {
	if len(frames) == 0 {
		return nil
	}
	out := make([]event.Frame, len(frames))
	for i, f := range frames {
		out[i] = event.Frame{Function: f.GetFunction(), File: f.GetFile(), Line: int(f.GetLine()), InProject: f.GetInProject()}
	}
	return out
}

// attributeValue returns nil when the value's oneof is unset; event.Validate
// rejects that.
func attributeValue(v *reporterv1.AttributeValue) any {
	switch k := v.GetKind().(type) {
	case *reporterv1.AttributeValue_StringValue:
		return k.StringValue
	case *reporterv1.AttributeValue_IntValue:
		return k.IntValue
	case *reporterv1.AttributeValue_DoubleValue:
		return k.DoubleValue
	case *reporterv1.AttributeValue_BoolValue:
		return k.BoolValue
	default:
		return nil
	}
}

func protoReason(r event.Reason) reporterv1.RejectionReason {
	switch r {
	case event.ReasonInvalidEventID:
		return reporterv1.RejectionReason_REJECTION_REASON_INVALID_EVENT_ID
	case event.ReasonDuplicateEventID:
		return reporterv1.RejectionReason_REJECTION_REASON_DUPLICATE_EVENT_ID
	case event.ReasonInvalidTimestamp:
		return reporterv1.RejectionReason_REJECTION_REASON_INVALID_TIMESTAMP
	case event.ReasonInvalidSeverity:
		return reporterv1.RejectionReason_REJECTION_REASON_INVALID_SEVERITY
	case event.ReasonMissingMessage:
		return reporterv1.RejectionReason_REJECTION_REASON_MISSING_MESSAGE
	case event.ReasonInvalidAttribute:
		return reporterv1.RejectionReason_REJECTION_REASON_INVALID_ATTRIBUTE
	case event.ReasonTooManyAttributes:
		return reporterv1.RejectionReason_REJECTION_REASON_TOO_MANY_ATTRIBUTES
	case event.ReasonTooManyCauses:
		return reporterv1.RejectionReason_REJECTION_REASON_TOO_MANY_CAUSES
	case event.ReasonTooManyFrames:
		return reporterv1.RejectionReason_REJECTION_REASON_TOO_MANY_FRAMES
	default:
		panic(fmt.Sprintf("ingest: no wire reason for %d", r))
	}
}
