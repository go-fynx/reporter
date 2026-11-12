package app

import (
	"errors"
	"fmt"
	"testing"

	reporter "github.com/go-fynx/reporter/reporter-go"

	"github.com/go-fynx/reporter/internal/event"
)

// TestSDKEndToEnd sends events through the reporter-go SDK's package-level
// API to the real service. It is the only test that calls reporter.Configure,
// which sets up process-wide state.
func TestSDKEndToEnd(t *testing.T) {
	cfg, apiKey := testConfig(t)
	b := newTestBackend()
	r := start(t, cfg, b)

	reporter.Configure(reporter.Configuration{
		APIKey:       apiKey,
		Endpoint:     r.baseURL,
		Service:      "booking-api",
		Environment:  "staging",
		AppVersion:   "1.4.2",
		Synchronous:  true,
		PanicHandler: func() {},
	})

	inner := errors.New("connection refused")
	if err := reporter.Notify(fmt.Errorf("create booking: %w", inner), reporter.Fields{"booking_id": 42, "db_password": "hunter2"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if err := reporter.Info("booking request received"); err != nil {
		t.Fatalf("Info: %v", err)
	}
	func() {
		defer func() { _ = recover() }()
		defer reporter.AutoNotify()
		panic("worker crashed")
	}()

	events := b.Events()
	if len(events) != 3 {
		t.Fatalf("stored %d events, want 3", len(events))
	}
	notified, info, crashed := events[0], events[1], events[2]

	if notified.Project != "booking" || notified.Source.Service != "booking-api" || notified.Source.Environment != "staging" ||
		notified.Source.SDKName != "reporter-go" || notified.Source.AppVersion != "1.4.2" {
		t.Errorf("source = %+v, project %q", notified.Source, notified.Project)
	}
	if notified.Severity != event.SeverityWarning || notified.ErrorType != "*fmt.wrapError" ||
		notified.Message != "create booking: connection refused" || len(notified.Causes) != 1 || len(notified.Stack) == 0 {
		t.Errorf("notified event = %+v", notified)
	}
	for _, a := range notified.Attributes {
		if a.Key == "db_password" && a.Value != event.Filtered {
			t.Errorf("db_password reached the service: %v", a.Value)
		}
		if a.Key == "booking_id" && a.Value != int64(42) {
			t.Errorf("booking_id = %#v", a.Value)
		}
	}
	if info.Severity != event.SeverityInfo || info.Message != "booking request received" || len(info.Stack) != 0 {
		t.Errorf("info event = %+v", info)
	}
	if crashed.Severity != event.SeverityError || !crashed.Unhandled || crashed.ErrorType != "panic" || crashed.Message != "worker crashed" {
		t.Errorf("AutoNotify event = %+v", crashed)
	}
	if got := reporter.Stats(); got.Sent != 3 || got.Failed != 0 || got.Dropped != 0 || got.Rejected != 0 {
		t.Errorf("SDK stats = %+v", got)
	}
}
