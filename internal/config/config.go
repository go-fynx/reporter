// Package config loads and validates the reporter service's JSON
// configuration file.
package config

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/go-fynx/reporter/internal/event"
)

// Production is the environment name that turns on the strictest checks.
const Production = "production"

const (
	StorageMongoDB = "mongodb"
	StorageDevLog  = "devlog"
)

type Config struct {
	// Environment of the reporter service itself, not of the services that
	// report to it. "production" forbids plaintext and the devlog storage.
	Environment string    `json:"environment"`
	Server      Server    `json:"server"`
	Limits      Limits    `json:"limits"`
	RateLimit   RateLimit `json:"rate_limit"`
	Redaction   Redaction `json:"redaction"`
	APIKeys     []APIKey  `json:"api_keys"`
	Storage     Storage   `json:"storage"`
	Log         Log       `json:"log"`
}

type Server struct {
	ListenAddress string `json:"listen_address"`
	// TLS must be set unless Insecure is true.
	TLS *TLS `json:"tls"`
	// Insecure serves plaintext HTTP/1.1 and HTTP/2 (h2c). For local
	// development only; refused in production.
	Insecure          bool     `json:"insecure"`
	ReadHeaderTimeout Duration `json:"read_header_timeout"`
	ReadTimeout       Duration `json:"read_timeout"`
	WriteTimeout      Duration `json:"write_timeout"`
	IdleTimeout       Duration `json:"idle_timeout"`
	ShutdownTimeout   Duration `json:"shutdown_timeout"`
}

type TLS struct {
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

type Limits struct {
	MaxRequestBytes       int      `json:"max_request_bytes"`
	MaxEventsPerRequest   int      `json:"max_events_per_request"`
	MaxInFlightRequests   int      `json:"max_in_flight_requests"`
	MaxAttributes         int      `json:"max_attributes"`
	MaxAttributeKeyLength int      `json:"max_attribute_key_length"`
	MaxCauses             int      `json:"max_causes"`
	MaxStackFrames        int      `json:"max_stack_frames"`
	MaxClockSkew          Duration `json:"max_clock_skew"`
	ProcessingTimeout     Duration `json:"processing_timeout"`
}

// RateLimit applies to each project separately, on each service instance.
type RateLimit struct {
	// EventsPerSecond of 0 turns rate limiting off.
	EventsPerSecond float64 `json:"events_per_second"`
	Burst           int     `json:"burst"`
}

type Redaction struct {
	// Filters are matched as case-insensitive substrings of attribute keys.
	Filters []string `json:"filters"`
}

// APIKey grants one project the right to submit events. Only the SHA-256 of
// the key is stored; `reporter keygen` prints both.
type APIKey struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	SHA256  string `json:"sha256"`
}

type Storage struct {
	// Type is "mongodb", or "devlog" to only log events during development.
	Type          string        `json:"type"`
	MongoDB       MongoDB       `json:"mongodb"`
	RetentionDays RetentionDays `json:"retention_days"`
}

type MongoDB struct {
	URI        string `json:"uri"`
	Database   string `json:"database"`
	Collection string `json:"collection"`
	// WriteConcern is "majority" or a number of nodes such as "1". Writes
	// always wait for the journal.
	WriteConcern   string   `json:"write_concern"`
	ConnectTimeout Duration `json:"connect_timeout"`
	MaxPoolSize    uint64   `json:"max_pool_size"`
}

// RetentionDays is how long events of each severity are kept.
type RetentionDays struct {
	Error   int `json:"error"`
	Warning int `json:"warning"`
	Info    int `json:"info"`
}

type Log struct {
	// Level is "debug", "info", "warn" or "error".
	Level string `json:"level"`
}

// Default returns the configuration used for every field the file leaves out.
func Default() Config {
	return Config{
		Environment: Production,
		Server: Server{
			ListenAddress:     ":8443",
			ReadHeaderTimeout: Duration(5 * time.Second),
			ReadTimeout:       Duration(30 * time.Second),
			WriteTimeout:      Duration(30 * time.Second),
			IdleTimeout:       Duration(2 * time.Minute),
			ShutdownTimeout:   Duration(30 * time.Second),
		},
		Limits: Limits{
			MaxRequestBytes:       1 << 20,
			MaxEventsPerRequest:   100,
			MaxInFlightRequests:   256,
			MaxAttributes:         100,
			MaxAttributeKeyLength: 128,
			MaxCauses:             32,
			MaxStackFrames:        256,
			MaxClockSkew:          Duration(24 * time.Hour),
			ProcessingTimeout:     Duration(10 * time.Second),
		},
		RateLimit: RateLimit{EventsPerSecond: 100, Burst: 1000},
		Redaction: Redaction{Filters: []string{
			"password", "passwd", "secret", "token", "authorization",
			"cookie", "api_key", "apikey", "credential",
		}},
		Storage: Storage{
			Type: StorageMongoDB,
			MongoDB: MongoDB{
				Database:       "reporter",
				Collection:     "events",
				WriteConcern:   "majority",
				ConnectTimeout: Duration(10 * time.Second),
				MaxPoolSize:    100,
			},
			RetentionDays: RetentionDays{Error: 90, Warning: 90, Info: 14},
		},
		Log: Log{Level: "info"},
	}
}

// Load reads the JSON file at path over the defaults and validates the
// result. Unknown fields are an error, so typos do not go unnoticed.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes and validates a configuration file's contents.
func Parse(data []byte) (*Config, error) {
	cfg := Default()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if dec.More() {
		return nil, errors.New("invalid JSON: unexpected data after the top-level object")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate reports every problem in the configuration at once.
func (c *Config) Validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}

	check(event.ValidName(c.Environment), "environment %q is not a valid name", c.Environment)
	production := c.Environment == Production

	s := c.Server
	check(s.ListenAddress != "", "server.listen_address is required")
	switch {
	case s.TLS == nil && !s.Insecure:
		errs = append(errs, errors.New("set server.tls, or server.insecure for local development"))
	case s.TLS != nil && s.Insecure:
		errs = append(errs, errors.New("server.tls and server.insecure cannot both be set"))
	case s.TLS != nil:
		check(s.TLS.CertFile != "" && s.TLS.KeyFile != "", "server.tls needs cert_file and key_file")
	}
	check(!(production && s.Insecure), "server.insecure is not allowed when environment is %q", Production)
	check(s.ReadHeaderTimeout > 0, "server.read_header_timeout must be positive")
	check(s.ReadTimeout > 0, "server.read_timeout must be positive")
	check(s.WriteTimeout > 0, "server.write_timeout must be positive")
	check(s.IdleTimeout > 0, "server.idle_timeout must be positive")
	check(s.ShutdownTimeout > 0, "server.shutdown_timeout must be positive")

	l := c.Limits
	check(l.MaxRequestBytes > 0, "limits.max_request_bytes must be positive")
	check(l.MaxEventsPerRequest > 0, "limits.max_events_per_request must be positive")
	check(l.MaxInFlightRequests > 0, "limits.max_in_flight_requests must be positive")
	check(l.MaxAttributes > 0, "limits.max_attributes must be positive")
	check(l.MaxAttributeKeyLength > 0, "limits.max_attribute_key_length must be positive")
	check(l.MaxCauses > 0, "limits.max_causes must be positive")
	check(l.MaxStackFrames > 0, "limits.max_stack_frames must be positive")
	check(l.MaxClockSkew > 0, "limits.max_clock_skew must be positive")
	check(l.ProcessingTimeout > 0, "limits.processing_timeout must be positive")

	r := c.RateLimit
	check(r.EventsPerSecond >= 0, "rate_limit.events_per_second cannot be negative")
	if r.EventsPerSecond > 0 {
		check(r.Burst >= l.MaxEventsPerRequest,
			"rate_limit.burst (%d) must be at least limits.max_events_per_request (%d), or full requests would always be refused",
			r.Burst, l.MaxEventsPerRequest)
	}

	for i, f := range c.Redaction.Filters {
		check(f != "", "redaction.filters[%d] is empty", i)
	}

	check(len(c.APIKeys) > 0, "api_keys needs at least one key; create one with `reporter keygen`")
	ids := make(map[string]bool)
	hashes := make(map[string]bool)
	for i, k := range c.APIKeys {
		check(k.ID != "", "api_keys[%d].id is required", i)
		check(!ids[k.ID], "api_keys[%d].id %q is used more than once", i, k.ID)
		ids[k.ID] = true
		check(event.ValidName(k.Project), "api_keys[%d].project %q is not a valid name", i, k.Project)
		b, err := hex.DecodeString(k.SHA256)
		check(err == nil && len(b) == 32, "api_keys[%d].sha256 must be 64 hex characters", i)
		check(!hashes[k.SHA256], "api_keys[%d].sha256 is used more than once", i)
		hashes[k.SHA256] = true
	}

	st := c.Storage
	switch st.Type {
	case StorageMongoDB:
		m := st.MongoDB
		check(m.URI != "", "storage.mongodb.uri is required")
		check(m.Database != "", "storage.mongodb.database is required")
		check(m.Collection != "", "storage.mongodb.collection is required")
		check(validWriteConcern(m.WriteConcern), `storage.mongodb.write_concern must be "majority" or a positive number`)
		check(m.ConnectTimeout > 0, "storage.mongodb.connect_timeout must be positive")
		check(m.MaxPoolSize > 0, "storage.mongodb.max_pool_size must be positive")
	case StorageDevLog:
		check(!production, "storage.type %q is not allowed when environment is %q", StorageDevLog, Production)
	default:
		errs = append(errs, fmt.Errorf("storage.type must be %q or %q", StorageMongoDB, StorageDevLog))
	}
	rd := st.RetentionDays
	check(rd.Error > 0 && rd.Warning > 0 && rd.Info > 0, "storage.retention_days must be positive for error, warning and info")

	var level slog.Level
	check(level.UnmarshalText([]byte(c.Log.Level)) == nil, "log.level must be debug, info, warn or error")

	return errors.Join(errs...)
}

func validWriteConcern(s string) bool {
	if s == "majority" {
		return true
	}
	n, err := strconv.Atoi(s)
	return err == nil && n > 0
}

// LogLevel returns the configured log level; Validate has already checked it.
func (c *Config) LogLevel() slog.Level {
	var level slog.Level
	_ = level.UnmarshalText([]byte(c.Log.Level))
	return level
}

// Duration is a time.Duration written in JSON as a string such as "5s".
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf(`durations are strings such as "5s" or "2m": %w`, err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}
