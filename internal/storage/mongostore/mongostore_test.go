package mongostore

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
	"uuid"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/go-fynx/reporter/internal/event"
	"github.com/go-fynx/reporter/internal/processor/processortest"
)

var testRetention = map[event.Severity]time.Duration{
	event.SeverityError:   90 * 24 * time.Hour,
	event.SeverityWarning: 30 * 24 * time.Hour,
	event.SeverityInfo:    14 * 24 * time.Hour,
}

// openTestStore opens a Store on a fresh database that is dropped when the
// test ends. It skips the test unless REPORTER_TEST_MONGO_URI is set.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("REPORTER_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("REPORTER_TEST_MONGO_URI is not set")
	}
	s, err := Open(t.Context(), Options{
		URI:            uri,
		Database:       "reporter_test_" + uuid.NewV4().String()[:8],
		Collection:     "events",
		WriteConcern:   "majority",
		ConnectTimeout: 5 * time.Second,
		MaxPoolSize:    10,
		Retention:      testRetention,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if err := s.collection.Database().Drop(ctx); err != nil {
			t.Errorf("drop test database: %v", err)
		}
		_ = s.Close(ctx)
	})
	return s
}

func TestContract(t *testing.T) {
	s := openTestStore(t)
	processortest.Run(t, s, func(t *testing.T, project string) int {
		n, err := s.collection.CountDocuments(t.Context(), bson.D{{Key: "project", Value: project}})
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		return int(n)
	})
}

func TestOpenCreatesIndexes(t *testing.T) {
	s := openTestStore(t)
	cursor, err := s.collection.Indexes().List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var specs []bson.M
	if err := cursor.All(t.Context(), &specs); err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]bson.M)
	for _, spec := range specs {
		byName[spec["name"].(string)] = spec
	}
	for _, name := range []string{"project_event_id", "project_service_environment_severity_timestamp", "attributes_k_v", "expire_at_ttl"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("index %q missing; have %v", name, byName)
		}
	}
	if unique, _ := byName["project_event_id"]["unique"].(bool); !unique {
		t.Error("project_event_id index is not unique")
	}
	if ttl, ok := byName["expire_at_ttl"]["expireAfterSeconds"]; !ok || ttl != int32(0) {
		t.Errorf("expire_at_ttl expireAfterSeconds = %v, want 0", ttl)
	}

	// Opening again against the same collection must not fail.
	again := reopen(t, s)
	_ = again.Close(t.Context())
}

// reopen opens a second Store on the same database as s.
func reopen(t *testing.T, s *Store) *Store {
	t.Helper()
	again, err := Open(t.Context(), Options{
		URI: os.Getenv("REPORTER_TEST_MONGO_URI"), Database: s.collection.Database().Name(), Collection: "events",
		WriteConcern: "1", ConnectTimeout: 5 * time.Second, MaxPoolSize: 1, Retention: testRetention,
	})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	return again
}

func TestStoredDocument(t *testing.T) {
	s := openTestStore(t)
	e := processortest.NewEvent("booking", event.SeverityError)
	e.Source = event.Source{
		Service: "booking-api", Environment: "production", AppVersion: "1.4.2", Hostname: "web-1",
		OS: "linux", RuntimeVersion: "go1.27.1", SDKName: "reporter-go", SDKVersion: "0.1.0",
	}
	e.Unhandled = true
	e.ErrorType = "*fs.PathError"
	e.Stack = []event.Frame{{Function: "main.open", File: "main.go", Line: 7, InProject: true}}
	e.Causes = []event.Cause{{ErrorType: "syscall.Errno", Message: "no such file", Stack: e.Stack}}
	e.Attributes = []event.Attribute{
		{Key: "attempt", Value: int64(3)},
		{Key: "path", Value: "/tmp/x"},
		{Key: "ratio", Value: 0.5},
		{Key: "retry", Value: true},
	}
	if err := s.Process(t.Context(), []event.Event{e}); err != nil {
		t.Fatalf("Process: %v", err)
	}

	var doc document
	if err := s.collection.FindOne(t.Context(), bson.D{{Key: "event_id", Value: e.EventID}}).Decode(&doc); err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if doc.Project != "booking" || doc.Service != "booking-api" || doc.SDKVersion != "0.1.0" || doc.Severity != "error" || !doc.Unhandled {
		t.Errorf("document = %+v", doc)
	}
	if len(doc.Stack) != 1 || doc.Stack[0].Line != 7 || len(doc.Causes) != 1 || len(doc.Causes[0].Stack) != 1 {
		t.Errorf("stack/causes = %+v / %+v", doc.Stack, doc.Causes)
	}
	wantAttrs := map[string]any{"attempt": int64(3), "path": "/tmp/x", "ratio": 0.5, "retry": true}
	if len(doc.Attributes) != len(wantAttrs) {
		t.Fatalf("attributes = %+v", doc.Attributes)
	}
	for _, a := range doc.Attributes {
		if wantAttrs[a.K] != a.V {
			t.Errorf("attribute %s = %#v, want %#v", a.K, a.V, wantAttrs[a.K])
		}
	}
	// BSON dates have millisecond precision.
	wantExpire := e.ReceivedAt.Add(90 * 24 * time.Hour).Truncate(time.Millisecond)
	if !doc.ExpireAt.Equal(wantExpire) {
		t.Errorf("expire_at = %v, want %v", doc.ExpireAt, wantExpire)
	}
}

func TestRetentionPerSeverity(t *testing.T) {
	s := openTestStore(t)
	for sev, keep := range testRetention {
		e := processortest.NewEvent("booking", sev)
		if err := s.Process(t.Context(), []event.Event{e}); err != nil {
			t.Fatal(err)
		}
		var doc document
		if err := s.collection.FindOne(t.Context(), bson.D{{Key: "event_id", Value: e.EventID}}).Decode(&doc); err != nil {
			t.Fatal(err)
		}
		if got := doc.ExpireAt.Sub(doc.ReceivedAt).Round(time.Second); got != keep {
			t.Errorf("%s events kept for %v, want %v", sev, got, keep)
		}
	}
}

func TestProcessFailsAfterClose(t *testing.T) {
	closed := reopen(t, openTestStore(t))
	if err := closed.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := closed.Process(t.Context(), []event.Event{processortest.NewEvent("booking", event.SeverityInfo)}); err == nil {
		t.Error("Process after Close succeeded")
	}
	if err := closed.Ready(t.Context()); err == nil {
		t.Error("Ready after Close succeeded")
	}
}

func TestOpenFailsWhenUnreachable(t *testing.T) {
	start := time.Now()
	_, err := Open(t.Context(), Options{
		URI: "mongodb://127.0.0.1:1/?connectTimeoutMS=200", Database: "x", Collection: "events",
		WriteConcern: "majority", ConnectTimeout: 300 * time.Millisecond, MaxPoolSize: 1, Retention: testRetention,
	})
	if err == nil {
		t.Fatal("Open succeeded against a closed port")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Open took %v to fail, want it bounded by the connect timeout", elapsed)
	}
}

func TestOpenRejectsBadOptions(t *testing.T) {
	if _, err := Open(t.Context(), Options{WriteConcern: "all", Retention: testRetention}); err == nil {
		t.Error("accepted write concern \"all\"")
	}
	if _, err := Open(t.Context(), Options{WriteConcern: "1"}); err == nil {
		t.Error("accepted missing retention")
	}
}

func TestParseWriteConcern(t *testing.T) {
	wc, err := parseWriteConcern("majority")
	if err != nil || wc.W != "majority" || wc.Journal == nil || !*wc.Journal {
		t.Errorf("majority = %+v, %v", wc, err)
	}
	wc, err = parseWriteConcern("2")
	if err != nil || wc.W != 2 {
		t.Errorf("2 = %+v, %v", wc, err)
	}
	for _, bad := range []string{"", "0", "-1", "all"} {
		if _, err := parseWriteConcern(bad); err == nil {
			t.Errorf("parseWriteConcern(%q) succeeded", bad)
		}
	}
}

func TestOnlyDuplicates(t *testing.T) {
	dup := mongo.BulkWriteError{WriteError: mongo.WriteError{Code: duplicateKeyCode}}
	other := mongo.BulkWriteError{WriteError: mongo.WriteError{Code: 121}}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"all duplicates", mongo.BulkWriteException{WriteErrors: []mongo.BulkWriteError{dup, dup}}, true},
		{"mixed", mongo.BulkWriteException{WriteErrors: []mongo.BulkWriteError{dup, other}}, false},
		{"write concern failed", mongo.BulkWriteException{
			WriteErrors:       []mongo.BulkWriteError{dup},
			WriteConcernError: &mongo.WriteConcernError{Code: 64},
		}, false},
		{"no write errors", mongo.BulkWriteException{}, false},
		{"other error", errors.New("network"), false},
	}
	for _, tt := range tests {
		if got := onlyDuplicates(tt.err); got != tt.want {
			t.Errorf("%s: onlyDuplicates = %v, want %v", tt.name, got, tt.want)
		}
	}
}
