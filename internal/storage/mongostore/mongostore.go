// Package mongostore stores events in MongoDB.
//
// Each event is one document. Attributes are stored as a list of {k, v}
// pairs so any key works and one index covers every attribute. A TTL index on
// expire_at deletes events once their severity's retention has passed.
package mongostore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"

	"github.com/go-fynx/reporter/internal/event"
)

// duplicateKeyCode is MongoDB's error code for a unique index violation.
const duplicateKeyCode = 11000

type Options struct {
	URI        string
	Database   string
	Collection string
	// WriteConcern is "majority" or a number of nodes such as "1".
	WriteConcern   string
	ConnectTimeout time.Duration
	MaxPoolSize    uint64
	// Retention is how long events of each severity are kept.
	Retention map[event.Severity]time.Duration
}

type Store struct {
	client     *mongo.Client
	collection *mongo.Collection
	retention  map[event.Severity]time.Duration
}

// Open connects to MongoDB, checks that it is reachable and creates the
// indexes the service needs. Creating indexes that already exist is a no-op.
func Open(ctx context.Context, opts Options) (*Store, error) {
	wc, err := parseWriteConcern(opts.WriteConcern)
	if err != nil {
		return nil, err
	}
	for _, sev := range []event.Severity{event.SeverityInfo, event.SeverityWarning, event.SeverityError} {
		if opts.Retention[sev] <= 0 {
			return nil, fmt.Errorf("no retention set for %s events", sev)
		}
	}

	client, err := mongo.Connect(options.Client().
		ApplyURI(opts.URI).
		SetAppName("reporter").
		SetConnectTimeout(opts.ConnectTimeout).
		SetServerSelectionTimeout(opts.ConnectTimeout).
		SetMaxPoolSize(opts.MaxPoolSize))
	if err != nil {
		return nil, fmt.Errorf("mongodb: %w", err)
	}
	s := &Store{
		client:     client,
		collection: client.Database(opts.Database).Collection(opts.Collection, options.Collection().SetWriteConcern(wc)),
		retention:  opts.Retention,
	}

	ctx, cancel := context.WithTimeout(ctx, opts.ConnectTimeout)
	defer cancel()
	if err := s.Ready(ctx); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("mongodb is not reachable: %w", err)
	}
	if err := s.ensureIndexes(ctx); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, err
	}
	return s, nil
}

func parseWriteConcern(s string) (*writeconcern.WriteConcern, error) {
	journal := true
	if s == "majority" {
		return &writeconcern.WriteConcern{W: "majority", Journal: &journal}, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return nil, fmt.Errorf(`write concern must be "majority" or a positive number, got %q`, s)
	}
	return &writeconcern.WriteConcern{W: n, Journal: &journal}, nil
}

// Index names are part of the deployed schema; changing one creates a second
// index instead of replacing the first.
var indexes = []mongo.IndexModel{
	{
		Keys:    bson.D{{Key: "project", Value: 1}, {Key: "event_id", Value: 1}},
		Options: options.Index().SetName("project_event_id").SetUnique(true),
	},
	{
		Keys: bson.D{
			{Key: "project", Value: 1}, {Key: "service", Value: 1}, {Key: "environment", Value: 1},
			{Key: "severity", Value: 1}, {Key: "timestamp", Value: -1},
		},
		Options: options.Index().SetName("project_service_environment_severity_timestamp"),
	},
	{
		Keys:    bson.D{{Key: "attributes.k", Value: 1}, {Key: "attributes.v", Value: 1}},
		Options: options.Index().SetName("attributes_k_v"),
	},
	{
		Keys:    bson.D{{Key: "expire_at", Value: 1}},
		Options: options.Index().SetName("expire_at_ttl").SetExpireAfterSeconds(0),
	},
}

func (s *Store) ensureIndexes(ctx context.Context) error {
	if _, err := s.collection.Indexes().CreateMany(ctx, indexes); err != nil {
		return fmt.Errorf("create mongodb indexes: %w", err)
	}
	return nil
}

// Process inserts events. Events whose (project, event_id) is already stored
// are skipped, so a client resending an event never creates a second copy.
func (s *Store) Process(ctx context.Context, events []event.Event) error {
	docs := make([]any, len(events))
	for i, e := range events {
		docs[i] = s.toDocument(e)
	}
	_, err := s.collection.InsertMany(ctx, docs, options.InsertMany().SetOrdered(false))
	if err == nil || onlyDuplicates(err) {
		return nil
	}
	return fmt.Errorf("insert events: %w", err)
}

// onlyDuplicates reports whether err means every failed insert was already
// stored and the write concern was met for the rest.
func onlyDuplicates(err error) bool {
	var bwe mongo.BulkWriteException
	if !errors.As(err, &bwe) || bwe.WriteConcernError != nil || len(bwe.WriteErrors) == 0 {
		return false
	}
	for _, we := range bwe.WriteErrors {
		if we.Code != duplicateKeyCode {
			return false
		}
	}
	return true
}

// Ready reports whether the primary is reachable.
func (s *Store) Ready(ctx context.Context) error {
	return s.client.Ping(ctx, readpref.Primary())
}

func (s *Store) Close(ctx context.Context) error {
	return s.client.Disconnect(ctx)
}

type document struct {
	EventID        string         `bson:"event_id"`
	Project        string         `bson:"project"`
	Service        string         `bson:"service"`
	Environment    string         `bson:"environment"`
	AppVersion     string         `bson:"app_version,omitempty"`
	Hostname       string         `bson:"hostname,omitempty"`
	OS             string         `bson:"os,omitempty"`
	RuntimeVersion string         `bson:"runtime_version,omitempty"`
	SDKName        string         `bson:"sdk_name,omitempty"`
	SDKVersion     string         `bson:"sdk_version,omitempty"`
	Timestamp      time.Time      `bson:"timestamp"`
	ReceivedAt     time.Time      `bson:"received_at"`
	ExpireAt       time.Time      `bson:"expire_at"`
	Severity       string         `bson:"severity"`
	Unhandled      bool           `bson:"unhandled"`
	Message        string         `bson:"message,omitempty"`
	ErrorType      string         `bson:"error_type,omitempty"`
	Stack          []frameDoc     `bson:"stack,omitempty"`
	Causes         []causeDoc     `bson:"causes,omitempty"`
	Attributes     []attributeDoc `bson:"attributes,omitempty"`
}

type frameDoc struct {
	Function  string `bson:"function"`
	File      string `bson:"file"`
	Line      int    `bson:"line"`
	InProject bool   `bson:"in_project"`
}

type causeDoc struct {
	ErrorType string     `bson:"error_type,omitempty"`
	Message   string     `bson:"message,omitempty"`
	Stack     []frameDoc `bson:"stack,omitempty"`
}

type attributeDoc struct {
	K string `bson:"k"`
	V any    `bson:"v"`
}

func (s *Store) toDocument(e event.Event) document {
	d := document{
		EventID:        e.EventID,
		Project:        e.Project,
		Service:        e.Source.Service,
		Environment:    e.Source.Environment,
		AppVersion:     e.Source.AppVersion,
		Hostname:       e.Source.Hostname,
		OS:             e.Source.OS,
		RuntimeVersion: e.Source.RuntimeVersion,
		SDKName:        e.Source.SDKName,
		SDKVersion:     e.Source.SDKVersion,
		Timestamp:      e.Timestamp,
		ReceivedAt:     e.ReceivedAt,
		ExpireAt:       e.ReceivedAt.Add(s.retention[e.Severity]),
		Severity:       e.Severity.String(),
		Unhandled:      e.Unhandled,
		Message:        e.Message,
		ErrorType:      e.ErrorType,
		Stack:          toFrames(e.Stack),
	}
	if len(e.Causes) > 0 {
		d.Causes = make([]causeDoc, len(e.Causes))
		for i, c := range e.Causes {
			d.Causes[i] = causeDoc{ErrorType: c.ErrorType, Message: c.Message, Stack: toFrames(c.Stack)}
		}
	}
	if len(e.Attributes) > 0 {
		d.Attributes = make([]attributeDoc, len(e.Attributes))
		for i, a := range e.Attributes {
			d.Attributes[i] = attributeDoc{K: a.Key, V: a.Value}
		}
	}
	return d
}

func toFrames(frames []event.Frame) []frameDoc {
	if len(frames) == 0 {
		return nil
	}
	out := make([]frameDoc, len(frames))
	for i, f := range frames {
		out[i] = frameDoc{Function: f.Function, File: f.File, Line: f.Line, InProject: f.InProject}
	}
	return out
}
