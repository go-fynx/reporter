package auth

import (
	"context"
	"slices"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"

	"github.com/go-fynx/reporter/internal/config"
	reporterv1 "github.com/go-fynx/reporter/reporter-go/proto/reporter/v1"
	"github.com/go-fynx/reporter/reporter-go/proto/reporter/v1/reporterv1connect"
)

func testKeys(t *testing.T) (*Keys, string) {
	t.Helper()
	apiKey, hash := Generate()
	keys, err := NewKeys([]config.APIKey{
		{ID: "k1", Project: "booking", SHA256: hash},
		{ID: "k2", Project: "payments", SHA256: Hash("other")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys, apiKey
}

func TestKeysLookup(t *testing.T) {
	keys, apiKey := testKeys(t)
	key, ok := keys.Lookup(apiKey)
	if !ok || key != (Key{ID: "k1", Project: "booking"}) {
		t.Errorf("Lookup(valid) = %+v, %v", key, ok)
	}
	if _, ok := keys.Lookup("wrong"); ok {
		t.Error("Lookup(wrong) succeeded")
	}
	projects := keys.Projects()
	slices.Sort(projects)
	if !slices.Equal(projects, []string{"booking", "payments"}) {
		t.Errorf("Projects() = %v", projects)
	}
}

func TestNewKeysRejectsBadHash(t *testing.T) {
	if _, err := NewKeys([]config.APIKey{{ID: "k", Project: "p", SHA256: "zz"}}); err == nil {
		t.Fatal("NewKeys accepted an invalid hash")
	}
}

func TestGenerate(t *testing.T) {
	a, hashA := Generate()
	b, _ := Generate()
	if len(a) != 64 || a == b {
		t.Errorf("Generate() keys %q and %q, want two distinct 64-char keys", a, b)
	}
	if hashA != Hash(a) || len(hashA) != 64 {
		t.Errorf("hash %q does not match Hash(key)", hashA)
	}
}

func TestBearerToken(t *testing.T) {
	tests := []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc ", "abc", true},
		{"Basic abc", "", false},
		{"Bearer ", "", false},
		{"abc", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		got, ok := bearerToken(tt.header)
		if got != tt.want || ok != tt.ok {
			t.Errorf("bearerToken(%q) = %q, %v; want %q, %v", tt.header, got, ok, tt.want, tt.ok)
		}
	}
}

func TestRateLimiter(t *testing.T) {
	unlimited := NewRateLimiter(0, 0, nil)
	if !unlimited.Allow("anything", 1_000_000) {
		t.Error("disabled limiter refused a request")
	}

	r := NewRateLimiter(0.001, 5, []string{"booking", "payments"})
	if !r.Allow("booking", 5) {
		t.Error("first burst refused")
	}
	if r.Allow("booking", 1) {
		t.Error("request beyond the burst allowed")
	}
	if !r.Allow("payments", 5) {
		t.Error("projects should not share a bucket")
	}
	if r.Allow("unknown", 1) {
		t.Error("unknown project allowed")
	}
}

type projectEcho struct {
	reporterv1connect.UnimplementedReporterServiceHandler
}

// Notify reports the authenticated project back through the service field.
func (projectEcho) Notify(ctx context.Context, _ *reporterv1.NotifyRequest) (*reporterv1.NotifyResponse, error) {
	key, ok := FromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeInternal, "no key in context")
	}
	return &reporterv1.NotifyResponse{Rejections: []*reporterv1.Rejection{{Detail: key.Project}}}, nil
}

func TestInterceptor(t *testing.T) {
	keys, apiKey := testKeys(t)
	server := connect.NewServer(Interceptor(keys))
	reporterv1connect.RegisterReporterServiceHandler(server, projectEcho{})
	client := reporterv1connect.NewReporterServiceClient(connect.NewClient(connectinprocess.New(server)))

	call := func(header string) (*reporterv1.NotifyResponse, error) {
		ctx, info := connect.NewClientContext(t.Context())
		if header != "" {
			info.RequestHeader().Set("Authorization", header)
		}
		return client.Notify(ctx, &reporterv1.NotifyRequest{})
	}

	res, err := call("Bearer " + apiKey)
	if err != nil {
		t.Fatalf("valid key: %v", err)
	}
	if got := res.GetRejections()[0].GetDetail(); got != "booking" {
		t.Errorf("handler saw project %q, want booking", got)
	}

	for _, header := range []string{"", "Bearer wrong", "Basic " + apiKey} {
		if _, err := call(header); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("header %q: code = %v, want Unauthenticated", header, connect.CodeOf(err))
		}
	}
}
