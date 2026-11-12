// Package testserver runs a fake reporter service for the SDK's tests. It
// serves the real generated handler over HTTP/2, plaintext or TLS, and records
// every request.
package testserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	reporterv1 "github.com/go-fynx/reporter/reporter-go/proto/reporter/v1"
	"github.com/go-fynx/reporter/reporter-go/proto/reporter/v1/reporterv1connect"
)

// Request is one call the server received.
type Request struct {
	Protocol      string
	Authorization string
	Msg           *reporterv1.NotifyRequest
}

// Respond decides the answer to a request.
type Respond func(*reporterv1.NotifyRequest) (*reporterv1.NotifyResponse, error)

// AcceptAll accepts every event.
func AcceptAll(req *reporterv1.NotifyRequest) (*reporterv1.NotifyResponse, error) {
	return &reporterv1.NotifyResponse{AcceptedCount: int32(len(req.GetEvents()))}, nil
}

type Server struct {
	URL  string
	http *httptest.Server

	mu       sync.Mutex
	requests []Request
	respond  Respond
}

// New starts a plaintext (h2c) server, stopped when the test ends.
func New(tb testing.TB) *Server {
	s, ts := newServer(tb)
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	ts.Config.Protocols = protocols
	ts.Start()
	s.URL = ts.URL
	return s
}

// NewTLS starts a TLS server; use Transport to trust its certificate.
func NewTLS(tb testing.TB) *Server {
	s, ts := newServer(tb)
	ts.EnableHTTP2 = true
	ts.StartTLS()
	s.URL = ts.URL
	return s
}

func newServer(tb testing.TB) (*Server, *httptest.Server) {
	s := &Server{respond: AcceptAll}
	rpc := connect.NewServer()
	reporterv1connect.RegisterReporterServiceHandler(rpc, handler{s})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, rpc)
	s.http = httptest.NewUnstartedServer(mux)
	tb.Cleanup(s.http.Close)
	return s, s.http
}

// Transport is an HTTP/2 transport that trusts the TLS server's certificate.
func (s *Server) Transport() http.RoundTripper {
	return s.http.Client().Transport
}

// SetRespond changes how later requests are answered.
func (s *Server) SetRespond(r Respond) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.respond = r
}

func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Events returns every event received, in order.
func (s *Server) Events() []*reporterv1.Event {
	var out []*reporterv1.Event
	for _, r := range s.Requests() {
		out = append(out, r.Msg.GetEvents()...)
	}
	return out
}

// WaitForEvents waits until at least n events have arrived.
func (s *Server) WaitForEvents(tb testing.TB, n int) []*reporterv1.Event {
	tb.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if events := s.Events(); len(events) >= n {
			return events
		}
		if time.Now().After(deadline) {
			tb.Fatalf("timed out waiting for %d events; got %d", n, len(s.Events()))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type handler struct {
	s *Server
}

func (h handler) Notify(ctx context.Context, req *reporterv1.NotifyRequest) (*reporterv1.NotifyResponse, error) {
	r := Request{Msg: req}
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		r.Protocol = info.Protocol
		r.Authorization = info.RequestHeader().Get("Authorization")
	}
	h.s.mu.Lock()
	h.s.requests = append(h.s.requests, r)
	respond := h.s.respond
	h.s.mu.Unlock()
	return respond(req)
}
