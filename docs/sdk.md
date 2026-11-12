# reporter-go: Go SDK guide

How to report errors and log events from a Go service to the reporter service. The SDK follows bugsnag-go's API and behaviour closely, so moving a service from Bugsnag is mostly a matter of renaming calls; see [Migrating from Bugsnag](#migrating-from-bugsnag).

Requires Go 1.27. The SDK's only dependencies are `connectrpc.com/connect/v2` and `google.golang.org/protobuf`.

## Install

```sh
go get github.com/go-fynx/reporter/reporter-go@latest
```

The SDK is a module inside the `go-fynx/reporter` repository. Its releases are tagged `reporter-go/vX.Y.Z`. If the repository is private, tell Go not to use the public proxy for it:

```sh
go env -w GOPRIVATE=github.com/go-fynx/*
```

## Quick start

```go
package main

import (
	"os"

	reporter "github.com/go-fynx/reporter/reporter-go"
)

func main() {
	// Must be the first statement in main; see "Crashes".
	reporter.Configure(reporter.Configuration{
		APIKey:     os.Getenv("REPORTER_API_KEY"),
		Endpoint:   "https://reporter.internal:8443",
		Service:    "booking-api",
		AppVersion: version,
	})

	// ...
	if err := createBooking(ctx); err != nil {
		reporter.Notify(err, reporter.Fields{"operation": "create_booking"})
	}
	reporter.Info("booking request received", reporter.Fields{"booking_id": id})
}
```

Every `Configuration` field can also come from a `REPORTER_*` environment variable, so the call can be just `reporter.Configure(reporter.Configuration{})`. A value set in code wins over the environment.

## Reporting

| Call | Severity | Stack trace | Notes |
|---|---|---|---|
| `reporter.Notify(err, ...)` | WARNING, handled | yes | Same default as bugsnag-go |
| `reporter.Error(err, ...)` | ERROR | yes | `Notify` with ERROR as default |
| `reporter.Warn(msg, ...)` | WARNING | no | Message only, no error type |
| `reporter.Info(msg, ...)` | INFO | no | Message only, no error type |

The optional arguments can be any of these, in any order:

| Value | Effect |
|---|---|
| `reporter.SeverityInfo`, `SeverityWarning`, `SeverityError` | Overrides the severity |
| `reporter.Fields{"key": value}` | Adds attributes. Several `Fields` are merged; later ones win. |
| `reporter.ErrorClass{Name: "BookingError"}` | Overrides the error type |
| `reporter.Message{Text: "..."}` | Overrides the message |

Any other type is ignored, and the SDK logs a line saying so.

**Fields:**
- Values are sent as strings, integers, floating-point numbers or booleans.
- `time.Time` becomes an RFC 3339 string and `time.Duration` a string such as `1.5s`.
- Anything else is formatted with `fmt.Sprint`.
- Fields are copied when you call, so changing the map afterwards has no effect.

**Shared defaults:** `reporter.New(rawData...)` returns a `*Notifier` that adds the same arguments to every event. It has the same methods as the package, for example `n := reporter.New(reporter.Fields{"component": "billing"})`.

**Return value:** the calls return an error only when nothing will be sent:
- a nil error or empty message;
- the SDK isn't configured;
- it's shutting down.

A full queue drops the event and returns nil, as bugsnag-go does. With `Synchronous`, the error from the request is returned.

## What is sent

For each event:
- **Identity:** a UUIDv7 `event_id` and the time of the call.
- **Classification:** severity, and whether it was unhandled.
- **Content:**
  - the message;
  - the error type, which is the Go type of the error you passed, such as `*fs.PathError`;
  - a stack trace;
  - the error's causes;
  - your fields.

Each request also carries the service, environment, app version, hostname, operating system, Go version and SDK version.

**Causes.** Every error in the `Unwrap()` chain becomes a cause, with its own type and message, outermost first. For `errors.Join` the chain follows the first error. At most 32 causes are sent.

**Stack traces:**
- The stack is recorded where you call `Notify` or `Error`, up to 50 frames, without the SDK's own frames.
- If the error carries its own stack, that stack is used instead. Errors carry one through a `Callers() []uintptr` method (as bugsnag-go's errors do) or a `StackTrace()` method (as `github.com/pkg/errors` errors do).
- Causes keep their own stacks the same way.
- `ProjectPackages` decides which frames are marked as your code.
- File paths are shortened: `SourceRoot` is removed, and paths under the module cache or standard library start at the package path.

**Sensitive data.** The value of any field whose key contains one of `ParamsFilters`, ignoring case, is replaced with `[FILTERED]` before it leaves the process.
- **Defaults:** `password`, `passwd`, `secret`, `token`, `authorization`, `cookie`, `api_key`, `apikey` and `credential`.
- **Limit:** messages and error texts are not filtered, so don't put secrets or personal data in them.

**Invalid UTF-8** in messages and fields is replaced, because protobuf can't carry it.

## Crashes

### The crash monitor

On Linux and macOS, the first `Configure` starts crash reporting the way bugsnag-go does:
1. The program runs itself a second time as a monitor process.
2. The program's standard error goes through the monitor, which copies everything to the real standard error.
3. When the program dies with a Go crash report (`panic: ...` or `fatal error: ...`), the monitor sends it as an unhandled ERROR event, with the crashing goroutine's stack.

The program's output and exit code are unchanged.

**What this means for your code:**
- **Call `Configure` first in `main`.** The monitor process runs `main` until it reaches `Configure`, so anything before it runs twice. The monitor's standard output is discarded.
- **Ignored signals.** The monitor ignores Ctrl-C and SIGTERM and exits when the program does. Stopping a service with systemd or Ctrl-C is therefore unaffected.
- **If the monitor dies** (for example, killed by the out-of-memory killer), the program gets its standard error back. Crashes are then no longer reported, and the SDK logs a line saying so.
- **Test binaries** never start the monitor; neither does Windows.
- **To turn it off,** set `PanicHandler: func() {}`, or `REPORTER_DISABLE_PANIC_HANDLER=1`.

### Recovering panics yourself

```go
defer reporter.AutoNotify() // report as unhandled ERROR, then keep panicking
defer reporter.Recover()    // report as handled WARNING and stop the panic
```

Both must be deferred directly; calling them inside another deferred function does nothing.
- **`AutoNotify`** sends the event before the panic continues. If the panic then crashes the program, the crash monitor reports it again, so you get two events.
- **`Recover`** queues its event like `Notify`.

### HTTP handlers

net/http recovers panics in handlers itself, so they never crash the program and the monitor never sees them. Wrap your handler to report them:

```go
http.ListenAndServe(":8080", reporter.Handler(mux))
```

The middleware sends an unhandled ERROR and then panics again, so net/http still logs the panic and closes the connection. `http.ErrAbortHandler` is not reported.

## Delivery

- **Background sending:** events go into an in-memory queue of 100 (`QueueSize`). One goroutine sends them, one request per event.
- **Full queue:** new events are dropped; the SDK logs this at most every 10 seconds.
- **No retries:** each request has `RequestTimeout` (5 s), and a failed request is logged and the event lost, as with Bugsnag.
- **Synchronous mode:** `Synchronous: true` sends each event inside the call and returns the error. That suits command-line tools and tests.
- **On exit:** events still queued when the program exits are lost. To send them on shutdown, pass a context to `Configuration.MainContext` and cancel it. New events are then refused, and the queue is sent in the background, so give it a moment before exiting:

  ```go
  ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
  defer stop()
  reporter.Configure(reporter.Configuration{MainContext: ctx /* ... */})
  ```

- **Counters:** `reporter.Stats()` returns counts of events that were sent, failed, dropped and rejected by the service. Export them to your own metrics if you need to watch for loss.
- **Transport:** events go over gRPC on HTTP/2, using TLS for `https://` endpoints. `http://` endpoints are for local development; the SDK logs a warning if one is used with the `production` environment. To trust a private certificate authority, set `Transport` to an `*http.Transport` with your `TLSClientConfig` and HTTP/2 enabled.

## Configuration

| Field | Environment variable | Default | Meaning |
|---|---|---|---|
| `APIKey` | `REPORTER_API_KEY` | — (required) | The project's API key, from `reporter keygen` |
| `Endpoint` | `REPORTER_ENDPOINT` | — (required) | Service URL, such as `https://reporter.internal:8443` |
| `Service` | `REPORTER_SERVICE` | — (required) | 1 to 100 letters, digits, `.`, `_` or `-` |
| `Environment` | `REPORTER_ENVIRONMENT` | `production` | Bugsnag's `ReleaseStage` |
| `AppVersion` | `REPORTER_APP_VERSION` | — | |
| `Hostname` | `REPORTER_HOSTNAME` | `os.Hostname()` | |
| `EnabledEnvironments` | `REPORTER_ENABLED_ENVIRONMENTS` | all | Only these environments send events (comma-separated in the variable) |
| `ProjectPackages` | `REPORTER_PROJECT_PACKAGES` | `main*` | Patterns for your packages; `github.com/acme/app/**` matches the package and everything below it |
| `SourceRoot` | `REPORTER_SOURCE_ROOT` | `$GOPATH/src` if set | Removed from file paths |
| `ParamsFilters` | `REPORTER_PARAMS_FILTERS` | see above | |
| `PanicHandler` | `REPORTER_DISABLE_PANIC_HANDLER=1` | crash monitor | Runs once at the end of the first `Configure` |
| `Synchronous` | `REPORTER_SYNCHRONOUS=1` | `false` | |
| `MainContext` | — | — | See [Delivery](#delivery) |
| `Logger` | — | the `log` package | Receives the SDK's own messages, prefixed `reporter:` |
| `Transport` | — | HTTP/2 transport | |
| `QueueSize` | `REPORTER_QUEUE_SIZE` | `100` | Read by the first `Configure` only |
| `RequestTimeout` | `REPORTER_REQUEST_TIMEOUT` | `5s` | |

**Calling `Configure` again** replaces the fields you set and keeps the rest.

**When configuration is invalid** (for example, no API key), the SDK logs the problem once, drops events and counts them as dropped. It never stops your program.

## Migrating from Bugsnag

| bugsnag-go | reporter-go |
|---|---|
| `bugsnag.Configure(bugsnag.Configuration{...})` | `reporter.Configure(reporter.Configuration{...})` |
| `APIKey`, `AppVersion`, `Hostname`, `ProjectPackages`, `SourceRoot`, `ParamsFilters`, `PanicHandler`, `Synchronous`, `MainContext`, `Logger`, `Transport` | Same names |
| `Endpoints.Notify` | `Endpoint` |
| `ReleaseStage` | `Environment` |
| `EnabledReleaseStages` / `NotifyReleaseStages` | `EnabledEnvironments` |
| — | `Service` (required; Bugsnag has projects only) |
| `AppType`, `AutoCaptureSessions` | Not supported; there are no sessions |
| `bugsnag.Notify(err, ...)` | `reporter.Notify(err, ...)` |
| `bugsnag.MetaData{"tab": {"k": v}}` | `reporter.Fields{"k": v}`. Fields are flat; there are no tabs. |
| `bugsnag.SeverityError` and friends | `reporter.SeverityError` and friends |
| `bugsnag.ErrorClass{Name: ...}` | `reporter.ErrorClass{Name: ...}` |
| `bugsnag.User`, `bugsnag.Context`, `context.Context`, `*http.Request` arguments | Not supported; ignored with a log line |
| `bugsnag.AutoNotify`, `bugsnag.Recover`, `bugsnag.Handler`, `bugsnag.HandlerFunc` | Same names |
| `bugsnag.New(...)` | `reporter.New(...)` |
| `BUGSNAG_*` environment variables | `REPORTER_*` environment variables |

**Deliberate differences:**
- **Request timeout:** every request has one (5 s).
- **Invalid UTF-8** is replaced instead of being sent.
- **Configure twice:** calling `Configure` twice is safe; bugsnag-go starts a second delivery goroutine.
- **The crash monitor** is the SDK's own rather than `bugsnag/panicwrap`:
  - it survives shutdown signals;
  - it hands standard error back if it dies;
  - it lets crash output through as it happens.
- **Message-only events:** `Info`, `Warn` and `Error` exist for events with no error value.

### Running both SDKs during the pilot

A service can report to Bugsnag and to the reporter at the same time; configure both at the start of `main`. This was tested with bugsnag-go v2.6.4, configuring the two in each order and then crashing:

| Setup | Exit code | Crash in standard error | Reporter events | Bugsnag events |
|---|---|---|---|---|
| reporter-go only | 2 | once | 1 | — |
| bugsnag-go only | 2 | once | — | 1 |
| reporter-go, then bugsnag-go | 2 | once | 1 | 1 |
| bugsnag-go, then reporter-go | 2 | once | 1 | 1 |

Getting one event each needs the SDK's coexistence handling. Without it, each SDK's monitor process starts the other's monitor too, and whichever SDK was configured first reports every crash twice. reporter-go therefore:
- doesn't start a monitor inside bugsnag-go's monitor process;
- starts its own monitor with `BUGSNAG_DISABLE_PANIC_HANDLER=1` and `BUGSNAG_AUTO_CAPTURE_SESSIONS=0`, so bugsnag-go starts neither a monitor nor a session inside it.

Handled events (`Notify` and friends) are independent; call both SDKs.
