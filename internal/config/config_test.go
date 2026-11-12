package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const devKeyHash = "7e9f8fd111802be56c379d597842e29b2cebd35ff2133d431a49fa556a18704e"

func TestShippedConfigsAreValid(t *testing.T) {
	for _, path := range []string{"../../deploy/config.dev.json", "../../deploy/config.example.json"} {
		if _, err := Load(path); err != nil {
			t.Errorf("Load(%s): %v", path, err)
		}
	}
}

func TestDefaultsFillMissingFields(t *testing.T) {
	cfg, err := Parse([]byte(`{
		"environment": "development",
		"server": {"insecure": true},
		"api_keys": [{"id": "k1", "project": "booking", "sha256": "` + devKeyHash + `"}],
		"storage": {"type": "devlog"}
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	def := Default()
	if cfg.Server.ListenAddress != def.Server.ListenAddress {
		t.Errorf("listen_address = %q, want default %q", cfg.Server.ListenAddress, def.Server.ListenAddress)
	}
	if cfg.Limits != def.Limits {
		t.Errorf("limits = %+v, want defaults %+v", cfg.Limits, def.Limits)
	}
	if cfg.Storage.RetentionDays != def.Storage.RetentionDays {
		t.Errorf("retention = %+v, want defaults", cfg.Storage.RetentionDays)
	}
	if len(cfg.Redaction.Filters) != len(def.Redaction.Filters) {
		t.Errorf("filters = %v, want defaults", cfg.Redaction.Filters)
	}
}

func TestParseRejectsBadJSON(t *testing.T) {
	tests := map[string]string{
		"unknown field":     `{"environmnet": "production"}`,
		"trailing data":     `{} {}`,
		"duration number":   `{"server": {"shutdown_timeout": 30}}`,
		"duration nonsense": `{"server": {"shutdown_timeout": "soon"}}`,
		"not an object":     `[]`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(input)); err == nil {
				t.Fatal("Parse succeeded, want error")
			}
		})
	}
}

func validConfig() Config {
	cfg := Default()
	cfg.Server.TLS = &TLS{CertFile: "server.crt", KeyFile: "server.key"}
	cfg.APIKeys = []APIKey{{ID: "k1", Project: "booking", SHA256: devKeyHash}}
	cfg.Storage.MongoDB.URI = "mongodb://127.0.0.1:27017"
	return cfg
}

func TestValidate(t *testing.T) {
	if cfg := validConfig(); cfg.Validate() != nil {
		t.Fatalf("validConfig().Validate() = %v", cfg.Validate())
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"no transport security", func(c *Config) { c.Server.TLS = nil }, "set server.tls"},
		{"tls and insecure", func(c *Config) { c.Server.Insecure = true }, "cannot both be set"},
		{"incomplete tls", func(c *Config) { c.Server.TLS.KeyFile = "" }, "cert_file and key_file"},
		{"insecure in production", func(c *Config) { c.Server.TLS = nil; c.Server.Insecure = true }, "not allowed"},
		{"devlog in production", func(c *Config) { c.Storage.Type = StorageDevLog }, "not allowed"},
		{"unknown storage", func(c *Config) { c.Storage.Type = "postgres" }, "storage.type"},
		{"bad environment", func(c *Config) { c.Environment = "prod env" }, "environment"},
		{"zero timeout", func(c *Config) { c.Server.ShutdownTimeout = 0 }, "shutdown_timeout"},
		{"zero limit", func(c *Config) { c.Limits.MaxInFlightRequests = 0 }, "max_in_flight_requests"},
		{"burst below request size", func(c *Config) { c.RateLimit.Burst = 10 }, "rate_limit.burst"},
		{"negative rate", func(c *Config) { c.RateLimit.EventsPerSecond = -1 }, "cannot be negative"},
		{"no keys", func(c *Config) { c.APIKeys = nil }, "at least one key"},
		{"bad key hash", func(c *Config) { c.APIKeys[0].SHA256 = "abc" }, "64 hex"},
		{"bad project", func(c *Config) { c.APIKeys[0].Project = "" }, "project"},
		{"duplicate key id", func(c *Config) {
			c.APIKeys = append(c.APIKeys, APIKey{ID: "k1", Project: "x", SHA256: strings.Repeat("ab", 32)})
		}, "more than once"},
		{"missing mongo uri", func(c *Config) { c.Storage.MongoDB.URI = "" }, "uri is required"},
		{"bad write concern", func(c *Config) { c.Storage.MongoDB.WriteConcern = "all" }, "write_concern"},
		{"zero retention", func(c *Config) { c.Storage.RetentionDays.Info = 0 }, "retention_days"},
		{"bad log level", func(c *Config) { c.Log.Level = "loud" }, "log.level"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate() = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	cfg := validConfig()
	cfg.Log.Level = "loud"
	cfg.APIKeys = nil
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "log.level") || !strings.Contains(err.Error(), "api_keys") {
		t.Fatalf("Validate() = %v, want both problems reported", err)
	}
}

func TestDurationJSON(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"1m30s"`), &d); err != nil {
		t.Fatal(err)
	}
	if d.Std() != 90*time.Second {
		t.Errorf("d = %v, want 1m30s", d.Std())
	}
	out, err := json.Marshal(d)
	if err != nil || string(out) != `"1m30s"` {
		t.Errorf("Marshal = %s, %v", out, err)
	}
}
