package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
	"uuid"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/go-fynx/reporter/internal/auth"
	"github.com/go-fynx/reporter/internal/event"
	"github.com/go-fynx/reporter/internal/processor/memory"
	reporterv1 "github.com/go-fynx/reporter/reporter-go/proto/reporter/v1"
)

var (
	testNow = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	testKey = auth.Key{ID: "k1", Project: "booking"}
)

func testOptions() Options {
	return Options{
		Limits: event.Limits{
			MaxAttributes:         10,
			MaxAttributeKeyLength: 32,
			MaxCauses:             4,
			MaxStackFrames:        8,
			MaxClockSkew:          time.Hour,
		},
		MaxEventsPerRequest: 5,
		ProcessingTimeout:   time.Second,
		Redactor:            event.NewRedactor([]string{"password"}),
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:                 func() time.Time { return testNow },
	}
}

func newService(opts Options) (*Service, *memory.Store) {
	store := memory.New()
	return NewService(store, opts), store
}

func authed() context.Context {
	return auth.NewContext(context.Background(), testKey)
}

func newEvent() *reporterv1.Event {
	return &reporterv1.Event{
		EventId:   uuid.NewV7().String(),
		Timestamp: timestamppb.New(testNow.Add(-time.Second)),
		Severity:  reporterv1.Severity_SEVERITY_ERROR,
		Message:   "payment failed",
		ErrorType: "*errors.errorString",
		Stack:     []*reporterv1.StackFrame{{Function: "main.pay", File: "pay.go", Line: 12, InProject: true}},
		Causes:    []*reporterv1.Cause{{ErrorType: "*net.OpError", Message: "connection refused"}},
		Attributes: map[string]*reporterv1.AttributeValue{
			"operation":     {Kind: &reporterv1.AttributeValue_StringValue{StringValue: "create_booking"}},
			"user_password": {Kind: &reporterv1.AttributeValue_StringValue{StringValue: "hunter2"}},
			"attempt":       {Kind: &reporterv1.AttributeValue_IntValue{IntValue: 3}},
		},
	}
}

func newRequest(events ...*reporterv1.Event) *reporterv1.NotifyRequest {
	return &reporterv1.NotifyRequest{
		Source: &reporterv1.Source{Service: "booking-api", Environment: " Staging ", AppVersion: "1.4.2", Hostname: "web-1"},
		Events: events,
	}
}

func TestNotifyStoresValidEvents(t *testing.T) {
	svc, store := newService(testOptions())
	e1, e2 := newEvent(), newEvent()
	e2.EventId = strings.ToUpper(e2.EventId)

	res, err := svc.Notify(authed(), newRequest(e1, e2))
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if res.GetAcceptedCount() != 2 || len(res.GetRejections()) != 0 {
		t.Fatalf("response = %v, want 2 accepted", res)
	}

	got := store.Events()
	if len(got) != 2 {
		t.Fatalf("stored %d events, want 2", len(got))
	}
	e := got[0]
	if e.Project != "booking" {
		t.Errorf("Project = %q, want the key's project", e.Project)
	}
	if !e.ReceivedAt.Equal(testNow) {
		t.Errorf("ReceivedAt = %v, want %v", e.ReceivedAt, testNow)
	}
	if e.Source.Service != "booking-api" || e.Source.Environment != "staging" || e.Source.AppVersion != "1.4.2" {
		t.Errorf("Source = %+v", e.Source)
	}
	if got[1].EventID != strings.ToLower(e2.EventId) {
		t.Errorf("EventID = %q, want lowercase canonical form", got[1].EventID)
	}
	wantAttrs := []event.Attribute{
		{Key: "attempt", Value: int64(3)},
		{Key: "operation", Value: "create_booking"},
		{Key: "user_password", Value: event.Filtered},
	}
	if len(e.Attributes) != len(wantAttrs) {
		t.Fatalf("Attributes = %v", e.Attributes)
	}
	for i, a := range wantAttrs {
		if e.Attributes[i] != a {
			t.Errorf("Attributes[%d] = %v, want %v", i, e.Attributes[i], a)
		}
	}
	if len(e.Stack) != 1 || e.Stack[0].Line != 12 || !e.Stack[0].InProject {
		t.Errorf("Stack = %+v", e.Stack)
	}
	if len(e.Causes) != 1 || e.Causes[0].ErrorType != "*net.OpError" {
		t.Errorf("Causes = %+v", e.Causes)
	}
}

func TestNotifyDefaultsEnvironment(t *testing.T) {
	svc, store := newService(testOptions())
	req := newRequest(newEvent())
	req.Source.Environment = ""
	if _, err := svc.Notify(authed(), req); err != nil {
		t.Fatal(err)
	}
	if env := store.Events()[0].Source.Environment; env != "production" {
		t.Errorf("Environment = %q, want production", env)
	}
}

func TestNotifyRejectsInvalidEventsIndividually(t *testing.T) {
	svc, store := newService(testOptions())

	noID := newEvent()
	noID.EventId = ""
	badSeverity := newEvent()
	badSeverity.Severity = reporterv1.Severity_SEVERITY_UNSPECIFIED
	noValue := newEvent()
	noValue.Attributes["empty"] = &reporterv1.AttributeValue{}
	noTimestamp := newEvent()
	noTimestamp.Timestamp = nil
	valid := newEvent()
	duplicate := proto.Clone(valid).(*reporterv1.Event)

	res, err := svc.Notify(authed(), newRequest(noID, badSeverity, valid, noValue, duplicate))
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if res.GetAcceptedCount() != 1 || store.Count("booking") != 1 {
		t.Errorf("accepted %d, stored %d; want 1 and 1", res.GetAcceptedCount(), store.Count("booking"))
	}
	want := map[int32]reporterv1.RejectionReason{
		0: reporterv1.RejectionReason_REJECTION_REASON_INVALID_EVENT_ID,
		1: reporterv1.RejectionReason_REJECTION_REASON_INVALID_SEVERITY,
		3: reporterv1.RejectionReason_REJECTION_REASON_INVALID_ATTRIBUTE,
		4: reporterv1.RejectionReason_REJECTION_REASON_DUPLICATE_EVENT_ID,
	}
	if len(res.GetRejections()) != len(want) {
		t.Fatalf("rejections = %v, want %d", res.GetRejections(), len(want))
	}
	for _, r := range res.GetRejections() {
		if want[r.GetIndex()] != r.GetReason() {
			t.Errorf("rejection at index %d = %v, want %v", r.GetIndex(), r.GetReason(), want[r.GetIndex()])
		}
		if r.GetDetail() == "" {
			t.Errorf("rejection at index %d has no detail", r.GetIndex())
		}
	}

	// The event without a timestamp is rejected on its own.
	res, err = svc.Notify(authed(), newRequest(noTimestamp))
	if err != nil || res.GetRejections()[0].GetReason() != reporterv1.RejectionReason_REJECTION_REASON_INVALID_TIMESTAMP {
		t.Errorf("missing timestamp: %v, %v", res, err)
	}
}

func TestNotifyRequestErrors(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		req  func() *reporterv1.NotifyRequest
		want connect.Code
	}{
		{"no credentials", context.Background(), func() *reporterv1.NotifyRequest { return newRequest(newEvent()) }, connect.CodeUnauthenticated},
		{"no source", authed(), func() *reporterv1.NotifyRequest {
			return &reporterv1.NotifyRequest{Events: []*reporterv1.Event{newEvent()}}
		}, connect.CodeInvalidArgument},
		{"bad service name", authed(), func() *reporterv1.NotifyRequest {
			r := newRequest(newEvent())
			r.Source.Service = "booking api"
			return r
		}, connect.CodeInvalidArgument},
		{"bad environment", authed(), func() *reporterv1.NotifyRequest {
			r := newRequest(newEvent())
			r.Source.Environment = "prod/eu"
			return r
		}, connect.CodeInvalidArgument},
		{"long hostname", authed(), func() *reporterv1.NotifyRequest {
			r := newRequest(newEvent())
			r.Source.Hostname = strings.Repeat("h", maxSourceFieldLength+1)
			return r
		}, connect.CodeInvalidArgument},
		{"no events", authed(), func() *reporterv1.NotifyRequest { return newRequest() }, connect.CodeInvalidArgument},
		{"too many events", authed(), func() *reporterv1.NotifyRequest {
			return newRequest(newEvent(), newEvent(), newEvent(), newEvent(), newEvent(), newEvent())
		}, connect.CodeInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, store := newService(testOptions())
			_, err := svc.Notify(tt.ctx, tt.req())
			if code := connect.CodeOf(err); code != tt.want {
				t.Errorf("code = %v (%v), want %v", code, err, tt.want)
			}
			if store.Count("booking") != 0 {
				t.Error("events were stored for a failed request")
			}
		})
	}
}

func TestNotifyRateLimit(t *testing.T) {
	opts := testOptions()
	opts.RateLimiter = auth.NewRateLimiter(0.001, 2, []string{"booking"})
	svc, store := newService(opts)

	if _, err := svc.Notify(authed(), newRequest(newEvent(), newEvent())); err != nil {
		t.Fatalf("within the limit: %v", err)
	}
	_, err := svc.Notify(authed(), newRequest(newEvent()))
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("over the limit: code = %v, want ResourceExhausted", connect.CodeOf(err))
	}
	if n := store.Count("booking"); n != 2 {
		t.Errorf("stored %d events, want 2", n)
	}
}

func TestNotifyStorageFailure(t *testing.T) {
	svc, store := newService(testOptions())
	store.FailWith(errors.New("mongo: connection refused to 10.0.0.5"))

	_, err := svc.Notify(authed(), newRequest(newEvent()))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("code = %v, want Unavailable", connect.CodeOf(err))
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) || strings.Contains(cerr.Message(), "10.0.0.5") {
		t.Errorf("client-facing message %q leaks the storage error", cerr.Message())
	}
}

func TestNotifyAllRejectedSkipsStorage(t *testing.T) {
	svc, store := newService(testOptions())
	store.FailWith(errors.New("storage must not be called"))
	bad := newEvent()
	bad.EventId = "nope"
	res, err := svc.Notify(authed(), newRequest(bad))
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if res.GetAcceptedCount() != 0 || len(res.GetRejections()) != 1 {
		t.Errorf("response = %v", res)
	}
}

func TestNotifyClientGaveUp(t *testing.T) {
	svc, _ := newService(testOptions())
	ctx, cancel := context.WithCancel(authed())
	cancel()
	_, err := svc.Notify(ctx, newRequest(newEvent()))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func FuzzConvertAndValidate(f *testing.F) {
	seed, err := proto.Marshal(newRequest(newEvent(), newEvent()))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte{})
	limits := testOptions().Limits
	f.Fuzz(func(t *testing.T, data []byte) {
		var req reporterv1.NotifyRequest
		if proto.Unmarshal(data, &req) != nil {
			return
		}
		_, _ = convertSource(req.GetSource())
		for _, pe := range req.GetEvents() {
			ev, problem := convertEvent(pe)
			if problem == nil {
				_ = event.Validate(&ev, limits, testNow)
			}
		}
	})
}
