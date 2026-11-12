// Package reporter sends log and error events from Go services to the reporter
// service. Its API and behaviour follow bugsnag-go: configure once as the first
// statement of main, then report errors with Notify.
//
//	func main() {
//		reporter.Configure(reporter.Configuration{
//			APIKey:   os.Getenv("REPORTER_API_KEY"),
//			Endpoint: "https://reporter.internal:8443",
//			Service:  "booking-api",
//		})
//		// ...
//		if err := createBooking(ctx); err != nil {
//			reporter.Notify(err, reporter.Fields{"operation": "create_booking"})
//		}
//	}
//
// Events are queued and sent in the background, one request per event, without
// retries. Events still queued when the process exits are lost, as with
// Bugsnag; see Configuration.MainContext and Configuration.Synchronous.
package reporter

import (
	"errors"
	"net/http"
)

// Version of this SDK. It is sent with every event.
const Version = "0.1.0"

// Severity of an event. Pass one to Notify to override the default.
type Severity int

const (
	SeverityInfo Severity = iota + 1
	SeverityWarning
	SeverityError
)

// Fields are custom attributes attached to an event. Values are sent as
// strings, integers, floating-point numbers or booleans; anything else is
// formatted with fmt.Sprint.
type Fields map[string]any

// ErrorClass overrides the error type reported for an event.
type ErrorClass struct {
	Name string
}

// Message overrides the message reported for an event.
type Message struct {
	Text string
}

// Counts reports what has happened to the events this process produced.
type Counts struct {
	// Sent events were stored by the service.
	Sent uint64
	// Failed events were lost because the request failed.
	Failed uint64
	// Dropped events were never sent: the queue was full, the SDK was shutting
	// down or it was not configured.
	Dropped uint64
	// Rejected events were refused by the service as invalid.
	Rejected uint64
}

var (
	errNilError     = errors.New("reporter: Notify was called with a nil error; nothing was sent")
	errEmptyMessage = errors.New("reporter: empty message; nothing was sent")
	errShutdown     = errors.New("reporter: shutting down; event not sent")
)

var (
	defaultClient   = newClient()
	defaultNotifier = &Notifier{client: defaultClient}
)

// Configure sets up the default notifier. Call it as the first statement of
// main.
//
// Unless Configuration.PanicHandler says otherwise, the first call also
// starts crash reporting on Linux and macOS: the program runs itself a second
// time as a monitor process, which reads the program's standard error and
// reports a crash after it happens. Code that runs before Configure therefore
// runs twice.
//
// Configure can be called again; fields that are set replace the current
// values. Problems are written to the logger rather than returned, and leave
// the notifier dropping events.
func Configure(config Configuration) {
	defaultClient.configure(config)
}

// Notify reports err. The event is a handled WARNING unless rawData contains
// a Severity. rawData may also contain Fields, ErrorClass and Message.
//
// The event is queued and Notify returns at once, unless the notifier is
// Synchronous. The error is not nil when nothing will be sent.
func Notify(err error, rawData ...any) error {
	return defaultNotifier.Notify(err, rawData...)
}

// Error reports err as an ERROR. It is Notify with SeverityError as default.
func Error(err error, rawData ...any) error {
	return defaultNotifier.Error(err, rawData...)
}

// Warn reports a message as a WARNING, without an error or stack trace.
func Warn(message string, rawData ...any) error {
	return defaultNotifier.Warn(message, rawData...)
}

// Info reports a message as INFO, without an error or stack trace.
func Info(message string, rawData ...any) error {
	return defaultNotifier.Info(message, rawData...)
}

// AutoNotify reports a panic as an unhandled ERROR and then panics again with
// the same value. It must be deferred directly:
//
//	defer reporter.AutoNotify()
//
// The event is sent before the panic continues. If the panic then crashes the
// program, the crash monitor reports it a second time.
func AutoNotify(rawData ...any) {
	if p := recover(); p != nil {
		defaultClient.notifyPanic(p, true, nil, rawData)
		panic(p)
	}
}

// Recover reports a panic as a handled WARNING and stops it. It must be
// deferred directly:
//
//	defer reporter.Recover()
func Recover(rawData ...any) {
	if p := recover(); p != nil {
		defaultClient.notifyPanic(p, false, nil, rawData)
	}
}

// Handler reports panics in h as unhandled ERROR events and then panics again,
// so net/http's own recovery still runs. A nil h means http.DefaultServeMux.
func Handler(h http.Handler, rawData ...any) http.Handler {
	return defaultNotifier.Handler(h, rawData...)
}

// HandlerFunc is Handler for a handler function.
func HandlerFunc(h http.HandlerFunc, rawData ...any) http.HandlerFunc {
	return defaultNotifier.HandlerFunc(h, rawData...)
}

// New returns a notifier that adds rawData, such as Fields or a Severity, to
// every event it reports. It shares the default notifier's configuration.
func New(rawData ...any) *Notifier {
	return &Notifier{client: defaultClient, rawData: rawData}
}

// Stats returns what has happened to this process's events so far.
func Stats() Counts {
	return defaultClient.counts()
}

// Notifier reports events with a fixed set of rawData; see New.
type Notifier struct {
	client  *client
	rawData []any
}

func (n *Notifier) Notify(err error, rawData ...any) error {
	if err == nil {
		n.client.logf("%v", errNilError)
		return errNilError
	}
	return n.client.notify(notice{err: err, severity: SeverityWarning, captureStack: true}, n.rawData, rawData)
}

func (n *Notifier) Error(err error, rawData ...any) error {
	if err == nil {
		n.client.logf("%v", errNilError)
		return errNilError
	}
	return n.client.notify(notice{err: err, severity: SeverityError, captureStack: true}, n.rawData, rawData)
}

func (n *Notifier) Warn(message string, rawData ...any) error {
	return n.message(SeverityWarning, message, rawData)
}

func (n *Notifier) Info(message string, rawData ...any) error {
	return n.message(SeverityInfo, message, rawData)
}

func (n *Notifier) message(severity Severity, message string, rawData []any) error {
	if message == "" {
		n.client.logf("%v", errEmptyMessage)
		return errEmptyMessage
	}
	return n.client.notify(notice{message: message, severity: severity}, n.rawData, rawData)
}

// AutoNotify is the package-level AutoNotify for this notifier. Defer it
// directly: defer n.AutoNotify().
func (n *Notifier) AutoNotify(rawData ...any) {
	if p := recover(); p != nil {
		n.client.notifyPanic(p, true, n.rawData, rawData)
		panic(p)
	}
}

// Recover is the package-level Recover for this notifier. Defer it directly:
// defer n.Recover().
func (n *Notifier) Recover(rawData ...any) {
	if p := recover(); p != nil {
		n.client.notifyPanic(p, false, n.rawData, rawData)
	}
}

func (n *Notifier) Handler(h http.Handler, rawData ...any) http.Handler {
	if h == nil {
		h = http.DefaultServeMux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				if p != http.ErrAbortHandler {
					n.client.notifyPanic(p, true, n.rawData, rawData)
				}
				panic(p)
			}
		}()
		h.ServeHTTP(w, r)
	})
}

func (n *Notifier) HandlerFunc(h http.HandlerFunc, rawData ...any) http.HandlerFunc {
	return n.Handler(h, rawData...).ServeHTTP
}
