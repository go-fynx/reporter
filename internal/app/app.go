// Package app wires the reporter service together and runs its HTTP server.
//
// One port serves gRPC (used by the Go SDK) and the Connect protocol's plain
// HTTP POST with JSON (used by services in other languages), plus /healthz and
// /readyz.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"sync/atomic"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/go-fynx/reporter/internal/auth"
	"github.com/go-fynx/reporter/internal/config"
	"github.com/go-fynx/reporter/internal/event"
	"github.com/go-fynx/reporter/internal/ingest"
	"github.com/go-fynx/reporter/internal/processor/devlog"
	"github.com/go-fynx/reporter/internal/storage/mongostore"
	"github.com/go-fynx/reporter/reporter-go/proto/reporter/v1/reporterv1connect"
)

const (
	// requestOverheadBytes allows for protocol framing and compression
	// headers on top of limits.max_request_bytes.
	requestOverheadBytes = 64 << 10
	maxHeaderBytes       = 64 << 10
	readinessTimeout     = 2 * time.Second
	closeStorageTimeout  = 10 * time.Second
)

// backend is the storage the service writes to.
type backend interface {
	ingest.Processor
	Ready(ctx context.Context) error
	Close(ctx context.Context) error
}

type App struct {
	cfg      *config.Config
	logger   *slog.Logger
	backend  backend
	listener net.Listener
	server   *http.Server
	ready    atomic.Bool
}

// New opens storage and starts listening. Requests are not served until
// Serve is called.
func New(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*App, error) {
	b, err := openBackend(ctx, cfg, logger)
	if err != nil {
		return nil, err
	}
	a, err := build(cfg, logger, b)
	if err != nil {
		_ = b.Close(context.Background())
		return nil, err
	}
	return a, nil
}

func openBackend(ctx context.Context, cfg *config.Config, logger *slog.Logger) (backend, error) {
	if cfg.Storage.Type == config.StorageDevLog {
		logger.Warn("storage type is devlog: events are written to the log and not stored")
		return devlog.New(logger), nil
	}
	m := cfg.Storage.MongoDB
	days := cfg.Storage.RetentionDays
	store, err := mongostore.Open(ctx, mongostore.Options{
		URI:            m.URI,
		Database:       m.Database,
		Collection:     m.Collection,
		WriteConcern:   m.WriteConcern,
		ConnectTimeout: m.ConnectTimeout.Std(),
		MaxPoolSize:    m.MaxPoolSize,
		Retention: map[event.Severity]time.Duration{
			event.SeverityError:   time.Duration(days.Error) * 24 * time.Hour,
			event.SeverityWarning: time.Duration(days.Warning) * 24 * time.Hour,
			event.SeverityInfo:    time.Duration(days.Info) * 24 * time.Hour,
		},
	})
	if err != nil {
		return nil, err
	}
	return store, nil
}

func build(cfg *config.Config, logger *slog.Logger, b backend) (*App, error) {
	keys, err := auth.NewKeys(cfg.APIKeys)
	if err != nil {
		return nil, err
	}
	l := cfg.Limits
	svc := ingest.NewService(b, ingest.Options{
		Limits: event.Limits{
			MaxAttributes:         l.MaxAttributes,
			MaxAttributeKeyLength: l.MaxAttributeKeyLength,
			MaxCauses:             l.MaxCauses,
			MaxStackFrames:        l.MaxStackFrames,
			MaxClockSkew:          l.MaxClockSkew.Std(),
		},
		MaxEventsPerRequest: l.MaxEventsPerRequest,
		ProcessingTimeout:   l.ProcessingTimeout.Std(),
		Redactor:            event.NewRedactor(cfg.Redaction.Filters),
		RateLimiter:         auth.NewRateLimiter(cfg.RateLimit.EventsPerSecond, cfg.RateLimit.Burst, keys.Projects()),
		Logger:              logger,
	})

	// Interceptors run in this order, before the request body is read.
	rpc := connect.NewServer(
		recoverPanics(logger),
		logCalls(logger),
		limitInFlight(l.MaxInFlightRequests),
		auth.Interceptor(keys),
	)
	reporterv1connect.RegisterReporterServiceHandler(rpc, svc)

	a := &App{cfg: cfg, logger: logger, backend: b}
	mux := http.NewServeMux()
	connecthttp.Mount(mux, rpc, connecthttp.WithReadMaxBytes(l.MaxRequestBytes))
	mux.HandleFunc("GET /healthz", a.handleHealth)
	mux.HandleFunc("GET /readyz", a.handleReady)

	s := cfg.Server
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	var tlsConfig *tls.Config
	if s.TLS != nil {
		cert, err := tls.LoadX509KeyPair(s.TLS.CertFile, s.TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load TLS certificate: %w", err)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		protocols.SetHTTP2(true)
	} else {
		// gRPC needs HTTP/2; without TLS that means h2c.
		protocols.SetUnencryptedHTTP2(true)
	}
	a.server = &http.Server{
		Handler:           http.MaxBytesHandler(mux, int64(l.MaxRequestBytes)+requestOverheadBytes),
		Protocols:         protocols,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: s.ReadHeaderTimeout.Std(),
		ReadTimeout:       s.ReadTimeout.Std(),
		WriteTimeout:      s.WriteTimeout.Std(),
		IdleTimeout:       s.IdleTimeout.Std(),
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	a.listener, err = net.Listen("tcp", s.ListenAddress)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", s.ListenAddress, err)
	}
	return a, nil
}

// Addr is the address the service listens on.
func (a *App) Addr() net.Addr {
	return a.listener.Addr()
}

// Serve handles requests until ctx is cancelled. It then stops reporting
// ready, gives in-flight requests up to server.shutdown_timeout to finish and
// closes storage. It returns an error if the server failed or the shutdown
// had to cut requests off.
func (a *App) Serve(ctx context.Context) error {
	served := make(chan error, 1)
	go func() {
		if a.server.TLSConfig != nil {
			served <- a.server.ServeTLS(a.listener, "", "")
		} else {
			served <- a.server.Serve(a.listener)
		}
	}()
	a.ready.Store(true)
	a.logger.Info("reporter service started",
		"address", a.Addr().String(),
		"tls", a.server.TLSConfig != nil,
		"environment", a.cfg.Environment,
		"storage", a.cfg.Storage.Type)

	select {
	case err := <-served:
		a.ready.Store(false)
		a.closeBackend()
		return fmt.Errorf("server stopped: %w", err)
	case <-ctx.Done():
	}

	a.ready.Store(false)
	timeout := a.cfg.Server.ShutdownTimeout.Std()
	a.logger.Info("shutting down", "timeout", timeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var result error
	if err := a.server.Shutdown(shutdownCtx); err != nil {
		result = fmt.Errorf("requests still running after %s were cut off", timeout)
		_ = a.server.Close()
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		a.logger.Warn("server returned an unexpected error during shutdown", "error", err)
	}
	a.closeBackend()
	if result == nil {
		a.logger.Info("reporter service stopped")
	}
	return result
}

func (a *App) closeBackend() {
	ctx, cancel := context.WithTimeout(context.Background(), closeStorageTimeout)
	defer cancel()
	if err := a.backend.Close(ctx); err != nil {
		a.logger.Warn("closing storage failed", "error", err)
	}
}

// handleHealth reports that the process is up.
func (a *App) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

// handleReady reports whether the service can accept events: it is serving,
// not shutting down, and storage is reachable.
func (a *App) handleReady(w http.ResponseWriter, r *http.Request) {
	if !a.ready.Load() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()
	if err := a.backend.Ready(ctx); err != nil {
		a.logger.Warn("readiness check failed", "error", err)
		http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

// recoverPanics turns a panic while handling a call into an Internal error,
// so one bad request cannot crash the service.
func recoverPanics(logger *slog.Logger) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) (err error) {
			defer func() {
				if r := recover(); r != nil {
					logger.Error("panic while handling a call",
						"procedure", spec.Procedure, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
					err = connect.NewError(connect.CodeInternal, "internal error")
				}
			}()
			return next(ctx, spec, stream)
		}
	}
}

// logCalls logs failed calls at warn level and every call at debug level.
func logCalls(logger *slog.Logger) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			start := time.Now()
			err := next(ctx, spec, stream)
			attrs := []any{"procedure", spec.Procedure, "duration", time.Since(start).String()}
			if info, ok := connect.CallInfoForServerContext(ctx); ok {
				attrs = append(attrs, "peer", info.PeerAddr, "protocol", info.Protocol)
			}
			if err != nil {
				logger.Warn("call failed", append(attrs, "code", connect.CodeOf(err).String(), "error", err)...)
			} else {
				logger.Debug("call", attrs...)
			}
			return err
		}
	}
}

// limitInFlight refuses calls beyond n concurrent ones instead of queueing
// them, which bounds the memory requests can use.
func limitInFlight(n int) connect.ServerInterceptor {
	slots := make(chan struct{}, n)
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
				return next(ctx, spec, stream)
			default:
				return connect.NewError(connect.CodeUnavailable, "server is busy")
			}
		}
	}
}
