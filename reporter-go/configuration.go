package reporter

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Configuration configures the default notifier. Fields left at their zero
// value keep their current setting, which starts as the matching REPORTER_*
// environment variable or the default. Set things in code to override the
// environment.
type Configuration struct {
	// APIKey identifies the project. Required. Env: REPORTER_API_KEY.
	APIKey string
	// Endpoint is the reporter service's base URL, such as
	// "https://reporter.internal:8443". An "http://" URL sends events without
	// TLS and is for local development only. Required. Env: REPORTER_ENDPOINT.
	Endpoint string
	// Service names this application, such as "booking-api": 1 to 100
	// letters, digits, '.', '_' or '-'. Required. Env: REPORTER_SERVICE.
	Service string
	// Environment, such as "production" or "staging" (Bugsnag's ReleaseStage).
	// Default "production". Env: REPORTER_ENVIRONMENT.
	Environment string
	// AppVersion of this application. Env: REPORTER_APP_VERSION.
	AppVersion string
	// Hostname of this machine. Default os.Hostname(). Env: REPORTER_HOSTNAME.
	Hostname string
	// EnabledEnvironments lists the environments that send events. When empty,
	// every environment does. Env: REPORTER_ENABLED_ENVIRONMENTS, comma-separated.
	EnabledEnvironments []string
	// ProjectPackages are patterns for this application's packages, used to
	// mark stack frames as in-project. "main*" matches package main, and
	// "github.com/acme/app/**" matches a package and every package below it.
	// Default ["main*"]. Env: REPORTER_PROJECT_PACKAGES, comma-separated.
	ProjectPackages []string
	// SourceRoot is removed from the start of file paths in stack traces.
	// Default $GOPATH/src when GOPATH is set. Env: REPORTER_SOURCE_ROOT.
	SourceRoot string
	// ParamsFilters hide the value of every attribute whose key contains one of
	// these strings, ignoring case. Env: REPORTER_PARAMS_FILTERS, comma-separated.
	ParamsFilters []string
	// PanicHandler runs once, at the end of the first Configure, to set up
	// crash reporting. The default re-runs the program as a monitor process
	// that reports crashes; see Configure. Set it to func() {}, or set
	// REPORTER_DISABLE_PANIC_HANDLER=1, to turn crash reporting off.
	PanicHandler func()
	// Synchronous sends each event before Notify returns instead of queueing
	// it. Env: REPORTER_SYNCHRONOUS=1.
	Synchronous bool
	// MainContext, once cancelled, stops new events from being queued and sends
	// the ones already in the queue.
	MainContext context.Context
	// Logger receives the SDK's own messages. Default: the standard log package.
	Logger interface {
		Printf(format string, v ...any)
	}
	// Transport replaces the HTTP transport used to reach the service, for
	// example to trust a private certificate authority. It must speak HTTP/2,
	// which gRPC needs.
	Transport http.RoundTripper
	// QueueSize is how many events can wait to be sent; more are dropped.
	// Read only by the first Configure. Default 100. Env: REPORTER_QUEUE_SIZE.
	QueueSize int
	// RequestTimeout bounds each request to the service. Default 5s.
	// Env: REPORTER_REQUEST_TIMEOUT, such as "5s".
	RequestTimeout time.Duration
}

const (
	defaultEnvironment    = "production"
	defaultQueueSize      = 100
	defaultRequestTimeout = 5 * time.Second
)

var defaultParamsFilters = []string{
	"password", "passwd", "secret", "token", "authorization",
	"cookie", "api_key", "apikey", "credential",
}

// merge returns c with every non-zero field of o copied over it.
func (c Configuration) merge(o Configuration) Configuration {
	setString := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	setString(&c.APIKey, o.APIKey)
	setString(&c.Endpoint, o.Endpoint)
	setString(&c.Service, o.Service)
	setString(&c.Environment, o.Environment)
	setString(&c.AppVersion, o.AppVersion)
	setString(&c.Hostname, o.Hostname)
	setString(&c.SourceRoot, o.SourceRoot)
	if o.EnabledEnvironments != nil {
		c.EnabledEnvironments = o.EnabledEnvironments
	}
	if o.ProjectPackages != nil {
		c.ProjectPackages = o.ProjectPackages
	}
	if o.ParamsFilters != nil {
		c.ParamsFilters = o.ParamsFilters
	}
	if o.PanicHandler != nil {
		c.PanicHandler = o.PanicHandler
	}
	if o.Synchronous {
		c.Synchronous = true
	}
	if o.MainContext != nil {
		c.MainContext = o.MainContext
	}
	if o.Logger != nil {
		c.Logger = o.Logger
	}
	if o.Transport != nil {
		c.Transport = o.Transport
	}
	if o.QueueSize != 0 {
		c.QueueSize = o.QueueSize
	}
	if o.RequestTimeout != 0 {
		c.RequestTimeout = o.RequestTimeout
	}
	return c
}

// configFromEnv reads the REPORTER_* variables. Values that cannot be parsed
// are reported in problems and otherwise ignored.
func configFromEnv(getenv func(string) string) (c Configuration, problems []string) {
	list := func(name string) []string {
		v := getenv(name)
		if v == "" {
			return nil
		}
		var out []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}

	c.APIKey = getenv("REPORTER_API_KEY")
	c.Endpoint = getenv("REPORTER_ENDPOINT")
	c.Service = getenv("REPORTER_SERVICE")
	c.Environment = getenv("REPORTER_ENVIRONMENT")
	c.AppVersion = getenv("REPORTER_APP_VERSION")
	c.Hostname = getenv("REPORTER_HOSTNAME")
	c.SourceRoot = getenv("REPORTER_SOURCE_ROOT")
	c.EnabledEnvironments = list("REPORTER_ENABLED_ENVIRONMENTS")
	c.ProjectPackages = list("REPORTER_PROJECT_PACKAGES")
	c.ParamsFilters = list("REPORTER_PARAMS_FILTERS")
	if getenv("REPORTER_DISABLE_PANIC_HANDLER") == "1" {
		c.PanicHandler = func() {}
	}
	if getenv("REPORTER_SYNCHRONOUS") == "1" {
		c.Synchronous = true
	}
	if v := getenv("REPORTER_QUEUE_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			problems = append(problems, fmt.Sprintf("REPORTER_QUEUE_SIZE=%q is not a positive number", v))
		} else {
			c.QueueSize = n
		}
	}
	if v := getenv("REPORTER_REQUEST_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			problems = append(problems, fmt.Sprintf("REPORTER_REQUEST_TIMEOUT=%q is not a positive duration such as 5s", v))
		} else {
			c.RequestTimeout = d
		}
	}
	return c, problems
}

// withDefaults fills in every field that has a default.
func (c Configuration) withDefaults() Configuration {
	if c.Environment == "" {
		c.Environment = defaultEnvironment
	}
	if c.Hostname == "" {
		c.Hostname, _ = os.Hostname()
	}
	if c.ProjectPackages == nil {
		c.ProjectPackages = []string{"main*"}
	}
	if c.SourceRoot == "" {
		if gopath := os.Getenv("GOPATH"); gopath != "" {
			c.SourceRoot = filepath.Join(filepath.SplitList(gopath)[0], "src")
		}
	}
	if c.ParamsFilters == nil {
		c.ParamsFilters = defaultParamsFilters
	}
	if c.Logger == nil {
		c.Logger = log.Default()
	}
	if c.QueueSize <= 0 {
		c.QueueSize = defaultQueueSize
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = defaultRequestTimeout
	}
	return c
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// validate reports the problems that stop events from being sent. It uses
// the same name rules as the service.
func (c Configuration) validate() error {
	var errs []error
	if c.APIKey == "" {
		errs = append(errs, errors.New("missing APIKey"))
	}
	if c.Endpoint == "" {
		errs = append(errs, errors.New("missing Endpoint"))
	} else if u, err := url.Parse(c.Endpoint); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		errs = append(errs, fmt.Errorf("invalid Endpoint %q: use an http:// or https:// URL", c.Endpoint))
	}
	if !namePattern.MatchString(c.Service) {
		errs = append(errs, fmt.Errorf("invalid Service %q: use 1 to 100 letters, digits, '.', '_' or '-'", c.Service))
	}
	if !namePattern.MatchString(c.Environment) {
		errs = append(errs, fmt.Errorf("invalid Environment %q: use 1 to 100 letters, digits, '.', '_' or '-'", c.Environment))
	}
	return errors.Join(errs...)
}

// enabled reports whether this environment sends events.
func (c Configuration) enabled() bool {
	if len(c.EnabledEnvironments) == 0 {
		return true
	}
	for _, env := range c.EnabledEnvironments {
		if strings.EqualFold(env, c.Environment) {
			return true
		}
	}
	return false
}
