// Package event defines the service's internal representation of a reported
// event and the rules an event must satisfy before it is stored. It knows
// nothing about the wire format or the database.
package event

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"uuid"
)

type Severity int

const (
	SeverityInfo Severity = iota + 1
	SeverityWarning
	SeverityError
)

func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "info"
	case SeverityWarning:
		return "warning"
	case SeverityError:
		return "error"
	default:
		return fmt.Sprintf("Severity(%d)", int(s))
	}
}

// Source describes the process that produced an event.
type Source struct {
	Service        string
	Environment    string
	AppVersion     string
	Hostname       string
	OS             string
	RuntimeVersion string
	SDKName        string
	SDKVersion     string
}

type Event struct {
	EventID string
	// Project comes from the API key, never from the client's payload.
	Project string
	Source  Source
	// Timestamp is when the event happened, by the client's clock.
	Timestamp time.Time
	// ReceivedAt is set by the service when the event arrives.
	ReceivedAt time.Time
	Severity   Severity
	Unhandled  bool
	Message    string
	ErrorType  string
	Stack      []Frame
	Causes     []Cause
	// Attributes are sorted by key. Values are string, int64, float64 or bool.
	Attributes []Attribute
}

type Frame struct {
	Function  string
	File      string
	Line      int
	InProject bool
}

type Cause struct {
	ErrorType string
	Message   string
	Stack     []Frame
}

type Attribute struct {
	Key   string
	Value any
}

// Reason identifies why an event was rejected.
type Reason int

const (
	ReasonInvalidEventID Reason = iota + 1
	ReasonDuplicateEventID
	ReasonInvalidTimestamp
	ReasonInvalidSeverity
	ReasonMissingMessage
	ReasonInvalidAttribute
	ReasonTooManyAttributes
	ReasonTooManyCauses
	ReasonTooManyFrames
)

// Problem explains why an event was rejected.
type Problem struct {
	Reason Reason
	Detail string
}

func (p *Problem) Error() string { return p.Detail }

func problemf(reason Reason, format string, args ...any) *Problem {
	return &Problem{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// Limits bound the size of a single event.
type Limits struct {
	MaxAttributes         int
	MaxAttributeKeyLength int
	MaxCauses             int
	MaxStackFrames        int
	// MaxClockSkew is how far in the future an event's timestamp may be.
	MaxClockSkew time.Duration
}

// Validate checks e against the limits and returns the first problem found,
// or nil. It also rewrites e.EventID in canonical lowercase form so that
// duplicates are detected regardless of how the client formatted the ID.
func Validate(e *Event, limits Limits, now time.Time) *Problem {
	id, err := uuid.Parse(e.EventID)
	if err != nil || id == uuid.Nil() {
		return problemf(ReasonInvalidEventID, "event_id must be a non-nil UUID")
	}
	e.EventID = id.String()

	if e.Timestamp.IsZero() {
		return problemf(ReasonInvalidTimestamp, "timestamp is required")
	}
	if e.Timestamp.After(now.Add(limits.MaxClockSkew)) {
		return problemf(ReasonInvalidTimestamp, "timestamp is more than %s in the future", limits.MaxClockSkew)
	}
	if e.Severity < SeverityInfo || e.Severity > SeverityError {
		return problemf(ReasonInvalidSeverity, "severity must be INFO, WARNING or ERROR")
	}
	if strings.TrimSpace(e.Message) == "" && strings.TrimSpace(e.ErrorType) == "" {
		return problemf(ReasonMissingMessage, "message or error_type is required")
	}

	if len(e.Attributes) > limits.MaxAttributes {
		return problemf(ReasonTooManyAttributes, "%d attributes, the limit is %d", len(e.Attributes), limits.MaxAttributes)
	}
	for _, a := range e.Attributes {
		if a.Key == "" || len(a.Key) > limits.MaxAttributeKeyLength {
			return problemf(ReasonInvalidAttribute, "attribute keys must be 1 to %d bytes", limits.MaxAttributeKeyLength)
		}
		switch a.Value.(type) {
		case string, int64, float64, bool:
		default:
			return problemf(ReasonInvalidAttribute, "attribute %q has no value", a.Key)
		}
	}

	if len(e.Causes) > limits.MaxCauses {
		return problemf(ReasonTooManyCauses, "%d causes, the limit is %d", len(e.Causes), limits.MaxCauses)
	}
	if len(e.Stack) > limits.MaxStackFrames {
		return problemf(ReasonTooManyFrames, "%d stack frames, the limit is %d", len(e.Stack), limits.MaxStackFrames)
	}
	for i, c := range e.Causes {
		if len(c.Stack) > limits.MaxStackFrames {
			return problemf(ReasonTooManyFrames, "cause %d has %d stack frames, the limit is %d", i, len(c.Stack), limits.MaxStackFrames)
		}
	}
	return nil
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// ValidName reports whether s can be used as a project, service or
// environment name: 1 to 100 letters, digits, '.', '_' or '-', starting with a
// letter or digit.
func ValidName(s string) bool {
	return namePattern.MatchString(s)
}

// Filtered replaces the value of every redacted attribute.
const Filtered = "[FILTERED]"

// Redactor hides the values of attributes whose keys look sensitive.
type Redactor struct {
	filters []string
}

// NewRedactor returns a Redactor that matches each filter as a
// case-insensitive substring of attribute keys, the way Bugsnag's
// ParamsFilters do.
func NewRedactor(filters []string) *Redactor {
	r := &Redactor{filters: make([]string, 0, len(filters))}
	for _, f := range filters {
		if f = strings.ToLower(strings.TrimSpace(f)); f != "" {
			r.filters = append(r.filters, f)
		}
	}
	return r
}

// Apply replaces matching attribute values with Filtered, in place.
func (r *Redactor) Apply(attrs []Attribute) {
	for i := range attrs {
		key := strings.ToLower(attrs[i].Key)
		for _, f := range r.filters {
			if strings.Contains(key, f) {
				attrs[i].Value = Filtered
				break
			}
		}
	}
}
