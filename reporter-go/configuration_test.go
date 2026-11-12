package reporter

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestConfigFromEnv(t *testing.T) {
	cfg, problems := configFromEnv(env(map[string]string{
		"REPORTER_API_KEY":               "key",
		"REPORTER_ENDPOINT":              "https://reporter.internal:8443",
		"REPORTER_SERVICE":               "booking-api",
		"REPORTER_ENVIRONMENT":           "staging",
		"REPORTER_APP_VERSION":           "1.2.3",
		"REPORTER_HOSTNAME":              "web-1",
		"REPORTER_SOURCE_ROOT":           "/src",
		"REPORTER_ENABLED_ENVIRONMENTS":  "production, staging ,",
		"REPORTER_PROJECT_PACKAGES":      "main*,github.com/acme/**",
		"REPORTER_PARAMS_FILTERS":        "password,ssn",
		"REPORTER_DISABLE_PANIC_HANDLER": "1",
		"REPORTER_SYNCHRONOUS":           "1",
		"REPORTER_QUEUE_SIZE":            "250",
		"REPORTER_REQUEST_TIMEOUT":       "2s",
	}))
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if cfg.APIKey != "key" || cfg.Endpoint != "https://reporter.internal:8443" || cfg.Service != "booking-api" ||
		cfg.Environment != "staging" || cfg.AppVersion != "1.2.3" || cfg.Hostname != "web-1" || cfg.SourceRoot != "/src" {
		t.Errorf("strings = %+v", cfg)
	}
	if !slices.Equal(cfg.EnabledEnvironments, []string{"production", "staging"}) ||
		!slices.Equal(cfg.ProjectPackages, []string{"main*", "github.com/acme/**"}) ||
		!slices.Equal(cfg.ParamsFilters, []string{"password", "ssn"}) {
		t.Errorf("lists = %v %v %v", cfg.EnabledEnvironments, cfg.ProjectPackages, cfg.ParamsFilters)
	}
	if cfg.PanicHandler == nil || !cfg.Synchronous || cfg.QueueSize != 250 || cfg.RequestTimeout != 2*time.Second {
		t.Errorf("others = %+v", cfg)
	}
}

func TestConfigFromEnvReportsBadValues(t *testing.T) {
	cfg, problems := configFromEnv(env(map[string]string{
		"REPORTER_QUEUE_SIZE":      "lots",
		"REPORTER_REQUEST_TIMEOUT": "5",
	}))
	if len(problems) != 2 || cfg.QueueSize != 0 || cfg.RequestTimeout != 0 {
		t.Errorf("problems %v, config %+v", problems, cfg)
	}
}

func TestCodeOverridesEnvironment(t *testing.T) {
	c := newClient()
	c.getenv = env(map[string]string{
		"REPORTER_API_KEY":  "from-env",
		"REPORTER_ENDPOINT": "http://env:8080",
		"REPORTER_SERVICE":  "env-service",
	})
	c.configure(Configuration{APIKey: "from-code", PanicHandler: func() {}, Logger: discard{}})
	st := c.state.Load()
	if st.err != nil {
		t.Fatalf("configuration error: %v", st.err)
	}
	if st.cfg.APIKey != "from-code" || st.cfg.Endpoint != "http://env:8080" || st.cfg.Service != "env-service" {
		t.Errorf("config = %+v", st.cfg)
	}

	// A second Configure changes only the fields it sets.
	c.configure(Configuration{Service: "renamed"})
	st = c.state.Load()
	if st.cfg.APIKey != "from-code" || st.cfg.Service != "renamed" {
		t.Errorf("after reconfiguring: %+v", st.cfg)
	}
}

func TestDefaults(t *testing.T) {
	cfg := Configuration{}.withDefaults()
	if cfg.Environment != "production" || cfg.QueueSize != 100 || cfg.RequestTimeout != 5*time.Second ||
		!slices.Equal(cfg.ProjectPackages, []string{"main*"}) || cfg.Logger == nil || cfg.Hostname == "" {
		t.Errorf("defaults = %+v", cfg)
	}
	if !slices.Contains(cfg.ParamsFilters, "authorization") || !slices.Contains(cfg.ParamsFilters, "cookie") {
		t.Errorf("default filters = %v", cfg.ParamsFilters)
	}
}

func TestValidate(t *testing.T) {
	valid := Configuration{APIKey: "k", Endpoint: "https://r:8443", Service: "svc"}.withDefaults()
	if err := valid.validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*Configuration)
		want   string
	}{
		{"no key", func(c *Configuration) { c.APIKey = "" }, "missing APIKey"},
		{"no endpoint", func(c *Configuration) { c.Endpoint = "" }, "missing Endpoint"},
		{"bad scheme", func(c *Configuration) { c.Endpoint = "grpc://r:8443" }, "http:// or https://"},
		{"no host", func(c *Configuration) { c.Endpoint = "https://" }, "http:// or https://"},
		{"no service", func(c *Configuration) { c.Service = "" }, "invalid Service"},
		{"bad environment", func(c *Configuration) { c.Environment = "prod eu" }, "invalid Environment"},
	}
	for _, tt := range tests {
		cfg := valid
		tt.mutate(&cfg)
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: validate() = %v, want it to mention %q", tt.name, err, tt.want)
		}
	}
}

func TestEnabled(t *testing.T) {
	cfg := Configuration{Environment: "staging"}
	if !cfg.enabled() {
		t.Error("no list should enable every environment")
	}
	cfg.EnabledEnvironments = []string{"production"}
	if cfg.enabled() {
		t.Error("staging enabled although not listed")
	}
}

type discard struct{}

func (discard) Printf(string, ...any) {}
