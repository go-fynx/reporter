# Architecture review: centralized logging and error reporting

**Status:** Review awaiting approval. No implementation has started.
**Date:** 2026-10-29
**Reviews:** [reporter_proposal.md](reporter_proposal.md)
**Superseded for v1 by:** [implementation_plan.md](implementation_plan.md), with decisions from [answers.csv](answers.csv) (2026-11-12)

## Current state

The only file in the repo is `docs/reporter_proposal.md`. `reporter-sdk/` and `reporting-service/` are empty. There's no proto module, no `go.mod` and no git repo yet. Go 1.27.1 is installed at `/usr/local/go`, but it isn't on the PATH for non-interactive shells. Neither `protoc` nor `buf` is installed. **None of the requirements are met yet.** Everything below is design review.

---

## 1. Understanding of the system

The goal is an internal replacement for Bugsnag-style error reporting that also collects application log events in one place, starting with Go services. There are three things to build now and one deferred:

| Component | Role | Proposal |
|---|---|---|
| `reporting-proto` | Versioned gRPC/protobuf contract and generated Go types, used by both the SDK and the service | §3.3 |
| Go Reporting SDK | Library inside each app: capture an event → add it to a bounded queue → a background worker batches it → send over gRPC | §3.1, §5 |
| Reporting Service | Stateless gRPC ingest: authenticate → validate → normalize → hand to `EventProcessor` | §3.2, §3.4 |
| Storage | Sits behind the processor interface; database not chosen yet | §3.4, §7 |

### Confirmed requirements (stated in the proposal)

- **SDK (§3.1, §5)**
  - Configuration that works like Bugsnag's.
  - Info, warning and error events, with error details and stack traces "where available".
  - Every event carries service, environment, app version, timestamp and custom attributes.
  - A bounded in-memory queue and asynchronous batched gRPC delivery that never blocks request processing.
  - `Flush` and `Close` with bounded timeouts.
  - Configurable queue size, events per batch, batch bytes, flush interval, RPC deadline, retry limit, and exponential backoff with jitter.
  - A defined policy for a full queue, with a dropped-event metric.
- **Service (§3.2, §3.4)**
  - Authentication by API key or service identity, with each credential limited to its service or project.
  - Validation of fields, batch size and payload size, then normalization.
  - Processing that doesn't depend on storage, and no database logic in the gRPC handlers.
  - Accept/reject results "where practical".
  - Health checks, metrics and graceful shutdown.
- **Contract (§3.3):** a separate versioned module containing the event and severity definitions, the batch request/response messages, and generated client/server code.
- **Event fields (§4):** `event_id`, `service`, `environment`, `severity`, `message`, `error_type`, `stack_trace`, `timestamp`, `attributes`. Size limits must be set before production.
- **Security (§6)**
  - TLS, scoped credentials, and limits on size, batch count and request rate.
  - Input validation.
  - Redaction before sending or storing.
  - Collect as little personal data as possible.
  - A retention and access policy before production events are stored.
  - Clients can't overwrite trusted identity fields.
  - No privileged secrets in the SDK config.
- **Tests (§7):** successful delivery, service unavailable, retries, queue overflow, invalid payloads, and shutdown.
- **Excluded:**
  - Distributed tracing, trace IDs and span IDs.
  - Dashboard and search UI.
  - Alerting.
  - Choosing the database, and advanced retention or search.
  - Durable client-side storage or a message broker (unless reliability requirements demand one).

### Gaps and inconsistencies in the proposal

1. §3.1 requires the **application version**, but the §4 event table leaves it out.
2. "Maximum in-memory queue size" doesn't say whether it counts **events or bytes**. A count alone doesn't bound memory.
3. §6 limits credentials to a "service or **project**", but "project" is never defined.
4. §3.4 names the interface `EventProcessor` but calls it the *storage* abstraction. Grouping and alerting are out of scope, so nothing happens between normalizing and storing. It should be **one interface, not a processing pipeline**.
5. **Where the logs come from isn't defined.** Is it only events sent through SDK calls, or also logs services already write with slog, zap or stdout? The answer changes volume by orders of magnitude.
6. **There's no way to read the data.** The dashboard is excluded, but replacing Bugsnag means engineers have to see errors somewhere.
7. §9 asks which metrics should detect dropped events, but drops happen **inside application processes**. Nothing in the proposal reports them centrally.

---

## 2. Architecture review

### 2.1 What's sound and should stay

- **Asynchronous SDK with a bounded queue and an explicit loss policy.** That's the right trade-off: reporting must never take an application down.
- **Batched unary gRPC.** I recommend keeping unary calls rather than streaming. A stream pins a client to one backend and blurs where each acknowledgement ends.
- **One versioned contract** shared by the SDK and the service.
- **The processor seam,** with the database decision deferred.
- **No message broker.** Batching in the SDK plus synchronous ingest is enough until the volume numbers say otherwise.

### 2.2 Delivery semantics

**Proposed guarantee (needs approval):** *best-effort, at-least-once delivery from the SDK queue to storage. Events can be lost in the cases listed below. They can be delivered more than once, and storage deduplicates them by `event_id`.*

| # | Scenario | Result | Detected by | Mitigation |
|---|---|---|---|---|
| 1 | Queue full | New event dropped | SDK `dropped{queue_full}` counter | Limit by count *and* bytes; drop info and warning events first (B3) |
| 2 | Crash, SIGKILL, OOM, `os.Exit`, `log.Fatal` | Queued and in-flight events lost | Can't be detected | Accepted per §7; a short flush interval limits exposure |
| 3 | Panic nobody recovers | The panic and everything queued are lost | Can't be detected | Optional `Recover` helper that sends before re-panicking (B7) |
| 4 | `Close` deadline expires | Remaining events dropped | `dropped{shutdown}` | Size the Close timeout to fit the orchestrator's grace period |
| 5 | Retries run out | Batch dropped | `dropped{retry_exhausted}` | Bounded retry budget |
| 6 | Server rejects an invalid event | Event dropped, never retried | Response, plus a counter on each side | SDK truncates and sanitizes first, so this should be rare |
| 7 | Server acknowledges before a durable write, then crashes | Acknowledged events lost | Client can't see it | Acknowledge only after `Process` returns (A2) |
| 8 | Server commits, but the response is lost | SDK retries, creating a duplicate | — | Stable `event_id` plus idempotent storage (A4) |
| 9 | Storage commits part of a batch, then errors | Retry duplicates the committed part | — | Same as #8 |
| 10 | Invalid UTF-8 in any string, e.g. `err.Error()` | **Go protobuf won't marshal the whole batch, so the batch fails every time** | Marshal error | SDK cleans strings when the event is captured |
| 11 | One event is bigger than the batch or server size limit | Rejected on every attempt | `RESOURCE_EXHAUSTED` | Truncate when captured; never build a batch over the server limit |

**When to acknowledge a batch** (left open in §3.4 and §9):

| Option | Lost if the server crashes | RPC latency | Server state | Backpressure |
|---|---|---|---|---|
| A. After validation | Yes | Lowest | Needs a buffer anyway | Clients can't see when storage is overloaded |
| B. After queuing in a server buffer | Yes, whatever is buffered | Low | A second queue to size and drain | Must reject explicitly when the buffer is full |
| C. **After `Process` returns** | No, once storage is durable | Includes the storage write | **Stateless** | Built in: slow storage slows RPCs, and the SDK queue absorbs or drops |

**Recommendation: C.**
- **Why not B:** the SDK already batches, so a server buffer mostly adds a second loss window (#7) and a second queue to tune.
- **What C gives:** a stateless service, a trivial graceful shutdown, and a guarantee you can state honestly.
- **Cost:** RPC latency includes the database write. Some databases, ClickHouse in particular, handle many small inserts poorly.
- **If storage needs buffering:** a buffering processor can wrap storage later without changing the contract. That would bring back loss window #7, so it would need re-approval.

**One more caveat:** with the development processor, "acknowledged" only means "validated and logged". The service should refuse to start with it in production.

**Partial acceptance:** reject individual events, not whole batches.
- **Why:** rejecting a whole batch either throws away 499 valid events or retries forever.
- **Response:** status OK, plus `accepted_count` and a list of `{event_id, reason}` rejections.
- **Request-level errors are only for:**
  - authentication failures (`UNAUTHENTICATED` / `PERMISSION_DENIED`)
  - malformed batches (`INVALID_ARGUMENT`)
  - overload (`RESOURCE_EXHAUSTED` with `RetryInfo`)
  - processor failure (`UNAVAILABLE`)

**How the SDK classifies errors for retry**

- **Retry:**
  - `UNAVAILABLE`, `DEADLINE_EXCEEDED`, `ABORTED`, `INTERNAL` and `UNKNOWN`, all bounded by the retry limit.
  - `RESOURCE_EXHAUSTED`, **only when the server attaches `RetryInfo`**. gRPC also uses `RESOURCE_EXHAUSTED` for "message larger than max", and retrying can't fix that.
- **Never retry:** `INVALID_ARGUMENT`, `UNAUTHENTICATED`, `PERMISSION_DENIED`, `FAILED_PRECONDITION`, `UNIMPLEMENTED`, and per-event rejections. Count them, and log at a limited rate through the SDK's own logger.
- **Backoff:** exponential with full jitter and a cap, following any `RetryInfo` delay. Every wait can be interrupted by the `Close` context.
- **Event IDs:** identical on every attempt.
- **Write the retry loop ourselves** rather than using gRPC's built-in retry policy:
  - built-in retries are invisible to the SDK's drop counters;
  - they're limited by gRPC's internal retry buffer;
  - they can't make the `RetryInfo` distinction above.
  - The cost is one small file we own and test.

**Backpressure chain:** slow storage → slower RPCs → the SDK worker waits (one batch in flight) → the queue fills → admission drops info events first → drops are counted.

On the server, `MaxConcurrentStreams` only applies per connection, so total in-flight RPCs are **unbounded**. The fix:
- Add a server-wide in-flight limiter that rejects new requests (`RESOURCE_EXHAUSTED` with `RetryInfo`) instead of queuing them.
- Worst-case memory is then `max_inflight × max_recv_msg`.

**Batch limits:**
- A batch closes at whichever comes first: maximum events, maximum encoded bytes, or the flush interval measured from its first event.
- SDK batch bytes must stay below the server's maximum receive size (gRPC defaults to 4 MiB).
- Config validation enforces `max_event_bytes ≤ max_batch_bytes`.
- The server's limits are authoritative.

**Graceful shutdown**

- **SDK `Close(ctx)`**
  - Safe to call more than once.
  - Stops accepting events; later calls are counted as `dropped{shutdown}`.
  - Flushes within `ctx` and cuts any backoff wait short.
  - Closes the connection.
  - Returns an error that includes the number of undelivered events.
- **SDK `Flush(ctx)`:** waits until everything queued before the call has been sent or dropped.
- **Signals:** the SDK **never installs signal handlers**; the application owns its lifecycle.
- **Service shutdown on SIGTERM:**
  1. Health switches to `NOT_SERVING`.
  2. A short drain period lets load balancers stop routing.
  3. `GracefulStop` with a timeout, falling back to `Stop`.
  4. The processor is closed.
  - Option C buffers nothing, so a graceful shutdown loses nothing.

### 2.3 SDK risks

- **Global state.**
  - `reporting.Init` with package-level functions is hard to test and can race on a double init.
  - Recommendation: a `Client` type created with `New(Config) (*Client, error)`, with package-level functions that delegate to a default client.
  - This doesn't change §3.1, which says the API is only illustrative.
- **The caller can change Fields after the call.** The map holds `any` values and the worker reads them later. Copy them and convert values to scalars **at capture time**.
- **Cost on the request path.**
  - When capturing: copy fields, generate the ID, take the timestamp, record raw stack addresses (`runtime.Callers`), and do a non-blocking enqueue.
  - On the worker goroutine: turn addresses into frames and marshal the protobuf.
- **Go errors don't carry stack traces.** "Where available" needs a definition. The practical choice is the stack where `Error()` was called, which shows where the error was *reported*, not where it *started*.
- **`error_type` for wrapped errors.** For an error wrapped with `fmt.Errorf(...%w)`, `%T` gives `*fmt.wrapError`, which tells you nothing (A8).
- **The SDK must never panic into the app or report through itself.** Recover in the worker, and send SDK diagnostics to an optional `*slog.Logger`.
- **Every service inherits the SDK's dependencies.** Keep them to grpc, protobuf and the proto module. Don't pull in Prometheus or a logging framework; expose a `Stats()` method instead.
- **The SDK's `go` directive sets the minimum Go version for every service that imports it.** grpc-go's own minimum adds a floor of its own (B11).
- **Plaintext by accident.**
  - Insecure connections must be an explicit opt-in for development.
  - gRPC already refuses to send credentials that require TLS over plaintext.
- **A disabled mode** (no endpoint configured, or `Enabled=false`) is needed so consuming services' tests don't need a server.

### 2.4 Service risks

- **Load balancing.**
  - gRPC keeps HTTP/2 connections open for a long time. A layer-4 balancer, such as a Kubernetes ClusterIP Service, pins each client to one replica.
  - Fix: set the server's `MaxConnectionAge` whatever the balancer, and use a gRPC-aware layer-7 balancer if available.
- **Trusted fields.**
  - The project comes from the API key, and the service is checked against the key's allowed services.
  - `received_at` is set by the server.
  - Server-owned fields **don't exist** in the client message at all.
- **Client clocks.** Keep the client `timestamp` and add `received_at`. Reject only missing or absurd timestamps, and never rewrite them.
- **`event_id` can be abused.** If deduplication were global, one client could claim another tenant's IDs. Make the dedup key `(project, event_id)`.
- **Keep normalization small:**
  - trim and lowercase the environment;
  - check UTF-8;
  - redact on the server too, as a second layer;
  - set project, service and `received_at`.
  - No enrichment pipeline.
- **Bound processing time on the server** with its own timeout, in addition to the client's deadline.

### 2.5 Contract risks

- **Proto3 rules:**
  - enum value 0 must be `SEVERITY_UNSPECIFIED`;
  - never reuse a field number, and reserve any you remove;
  - name the package `reporting.v1`.
  - Run `buf lint` and `buf breaking` in CI.
- **Some choices are expensive to change later,** because changing a field's type breaks the contract: attribute value types (A6), stack trace shape (A7), and batch-level vs per-event source fields (A5). **Settle these before Phase 1.**
- **Keep generated types out of the SDK's public API.** Then contract changes never break application code.
- **Old SDK versions will run for months.** The server must accept them, and each batch should carry `sdk_name` and `sdk_version` for diagnosis.

### 2.6 Security

- **TLS:** agreed. It still has to be decided where TLS terminates: in the service, at ingress, or in a service mesh (B12).
- **API keys:**
  - Store only SHA-256 hashes. Keys are high-entropy random values, so a slow key-derivation function isn't needed.
  - Allow several active keys per scope so keys can be rotated.
  - Keys can only *submit* events, which keeps §6's "no privileged secrets in the SDK" true.
  - Service identity via mTLS is stronger, but only worth it if a mesh or PKI already exists.
- **Rate limits:**
  - A token bucket per key, kept in memory on each replica (`golang.org/x/time/rate`), returning `RESOURCE_EXHAUSTED` with `RetryInfo`.
  - Limits are approximate across replicas, which is acceptable for v1.
- **Redaction:**
  - A case-insensitive list of attribute keys to redact (password, secret, token, authorization, cookie, api_key, and so on), applied in the SDK **and** again on the server.
  - **Free-text messages and error strings can't be redacted reliably.** Document that rule for developers.
- **Personal data:** error strings often contain emails and IDs. The §6 retention and access policy is a **hard prerequisite** before Phase 7 stores production events.
- **Validation:** check lengths, counts, and the allowed characters in service and environment names, which also keeps their number of distinct values under control.

### 2.7 Storage evaluation (kept separate; nothing chosen)

**Inputs needed:**
- events per second, average and peak, broken down by severity;
- average and p99 event size;
- retention per severity;
- query patterns: filters (service, environment, severity, time), text search on messages, attribute filters, counts over time;
- whether duplicates must be removed exactly or eventually;
- deletion needs, such as erasure requests;
- whether the database is managed or self-hosted, and what the team knows;
- how data is read in this phase (C5).

| | PostgreSQL (partitioned) | ClickHouse | OpenSearch |
|---|---|---|---|
| Exact deduplication | Yes: unique `(project, event_id, timestamp)` with `ON CONFLICT DO NOTHING`. A partitioned table's unique key must include the partition column; that still works because retries resend the same timestamp | Eventual (ReplacingMergeTree) or block-level insert dedup | Yes: use `event_id` as the document `_id` |
| High-volume ingest | Moderate | Very high, but wants large batches | High, but resource-heavy |
| Retention | Drop partitions | Built-in TTL | Lifecycle policies (ILM) |
| Text search | Basic | Basic to moderate | Strong |
| Operations cost | Lowest | Medium | Highest |

**Preliminary view (not a decision):**
- If volume is mostly errors and moderate, PostgreSQL is the lowest-risk choice.
- If high-volume info logs are central, ClickHouse fits better, and A2 should be revisited.
- Avoid running two databases unless the numbers force it.

**Whatever database is chosen,** `Process` must be idempotent per `(project, event_id)`.

### 2.8 What I'm deliberately not recommending

- A broker, a separate worker service, or a client-side disk spool.
- Streaming RPCs.
- A processor pipeline or plugin framework, repository layers, or dependency-injection frameworks.
- OpenTelemetry in the SDK, or any tracing fields.
- A distributed rate limiter.
- A server-side in-memory dedup cache: it doesn't survive restarts or work across replicas.

---

## 3. Recommended structure

**One repo with three Go modules.**
- A contract change ships together with the SDK and service changes that depend on it.
- Separate modules keep the service's dependencies (database drivers, metrics libraries) out of every app that imports the SDK.

```
reporter/
├── go.work                      # committed; ignored when a module is consumed as a dependency
├── docs/
│   ├── reporter_proposal.md
│   ├── architecture_review.md
│   └── decisions/               # one short ADR per approved decision
├── reporting-proto/             # §3.3; tags: reporting-proto/vX.Y.Z
│   ├── buf.yaml, buf.gen.yaml
│   └── reporting/v1/            # .proto plus committed generated *.pb.go
├── reporting-sdk/               # package `reporting` (rename of reporter-sdk/, see B11)
│   ├── client.go   global.go   config.go   stats.go
│   ├── event.go    redact.go   stack.go           # capture path
│   ├── queue.go    sender.go   transport.go       # delivery path
│   └── internal/testserver/     # fake ReportingService for SDK tests
└── reporting-service/
    ├── cmd/reporting-service/   # config, wiring, signals, shutdown order
    ├── e2e/                     # SDK + service integration tests
    └── internal/
        ├── event/       # internal Event, Validate, Normalize, limits; imports no proto and no DB
        ├── ingest/      # gRPC handler, proto↔event conversion, error→status mapping; defines Processor
        ├── auth/        # API-key interceptor, hashed key store, per-key rate limiter
        ├── processor/devlog/  processor/memory/   # development and test implementations
        ├── storage/<db>/      # Phase 7
        ├── server/      # grpc.Server limits, keepalive, in-flight limiter, health, metrics
        └── config/
```

**Dependency rules:**
- `event` imports nothing internal.
- `ingest` imports `event` and the proto module.
- `processor/*` and `storage/*` import only `event`.
- Only `main` connects them.

So transport and storage both depend on `event` and never on each other, which is how §1's independence requirement gets met.

**The SDK is one flat package** with unexported internals. The batching and retry logic sits behind a small unexported sender seam so it can be tested deterministically with `testing/synctest`.

**Processor contract** (described here; the code comes later):
- `Process(ctx, events)` returns nil only once events are as durable as that implementation promises.
- It's idempotent per `(project, event_id)` and respects the `ctx` deadline.

---

## 4. Decisions that need approval

### Group A: needed before Phase 1 (fixes the contract)

| # | Decision | Recommendation | Trade-off / alternative |
|---|---|---|---|
| A1 | Delivery guarantee | Best-effort at-least-once with deduplication (§2.2) | Durable delivery needs a client spool or a broker, both out of scope |
| A2 | When to acknowledge | After `Process` returns (C) | B is faster but adds a loss window |
| A3 | Invalid events in a batch | Reject per event in the response | Rejecting whole batches is simpler but loses valid events |
| A4 | Event ID | SDK generates a UUIDv7 (time-ordered, index-friendly); required; stable across retries; dedup on `(project, event_id)` | Server-generated IDs make deduplication impossible |
| A5 | Per-process fields | One `source` block per batch (service, environment, app version, hostname, SDK version). The stored event still has every §4 field | Per-event fields (as in the §4 table) map more simply but send more bytes |
| A6 | Attribute values | Flat map of key → scalar (string, int64, double, bool) | `map<string,string>` loses types; `Struct` allows nesting and is hard to bound |
| A7 | Stack trace | Repeated `{function, file, line}` frames, capped, captured where `Error()` is called; first reuse a stack already attached to the error via a `Callers() []uintptr` method (no extra dependency) | A single string is simpler but harder to limit or group later |
| A8 | `error_type` | Type of the innermost error in the `Unwrap` chain (first error for `errors.Join`) | Outermost type, usually `*fmt.wrapError` |
| A9 | Severities | `UNSPECIFIED` (rejected), `INFO`, `WARNING`, `ERROR` | Confirm: no DEBUG or FATAL |
| A10 | Fields to add | `app_version` (fixes the §3.1/§4 gap), server-set `received_at` and `project`; `hostname` optional | Each one needs a decision |
| A11 | Oversized data | SDK truncates at UTF-8 character boundaries and sets a `truncated` flag; server rejects anything over its hard limits | Server-side truncation silently changes data |
| A12 | Client drop visibility | `Stats()` in the SDK, plus drop counters carried in each batch header and exposed as metrics per service on the server | `Stats()` only, so drops are only visible if every app exports them |
| A13 | Starting limits (until Phase 6) | Queue 10k events / 16 MiB; batch 500 events / 1 MiB; flush 1 s; RPC deadline 5 s; 3 retries with 200 ms→5 s backoff; message 8 KiB; 64 attributes; 64 frames; server receive limit 4 MiB | Starting points only, replaced by load-test results |

### Group B: needed before the service and SDK phases

| # | Decision | Recommendation |
|---|---|---|
| B1 | Authentication | API key as bearer metadata over TLS, hashed, submit-only, with rotation. **Open question: does a service mesh or PKI already exist?** If so, mTLS is an option |
| B2 | Key scope | Key → project plus allowed services (and optionally allowed environments). **Needs a definition of "project".** |
| B3 | Queue policy | Limit by count and bytes; drop newest; info and warning admitted only below about 80% full, errors up to 100% |
| B4 | Retries | Our own retry loop, classified as in §2.2, one batch in flight per client |
| B5 | Public API shape | `Client` with global wrappers; `New` returns an error; `Flush(ctx)` and `Close(ctx)`; disabled mode; `Info` and `Warn` accept optional Fields; `Error(nil)` does nothing and is counted |
| B6 | Service metrics | Prometheus, unless the organization standardizes on OpenTelemetry metrics |
| B7 | Panic helper (`Recover`) | Include it: it's small and closes loss case #3. **Not in the proposal, so it needs an explicit decision.** |
| B8 | `slog` handler adapter | Defer until C6 is answered |
| B9 | Module paths | **Needs the VCS host and organization**, e.g. `github.com/<org>/reporter/...` |
| B10 | Tooling | `buf` for linting, breaking-change checks and code generation; generated code committed |
| B11 | Naming and Go floor | Rename `reporter-sdk/` → `reporting-sdk/` to match `reporting-service` and `reporting-proto`. **Open question: what's the oldest Go version among the services that will import the SDK?** |
| B12 | Deployment | Platform (Kubernetes?), where TLS terminates, which load balancer; `MaxConnectionAge` either way |

### Group C: needed before storage and production

- **C1 Capacity:** events per second (average and peak) by severity, and event sizes.
- **C2 Database:** chosen using §2.7.
- **C3 Retention:** retention per severity, and the access policy.
- **C4 Personal data:** the redaction key list and the personal-data policy.
- **C5 Reading the data:** how engineers read stored errors in this phase. Options are direct SQL, a small CLI, or keeping Bugsnag until a UI exists.
- **C6 Log sources:** SDK calls only, or also existing slog, zap or stdout logs.
- **C7 Error grouping:** grouping errors by fingerprint, which is Bugsnag's core feature. Adding the field later doesn't break the contract, so I recommend deferring it.
- **C8 Bugsnag migration:** whether services report to both during a transition, and the criteria for switching over.
- **C9 Alerts:** drop rate, rejection rate, authentication failures, processor errors, latency.

---

## 5. Phased plan

This follows the order in proposal §8. Every phase runs `go vet`, the linter and `go test -race`.

### Phase 0: Decisions (no code)
- **Deliverables:**
  - Answers to Groups A and B.
  - ADRs in `docs/decisions/`.
  - The written delivery-guarantee statement.
- **Acceptance:** every A and B item has an approved option, and the loss/duplicate table is accepted.

### Phase 1: Scaffolding and contract v1
- **Deliverables:**
  - The three modules and `go.work`.
  - buf configuration.
  - `reporting/v1` proto with committed generated code.
  - Make targets and CI (format, vet, lint, race tests, `buf lint`, `buf breaking`).
  - Installing `buf` locally and putting Go on the PATH.
- **Acceptance:**
  - `buf lint` passes.
  - Regenerating code produces no diff.
  - The SDK and service modules compile against the proto module.
  - Every enum's zero value is UNSPECIFIED.
  - The client message has no server-owned fields.
- **Tests:**
  - Round-trip marshalling of a fully populated request.
  - A size test proving an event at every maximum fits inside the batch byte budget.
  - A baseline for `buf breaking`.

### Phase 2: Service core
- **Deliverables:**
  - The `event` package: validation, normalization, limits.
  - The `ingest` handler with per-event rejections.
  - The authentication interceptor and the store of hashed keys.
  - Limits: receive size, events per batch, in-flight limiter, processing timeout, per-key rate limit.
  - The devlog and memory processors.
  - gRPC health service, metrics, graceful shutdown.
  - The rule that devlog won't start in production.
- **Acceptance:**
  - `ingest` imports no storage or processor implementation.
  - Every rejection has a reason code.
  - Trusted fields come only from authentication.
  - On SIGTERM, health switches to NOT_SERVING, in-flight RPCs finish, and the process exits within the timeout.
- **Tests:**
  - Table tests at every validation boundary: invalid UTF-8, unknown severity, missing ID, duplicate IDs in one batch, timestamp bounds, redaction.
  - **Fuzz test** that validation and normalization never panic.
  - Authentication: missing, invalid and revoked keys; service outside the key's scope; a payload trying to override the project.
  - In-process gRPC (`bufconn`):
    - partial acceptance;
    - oversized message;
    - too many events;
    - processor error → `UNAVAILABLE`;
    - in-flight limit → `RESOURCE_EXHAUSTED` with `RetryInfo`;
    - rate limit.
  - Shutdown: a slow `Process` finishes during `GracefulStop`.

### Phase 3: SDK core
- **Deliverables:**
  - Config and its validation.
  - `Client`, `New`, `Init` and the global functions.
  - Capture: ID, timestamp, field copy, redaction, truncation, UTF-8 cleanup, stack addresses.
  - The bounded queue with its admission policy.
  - The batcher.
  - `Flush`, `Close`, `Stats` and disabled mode.
  - The internal test server.
- **Acceptance:**
  - Capturing never blocks when the server stalls.
  - `Close` returns by its deadline, with no goroutines left behind.
  - No generated types in the public API.
  - Dependencies match the approved list.
- **Tests (using `synctest` for timing):**
  - A batch closes on count, bytes or interval.
  - `Flush` waits only for events queued before it.
  - `Close` with an expired context returns promptly and reports undelivered events.
  - Captures after `Close` are counted.
  - Overflow drops the newest event; info is rejected at the threshold while errors are still accepted.
  - Changing Fields after the call has no effect (checked with the race detector).
  - Invalid UTF-8 doesn't poison a batch.
  - Truncation lands on character boundaries.
  - Benchmarks record time and allocations per capture as a baseline.

### Phase 4: SDK reliability
- **Deliverables:**
  - Retry with backoff and jitter, using the classification from §2.2.
  - Honoring `RetryInfo`.
  - Handling per-event rejections.
  - Drop counters by reason.
  - Drop counters carried in batch headers.
- **Acceptance:**
  - Event IDs are identical across attempts.
  - Retries never exceed the limit.
  - Errors classed as non-retryable are never retried.
  - `Close` interrupts a backoff wait.
  - Drop counts are exact.
- **Tests:**
  - Scripted fake-server sequences:
    - fail N times, then succeed;
    - always `UNAVAILABLE`;
    - `INVALID_ARGUMENT`;
    - `UNAUTHENTICATED`;
    - `RESOURCE_EXHAUSTED` with and without `RetryInfo`;
    - server commits, then the call times out → a duplicate ID is observed.
  - A property test for backoff bounds.

### Phase 5: Integration and failure paths (each §7 item gets a named test)
These run the real SDK against the real service, over TLS with test certificates and an API key, using the memory processor.
- **Successful delivery:** fields, source block and server-set project are all correct.
- **Service unavailable:**
  - started late → events are delivered within the retry budget;
  - never started → events are counted as `retry_exhausted` and the app is unaffected.
- **Retries:** injected failures still store each event exactly once, thanks to deduplication.
- **Queue overflow:** with processing stalled, capture latency stays bounded, drops are counted, and errors are kept in preference.
- **Invalid payloads:** raw gRPC requests that bypass the SDK, one for each rejection reason.
- **Shutdown:** SDK `Close` against a slow server stays bounded; a service `GracefulStop` mid-send leads to a retry or a counted drop.
- **Security:** plaintext is refused when TLS is required, and a bad key is rejected.

### Phase 6: Load testing and defaults
- **Deliverables:**
  - `ghz` load runs against the service.
  - A harness that runs many SDK clients.
  - A results document and the chosen defaults.
- **Acceptance:**
  - Each default is backed by a recorded measurement.
  - The service handles the C1 peak with agreed headroom, at an agreed p99 latency.
  - SDK memory stays within its limits under overload.
  - No drops at expected steady-state volume.
- **Tests:**
  - Load runs that can be repeated with make targets.
  - Benchmark regression thresholds.

### Phase 7: Storage
- **Deliverables:**
  - The storage selection document (C1–C3 answered).
  - The storage processor, schema and migrations.
  - Retention.
  - Idempotent inserts.
- **Acceptance:**
  - **One shared processor test suite** passes for both the memory and the database implementations.
  - A duplicate submission produces one row.
  - Acknowledgement happens only after commit.
  - Retention removes expired data.
- **Tests:**
  - The test suite run against a real database in a container.
  - A database outage → `UNAVAILABLE` → the SDK retries.
  - Phase 6 throughput re-run against real storage.

### Phase 8: Production readiness
- **Deliverables:**
  - A security review that maps every §6 control to a test or a config setting.
  - The procedure for rotating keys.
  - Alert definitions (C9).
  - A runbook.
  - The published delivery guarantee.
  - An SDK usage guide.
  - The Bugsnag migration plan.
- **Acceptance:**
  - Key rotation tested in staging.
  - A pilot service calls `Close` in its shutdown.
  - Alerts fire in a staging drill.
- **Tests:**
  - A staging soak test.
  - Killing a replica mid-traffic.
  - Rotating a key under load with no loss beyond what's documented.

---

## Next steps

**Direction update (2026-10-30):** v1 will match Bugsnag's behaviour (no retries, Bugsnag-style panic handling) and deliver over gRPC instead of HTTP. Retries, batching and the other reliability work above become later optimizations. The `v1_bugsnag_parity` and `v1_status` columns in the CSV show what this means for each decision. Where parity conflicts with the proposal, the row is marked for confirmation rather than changed silently.

Implementation is on hold until this review is approved. Every decision is listed in [decision_questions.csv](decision_questions.csv) with its options, the recommended option, and how Bugsnag handles the same case. Fill in `your_answer` and `your_notes` and return it as `answers.csv`. The CSV splits B1, B5 and B11 into `a`/`b` rows and adds B5b (configuration from environment variables).

Before Phase 1 starts, these are needed:

- **Groups A and B:** approval or changes. Approving the recommendations as-is is fine.
- **B9:** the VCS host and organization for module paths.
- **B11:** the oldest Go version among the services that will import the SDK.
- **B1:** whether a service mesh or PKI exists.
- **B12:** the deployment platform.

Group C can wait until Phase 6.
