package reporter_test

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"connectrpc.com/connect/v2"

	reporter "github.com/go-fynx/reporter/reporter-go"
	"github.com/go-fynx/reporter/reporter-go/internal/testserver"
	reporterv1 "github.com/go-fynx/reporter/reporter-go/proto/reporter/v1"
)

const thisPackage = "github.com/go-fynx/reporter/reporter-go_test"

// logs records the SDK's log output.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) Printf(format string, v ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, v...))
}

func (l *logs) contains(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, s) {
			return true
		}
	}
	return false
}

// setup returns a synchronous notifier that sends to a fresh fake service.
func setup(t *testing.T, cfg reporter.Configuration) (*reporter.Notifier, *testserver.Server, *logs) {
	t.Helper()
	srv := testserver.New(t)
	l := &logs{}
	base := reporter.Configuration{
		APIKey:      "test-key",
		Endpoint:    srv.URL,
		Service:     "booking-api",
		AppVersion:  "1.4.2",
		Synchronous: true,
		Logger:      l,
	}
	return reporter.NewTestNotifier(mergeConfig(base, cfg)), srv, l
}

// mergeConfig lets tests override the base settings they care about.
func mergeConfig(base, o reporter.Configuration) reporter.Configuration {
	if o.Endpoint != "" {
		base.Endpoint = o.Endpoint
	}
	if o.APIKey != "" {
		base.APIKey = o.APIKey
	}
	if o.Service != "" {
		base.Service = o.Service
	}
	if o.Environment != "" {
		base.Environment = o.Environment
	}
	base.EnabledEnvironments = o.EnabledEnvironments
	base.ProjectPackages = o.ProjectPackages
	base.SourceRoot = o.SourceRoot
	base.ParamsFilters = o.ParamsFilters
	if o.Transport != nil {
		base.Transport = o.Transport
	}
	if o.QueueSize != 0 {
		base.QueueSize = o.QueueSize
		base.Synchronous = false
	}
	return base
}

func only(t *testing.T, srv *testserver.Server) *reporterv1.Event {
	t.Helper()
	events := srv.Events()
	if len(events) != 1 {
		t.Fatalf("server received %d events, want 1", len(events))
	}
	return events[0]
}

func TestNotifySendsEventOverGRPC(t *testing.T) {
	n, srv, _ := setup(t, reporter.Configuration{})
	before := time.Now()
	if err := n.Notify(errors.New("payment failed")); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	reqs := srv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	if reqs[0].Protocol != connect.ProtocolNameGRPC {
		t.Errorf("protocol = %q, want grpc", reqs[0].Protocol)
	}
	if reqs[0].Authorization != "Bearer test-key" {
		t.Errorf("Authorization = %q", reqs[0].Authorization)
	}
	src := reqs[0].Msg.GetSource()
	hostname, _ := os.Hostname()
	if src.GetService() != "booking-api" || src.GetEnvironment() != "production" || src.GetAppVersion() != "1.4.2" ||
		src.GetHostname() != hostname || src.GetOs() != runtime.GOOS || src.GetRuntimeVersion() != runtime.Version() ||
		src.GetSdkName() != "reporter-go" || src.GetSdkVersion() != reporter.Version {
		t.Errorf("source = %v", src)
	}

	e := only(t, srv)
	if e.GetSeverity() != reporterv1.Severity_SEVERITY_WARNING || e.GetUnhandled() {
		t.Errorf("severity %v unhandled %v, want a handled WARNING like bugsnag-go", e.GetSeverity(), e.GetUnhandled())
	}
	if e.GetErrorType() != "*errors.errorString" || e.GetMessage() != "payment failed" {
		t.Errorf("type %q message %q", e.GetErrorType(), e.GetMessage())
	}
	id, err := uuid.Parse(e.GetEventId())
	if err != nil || id[6]>>4 != 7 {
		t.Errorf("event_id %q is not a UUIDv7", e.GetEventId())
	}
	if ts := e.GetTimestamp().AsTime(); ts.Before(before.Add(-time.Second)) || ts.After(time.Now()) {
		t.Errorf("timestamp %v is not now", ts)
	}
	if got := reporter.StatsOf(n); got != (reporter.Counts{Sent: 1}) {
		t.Errorf("stats = %+v", got)
	}
}

func TestNotifyNilError(t *testing.T) {
	n, srv, l := setup(t, reporter.Configuration{})
	if err := n.Notify(nil); err == nil {
		t.Error("Notify(nil) returned nil")
	}
	if len(srv.Requests()) != 0 || !l.contains("nil error") {
		t.Error("Notify(nil) sent something or did not log")
	}
}

func TestRawDataOverrides(t *testing.T) {
	n, srv, l := setup(t, reporter.Configuration{})
	err := n.Notify(errors.New("boom"),
		reporter.SeverityInfo,
		reporter.Fields{"operation": "create_booking", "attempt": 2},
		reporter.Fields{"attempt": 3},
		reporter.ErrorClass{Name: "BookingError"},
		reporter.Message{Text: "booking could not be created"},
		struct{}{},
	)
	if err != nil {
		t.Fatal(err)
	}
	e := only(t, srv)
	if e.GetSeverity() != reporterv1.Severity_SEVERITY_INFO || e.GetErrorType() != "BookingError" || e.GetMessage() != "booking could not be created" {
		t.Errorf("event = %v", e)
	}
	if e.GetAttributes()["operation"].GetStringValue() != "create_booking" || e.GetAttributes()["attempt"].GetIntValue() != 3 {
		t.Errorf("attributes = %v", e.GetAttributes())
	}
	if !l.contains("ignoring an argument of type struct {}") {
		t.Error("unknown argument type was not logged")
	}
}

// TestNotifierDefaults covers New: its rawData applies to every event, and
// arguments to each call take precedence.
func TestNotifierDefaults(t *testing.T) {
	n, srv, _ := setup(t, reporter.Configuration{})
	scoped := reporter.WithRawData(n, reporter.Fields{"component": "billing", "request_id": "r-0"}, reporter.SeverityError)

	_ = scoped.Notify(errors.New("x"))
	_ = scoped.Info("hello", reporter.Fields{"request_id": "r-1"})

	events := srv.Events()
	if len(events) != 2 {
		t.Fatalf("%d events, want 2", len(events))
	}
	if a := events[0].GetAttributes(); a["component"].GetStringValue() != "billing" || a["request_id"].GetStringValue() != "r-0" ||
		events[0].GetSeverity() != reporterv1.Severity_SEVERITY_ERROR {
		t.Errorf("defaults not applied: %v", events[0])
	}
	if a := events[1].GetAttributes(); a["component"].GetStringValue() != "billing" || a["request_id"].GetStringValue() != "r-1" {
		t.Errorf("call arguments should override defaults: %v", a)
	}
	if events[1].GetSeverity() != reporterv1.Severity_SEVERITY_ERROR {
		t.Errorf("the notifier's Severity applies to Info too, as in bugsnag-go: %v", events[1].GetSeverity())
	}
}

type pathError struct{ path string }

func (e *pathError) Error() string { return "cannot open " + e.path }

func TestErrorTypeAndCauses(t *testing.T) {
	n, srv, _ := setup(t, reporter.Configuration{})
	inner := &pathError{path: "/etc/app.conf"}
	err := fmt.Errorf("loading config: %w", fmt.Errorf("reading file: %w", inner))
	if e := n.Notify(err); e != nil {
		t.Fatal(e)
	}
	ev := only(t, srv)
	if ev.GetErrorType() != "*fmt.wrapError" || ev.GetMessage() != "loading config: reading file: cannot open /etc/app.conf" {
		t.Errorf("type %q message %q", ev.GetErrorType(), ev.GetMessage())
	}
	causes := ev.GetCauses()
	if len(causes) != 2 || causes[0].GetErrorType() != "*fmt.wrapError" || causes[1].GetErrorType() != "*reporter_test.pathError" ||
		causes[1].GetMessage() != "cannot open /etc/app.conf" {
		t.Errorf("causes = %v", causes)
	}
}

func TestJoinedErrorsFollowTheFirst(t *testing.T) {
	n, srv, _ := setup(t, reporter.Configuration{})
	_ = n.Notify(errors.Join(&pathError{path: "a"}, errors.New("b")))
	ev := only(t, srv)
	if ev.GetErrorType() != "*errors.joinError" || len(ev.GetCauses()) != 1 || ev.GetCauses()[0].GetMessage() != "cannot open a" {
		t.Errorf("event = %v", ev)
	}
}

// loop unwraps to itself forever.
type loop struct{}

func (loop) Error() string   { return "loop" }
func (l loop) Unwrap() error { return l }

func TestCyclicUnwrapIsBounded(t *testing.T) {
	n, srv, _ := setup(t, reporter.Configuration{})
	_ = n.Notify(loop{})
	if got := len(only(t, srv).GetCauses()); got != 32 {
		t.Errorf("%d causes, want the limit of 32", got)
	}
}

func TestStackStartsAtTheCaller(t *testing.T) {
	n, srv, _ := setup(t, reporter.Configuration{})
	_, file, line, _ := runtime.Caller(0)
	_ = n.Notify(errors.New("boom"))

	stack := only(t, srv).GetStack()
	if len(stack) == 0 {
		t.Fatal("no stack")
	}
	top := stack[0]
	if top.GetFunction() != thisPackage+".TestStackStartsAtTheCaller" || top.GetLine() != int32(line+1) || top.GetFile() != file {
		t.Errorf("top frame = %v, want this test at line %d", top, line+1)
	}
	for _, f := range stack {
		if strings.HasPrefix(f.GetFunction(), "github.com/go-fynx/reporter/reporter-go.") {
			t.Errorf("SDK frame %q in the stack", f.GetFunction())
		}
	}
}

func recurse(depth int, f func()) {
	if depth == 0 {
		f()
		return
	}
	recurse(depth-1, f)
}

func TestStackIsCappedAt50Frames(t *testing.T) {
	n, srv, _ := setup(t, reporter.Configuration{})
	recurse(80, func() { _ = n.Notify(errors.New("deep")) })
	if got := len(only(t, srv).GetStack()); got != 50 {
		t.Errorf("%d frames, want 50", got)
	}
}

// withCallers carries the stack where it was created, like bugsnag-go's
// errors.Error.
type withCallers struct{ pcs []uintptr }

func (withCallers) Error() string        { return "with callers" }
func (e withCallers) Callers() []uintptr { return e.pcs }

func newWithCallers() error {
	pcs := make([]uintptr, 32)
	return withCallers{pcs: pcs[:runtime.Callers(1, pcs)]}
}

// pkgErrorsStyle mimics github.com/pkg/errors: StackTrace returns a slice of
// a named uintptr type.
type frame uintptr
type stackTrace []frame
type pkgErrorsStyle struct{ st stackTrace }

func (pkgErrorsStyle) Error() string            { return "pkg/errors style" }
func (e pkgErrorsStyle) StackTrace() stackTrace { return e.st }

func newPkgErrorsStyle() error {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(1, pcs)
	st := make(stackTrace, n)
	for i := range st {
		st[i] = frame(pcs[i])
	}
	return pkgErrorsStyle{st: st}
}

func TestStackCarriedByTheError(t *testing.T) {
	for name, newErr := range map[string]func() error{
		"Callers":    newWithCallers,
		"StackTrace": newPkgErrorsStyle,
	} {
		t.Run(name, func(t *testing.T) {
			n, srv, _ := setup(t, reporter.Configuration{})
			err := newErr()
			_ = n.Notify(fmt.Errorf("wrapped: %w", err))
			ev := only(t, srv)
			if top := ev.GetStack()[0].GetFunction(); top != thisPackage+".TestStackCarriedByTheError.func1" {
				t.Errorf("outer error has no stack of its own, so the call site is used; top = %q", top)
			}
			cause := ev.GetCauses()[0]
			if len(cause.GetStack()) == 0 || !strings.HasPrefix(cause.GetStack()[0].GetFunction(), thisPackage+".newWith") &&
				!strings.HasPrefix(cause.GetStack()[0].GetFunction(), thisPackage+".newPkg") {
				t.Errorf("cause stack should start where the error was created: %v", cause.GetStack())
			}

			srv2 := testserver.New(t)
			n2 := reporter.NewTestNotifier(reporter.Configuration{APIKey: "k", Endpoint: srv2.URL, Service: "svc", Synchronous: true})
			_ = n2.Notify(err)
			if top := only(t, srv2).GetStack()[0].GetFunction(); !strings.Contains(top, ".new") {
				t.Errorf("the error's own stack should be used; top = %q", top)
			}
		})
	}
}

func TestInProjectAndSourceRoot(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	n, srv, _ := setup(t, reporter.Configuration{
		ProjectPackages: []string{"github.com/go-fynx/reporter/**"},
		SourceRoot:      filepath.Dir(file),
	})
	_ = n.Notify(errors.New("boom"))
	stack := only(t, srv).GetStack()
	if !stack[0].GetInProject() || stack[0].GetFile() != "notify_test.go" {
		t.Errorf("top frame = %v, want in project with a trimmed path", stack[0])
	}
	for _, f := range stack {
		if strings.HasPrefix(f.GetFunction(), "testing.") {
			if f.GetInProject() {
				t.Errorf("%s marked in project", f.GetFunction())
			}
			if !strings.HasPrefix(f.GetFile(), "testing/") {
				t.Errorf("standard library path %q not trimmed to its package", f.GetFile())
			}
		}
	}

	// The default, "main*", marks nothing in a test binary as in project.
	n2, srv2, _ := setup(t, reporter.Configuration{})
	_ = n2.Notify(errors.New("boom"))
	if only(t, srv2).GetStack()[0].GetInProject() {
		t.Error("test frame in project with the default main* pattern")
	}
}

func TestRedaction(t *testing.T) {
	n, srv, _ := setup(t, reporter.Configuration{})
	_ = n.Info("login", reporter.Fields{"User_Password": "hunter2", "api_key": "k", "SessionToken": 1, "user": "ann"})
	attrs := only(t, srv).GetAttributes()
	for _, k := range []string{"User_Password", "api_key", "SessionToken"} {
		if attrs[k].GetStringValue() != "[FILTERED]" {
			t.Errorf("%s = %v, want [FILTERED]", k, attrs[k])
		}
	}
	if attrs["user"].GetStringValue() != "ann" {
		t.Errorf("user = %v", attrs["user"])
	}

	custom, srv2, _ := setup(t, reporter.Configuration{ParamsFilters: []string{"ssn"}})
	_ = custom.Info("x", reporter.Fields{"customer_SSN": "123", "password": "visible"})
	attrs = only(t, srv2).GetAttributes()
	if attrs["customer_SSN"].GetStringValue() != "[FILTERED]" || attrs["password"].GetStringValue() != "visible" {
		t.Errorf("custom filters: %v", attrs)
	}
}

func TestInvalidUTF8IsReplaced(t *testing.T) {
	n, srv, _ := setup(t, reporter.Configuration{})
	if err := n.Notify(errors.New("bad \xff byte"), reporter.Fields{"k\xfe": "v\xfd"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	e := only(t, srv)
	if e.GetMessage() != "bad � byte" || e.GetAttributes()["k�"].GetStringValue() != "v�" {
		t.Errorf("event = %v", e)
	}
}

func TestEnabledEnvironments(t *testing.T) {
	n, srv, _ := setup(t, reporter.Configuration{Environment: "staging", EnabledEnvironments: []string{"production"}})
	if err := n.Notify(errors.New("x")); err != nil {
		t.Errorf("Notify = %v, want nil", err)
	}
	if len(srv.Requests()) != 0 {
		t.Error("event sent from an environment that is not enabled")
	}

	n2, srv2, _ := setup(t, reporter.Configuration{Environment: "staging", EnabledEnvironments: []string{"Staging"}})
	_ = n2.Notify(errors.New("x"))
	if len(srv2.Requests()) != 1 {
		t.Error("event not sent from an enabled environment")
	}
}

func TestMessageHelpers(t *testing.T) {
	n, srv, _ := setup(t, reporter.Configuration{})
	_ = n.Info("booking request received", reporter.Fields{"booking": 1})
	_ = n.Warn("third-party response was slow")
	_ = n.Error(errors.New("payment failed"))
	if err := n.Info(""); err == nil {
		t.Error("Info(\"\") returned nil")
	}
	if err := n.Error(nil); err == nil {
		t.Error("Error(nil) returned nil")
	}

	events := srv.Events()
	if len(events) != 3 {
		t.Fatalf("%d events, want 3", len(events))
	}
	info, warn, errEv := events[0], events[1], events[2]
	if info.GetSeverity() != reporterv1.Severity_SEVERITY_INFO || info.GetMessage() != "booking request received" ||
		info.GetErrorType() != "" || len(info.GetStack()) != 0 {
		t.Errorf("Info event = %v", info)
	}
	if warn.GetSeverity() != reporterv1.Severity_SEVERITY_WARNING || len(warn.GetStack()) != 0 {
		t.Errorf("Warn event = %v", warn)
	}
	if errEv.GetSeverity() != reporterv1.Severity_SEVERITY_ERROR || len(errEv.GetStack()) == 0 || errEv.GetUnhandled() {
		t.Errorf("Error event = %v", errEv)
	}
}

func panicHere() {
	panic("something broke")
}

func TestAutoNotify(t *testing.T) {
	n, srv, _ := setup(t, reporter.Configuration{})
	var repanicked any
	func() {
		defer func() { repanicked = recover() }()
		defer n.AutoNotify(reporter.Fields{"job": "nightly"})
		panicHere()
	}()
	if repanicked != "something broke" {
		t.Fatalf("AutoNotify did not panic again with the same value: %v", repanicked)
	}
	e := only(t, srv)
	if e.GetSeverity() != reporterv1.Severity_SEVERITY_ERROR || !e.GetUnhandled() || e.GetErrorType() != "panic" ||
		e.GetMessage() != "something broke" || e.GetAttributes()["job"].GetStringValue() != "nightly" {
		t.Errorf("event = %v", e)
	}
	if top := e.GetStack()[0].GetFunction(); top != thisPackage+".panicHere" {
		t.Errorf("top frame %q, want the function that panicked", top)
	}
}

func TestAutoNotifyKeepsErrorPanics(t *testing.T) {
	n, srv, _ := setup(t, reporter.Configuration{})
	func() {
		defer func() { _ = recover() }()
		defer n.AutoNotify()
		var s []int
		_ = s[len(os.Args)+3]
	}()
	e := only(t, srv)
	if e.GetErrorType() != "runtime.boundsError" || !strings.Contains(e.GetMessage(), "index out of range") {
		t.Errorf("type %q message %q", e.GetErrorType(), e.GetMessage())
	}
}

func TestRecover(t *testing.T) {
	n, srv, _ := setup(t, reporter.Configuration{})
	func() {
		defer n.Recover()
		panicHere()
	}()
	e := only(t, srv)
	if e.GetSeverity() != reporterv1.Severity_SEVERITY_WARNING || e.GetUnhandled() || e.GetMessage() != "something broke" {
		t.Errorf("event = %v", e)
	}
}

func TestHandlerMiddleware(t *testing.T) {
	n, srv, _ := setup(t, reporter.Configuration{})
	mux := http.NewServeMux()
	mux.HandleFunc("/boom", func(http.ResponseWriter, *http.Request) { panicHere() })
	mux.HandleFunc("/abort", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "fine") })
	app := httptest.NewUnstartedServer(n.Handler(mux, reporter.Fields{"component": "http"}))
	app.Config.ErrorLog = log.New(io.Discard, "", 0) // net/http logs every panic it recovers
	app.Start()
	defer app.Close()

	if res, err := http.Get(app.URL + "/ok"); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("GET /ok: %v %v", res, err)
	}
	if _, err := http.Get(app.URL + "/boom"); err == nil {
		t.Error("a panicking handler returned a response; net/http should drop the connection")
	}
	_, _ = http.Get(app.URL + "/abort")

	e := only(t, srv)
	if !e.GetUnhandled() || e.GetSeverity() != reporterv1.Severity_SEVERITY_ERROR || e.GetMessage() != "something broke" ||
		e.GetAttributes()["component"].GetStringValue() != "http" {
		t.Errorf("event = %v", e)
	}
}

func TestTLS(t *testing.T) {
	srv := testserver.NewTLS(t)
	n := reporter.NewTestNotifier(reporter.Configuration{
		APIKey: "k", Endpoint: srv.URL, Service: "svc", Synchronous: true, Transport: srv.Transport(),
	})
	if err := n.Notify(errors.New("over tls")); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if reqs := srv.Requests(); len(reqs) != 1 || reqs[0].Protocol != connect.ProtocolNameGRPC {
		t.Errorf("requests = %v", reqs)
	}
	if !strings.HasPrefix(srv.URL, "https://") {
		t.Errorf("server URL %q is not TLS", srv.URL)
	}
}

func TestRejectedEvent(t *testing.T) {
	n, srv, l := setup(t, reporter.Configuration{})
	srv.SetRespond(func(req *reporterv1.NotifyRequest) (*reporterv1.NotifyResponse, error) {
		return &reporterv1.NotifyResponse{Rejections: []*reporterv1.Rejection{{
			EventId: req.GetEvents()[0].GetEventId(),
			Reason:  reporterv1.RejectionReason_REJECTION_REASON_TOO_MANY_ATTRIBUTES,
			Detail:  "101 attributes, the limit is 100",
		}}}, nil
	})
	if err := n.Notify(errors.New("x")); err == nil {
		t.Error("synchronous Notify of a rejected event returned nil")
	}
	if got := reporter.StatsOf(n); got != (reporter.Counts{Rejected: 1}) {
		t.Errorf("stats = %+v", got)
	}
	if !l.contains("TOO_MANY_ATTRIBUTES") {
		t.Error("rejection was not logged")
	}
}

func TestFailedRequestIsNotRetried(t *testing.T) {
	n, srv, l := setup(t, reporter.Configuration{})
	srv.SetRespond(func(*reporterv1.NotifyRequest) (*reporterv1.NotifyResponse, error) {
		return nil, connect.NewError(connect.CodeUnavailable, "storage down")
	})
	if err := n.Notify(errors.New("x")); err == nil {
		t.Error("Notify returned nil although the request failed")
	}
	if got := len(srv.Requests()); got != 1 {
		t.Errorf("%d attempts, want exactly 1", got)
	}
	if got := reporter.StatsOf(n); got != (reporter.Counts{Failed: 1}) {
		t.Errorf("stats = %+v", got)
	}
	if !l.contains("failed to send event") {
		t.Error("failure was not logged")
	}
}

func TestServiceUnreachable(t *testing.T) {
	n, _, _ := setup(t, reporter.Configuration{Endpoint: "http://127.0.0.1:1"})
	start := time.Now()
	if err := n.Notify(errors.New("x")); err == nil {
		t.Error("Notify returned nil with nothing listening")
	}
	if time.Since(start) > 6*time.Second {
		t.Errorf("Notify took %v, more than the request timeout", time.Since(start))
	}
}

func TestQueuedDelivery(t *testing.T) {
	n, srv, _ := setup(t, reporter.Configuration{QueueSize: 10})
	for i := 0; i < 5; i++ {
		if err := n.Info(fmt.Sprintf("event %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	reporter.Drain(n)
	if got := len(srv.Events()); got != 5 {
		t.Errorf("%d events delivered, want 5", got)
	}
	if err := n.Info("after shutdown"); err == nil {
		t.Error("Info after the queue closed returned nil")
	}
	if got := reporter.StatsOf(n); got.Sent != 5 || got.Dropped != 1 {
		t.Errorf("stats = %+v", got)
	}
}

func TestInvalidConfiguration(t *testing.T) {
	l := &logs{}
	n := reporter.NewTestNotifier(reporter.Configuration{Endpoint: "ftp://x", Service: "bad name", Logger: l})
	if err := n.Notify(errors.New("x")); err == nil {
		t.Error("Notify succeeded with an invalid configuration")
	}
	for _, want := range []string{"missing APIKey", "invalid Endpoint \"ftp://x\"", "invalid Service \"bad name\""} {
		if !l.contains(want) {
			t.Errorf("log does not mention %q", want)
		}
	}
	if got := reporter.StatsOf(n); got.Dropped != 1 {
		t.Errorf("stats = %+v", got)
	}
}

func TestFieldsCanChangeAfterNotify(t *testing.T) {
	n, srv, _ := setup(t, reporter.Configuration{QueueSize: 10})
	fields := reporter.Fields{"step": "one"}
	_ = n.Info("x", fields)
	fields["step"] = "two"
	reporter.Drain(n)
	if got := only(t, srv).GetAttributes()["step"].GetStringValue(); got != "one" {
		t.Errorf("step = %q, want the value at the time of the call", got)
	}
}
