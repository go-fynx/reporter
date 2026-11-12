package reporter

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect/v2"

	reporterv1 "github.com/go-fynx/reporter/reporter-go/proto/reporter/v1"
)

const sdkName = "reporter-go"

// sender delivers one request to the reporter service. The generated client
// implements it; tests replace it.
type sender interface {
	Notify(ctx context.Context, req *reporterv1.NotifyRequest) (*reporterv1.NotifyResponse, error)
}

// client holds everything one notifier configuration needs: its settings,
// its queue and its counters. The package keeps one, used by Configure and
// the package-level functions.
type client struct {
	mu         sync.Mutex
	configured bool
	userConfig Configuration // environment plus every Configure call, before defaults
	pub        *publisher
	watchedCtx context.Context
	panicOnce  sync.Once

	state atomic.Pointer[state]

	// Replaced in tests.
	getenv    func(string) string
	newSender func(Configuration) (sender, error)

	sent, failed, dropped, rejected atomic.Uint64
	dropLog                         dropLogger
	unconfiguredOnce                sync.Once
}

// state is an immutable snapshot of a configuration, ready to use. Every
// event keeps the state it was captured with.
type state struct {
	cfg     Configuration
	err     error // why events cannot be sent, or nil
	enabled bool
	source  *reporterv1.Source
	sender  sender
	filters []string
}

var errNotConfigured = errors.New("reporter: Configure has not been called; event not sent")

func newClient() *client {
	c := &client{getenv: os.Getenv, newSender: newSender}
	c.state.Store(&state{cfg: Configuration{}.withDefaults(), err: errNotConfigured})
	return c
}

func (c *client) configure(cfg Configuration) {
	c.mu.Lock()
	if !c.configured {
		env, problems := configFromEnv(c.getenv)
		c.userConfig = env
		c.configured = true
		for _, p := range problems {
			c.logfWith(cfg.Logger, "ignoring %s", p)
		}
	}
	c.userConfig = c.userConfig.merge(cfg)
	st := c.newState(c.userConfig)
	c.state.Store(st)
	if st.err != nil {
		st.logf("not configured, events will not be sent: %v", st.err)
	} else if st.cfg.Environment == defaultEnvironment && isPlaintext(st.cfg.Endpoint) {
		st.logf("sending production events without TLS to %s", st.cfg.Endpoint)
	}

	if c.pub == nil {
		c.pub = newPublisher(st.cfg.QueueSize, c.deliverQueued)
	}
	if ctx := c.userConfig.MainContext; ctx != nil && ctx != c.watchedCtx {
		c.watchedCtx = ctx
		context.AfterFunc(ctx, c.pub.close)
	}
	panicHandler := c.userConfig.PanicHandler
	if panicHandler == nil {
		panicHandler = c.startPanicMonitor
	}
	c.mu.Unlock()

	// In the crash monitor process this call never returns, so it runs last
	// and without the lock held.
	c.panicOnce.Do(panicHandler)
}

func (c *client) newState(cfg Configuration) *state {
	cfg = cfg.withDefaults()
	st := &state{cfg: cfg, enabled: cfg.enabled(), filters: lowerAll(cfg.ParamsFilters)}
	if st.err = cfg.validate(); st.err != nil {
		return st
	}
	st.sender, st.err = c.newSender(cfg)
	st.source = &reporterv1.Source{
		Service:        cfg.Service,
		Environment:    cfg.Environment,
		AppVersion:     validUTF8(cfg.AppVersion),
		Hostname:       validUTF8(cfg.Hostname),
		Os:             runtime.GOOS,
		RuntimeVersion: runtime.Version(),
		SdkName:        sdkName,
		SdkVersion:     Version,
	}
	return st
}

// notice describes one event before rawData is applied.
type notice struct {
	err          error
	message      string // for Info and Warn
	errorType    string // preset type, such as "panic"
	severity     Severity
	unhandled    bool
	synchronous  bool
	captureStack bool
	panicking    bool                     // capturing inside a deferred recover
	frames       []*reporterv1.StackFrame // already parsed, for crash reports
}

// notify captures an event and sends or queues it.
func (c *client) notify(n notice, rawData ...[]any) error {
	st := c.state.Load()
	if st.err != nil {
		c.dropped.Add(1)
		if errors.Is(st.err, errNotConfigured) {
			c.unconfiguredOnce.Do(func() { st.logf("%v", errNotConfigured) })
		}
		return st.err
	}
	if !st.enabled {
		return nil
	}

	p := capture(st, n, rawData...)
	if n.synchronous || st.cfg.Synchronous {
		return c.deliver(p)
	}
	switch err := c.pub.enqueue(p); {
	case err == nil:
		return nil
	case errors.Is(err, errQueueFull):
		c.dropped.Add(1)
		c.dropLog.dropped(st, st.cfg.QueueSize)
		return nil
	default:
		c.dropped.Add(1)
		return err
	}
}

// notifyPanic reports a recovered panic value. Unhandled panics are sent
// before returning, because the program is about to crash.
func (c *client) notifyPanic(p any, unhandled bool, rawData ...[]any) {
	err, ok := p.(error)
	n := notice{err: err, unhandled: unhandled, synchronous: unhandled, captureStack: true, panicking: true, severity: SeverityWarning}
	if !ok {
		n.err = fmt.Errorf("%v", p)
		n.errorType = "panic"
	}
	if unhandled {
		n.severity = SeverityError
	}
	_ = c.notify(n, rawData...)
}

// deliverQueued is the publisher's send function.
func (c *client) deliverQueued(p *pending) {
	_ = c.deliver(p)
}

// deliver sends one event and records the outcome. It never panics.
func (c *client) deliver(p *pending) (err error) {
	st := p.state
	defer func() {
		if r := recover(); r != nil {
			c.failed.Add(1)
			err = fmt.Errorf("reporter: panic while sending event: %v", r)
			st.logf("%v", err)
		}
	}()

	req := &reporterv1.NotifyRequest{Source: st.source, Events: []*reporterv1.Event{p.finish()}}
	ctx, cancel := context.WithTimeout(context.Background(), st.cfg.RequestTimeout)
	defer cancel()
	ctx, info := connect.NewClientContext(ctx)
	info.RequestHeader().Set("Authorization", "Bearer "+st.cfg.APIKey)

	res, err := st.sender.Notify(ctx, req)
	if err != nil {
		c.failed.Add(1)
		st.logf("failed to send event %s: %v", p.event.GetEventId(), err)
		return fmt.Errorf("reporter: send event: %w", err)
	}
	c.sent.Add(uint64(res.GetAcceptedCount()))
	for _, r := range res.GetRejections() {
		c.rejected.Add(1)
		err = fmt.Errorf("reporter: event %s rejected: %s: %s", r.GetEventId(), r.GetReason(), r.GetDetail())
		st.logf("event %s rejected by the service: %s: %s", r.GetEventId(), r.GetReason(), r.GetDetail())
	}
	return err
}

func (c *client) counts() Counts {
	return Counts{Sent: c.sent.Load(), Failed: c.failed.Load(), Dropped: c.dropped.Load(), Rejected: c.rejected.Load()}
}

func (c *client) logf(format string, v ...any) {
	c.state.Load().logf(format, v...)
}

// logfWith logs before a state exists, through the logger the caller is
// configuring if there is one.
func (c *client) logfWith(logger interface{ Printf(string, ...any) }, format string, v ...any) {
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf("reporter: "+format, v...)
}

// logf writes one line prefixed with "reporter: ", once, even when the
// message is an error that already starts with it.
func (st *state) logf(format string, v ...any) {
	msg := fmt.Sprintf(format, v...)
	if !strings.HasPrefix(msg, "reporter: ") {
		msg = "reporter: " + msg
	}
	st.cfg.Logger.Printf("%s", msg)
}

var errQueueFull = errors.New("reporter: queue is full")

// publisher queues events and sends them one at a time on a single goroutine.
type publisher struct {
	mu     sync.RWMutex
	closed bool
	queue  chan *pending
	done   chan struct{} // closed once every queued event has been handled
}

func newPublisher(size int, send func(*pending)) *publisher {
	p := &publisher{queue: make(chan *pending, size), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		for e := range p.queue {
			send(e)
		}
	}()
	return p
}

// enqueue never blocks. It fails when the queue is full or closed.
func (p *publisher) enqueue(e *pending) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return errShutdown
	}
	select {
	case p.queue <- e:
		return nil
	default:
		return errQueueFull
	}
}

// close stops new events; the goroutine sends what is queued, then exits.
func (p *publisher) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		close(p.queue)
	}
}

// dropLogger reports dropped events at most once every 10 seconds, so a
// flood of events cannot flood the log.
type dropLogger struct {
	mu      sync.Mutex
	last    time.Time
	pending int
}

func (d *dropLogger) dropped(st *state, queueSize int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending++
	if now := time.Now(); now.Sub(d.last) >= 10*time.Second {
		st.logf("event queue is full (%d events); dropped %d event(s)", queueSize, d.pending)
		d.last, d.pending = now, 0
	}
}
