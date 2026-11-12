package reporter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	reporterv1 "github.com/go-fynx/reporter/reporter-go/proto/reporter/v1"
)

// fakeSender records requests. While block is open, each call waits for it
// to close or for the request's context to end.
type fakeSender struct {
	mu    sync.Mutex
	calls []*reporterv1.NotifyRequest
	block chan struct{}
	err   error
}

func (f *fakeSender) Notify(ctx context.Context, req *reporterv1.NotifyRequest) (*reporterv1.NotifyResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	block, err := f.block, f.err
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return &reporterv1.NotifyResponse{AcceptedCount: int32(len(req.GetEvents()))}, nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// recorder is a Logger that keeps every line.
type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) Printf(format string, v ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, v...))
}

func (r *recorder) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

// stop closes c's queue and waits for its goroutine to finish; synctest
// requires that before a test ends.
func stop(c *client) {
	c.pub.close()
	<-c.pub.done
}

func testClient(s sender, cfg Configuration) (*client, *Notifier, *recorder) {
	c := newClient()
	c.getenv = func(string) string { return "" }
	c.newSender = func(Configuration) (sender, error) { return s, nil }
	logs := &recorder{}
	base := Configuration{APIKey: "k", Endpoint: "http://127.0.0.1:1", Service: "svc", Logger: logs, PanicHandler: func() {}}
	c.configure(base.merge(cfg))
	return c, &Notifier{client: c}, logs
}

func TestFullQueueDropsNewestWithoutBlocking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := &fakeSender{block: make(chan struct{})}
		c, n, logs := testClient(fs, Configuration{QueueSize: 2})
		defer stop(c)

		_ = n.Info("first")
		synctest.Wait() // the delivery goroutine is now stuck sending "first"
		for _, msg := range []string{"second", "third", "fourth", "fifth"} {
			if err := n.Info(msg); err != nil {
				t.Fatalf("Info(%q) = %v; a full queue drops silently, as in bugsnag-go", msg, err)
			}
		}
		if got := c.counts().Dropped; got != 2 {
			t.Errorf("dropped %d, want 2", got)
		}
		if !strings.Contains(logs.text(), "event queue is full (2 events); dropped 1 event(s)") {
			t.Errorf("drop not logged:\n%s", logs.text())
		}

		close(fs.block)
		stop(c)
		if got := c.counts(); got.Sent != 3 || got.Dropped != 2 {
			t.Errorf("counts = %+v, want 3 sent and 2 dropped", got)
		}
		var sent []string
		for _, req := range fs.calls {
			sent = append(sent, req.GetEvents()[0].GetMessage())
		}
		if strings.Join(sent, ",") != "first,second,third" {
			t.Errorf("sent %v; the newest events should be the ones dropped", sent)
		}
	})
}

func TestRequestTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := &fakeSender{block: make(chan struct{})}
		c, n, _ := testClient(fs, Configuration{Synchronous: true, RequestTimeout: 3 * time.Second})
		defer stop(c)

		start := time.Now()
		err := n.Notify(errors.New("x"))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want a deadline error", err)
		}
		if elapsed := time.Since(start); elapsed != 3*time.Second {
			t.Errorf("gave up after %v, want the 3s request timeout", elapsed)
		}
		if got := c.counts(); got != (Counts{Failed: 1}) || fs.count() != 1 {
			t.Errorf("counts = %+v after %d attempts, want 1 failure and no retry", got, fs.count())
		}
	})
}

func TestMainContextDrainsTheQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		fs := &fakeSender{block: make(chan struct{})}
		c, n, _ := testClient(fs, Configuration{MainContext: ctx})
		defer stop(c)

		for i := range 3 {
			if err := n.Info(fmt.Sprintf("event %d", i)); err != nil {
				t.Fatal(err)
			}
		}
		cancel()
		synctest.Wait()
		if err := n.Info("too late"); !errors.Is(err, errShutdown) {
			t.Errorf("Info after MainContext ended = %v, want errShutdown", err)
		}

		close(fs.block)
		<-c.pub.done
		if got := c.counts(); got.Sent != 3 || got.Dropped != 1 {
			t.Errorf("counts = %+v, want the 3 queued events sent", got)
		}
	})
}

func TestSynchronousReturnsTheError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := &fakeSender{err: errors.New("connection refused")}
		c, n, logs := testClient(fs, Configuration{Synchronous: true})
		defer stop(c)
		if err := n.Notify(errors.New("x")); err == nil || !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("err = %v", err)
		}
		if fs.count() != 1 || c.counts().Failed != 1 {
			t.Errorf("%d attempts, counts %+v", fs.count(), c.counts())
		}
		if !strings.Contains(logs.text(), "failed to send event") {
			t.Errorf("failure not logged:\n%s", logs.text())
		}
	})
}

func TestAsyncFailureIsCountedNotRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := &fakeSender{err: errors.New("unavailable")}
		c, n, _ := testClient(fs, Configuration{})
		defer stop(c)
		if err := n.Notify(errors.New("x")); err != nil {
			t.Errorf("queued Notify = %v, want nil", err)
		}
		synctest.Wait()
		if fs.count() != 1 || c.counts() != (Counts{Failed: 1}) {
			t.Errorf("%d attempts, counts %+v; want exactly one attempt", fs.count(), c.counts())
		}
	})
}

func TestConfigureTwiceKeepsOneQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := &fakeSender{}
		c, n, _ := testClient(fs, Configuration{})
		defer stop(c)
		pub := c.pub
		c.configure(Configuration{Service: "renamed"})
		if c.pub != pub {
			t.Fatal("a second Configure replaced the queue")
		}
		_ = n.Info("x")
		synctest.Wait()
		if fs.count() != 1 || fs.calls[0].GetSource().GetService() != "renamed" {
			t.Errorf("calls = %v", fs.calls)
		}
	})
}

func TestNotifyBeforeConfigure(t *testing.T) {
	c := newClient()
	n := &Notifier{client: c}
	if err := n.Notify(errors.New("x")); !errors.Is(err, errNotConfigured) {
		t.Errorf("err = %v, want errNotConfigured", err)
	}
	if c.counts().Dropped != 1 {
		t.Errorf("counts = %+v", c.counts())
	}
}

func TestDeliverRecoversFromPanics(t *testing.T) {
	c, n, logs := testClient(panicSender{}, Configuration{Synchronous: true})
	if err := n.Notify(errors.New("x")); err == nil {
		t.Error("Notify returned nil although sending panicked")
	}
	if c.counts().Failed != 1 || !strings.Contains(logs.text(), "panic while sending event") {
		t.Errorf("counts %+v, logs:\n%s", c.counts(), logs.text())
	}
}

type panicSender struct{}

func (panicSender) Notify(context.Context, *reporterv1.NotifyRequest) (*reporterv1.NotifyResponse, error) {
	panic("bug in sender")
}

// The benchmarks measure what a caller pays for Notify: capturing the event
// and queueing it. A fake sender keeps the network out of the numbers.

func BenchmarkNotify(b *testing.B) {
	c, n, _ := testClient(&fakeSender{}, Configuration{QueueSize: 1 << 20})
	defer stop(c)
	err := errors.New("benchmark")
	fields := Fields{"operation": "create_booking", "attempt": 2}
	b.ReportAllocs()
	for b.Loop() {
		_ = n.Notify(err, fields)
	}
}

func BenchmarkInfo(b *testing.B) {
	c, n, _ := testClient(&fakeSender{}, Configuration{QueueSize: 1 << 20})
	defer stop(c)
	b.ReportAllocs()
	for b.Loop() {
		_ = n.Info("booking request received")
	}
}
