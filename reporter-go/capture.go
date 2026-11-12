package reporter

import (
	"fmt"
	"math"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"google.golang.org/protobuf/types/known/timestamppb"

	reporterv1 "github.com/go-fynx/reporter/reporter-go/proto/reporter/v1"
)

const (
	// maxFrames is the most stack frames sent per stack, as in bugsnag-go.
	maxFrames = 50
	// maxCauses bounds the Unwrap chain, which also stops a cyclic chain from
	// looping forever. It matches the service's default limit.
	maxCauses = 32
	// filtered replaces redacted attribute values.
	filtered = "[FILTERED]"
)

// pending is a captured event waiting to be sent. Stack frames are resolved
// by finish, on the goroutine that sends the event, to keep Notify cheap.
type pending struct {
	state  *state
	event  *reporterv1.Event
	stack  stackSource
	causes []stackSource // one per event.Causes entry
}

// capture builds an event in the caller's goroutine. It copies everything it
// needs, so the caller may change its Fields map afterwards.
func capture(st *state, n notice, rawData ...[]any) *pending {
	ev := &reporterv1.Event{
		EventId:   uuid.NewV7().String(),
		Timestamp: timestamppb.Now(),
		Unhandled: n.unhandled,
		Message:   validUTF8(n.message),
	}
	p := &pending{state: st, event: ev}
	severity := n.severity

	if n.err != nil {
		ev.ErrorType = typeName(n.err)
		ev.Message = validUTF8(errorText(n.err))
		if pcs, ok := carriedStack(n.err); ok {
			p.stack = stackSource{pcs: pcs}
		} else if n.captureStack {
			p.stack = callSite(n.panicking)
		}
		for _, cause := range causes(n.err) {
			ev.Causes = append(ev.Causes, &reporterv1.Cause{ErrorType: typeName(cause), Message: validUTF8(errorText(cause))})
			pcs, _ := carriedStack(cause)
			p.causes = append(p.causes, stackSource{pcs: pcs})
		}
	}
	if n.errorType != "" {
		ev.ErrorType = n.errorType
	}
	if n.frames != nil {
		p.stack = stackSource{frames: n.frames}
	}

	for _, list := range rawData {
		for _, d := range list {
			switch v := d.(type) {
			case Severity:
				if v >= SeverityInfo && v <= SeverityError {
					severity = v
				}
			case Fields:
				if ev.Attributes == nil {
					ev.Attributes = make(map[string]*reporterv1.AttributeValue, len(v))
				}
				for k, val := range v {
					ev.Attributes[validUTF8(k)] = attributeValue(val)
				}
			case ErrorClass:
				if v.Name != "" {
					ev.ErrorType = validUTF8(v.Name)
				}
			case Message:
				if v.Text != "" {
					ev.Message = validUTF8(v.Text)
				}
			case nil:
			default:
				st.logf("ignoring an argument of type %T; pass Severity, Fields, ErrorClass or Message", d)
			}
		}
	}
	ev.Severity = protoSeverity(severity)
	redact(ev.Attributes, st.filters)
	return p
}

// finish resolves stack frames and returns the event to send.
func (p *pending) finish() *reporterv1.Event {
	cfg := p.state.cfg
	p.event.Stack = p.stack.resolve(cfg)
	for i, c := range p.causes {
		p.event.Causes[i].Stack = c.resolve(cfg)
	}
	return p.event
}

func protoSeverity(s Severity) reporterv1.Severity {
	switch s {
	case SeverityInfo:
		return reporterv1.Severity_SEVERITY_INFO
	case SeverityError:
		return reporterv1.Severity_SEVERITY_ERROR
	default:
		return reporterv1.Severity_SEVERITY_WARNING
	}
}

// typeName is the Go type of err, such as "*fs.PathError".
func typeName(err error) string {
	return reflect.TypeOf(err).String()
}

// errorText is err.Error(), protected against Error methods that panic, such
// as one called on a nil pointer.
func errorText(err error) (s string) {
	defer func() {
		if r := recover(); r != nil {
			s = fmt.Sprintf("%T (Error method panicked: %v)", err, r)
		}
	}()
	return err.Error()
}

// causes walks the Unwrap chain below err, outermost first. For an error
// that wraps several, such as errors.Join, it follows the first.
func causes(err error) []error {
	var out []error
	for cur := unwrapOne(err); cur != nil && len(out) < maxCauses; cur = unwrapOne(cur) {
		out = append(out, cur)
	}
	return out
}

func unwrapOne(err error) error {
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		return u.Unwrap()
	case interface{ Unwrap() []error }:
		if errs := u.Unwrap(); len(errs) > 0 {
			return errs[0]
		}
	}
	return nil
}

// carriedStack returns the stack recorded inside err, if it has one: from a
// Callers() []uintptr method, or from a StackTrace() method returning a slice
// of program counters, as github.com/pkg/errors errors have. Reflection finds
// the latter without depending on that package.
func carriedStack(err error) (pcs []uintptr, ok bool) {
	defer func() {
		if recover() != nil {
			pcs, ok = nil, false
		}
	}()
	if c, isCallers := err.(interface{ Callers() []uintptr }); isCallers {
		recorded := c.Callers()
		return recorded, len(recorded) > 0
	}
	m := reflect.ValueOf(err).MethodByName("StackTrace")
	if !m.IsValid() || m.Type().NumIn() != 0 || m.Type().NumOut() != 1 {
		return nil, false
	}
	if out := m.Type().Out(0); out.Kind() != reflect.Slice || out.Elem().Kind() != reflect.Uintptr {
		return nil, false
	}
	v := m.Call(nil)[0]
	pcs = make([]uintptr, v.Len())
	for i := range pcs {
		pcs[i] = uintptr(v.Index(i).Uint())
	}
	return pcs, len(pcs) > 0
}

// callSite records the program counters of the code that is reporting. The
// SDK's own frames, and runtime frames when panicking, are removed later.
func callSite(panicking bool) stackSource {
	pcs := make([]uintptr, maxFrames+16)
	n := runtime.Callers(2, pcs)
	return stackSource{pcs: pcs[:n], fromCallSite: true, panicking: panicking}
}

// attributeValue converts a Fields value to one the service can store.
func attributeValue(v any) *reporterv1.AttributeValue {
	str := func(s string) *reporterv1.AttributeValue {
		return &reporterv1.AttributeValue{Kind: &reporterv1.AttributeValue_StringValue{StringValue: validUTF8(s)}}
	}
	integer := func(i int64) *reporterv1.AttributeValue {
		return &reporterv1.AttributeValue{Kind: &reporterv1.AttributeValue_IntValue{IntValue: i}}
	}
	unsigned := func(u uint64) *reporterv1.AttributeValue {
		if u > math.MaxInt64 {
			return str(strconv.FormatUint(u, 10))
		}
		return integer(int64(u))
	}
	float := func(f float64) *reporterv1.AttributeValue {
		return &reporterv1.AttributeValue{Kind: &reporterv1.AttributeValue_DoubleValue{DoubleValue: f}}
	}

	switch x := v.(type) {
	case nil:
		return str("<nil>")
	case string:
		return str(x)
	case bool:
		return &reporterv1.AttributeValue{Kind: &reporterv1.AttributeValue_BoolValue{BoolValue: x}}
	case int:
		return integer(int64(x))
	case int8:
		return integer(int64(x))
	case int16:
		return integer(int64(x))
	case int32:
		return integer(int64(x))
	case int64:
		return integer(x)
	case uint:
		return unsigned(uint64(x))
	case uint8:
		return unsigned(uint64(x))
	case uint16:
		return unsigned(uint64(x))
	case uint32:
		return unsigned(uint64(x))
	case uint64:
		return unsigned(x)
	case uintptr:
		return unsigned(uint64(x))
	case float32:
		return float(float64(x))
	case float64:
		return float(x)
	case time.Time:
		return str(x.Format(time.RFC3339Nano))
	case time.Duration:
		return str(x.String())
	default:
		// fmt calls Error and String methods and recovers if they panic.
		return str(fmt.Sprint(x))
	}
}

// redact replaces the value of every attribute whose key contains one of the
// lowercase filters.
func redact(attrs map[string]*reporterv1.AttributeValue, filters []string) {
	for k := range attrs {
		key := strings.ToLower(k)
		for _, f := range filters {
			if strings.Contains(key, f) {
				attrs[k] = &reporterv1.AttributeValue{Kind: &reporterv1.AttributeValue_StringValue{StringValue: filtered}}
				break
			}
		}
	}
}

func lowerAll(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// validUTF8 replaces invalid UTF-8, which protobuf refuses to encode.
func validUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "�")
}
