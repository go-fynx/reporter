package reporter

import (
	"errors"
	"math"
	"testing"
	"time"

	reporterv1 "github.com/go-fynx/reporter/reporter-go/proto/reporter/v1"
)

type stringer struct{}

func (stringer) String() string { return "from String" }

type panicky struct{}

func (*panicky) String() string { panic("nil receiver") }

func TestAttributeValue(t *testing.T) {
	when := time.Date(2026, 11, 12, 8, 58, 49, 0, time.UTC)
	var nilPanicky *panicky
	tests := []struct {
		in   any
		want *reporterv1.AttributeValue
	}{
		{"text", str("text")},
		{true, &reporterv1.AttributeValue{Kind: &reporterv1.AttributeValue_BoolValue{BoolValue: true}}},
		{42, integer(42)},
		{int8(-8), integer(-8)},
		{uint32(7), integer(7)},
		{uint64(math.MaxUint64), str("18446744073709551615")},
		{float32(1.5), &reporterv1.AttributeValue{Kind: &reporterv1.AttributeValue_DoubleValue{DoubleValue: 1.5}}},
		{when, str("2026-11-12T08:58:49Z")},
		{1500 * time.Millisecond, str("1.5s")},
		{nil, str("<nil>")},
		{errors.New("an error"), str("an error")},
		{stringer{}, str("from String")},
		{struct{ A int }{3}, str("{3}")},
		{[]string{"a", "b"}, str("[a b]")},
		{"bad \xff", str("bad �")},
	}
	for _, tt := range tests {
		got := attributeValue(tt.in)
		if got.String() != tt.want.String() {
			t.Errorf("attributeValue(%#v) = %v, want %v", tt.in, got, tt.want)
		}
	}
	// A String method that panics must not crash the caller.
	if got := attributeValue(nilPanicky).GetStringValue(); got == "" {
		t.Error("panicking String method produced nothing")
	}
}

func str(s string) *reporterv1.AttributeValue {
	return &reporterv1.AttributeValue{Kind: &reporterv1.AttributeValue_StringValue{StringValue: s}}
}

func integer(i int64) *reporterv1.AttributeValue {
	return &reporterv1.AttributeValue{Kind: &reporterv1.AttributeValue_IntValue{IntValue: i}}
}

type nilPointerError struct{ msg string }

func (e *nilPointerError) Error() string { return e.msg }

func TestErrorTextSurvivesNilPointers(t *testing.T) {
	var err *nilPointerError
	if got := errorText(err); got == "" {
		t.Error("errorText of a nil pointer error returned nothing")
	}
	if got := typeName(err); got != "*reporter.nilPointerError" {
		t.Errorf("typeName = %q", got)
	}
}

func TestRedactIsCaseInsensitive(t *testing.T) {
	attrs := map[string]*reporterv1.AttributeValue{"X-Auth-TOKEN": str("t"), "user": str("u")}
	redact(attrs, lowerAll([]string{"Token"}))
	if attrs["X-Auth-TOKEN"].GetStringValue() != filtered || attrs["user"].GetStringValue() != "u" {
		t.Errorf("attrs = %v", attrs)
	}
}
