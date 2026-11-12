// Package auth checks API keys and applies per-project rate limits.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"golang.org/x/time/rate"

	"github.com/go-fynx/reporter/internal/config"
)

// Key identifies the API key a request was made with.
type Key struct {
	ID      string
	Project string
}

// Keys looks up API keys by their SHA-256 hash, so the service never holds
// the keys themselves.
type Keys struct {
	byHash map[[sha256.Size]byte]Key
}

func NewKeys(entries []config.APIKey) (*Keys, error) {
	k := &Keys{byHash: make(map[[sha256.Size]byte]Key, len(entries))}
	for _, e := range entries {
		b, err := hex.DecodeString(e.SHA256)
		if err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("api key %q: sha256 must be 64 hex characters", e.ID)
		}
		k.byHash[[sha256.Size]byte(b)] = Key{ID: e.ID, Project: e.Project}
	}
	return k, nil
}

func (k *Keys) Lookup(apiKey string) (Key, bool) {
	key, ok := k.byHash[sha256.Sum256([]byte(apiKey))]
	return key, ok
}

// Projects returns every project that has at least one key.
func (k *Keys) Projects() []string {
	seen := make(map[string]bool)
	var projects []string
	for _, key := range k.byHash {
		if !seen[key.Project] {
			seen[key.Project] = true
			projects = append(projects, key.Project)
		}
	}
	return projects
}

// Generate returns a new random API key and the hex SHA-256 hash that goes
// into the configuration file.
func Generate() (apiKey, sha256Hex string) {
	b := make([]byte, 32)
	rand.Read(b) // never returns an error
	apiKey = hex.EncodeToString(b)
	return apiKey, Hash(apiKey)
}

// Hash returns the hex SHA-256 of apiKey.
func Hash(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])
}

type contextKey struct{}

// NewContext returns a context carrying the authenticated key.
func NewContext(ctx context.Context, key Key) context.Context {
	return context.WithValue(ctx, contextKey{}, key)
}

// FromContext returns the key that Interceptor authenticated.
func FromContext(ctx context.Context) (Key, bool) {
	key, ok := ctx.Value(contextKey{}).(Key)
	return key, ok
}

// Interceptor rejects calls that lack a valid "Authorization: Bearer <key>"
// header. It runs before the request body is read, so unauthenticated
// clients cannot make the service decode their payloads.
func Interceptor(keys *Keys) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			info, ok := connect.CallInfoForServerContext(ctx)
			if !ok {
				return connect.NewError(connect.CodeUnauthenticated, "missing credentials")
			}
			token, ok := bearerToken(info.RequestHeader().Get("Authorization"))
			if !ok {
				return connect.NewError(connect.CodeUnauthenticated, `missing "Authorization: Bearer <api key>" header`)
			}
			key, ok := keys.Lookup(token)
			if !ok {
				return connect.NewError(connect.CodeUnauthenticated, "invalid API key")
			}
			return next(NewContext(ctx, key), spec, stream)
		}
	}
}

func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// RateLimiter limits how many events each project may submit. Limits are per
// service instance. A nil RateLimiter allows everything.
type RateLimiter struct {
	byProject map[string]*rate.Limiter
}

// NewRateLimiter returns nil, which allows everything, when eventsPerSecond
// is 0.
func NewRateLimiter(eventsPerSecond float64, burst int, projects []string) *RateLimiter {
	if eventsPerSecond <= 0 {
		return nil
	}
	r := &RateLimiter{byProject: make(map[string]*rate.Limiter, len(projects))}
	for _, p := range projects {
		r.byProject[p] = rate.NewLimiter(rate.Limit(eventsPerSecond), burst)
	}
	return r
}

// Allow reports whether project may submit n more events now. Unknown
// projects are refused.
func (r *RateLimiter) Allow(project string, n int) bool {
	if r == nil {
		return true
	}
	l, ok := r.byProject[project]
	return ok && l.AllowN(time.Now(), n)
}
