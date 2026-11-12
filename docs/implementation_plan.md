# Implementation plan: reporter v1

**Status:** Final draft. Every decision that blocks implementation is answered; waiting for approval to start Phase 1. No code has been written.
**Date:** 2026-11-12
**Based on:** [reporter_proposal.md](reporter_proposal.md), [architecture_review.md](architecture_review.md), [answers.csv](answers.csv)

---

## 1. What v1 is

v1 replaces Bugsnag for Go services by copying Bugsnag's behaviour, with two differences:

- Go services send events over **gRPC** instead of Bugsnag's JSON over HTTP.
- The same endpoint also accepts a **plain HTTP POST with JSON**, so services written in other languages can report too (B10).

Work that Bugsnag doesn't do, such as retries, batching, `Flush`/`Close` and metrics, waits for later optimization. The exceptions are the few places where gRPC or the proposal makes it necessary (section 3).

**In v1:**
- the contract (`.proto` and generated code);
- the Go SDK `reporter-go`, with a Bugsnag-style API, Bugsnag-style delivery and Bugsnag-style panic handling;
- the service `reporter`, with API-key authentication, validation, rate limiting, health checks and graceful shutdown;
- MongoDB storage with retention per severity;
- tests for every phase;
- a pilot where one service reports to both Bugsnag and reporter.

**Not in v1:**
- retries and backoff;
- batching;
- `Flush`/`Close` with timeouts;
- Prometheus metrics and alerts (B6, C9);
- a UI and error grouping (C5, C7);
- an slog adapter (B8);
- sessions, breadcrumbs, and user or request data (A10);
- tracing.

---

## 2. Decision record

"Parity" means the row was marked as decided by matching Bugsnag. "Assumed" means a confirm row you left blank, where I took the suggestion. Say so if any assumed row is wrong.

| ID | Decision | Source |
|---|---|---|
| A1 | At-most-once delivery: no retries; a failed send is logged and dropped | Parity |
| A2 | The service returns success only after MongoDB has stored the events | Answered C |
| A3 | The contract carries a list of events and per-event results; the v1 SDK sends one event per request | Assumed |
| A4 | The SDK sets a UUIDv7 `event_id` (Go 1.27 standard library `uuid.NewV7`); storage ignores duplicates | Assumed |
| A5 | Per-process fields are sent once per request in a `source` block | Assumed |
| A6 | Attributes are a flat map whose values are string, int64, double or bool | Answered A |
| A7 | Stack captured at the call, max 50 frames, reusing a stack already on the error (`Callers()`, pkg/errors `StackTrace()`) | Parity |
| A8 | `error_type` is the outermost type; every wrapped error is sent as a cause | Parity |
| A9 | Severities INFO, WARNING and ERROR, plus an `unhandled` flag | Parity |
| A10 | Adds app version, hostname, OS, runtime version, SDK name/version, and server-set `project` and `received_at`. No user or request data | Assumed |
| A11 | No client-side truncation; the server rejects requests over 1 MiB; the SDK replaces invalid UTF-8 | Parity |
| A12 | Dropped events are logged and counted in a local `Stats()` | Assumed |
| A13 | Queue of 100, 50 frames, one event per request, 5 s request timeout, no retries | Assumed |
| B1a, B1b | One API key per project, sent in a request header over TLS; no service mesh exists | Parity, Answered |
| B2 | Key → project; service and environment are set by the client | Parity |
| B3 | Queue of 100 (configurable), drop the newest event when full, log it | Parity |
| B4 | No retries | Parity |
| B5a | Bugsnag-style API: `Configure`, `Notify`, `AutoNotify`, `Recover`, with a `MainContext` drain instead of `Flush`/`Close` | Answered "same as Bugsnag" |
| B5b | `REPORTER_*` environment variables fill in unset configuration | Parity |
| Q4 | `Info` and `Warn` helpers (no error, no stack trace) and `Error` (`Notify` at ERROR) next to the Bugsnag-style `Notify` | Answered |
| B6 | No metrics in v1; Prometheus later | Answered |
| B7 | Panic monitor process (panicwrap), `AutoNotify`, `Recover`, net/http middleware | Parity |
| B8 | No slog adapter | Parity |
| B9, B11a | One repo: the service `github.com/go-fynx/reporter` at the root, and the SDK `github.com/go-fynx/reporter/reporter-go` (package `reporter`) as a nested module that holds the `.proto` and generated code; no separate proto module | Answered, plus Q1 and Q2 |
| B10 | buf and ConnectRPC: one endpoint for gRPC and HTTP/JSON | Answered, plus Q3 |
| B11b | Go 1.27 minimum | Answered |
| B12 | Plain binary, no Kubernetes; the service terminates TLS itself | Answered |
| C1 | Limits are configurable. Provisional defaults: 100 events/s per project with bursts up to 1,000, and 1 MiB per request; Phase 7 sets the final values | Answered, plus Q5 |
| C2 | MongoDB with mongo-driver v2, behind the processor interface | Answered |
| Q6 | MongoDB installed locally for development and integration tests; CI uses a MongoDB service container | Answered |
| C3 | Retention 90 days for errors and warnings, 14 days for info, configurable | Answered A |
| C4 | Redact attribute keys containing password, passwd, secret, token, authorization, cookie, api_key, apikey or credential (replaced with `[FILTERED]`), in both the SDK and the service | Assumed |
| C5 | Bugsnag stays the viewing tool during migration; the new data is checked with read-only MongoDB queries; a UI comes later | Answered C |
| C6 | Only events sent through the SDK or the HTTP endpoint | Parity |
| C7 | No grouping in v1 | Parity |
| C8 | A pilot service reports to both; compare; then switch service by service | Answered A |
| C9 | Alert list accepted, but alerts need metrics, so they move to later with B6 | Answered A, deferred |

---

## 3. Changes to the proposal

These are deliberate and recorded here so nothing changes silently.

| Proposal | Says | v1 does | Why |
|---|---|---|---|
| §3.1, §5 | Bounded retries with exponential backoff and jitter | No retries | A1, B4 |
| §3.1, §5, §7 | Batched delivery; batch size, batch bytes and flush interval configurable | One event per request (the contract still allows lists) | Parity |
| §3.1, §5 | `Flush` and `Close` with bounded timeouts | `MainContext` drain and `Synchronous` mode | B5a |
| §5 | Dropped-event metric | Log line plus a local `Stats()` counter | A12 |
| §3.2, §7 | Service metrics | Health checks only; metrics later | B6 |
| §3.3 | Separate proto module | Proto inside the SDK module (Q2) | B9 |
| §1 | Go services first | Also HTTP/JSON for services in other languages | B10 |
| (not in proposal) | — | Bugsnag-style panic capture | B7 |
| §7 | Retry tests | "One attempt, then drop" tests | Follows A1 |

**Kept from the proposal even where Bugsnag differs:**
- a request timeout (A13);
- TLS everywhere except an explicit development mode;
- project-scoped API keys;
- input validation;
- the wider redaction list (C4);
- rate limits (§6).

---

## 4. Questions resolved on 2026-11-12

| Question | Answer |
|---|---|
| Q1 Repository layout | One repo. The service is `github.com/go-fynx/reporter` at the root; the SDK is the nested module `github.com/go-fynx/reporter/reporter-go`, package `reporter` |
| Q2 Proto location | Inside the SDK module at `reporter-go/proto/reporter/v1/`; the service imports it from there |
| Q3 gRPC and HTTP/JSON | ConnectRPC (`connectrpc.com/connect/v2`) with buf: one handler serves gRPC and a plain HTTP `POST` with JSON on the same port |
| Q4 Log helpers | Add `Info` and `Warn` (message only, no stack trace) and `Error` (`Notify` at ERROR) next to `Notify` |
| Q5 Rate limit | Provisional default of 100 events/s per project with bursts up to 1,000, on each service instance; Phase 7 replaces it with measured values |
| Q6 Development MongoDB | Installed locally on the development machine, run as a single-node replica set so `majority` write concern behaves as it would on a replica set |

### Still to decide (not blocking)

| Item | Needed by |
|---|---|
| Production MongoDB: version, and whether it's a replica set, a standalone server or Atlas (sets the write concern) | Phase 8 |
| Where the service's TLS certificates come from | Phase 8 |
| Which service runs the pilot, for how many days, and how much difference from Bugsnag's counts is acceptable | Phase 8 |
| Final rate-limit, in-flight and request-size values | Phase 7 (from measurements) |

---

## 5. Design

### 5.1 Repository layout

```
reporter/                              module github.com/go-fynx/reporter  (the service)
├── go.mod                             replace github.com/go-fynx/reporter/reporter-go => ./reporter-go
├── Makefile                           tools, generate, lint, test, test-integration, build
├── .github/workflows/ci.yml           lint + tests; MongoDB service container for integration tests
├── cmd/reporter/                      `reporter serve`, `reporter keygen`
├── internal/
│   ├── config/                        flags and REPORTER_SERVER_* environment variables
│   ├── event/                         internal Event type, Validate, Normalize, redaction
│   ├── ingest/                        Connect handler, proto <-> event conversion, Processor interface
│   ├── auth/                          API-key check, per-project rate limit
│   ├── processor/memory/              in-memory processor for tests
│   ├── processor/devlog/              logs events; local development only
│   ├── storage/mongo/                 MongoDB processor (mongo-driver v2)
│   └── server/                        http.Server, TLS / h2c, size and in-flight limits, health, shutdown
├── reporter-go/                       module github.com/go-fynx/reporter/reporter-go, package reporter (the SDK)
│   ├── go.mod
│   ├── buf.yaml, buf.gen.yaml
│   ├── proto/reporter/v1/             reporter.proto plus committed generated Go and Connect code
│   ├── configuration.go  notifier.go  event.go  errors.go  stack.go  redact.go
│   ├── delivery.go  transport.go  stats.go
│   ├── panic.go  panicparse.go  middleware.go
│   └── internal/testserver/           fake service for SDK tests
└── docs/
```

- `reporter-sdk/` is renamed to `reporter-go/`, and the empty `reporting-service/` is removed.
- **Dependency direction:** the service depends on the SDK module to get the contract. The SDK never depends on the service.
- **SDK dependencies:**
  - Connect v2;
  - protobuf;
  - `github.com/bugsnag/panicwrap` (MIT);
  - `github.com/pkg/errors`, only to read stacks from pkg/errors values, as Bugsnag does.
  - UUIDs come from the Go standard library.

### 5.2 Contract

One RPC: `reporter.v1.ReporterService/Notify`. The name matches Bugsnag's notify endpoint.

| Message | Fields |
|---|---|
| `NotifyRequest` | `source`, `events` (list) |
| `Source` | `service`, `environment`, `app_version`, `hostname`, `os`, `runtime_version`, `sdk_name`, `sdk_version` |
| `Event` | `event_id`, `timestamp`, `severity`, `unhandled`, `message`, `error_type`, `stack` (frames), `causes`, `attributes` |
| `StackFrame` | `function`, `file`, `line`, `in_project` |
| `Cause` | `error_type`, `message`, `stack` |
| `AttributeValue` | one of `string`, `int64`, `double`, `bool` |
| `NotifyResponse` | `accepted_count`, `rejections` (`event_id`, `reason`, `detail`) |
| `Severity` | `UNSPECIFIED` (always rejected), `INFO`, `WARNING`, `ERROR` |

- Server-owned fields (`project`, `received_at`) don't exist in the request.
- Phase 1 adds `docs/api.md`. It covers the JSON shape, the error codes and their HTTP statuses, and a curl example for non-Go clients.

### 5.3 SDK: Bugsnag behaviour

**Public API**
- `Configure(Configuration)`: sets up the default notifier and starts the delivery goroutine and the panic monitor.
- `Notify(err, rawData...)`: identical to Bugsnag's.
- `Info(message, rawData...)` and `Warn(message, rawData...)`: message-only events with no error type and no stack trace (Q4).
- `Error(err, rawData...)`: `Notify` at ERROR severity (Q4).
- `AutoNotify(rawData...)` and `Recover(rawData...)`: used with `defer`.
- `Handler(h, rawData...)` and `HandlerFunc(f, rawData...)`: net/http middleware.
- `New(rawData...)`: a `Notifier` with its own defaults and the same methods.
- `Stats()`: counts of sent, failed, dropped and rejected events.

**Configuration**

| Field | Bugsnag equivalent | Environment variable | Default |
|---|---|---|---|
| `APIKey` | `APIKey` | `REPORTER_API_KEY` | required |
| `Endpoint` | `Endpoints.Notify` | `REPORTER_ENDPOINT` | required; `https://` means TLS, `http://` means plaintext for development only |
| `Service` | (proposal field) | `REPORTER_SERVICE` | required |
| `Environment` | `ReleaseStage` | `REPORTER_ENVIRONMENT` | `production` |
| `AppVersion` | `AppVersion` | `REPORTER_APP_VERSION` | empty |
| `Hostname` | `Hostname` | `REPORTER_HOSTNAME` | `os.Hostname()` |
| `EnabledEnvironments` | `EnabledReleaseStages` | `REPORTER_ENABLED_ENVIRONMENTS` | all |
| `ProjectPackages` | `ProjectPackages` | `REPORTER_PROJECT_PACKAGES` | `main*` |
| `SourceRoot` | `SourceRoot` | `REPORTER_SOURCE_ROOT` | as Bugsnag |
| `ParamsFilters` | `ParamsFilters` | `REPORTER_PARAMS_FILTERS` | C4 list |
| `PanicHandler` | `PanicHandler` | `REPORTER_DISABLE_PANIC_HANDLER=1` | panic monitor |
| `Synchronous` | `Synchronous` | `REPORTER_SYNCHRONOUS=1` | false |
| `MainContext` | `MainContext` | — | none |
| `Logger` | `Logger` | — | `log.Printf` |
| `Transport` | `Transport` | — | HTTP/2-capable transport |
| `QueueSize` | (fixed at 100) | `REPORTER_QUEUE_SIZE` | 100 |
| `RequestTimeout` | (none) | `REPORTER_REQUEST_TIMEOUT` | 5 s |

Bugsnag's `AppType` and `AutoCaptureSessions` aren't carried over, because sessions are out of scope.

**Parity checklist.** Each line becomes at least one test in Phase 3, 4 or 6.

1. `Notify(nil)` logs an error, returns an error and sends nothing.
2. `Notify` defaults to WARNING, handled.
3. Optional arguments to `Notify` can set severity, `Fields`, `ErrorClass` and `Message`.
4. `error_type` is the outermost error's type, and each `Unwrap()` level becomes a cause with its own type, message and stack.
5. The stack is captured at the call, up to 50 frames, unless the error already carries one (`Callers()` or pkg/errors `StackTrace()`).
6. `ProjectPackages` marks frames as `in_project`, and `SourceRoot` trims file paths.
7. Attribute keys that contain a `ParamsFilters` entry, ignoring case, have their value replaced with `[FILTERED]`.
8. Environments not listed in `EnabledEnvironments` send nothing.
9. The queue holds 100 events and never blocks. When it's full, the newest event is dropped and logged, and `Notify` still returns nil.
10. One delivery goroutine sends one event per request with no retries. Failures are logged.
11. `Synchronous` sends in the caller's goroutine and returns the error.
12. Cancelling `MainContext` stops new events (`Notify` returns an error) and sends whatever is queued.
13. `REPORTER_*` environment variables fill in unset fields, and values set in code win.
14. The panic monitor reports crashes synchronously as unhandled ERROR events.
15. `AutoNotify` reports ERROR, unhandled, synchronously, then re-panics. `Recover` reports WARNING, handled, and swallows the panic.
16. The net/http middleware reports panics from handlers.

**Deliberate differences from bugsnag-go:**
- a 5 s request timeout;
- invalid UTF-8 is replaced, because protobuf refuses it and JSON silently fixes it;
- no data races (bugsnag-go has one on its shutdown flag);
- calling `Configure` twice is safe (bugsnag-go starts another delivery goroutine);
- the panic monitor isn't started inside `go test` binaries;
- the panic monitor uses its own panicwrap cookie, so it can't take over bugsnag-go's monitor during the pilot.

**Capture path** (the caller's goroutine):
1. Check the environment filter.
2. Build the event:
   - ID and timestamp;
   - error type and causes;
   - stack addresses;
   - a copy of `Fields` with values converted to scalars;
   - redaction;
   - UTF-8 cleanup.
3. Do a non-blocking enqueue.

Turning stack addresses into frames, and encoding the protobuf message, happen on the delivery goroutine.

**Panic handling.** `Configure` starts the monitor with `bugsnag/panicwrap` on Linux and macOS:
- **How it works:**
  - The program re-runs itself as a monitor process, and the app's stderr is piped to it.
  - When the app crashes, the monitor parses the panic text: the message, plus the panicking goroutine's frames.
  - It sends the event synchronously with `error_type` `panic` (or `fatal error`) and `unhandled=true`.
- **Requirement:** `Configure` must be the first statement in `main`, because code before it runs twice.
- **Middleware:** `Handler`/`HandlerFunc` report handler panics synchronously and re-panic, so net/http's own recovery still runs. They ignore `http.ErrAbortHandler`.

### 5.4 Service

**Listening**
- One port.
- TLS from certificate and key files, serving HTTP/1.1 and HTTP/2; gRPC needs HTTP/2.
- `REPORTER_SERVER_INSECURE=true` serves plaintext HTTP/1.1 and h2c, for local development only.

**Authentication**
- `Authorization: Bearer <key>` works for both protocols.
- The key is hashed with SHA-256 and looked up in a JSON keys file of entries `{id, project, sha256}`.
- A project can have several keys, so rotation can overlap; that costs nothing.
- `reporter keygen --project <name>` prints a new key and its keys-file entry.

**Request steps, in order:**
1. Reject requests over 1 MiB.
2. Apply the in-flight limit (over the limit → `Unavailable`, HTTP 503).
3. Authenticate (failure → `Unauthenticated`, 401).
4. Apply the per-project rate limit, counting events (over the limit → `ResourceExhausted`, 429).
5. Decode the request.
6. Validate and normalize each event.
7. Pass the accepted events to the processor.
8. Respond with `accepted_count` and the per-event rejections.

A processor error returns `Unavailable` (503).

**Validation**
- `event_id` must be a UUID.
- Severity must be known.
- An event needs a message or an `error_type`.
- The timestamp must be set.
- Configurable hard limits, starting at:
  - 100 attributes, keys up to 128 characters;
  - 32 causes;
  - 256 frames per stack.

**Normalization**
- Trim whitespace and lowercase the environment.
- Set `project` from the key and `received_at` from the server clock.
- Apply server-side redaction with the C4 list.

**Health and shutdown**
- `GET /healthz` reports whether the process is up.
- `GET /readyz` reports whether MongoDB is reachable and the service isn't shutting down.
- On SIGINT or SIGTERM:
  1. `/readyz` switches to 503.
  2. `http.Server.Shutdown` runs with a 30 s default timeout.
  3. The MongoDB client is closed.

**Logging:** slog JSON to stdout. Authentication failures, rejections, rate-limit hits and processor errors are logged at warning level.

**Configuration:** flags, with `REPORTER_SERVER_*` environment variables covering:
- listen address;
- TLS files;
- insecure mode;
- keys file;
- MongoDB URI and database;
- retention days per severity;
- rate limit and burst;
- request size and in-flight limits;
- shutdown timeout;
- environment.

### 5.5 Storage (MongoDB, mongo-driver v2)

**Location:** database `reporter` (configurable), collection `events`.

**Documents**
- The event fields plus `project`, `received_at` and `expire_at`.
- Attributes are stored as a list of `{k, v}` pairs rather than as field names. MongoDB is awkward with `.` and `$` in field names, and one index then covers every attribute key.

**Indexes**
- unique `(project, event_id)`;
- `(project, service, environment, severity, timestamp desc)`;
- `(attributes.k, attributes.v)`;
- a TTL index on `expire_at`.

**Retention**
- `expire_at` is `received_at` plus the retention for that severity: 90, 90 and 14 days by default, configurable.
- Changing the retention only affects new events.
- MongoDB removes expired documents in a background pass about every 60 seconds, so deletion can lag.

**Writes**
- `InsertMany` without ordering. Duplicate-key errors count as success, because the event is already stored.
- Any other error makes the request fail with `Unavailable`.
- Default write concern: majority, waiting for the journal; configurable. The production setting is confirmed once the production MongoDB is known (section 4).
- Indexes are created at startup; this is safe to repeat.
- The service fails fast if MongoDB can't be reached at startup.

**Access (C3)**
- The service user has read/write on its database.
- Engineers get a read-only role, used for the C5 checks.

### 5.6 Tooling

- **Tools:** buf, `protoc-gen-go` and `protoc-gen-connect-go` are pinned to fixed versions in the Makefile and installed into a repo-local `bin/` by `make tools`. Nothing is installed globally, and nothing is added to the SDK's or service's dependency graph.
- **Generated code:** committed, so consumers don't need any of these tools.
- **Lint:** `golangci-lint`, which is already installed in `~/go/bin`.
- **Tests:** always run with `-race`. Integration tests read `REPORTER_TEST_MONGO_URI` and skip when it isn't set. Locally it points at the MongoDB installed on the development machine; CI always sets it.
- **Go:** 1.27 for both modules. Go isn't on the PATH for non-interactive shells on this machine, so the Makefile uses `go` from your shell.

---

## 6. Phases

Each phase ends with its tests passing in CI.

### Phase 1: Layout, tooling and contract

**Deliverables**
- The layout from 5.1: the rename, both `go.mod` files and the `replace` directive.
- Makefile with pinned tools; `make tools generate lint test build`.
- `reporter.proto` as in 5.2, with the generated Go and Connect code committed.
- `docs/api.md`.
- A GitHub Actions workflow running lint and tests.

**Acceptance criteria**
- `make generate` on a clean checkout produces no diff.
- `buf lint` passes.
- `Severity` value 0 is `UNSPECIFIED`, and `project` and `received_at` don't appear in request messages.
- Both modules build, and CI is green.

**Tests**
- Binary round trip of a fully filled `NotifyRequest`.
- JSON round trip of the `docs/api.md` example. This fixes the JSON field names that other languages rely on.
- A size check: an event with 50 frames, 10 causes and 100 attributes stays under 1 MiB.

### Phase 2: Service ingest, without a database

**Deliverables**
- The `event`, `ingest`, `auth`, `server` and `config` packages.
- The memory and devlog processors.
- `reporter serve` and `reporter keygen`.

**Acceptance criteria**
- The same request is accepted over gRPC (Connect client in gRPC mode) and as curl-style JSON.
- Status codes:
  - a missing or wrong key returns 401;
  - an oversized request is rejected before decoding;
  - going over the rate limit returns 429;
  - going over the in-flight limit returns 503.
- Invalid events are rejected individually with a reason, and valid events in the same request are accepted.
- `project` and `received_at` only ever come from the server.
- On SIGTERM:
  - `/readyz` returns 503;
  - in-flight requests finish;
  - the process exits within the shutdown timeout.
- The devlog processor refuses to start when the service environment is `production`.

**Tests**
- Table tests for every validation rule and limit boundary, for redaction and for normalization.
- A fuzz test showing validation and normalization never panic.
- `httptest` server tests over both protocols:
  - success;
  - partial rejection;
  - authentication;
  - size limit;
  - rate limit;
  - in-flight limit;
  - processor error.
- A shutdown test with a slow processor.
- A test that a key from `keygen` passes the authentication check.

### Phase 3: SDK core

**Deliverables**
- `Configuration` and environment variables.
- `Configure`, `Notify`, `Info`, `Warn`, `Error` and `New`/`Notifier`.
- Event capture, stack capture, causes, redaction, UTF-8 cleanup and the environment filter.
- Queue, delivery goroutine, `Synchronous`, `MainContext` drain, `Stats()` and logger.

**Acceptance criteria**
- Checklist items 1–13 in 5.3 each have a passing test.
- `Info` and `Warn` send message-only events with no stack trace, and `Error` behaves as `Notify` at ERROR.
- In asynchronous mode, `Notify` never blocks, even when the queue is full or the server hangs.
- Each event gets exactly one attempt; failures are logged and counted.
- No data races, and calling `Configure` twice is safe.
- SDK dependencies are limited to those listed in 5.1.

**Tests**
- **Unit tests:**
  - error type and causes for plain errors, `%w`-wrapped errors, `errors.Join`, pkg/errors errors and errors with `Callers()`;
  - the frame cap and stack reuse;
  - `in_project`;
  - `SourceRoot` trimming;
  - redaction;
  - UTF-8 cleanup;
  - conversion of `Fields` values;
  - environment-variable precedence;
  - the environment filter;
  - the `Info`, `Warn` and `Error` helpers.
- **Delivery tests with `testing/synctest` and a fake transport:**
  - the 101st event is dropped and logged;
  - the request timeout;
  - the `MainContext` drain;
  - errors returned in `Synchronous` mode.
- **Transport tests** against an `httptest` server running the real generated handler:
  - the gRPC protocol is used;
  - the authentication header is sent;
  - TLS and plaintext modes both work.
- **`Notify` benchmarks** (time and allocations) recorded as a baseline.

### Phase 4: Panic handling

**Deliverables**
- The panic monitor, with its own cookie.
- The panic-text parser.
- `AutoNotify` and `Recover`.
- The `Handler`/`HandlerFunc` middleware.
- The disable switches.
- The guards for `go test` and Windows.

**Acceptance criteria**
- A crashing helper program produces exactly one unhandled ERROR event containing the panicking goroutine's frames. The program's exit code and stderr output are unchanged.
- `AutoNotify` reports and then re-panics. `Recover` reports and continues.
- The middleware reports handler panics, ignores `http.ErrAbortHandler`, and re-panics.
- **Coexistence with bugsnag-go is measured before the pilot.**
  - A helper program configures both SDKs and crashes, and the result is written up.
  - The write-up includes the setup chosen for the pilot.
  - It needs testing because both libraries re-run the program and redirect stderr, so they can duplicate or miss each other's panic reports.

**Tests**
- **Golden-file tests for the parser**, using real output from helper programs:
  - `panic(error)`;
  - `panic(string)`;
  - a write to a nil map;
  - an index out of range;
  - a custom panic value;
  - a panic in another goroutine;
  - a re-panic;
  - a `fatal error` from concurrent map writes.
- Subprocess tests that build the helper, point it at an `httptest` server, and check the event and the exit code.
- Middleware tests with `httptest`.
- Tests that the monitor doesn't start under `go test` or when disabled.

### Phase 5: MongoDB storage

**Deliverables**
- `storage/mongo`.
- Index creation.
- Retention and write-concern configuration.
- `/readyz` checking MongoDB.
- Setup notes for the local MongoDB: install, start as a single-node replica set, and the test URI.

**Acceptance criteria**
- One shared processor test suite passes for both the memory processor and the MongoDB processor.
- Sending the same event twice stores one document.
- `expire_at` matches the retention for each severity, and the TTL index exists.
- With MongoDB down, requests return 503 within the timeout, and the service recovers when MongoDB returns.
- Success is only returned after the write concern is met.

**Tests**
- The shared processor test suite.
- Integration tests against a real MongoDB, through `REPORTER_TEST_MONGO_URI`. CI uses a MongoDB service container.
- Tests for indexes and TTL, duplicates, and an outage.

### Phase 6: End-to-end and failure paths

These run the real SDK against the real service and MongoDB, with one named test for each scenario:

- **Delivery:** every field arrives, along with the source block, `project` and `received_at`.
- **Service unavailable:** `Notify` returns immediately, the event is logged and counted as failed, and the app is unaffected.
- **No retry:** when the server fails once, exactly one attempt is recorded and nothing is stored.
- **Queue overflow:** with the server hung, the 101st event is dropped and `Notify` still doesn't block.
- **Invalid payloads:** hand-made JSON and gRPC requests, one for each rejection reason.
- **Shutdown:** the SDK's `MainContext` drain delivers queued events, and the service's graceful shutdown finishes in-flight requests.
- **Panic:** a crashing helper program produces an event in MongoDB.
- **Other languages:** a plain HTTP/JSON request, like curl would send, produces an event in MongoDB.
- **TLS:** test certificates work, and plaintext is refused when TLS is configured.

**Acceptance criteria:** every test passes in CI, and the whole parity checklist in 5.3 is covered.

### Phase 7: Load test and defaults

**Deliverables**
- A Go load program in the repo: many goroutines using the SDK in `Synchronous` mode.
- `ghz` runs for raw gRPC load.
- A results document.

**Acceptance criteria**
- Measured events per second and p99 latency, on the target hardware with MongoDB.
- Defaults chosen, with the reasoning written down, for:
  - rate limit;
  - in-flight limit;
  - request size;
  - MongoDB connection pool.
- SDK `Notify` overhead recorded.

**Tests**
- Load runs started from a make target, with results that can be reproduced.

### Phase 8: Pilot and production readiness

**Deliverables**
- TLS certificates and API keys issued.
- Run instructions, including a systemd unit example.
- **Runbook:**
  - start and stop;
  - adding and rotating keys;
  - changing retention;
  - read-only MongoDB queries for the C5 checks.
- **SDK usage guide:**
  - `Configure` first in `main`;
  - the panic caveats;
  - the middleware;
  - the environment variables.
- **Pilot:** one service reporting to both Bugsnag and reporter, with a comparison report.

**Acceptance criteria**
- The pilot runs for an agreed number of days. Daily event counts per severity match Bugsnag within an agreed tolerance, and any differences are explained.
- No measurable impact on the pilot service.
- Every §6 control maps to a test or a configuration setting.
- Sign-off to move more services across.

**Tests**
- The comparison queries.
- A soak run.
- Restarting the service during traffic, where drops are expected, and checking they're logged and counted.

---

## 7. Risks accepted for v1

- **More loss than the proposal intended.** With no retries, any service restart, network blip or MongoDB hiccup drops the events in flight.
- **Shutdown loss.**
  - Events still queued when `main` returns are lost unless `MainContext` is cancelled and the app waits for the drain.
  - The drain can take up to 100 × 5 s if the service hangs.
  - Bugsnag has the same limitation.
- **The panic monitor:**
  - code before `Configure` runs twice;
  - the app's stderr flows through the monitor, so if the monitor is killed, the app dies on its next write to stderr;
  - it works only on Linux and macOS.
- **Running both SDKs' panic handlers during the pilot** may duplicate or miss panic events. Phase 4 measures this first.
- **No metrics or alerts.** Problems only show up in logs until Prometheus is added.
- **Go 1.27 floor.** Every service that imports the SDK must use Go 1.27.
- **Rate limits are per instance** when more than one service instance runs.

---

## 8. After v1 (optimization backlog)

- Retries with backoff and jitter, using the existing `event_id` for deduplication.
- Batching, which the contract already allows.
- `Flush`/`Close` with bounded timeouts.
- A queue bounded by bytes, with priority by severity.
- Prometheus metrics and the C9 alerts.
- A UI, and error grouping.
- An slog adapter.
- Moving the SDK to its own repository, if that's wanted later (it changes the import path in every service using it).
