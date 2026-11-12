package event

import (
	"strings"
	"testing"
	"time"
)

var testLimits = Limits{
	MaxAttributes:         3,
	MaxAttributeKeyLength: 8,
	MaxCauses:             2,
	MaxStackFrames:        2,
	MaxClockSkew:          time.Hour,
}

var testNow = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

func validEvent() Event {
	return Event{
		EventID:    "0195F1C8-6B2E-7A3C-9D4E-5F6A7B8C9D0E",
		Timestamp:  testNow.Add(-time.Minute),
		Severity:   SeverityError,
		Message:    "boom",
		Stack:      []Frame{{Function: "main.main", File: "main.go", Line: 1}},
		Causes:     []Cause{{ErrorType: "*errors.errorString", Message: "inner"}},
		Attributes: []Attribute{{Key: "op", Value: "create"}},
	}
}

func TestValidateAcceptsValidEventAndCanonicalisesID(t *testing.T) {
	e := validEvent()
	if p := Validate(&e, testLimits, testNow); p != nil {
		t.Fatalf("Validate() = %v, want nil", p)
	}
	if want := "0195f1c8-6b2e-7a3c-9d4e-5f6a7b8c9d0e"; e.EventID != want {
		t.Errorf("EventID = %q, want %q", e.EventID, want)
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Event)
		want   Reason
	}{
		{"empty event id", func(e *Event) { e.EventID = "" }, ReasonInvalidEventID},
		{"malformed event id", func(e *Event) { e.EventID = "not-a-uuid" }, ReasonInvalidEventID},
		{"nil event id", func(e *Event) { e.EventID = "00000000-0000-0000-0000-000000000000" }, ReasonInvalidEventID},
		{"missing timestamp", func(e *Event) { e.Timestamp = time.Time{} }, ReasonInvalidTimestamp},
		{"timestamp too far ahead", func(e *Event) { e.Timestamp = testNow.Add(2 * time.Hour) }, ReasonInvalidTimestamp},
		{"unknown severity", func(e *Event) { e.Severity = 0 }, ReasonInvalidSeverity},
		{"severity out of range", func(e *Event) { e.Severity = 7 }, ReasonInvalidSeverity},
		{"no message or error type", func(e *Event) { e.Message, e.ErrorType = " ", "" }, ReasonMissingMessage},
		{"too many attributes", func(e *Event) {
			e.Attributes = []Attribute{{"a", "1"}, {"b", "2"}, {"c", "3"}, {"d", "4"}}
		}, ReasonTooManyAttributes},
		{"empty attribute key", func(e *Event) { e.Attributes = []Attribute{{"", "x"}} }, ReasonInvalidAttribute},
		{"attribute key too long", func(e *Event) { e.Attributes = []Attribute{{"123456789", "x"}} }, ReasonInvalidAttribute},
		{"attribute without value", func(e *Event) { e.Attributes = []Attribute{{"k", nil}} }, ReasonInvalidAttribute},
		{"too many causes", func(e *Event) { e.Causes = make([]Cause, 3) }, ReasonTooManyCauses},
		{"too many frames", func(e *Event) { e.Stack = make([]Frame, 3) }, ReasonTooManyFrames},
		{"too many frames in a cause", func(e *Event) { e.Causes = []Cause{{Stack: make([]Frame, 3)}} }, ReasonTooManyFrames},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := validEvent()
			tt.mutate(&e)
			p := Validate(&e, testLimits, testNow)
			if p == nil {
				t.Fatal("Validate() = nil, want a problem")
			}
			if p.Reason != tt.want {
				t.Errorf("Reason = %d (%s), want %d", p.Reason, p.Detail, tt.want)
			}
		})
	}
}

func TestValidateAcceptsLimitBoundaries(t *testing.T) {
	e := validEvent()
	e.Message = ""
	e.ErrorType = "*os.PathError"
	e.Timestamp = testNow.Add(time.Hour)
	e.Attributes = []Attribute{{"12345678", int64(1)}, {"b", 1.5}, {"c", true}}
	e.Causes = make([]Cause, 2)
	e.Stack = make([]Frame, 2)
	if p := Validate(&e, testLimits, testNow); p != nil {
		t.Fatalf("Validate() = %v, want nil", p)
	}
}

func TestValidName(t *testing.T) {
	for _, s := range []string{"booking-api", "prod", "a", "svc_1.v2", strings.Repeat("a", 100)} {
		if !ValidName(s) {
			t.Errorf("ValidName(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "-lead", "has space", "slash/name", "ünïcode", strings.Repeat("a", 101)} {
		if ValidName(s) {
			t.Errorf("ValidName(%q) = true, want false", s)
		}
	}
}

func TestRedactor(t *testing.T) {
	r := NewRedactor([]string{"Password", "token", " "})
	attrs := []Attribute{
		{"user_password", "hunter2"},
		{"AccessToken", int64(42)},
		{"operation", "login"},
	}
	r.Apply(attrs)
	if attrs[0].Value != Filtered || attrs[1].Value != Filtered {
		t.Errorf("sensitive values not redacted: %+v", attrs)
	}
	if attrs[2].Value != "login" {
		t.Errorf("operation = %v, want unchanged", attrs[2].Value)
	}
}
