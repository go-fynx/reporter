package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/go-fynx/reporter/internal/auth"
	"github.com/go-fynx/reporter/internal/config"
	"github.com/go-fynx/reporter/internal/event"
	"github.com/go-fynx/reporter/internal/processor/memory"
	reporterv1 "github.com/go-fynx/reporter/reporter-go/proto/reporter/v1"
	"github.com/go-fynx/reporter/reporter-go/proto/reporter/v1/reporterv1connect"
)

// testBackend is an in-memory backend whose behaviour tests can control.
type testBackend struct {
	*memory.Store
	// onProcess, when set before the app starts, runs at the start of every
	// Process call.
	onProcess func(ctx context.Context)
	readyErr  atomic.Pointer[error]
	closed    atomic.Bool
}

func newTestBackend() *testBackend { return &testBackend{Store: memory.New()} }

func (b *testBackend) Process(ctx context.Context, events []event.Event) error {
	if b.onProcess != nil {
		b.onProcess(ctx)
	}
	return b.Store.Process(ctx, events)
}

func (b *testBackend) Ready(context.Context) error {
	if err := b.readyErr.Load(); err != nil {
		return *err
	}
	return nil
}

func (b *testBackend) Close(context.Context) error {
	b.closed.Store(true)
	return nil
}

func testConfig(t *testing.T) (*config.Config, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Environment = "test"
	cfg.Server.ListenAddress = "127.0.0.1:0"
	cfg.Server.Insecure = true
	cfg.Server.ShutdownTimeout = config.Duration(5 * time.Second)
	cfg.Storage.Type = config.StorageDevLog
	apiKey, hash := auth.Generate()
	cfg.APIKeys = []config.APIKey{{ID: "test", Project: "booking", SHA256: hash}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config: %v", err)
	}
	return &cfg, apiKey
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type running struct {
	app      *App
	baseURL  string
	shutdown func() error
}

func start(t *testing.T, cfg *config.Config, b backend) *running {
	t.Helper()
	a, err := build(cfg, testLogger(t), b)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return serve(t, a)
}

func serve(t *testing.T, a *App) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx) }()
	scheme := "http"
	if a.cfg.Server.TLS != nil {
		scheme = "https"
	}
	r := &running{
		app:     a,
		baseURL: scheme + "://" + a.Addr().String(),
		shutdown: sync.OnceValue(func() error {
			cancel()
			return <-done
		}),
	}
	t.Cleanup(func() { _ = r.shutdown() })
	return r
}

func h2cClient() *http.Client {
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: p}}
}

func grpcClient(httpClient *http.Client, baseURL string) reporterv1connect.ReporterServiceClient {
	return reporterv1connect.NewReporterServiceClient(
		connect.NewClient(connecthttp.NewTransport(httpClient, baseURL, connecthttp.WithGRPC())))
}

func withKey(ctx context.Context, apiKey string) context.Context {
	ctx, info := connect.NewClientContext(ctx)
	info.RequestHeader().Set("Authorization", "Bearer "+apiKey)
	return ctx
}

func notifyRequest() *reporterv1.NotifyRequest {
	return &reporterv1.NotifyRequest{
		Source: &reporterv1.Source{Service: "booking-api", Environment: "production", SdkName: "reporter-go"},
		Events: []*reporterv1.Event{{
			EventId:   uuid.NewV7().String(),
			Timestamp: timestamppb.Now(),
			Severity:  reporterv1.Severity_SEVERITY_ERROR,
			Message:   "payment failed",
			ErrorType: "*errors.errorString",
		}},
	}
}

func postJSON(t *testing.T, client *http.Client, url, apiKey, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+reporterv1connect.ReporterServiceNotifyProcedure, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode response (status %d): %v", res.StatusCode, err)
	}
	return res.StatusCode, out
}

// TestJSONClient sends the request a service in another language would write
// by hand.
func TestJSONClient(t *testing.T) {
	cfg, apiKey := testConfig(t)
	b := newTestBackend()
	r := start(t, cfg, b)

	body := fmt.Sprintf(`{
		"source": {"service": "billing-worker", "environment": "staging", "appVersion": "2.0.1"},
		"events": [
			{
				"eventId": %q,
				"timestamp": %q,
				"severity": "SEVERITY_WARNING",
				"message": "invoice retry scheduled",
				"attributes": {
					"invoice_id": {"stringValue": "inv_123"},
					"attempt": {"intValue": "2"},
					"api_token": {"stringValue": "secret-value"}
				}
			},
			{"eventId": "not-a-uuid", "timestamp": %q, "severity": "SEVERITY_INFO", "message": "x"}
		]
	}`, uuid.NewV7(), time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))

	// A field this server does not know, as a newer client might send, is ignored.
	body = strings.Replace(body, `"source": {`, `"addedInAFutureVersion": true, "source": {`, 1)

	status, res := postJSON(t, http.DefaultClient, r.baseURL, apiKey, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %v", status, res)
	}
	if res["acceptedCount"] != float64(1) {
		t.Errorf("acceptedCount = %v, want 1", res["acceptedCount"])
	}
	rejections, _ := res["rejections"].([]any)
	if len(rejections) != 1 || rejections[0].(map[string]any)["reason"] != "REJECTION_REASON_INVALID_EVENT_ID" {
		t.Errorf("rejections = %v", res["rejections"])
	}

	stored := b.Events()
	if len(stored) != 1 {
		t.Fatalf("stored %d events, want 1", len(stored))
	}
	e := stored[0]
	if e.Project != "booking" || e.Source.Service != "billing-worker" || e.Severity != event.SeverityWarning {
		t.Errorf("stored event = %+v", e)
	}
	for _, a := range e.Attributes {
		if a.Key == "api_token" && a.Value != event.Filtered {
			t.Errorf("api_token was not redacted: %v", a.Value)
		}
		if a.Key == "attempt" && a.Value != int64(2) {
			t.Errorf("attempt = %#v, want int64(2)", a.Value)
		}
	}
}

// TestJSONAcceptsSnakeCase covers two forms docs/api.md promises: proto field
// names and 64-bit integers written as JSON numbers.
func TestJSONAcceptsSnakeCase(t *testing.T) {
	cfg, apiKey := testConfig(t)
	b := newTestBackend()
	r := start(t, cfg, b)

	body := fmt.Sprintf(`{
		"source": {"service": "svc", "app_version": "1.0.0"},
		"events": [{"event_id": %q, "timestamp": %q, "severity": "SEVERITY_INFO", "error_type": "TypeError",
			"attributes": {"attempt": {"int_value": 7}}}]
	}`, uuid.NewV7(), time.Now().UTC().Format(time.RFC3339))
	status, res := postJSON(t, http.DefaultClient, r.baseURL, apiKey, body)
	if status != http.StatusOK || res["acceptedCount"] != float64(1) {
		t.Fatalf("got %d %v", status, res)
	}
	e := b.Events()[0]
	if e.Source.AppVersion != "1.0.0" || e.ErrorType != "TypeError" || e.Attributes[0].Value != int64(7) {
		t.Errorf("stored event = %+v", e)
	}
}

func TestJSONClientErrors(t *testing.T) {
	cfg, apiKey := testConfig(t)
	cfg.Limits.MaxRequestBytes = 2048
	r := start(t, cfg, newTestBackend())

	tests := []struct {
		name       string
		apiKey     string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"missing key", "", `{}`, http.StatusUnauthorized, "unauthenticated"},
		{"wrong key", "nope", `{}`, http.StatusUnauthorized, "unauthenticated"},
		{"malformed JSON", apiKey, `{"source":`, http.StatusBadRequest, "invalid_argument"},
		// Unknown fields are ignored so newer clients work with older servers;
		// this body is refused only because it has no source.
		{"only unknown fields", apiKey, `{"sauce": {}}`, http.StatusBadRequest, "invalid_argument"},
		{"no events", apiKey, `{"source": {"service": "svc"}}`, http.StatusBadRequest, "invalid_argument"},
		{"too large", apiKey, `{"source": {"service": "svc"}, "events": [{"message": "` + strings.Repeat("x", 4096) + `"}]}`,
			http.StatusTooManyRequests, "resource_exhausted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, res := postJSON(t, http.DefaultClient, r.baseURL, tt.apiKey, tt.body)
			if status != tt.wantStatus || res["code"] != tt.wantCode {
				t.Errorf("got %d %v, want %d %q", status, res, tt.wantStatus, tt.wantCode)
			}
		})
	}
}

func TestGRPCClient(t *testing.T) {
	cfg, apiKey := testConfig(t)
	b := newTestBackend()
	var protocol atomic.Value
	b.onProcess = func(ctx context.Context) {
		if info, ok := connect.CallInfoForServerContext(ctx); ok {
			protocol.Store(info.Protocol)
		}
	}
	r := start(t, cfg, b)
	client := grpcClient(h2cClient(), r.baseURL)

	res, err := client.Notify(withKey(t.Context(), apiKey), notifyRequest())
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if res.GetAcceptedCount() != 1 || b.Count("booking") != 1 {
		t.Errorf("accepted %d, stored %d", res.GetAcceptedCount(), b.Count("booking"))
	}
	if protocol.Load() != connect.ProtocolNameGRPC {
		t.Errorf("server saw protocol %v, want gRPC", protocol.Load())
	}

	_, err = client.Notify(withKey(t.Context(), "wrong"), notifyRequest())
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("wrong key: code = %v", connect.CodeOf(err))
	}

	big := notifyRequest()
	big.Events[0].Message = strings.Repeat("x", cfg.Limits.MaxRequestBytes+1)
	_, err = client.Notify(withKey(t.Context(), apiKey), big)
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("oversized request: code = %v", connect.CodeOf(err))
	}
}

func TestTLS(t *testing.T) {
	cfg, apiKey := testConfig(t)
	certFile, keyFile, pool := writeTestCert(t)
	cfg.Server.Insecure = false
	cfg.Server.TLS = &config.TLS{CertFile: certFile, KeyFile: keyFile}
	b := newTestBackend()
	r := start(t, cfg, b)

	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetHTTP2(true)
	tlsClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, Protocols: p}}

	if _, err := grpcClient(tlsClient, r.baseURL).Notify(withKey(t.Context(), apiKey), notifyRequest()); err != nil {
		t.Fatalf("gRPC over TLS: %v", err)
	}
	status, res := postJSON(t, tlsClient, r.baseURL, apiKey, `{"source": {"service": "svc"}, "events": []}`)
	if status != http.StatusBadRequest || res["code"] != "invalid_argument" {
		t.Errorf("JSON over TLS: %d %v", status, res)
	}

	plain, err := http.Post("http://"+r.app.Addr().String()+reporterv1connect.ReporterServiceNotifyProcedure, "application/json", strings.NewReader("{}"))
	if err == nil {
		plain.Body.Close()
		if plain.StatusCode == http.StatusOK {
			t.Error("plaintext request to the TLS port succeeded")
		}
	}
	if b.Count("booking") != 1 {
		t.Errorf("stored %d events, want 1", b.Count("booking"))
	}
}

func TestBuildFailsWithMissingCertificate(t *testing.T) {
	cfg, _ := testConfig(t)
	cfg.Server.Insecure = false
	cfg.Server.TLS = &config.TLS{CertFile: "missing.crt", KeyFile: "missing.key"}
	if _, err := build(cfg, testLogger(t), newTestBackend()); err == nil {
		t.Fatal("build succeeded without a certificate")
	}
}

func TestInFlightLimit(t *testing.T) {
	cfg, apiKey := testConfig(t)
	cfg.Limits.MaxInFlightRequests = 1
	b := newTestBackend()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	b.onProcess = func(ctx context.Context) {
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	}
	r := start(t, cfg, b)
	client := grpcClient(h2cClient(), r.baseURL)

	first := make(chan error, 1)
	go func() {
		_, err := client.Notify(withKey(context.Background(), apiKey), notifyRequest())
		first <- err
	}()
	<-entered

	_, err := client.Notify(withKey(t.Context(), apiKey), notifyRequest())
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("second call: code = %v, want Unavailable", connect.CodeOf(err))
	}
	close(release)
	if err := <-first; err != nil {
		t.Errorf("first call: %v", err)
	}
	if _, err := client.Notify(withKey(t.Context(), apiKey), notifyRequest()); err != nil {
		t.Errorf("call after the slot was freed: %v", err)
	}
}

func TestHealthAndReadiness(t *testing.T) {
	cfg, _ := testConfig(t)
	b := newTestBackend()
	r := start(t, cfg, b)

	get := func(path string) int {
		res, err := http.Get(r.baseURL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if got := get("/healthz"); got != http.StatusOK {
		t.Errorf("/healthz = %d", got)
	}
	if got := get("/readyz"); got != http.StatusOK {
		t.Errorf("/readyz = %d", got)
	}
	storageDown := errors.New("storage down")
	b.readyErr.Store(&storageDown)
	if got := get("/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("/readyz with storage down = %d, want 503", got)
	}
	if got := get("/healthz"); got != http.StatusOK {
		t.Errorf("/healthz with storage down = %d, want 200", got)
	}
	if got := get("/nothing-here"); got != http.StatusNotFound {
		t.Errorf("unknown path = %d, want 404", got)
	}
}

func TestPanicIsRecovered(t *testing.T) {
	cfg, apiKey := testConfig(t)
	b := newTestBackend()
	var calls atomic.Int32
	b.onProcess = func(context.Context) {
		if calls.Add(1) == 1 {
			panic("boom")
		}
	}
	r := start(t, cfg, b)
	client := grpcClient(h2cClient(), r.baseURL)

	if _, err := client.Notify(withKey(t.Context(), apiKey), notifyRequest()); connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("panicking call: code = %v, want Internal", connect.CodeOf(err))
	}
	if _, err := client.Notify(withKey(t.Context(), apiKey), notifyRequest()); err != nil {
		t.Errorf("call after the panic: %v", err)
	}
}

func TestGracefulShutdown(t *testing.T) {
	cfg, apiKey := testConfig(t)
	b := newTestBackend()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	b.onProcess = func(context.Context) {
		entered <- struct{}{}
		<-release
	}
	r := start(t, cfg, b)
	client := grpcClient(h2cClient(), r.baseURL)

	inFlight := make(chan error, 1)
	go func() {
		_, err := client.Notify(withKey(context.Background(), apiKey), notifyRequest())
		inFlight <- err
	}()
	<-entered

	stopped := make(chan error, 1)
	go func() { stopped <- r.shutdown() }()
	waitFor(t, "readiness to turn off", func() bool { return !r.app.ready.Load() })
	waitFor(t, "new connections to be refused", func() bool {
		c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: time.Second}
		res, err := c.Get(r.baseURL + "/healthz")
		if err == nil {
			res.Body.Close()
		}
		return err != nil
	})

	close(release)
	if err := <-inFlight; err != nil {
		t.Errorf("in-flight call failed during shutdown: %v", err)
	}
	if err := <-stopped; err != nil {
		t.Errorf("Serve returned %v, want nil after a graceful shutdown", err)
	}
	if b.Count("booking") != 1 {
		t.Error("the in-flight event was not stored")
	}
	if !b.closed.Load() {
		t.Error("storage was not closed")
	}
}

func TestShutdownTimeout(t *testing.T) {
	cfg, apiKey := testConfig(t)
	cfg.Server.ShutdownTimeout = config.Duration(200 * time.Millisecond)
	b := newTestBackend()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	b.onProcess = func(ctx context.Context) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	r := start(t, cfg, b)
	go func() {
		_, _ = grpcClient(h2cClient(), r.baseURL).Notify(withKey(context.Background(), apiKey), notifyRequest())
	}()
	<-entered

	begin := time.Now()
	if err := r.shutdown(); err == nil {
		t.Error("Serve returned nil although a request was cut off")
	}
	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Errorf("shutdown took %v, want about the 200ms timeout", elapsed)
	}
	if !b.closed.Load() {
		t.Error("storage was not closed")
	}
}

// TestServeWithMongoDB runs the whole service against a real MongoDB.
func TestServeWithMongoDB(t *testing.T) {
	uri := os.Getenv("REPORTER_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("REPORTER_TEST_MONGO_URI is not set")
	}
	cfg, apiKey := testConfig(t)
	cfg.Storage.Type = config.StorageMongoDB
	cfg.Storage.MongoDB.URI = uri
	cfg.Storage.MongoDB.Database = "reporter_test_" + uuid.NewV4().String()[:8]

	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	db := client.Database(cfg.Storage.MongoDB.Database)
	t.Cleanup(func() {
		_ = db.Drop(context.Background())
		_ = client.Disconnect(context.Background())
	})

	a, err := New(t.Context(), cfg, testLogger(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r := serve(t, a)

	status, res := postJSON(t, http.DefaultClient, r.baseURL, apiKey, fmt.Sprintf(
		`{"source": {"service": "svc"}, "events": [{"eventId": %q, "timestamp": %q, "severity": "SEVERITY_ERROR", "message": "stored"}]}`,
		uuid.NewV7(), time.Now().UTC().Format(time.RFC3339)))
	if status != http.StatusOK || res["acceptedCount"] != float64(1) {
		t.Fatalf("POST: %d %v", status, res)
	}
	readyRes, err := http.Get(r.baseURL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	readyRes.Body.Close()
	if readyRes.StatusCode != http.StatusOK {
		t.Errorf("/readyz = %d", readyRes.StatusCode)
	}

	n, err := db.Collection("events").CountDocuments(t.Context(), bson.D{{Key: "project", Value: "booking"}, {Key: "message", Value: "stored"}})
	if err != nil || n != 1 {
		t.Errorf("documents = %d, %v; want 1", n, err)
	}
	if err := r.shutdown(); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// writeTestCert writes a self-signed certificate for 127.0.0.1 and returns
// its files and a pool that trusts it.
func writeTestCert(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "reporter-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "server.crt")
	keyFile = filepath.Join(dir, "server.key")
	writePEM(t, certFile, "CERTIFICATE", der)
	writePEM(t, keyFile, "PRIVATE KEY", keyDER)

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
