
# Proposal: Centralized Logging and Error Reporting System

**Status:** Initial proposal for review  
**Scope:** Logging and error reporting  
**Storage:** Database selection to be decided separately

## 1. Purpose

The goal is to build an internal reporting system to reduce our dependency on Bugsnag for application error reporting and provide a centralized place to receive, process, and store application logs and error events.

The initial implementation will focus on Go services. The design should keep event processing independent of the transport layer and database so that these components can be maintained and changed independently.

Distributed tracing, trace IDs, and span IDs are outside the scope of this phase. They can be designed separately when required.

## 2. Proposed Architecture

```text
Go Application
      |
      v
Go Reporting SDK
  - Capture events
  - Bounded in-memory queue
  - Background worker
  - Batch delivery
      |
      | gRPC
      v
Central Reporting Service
  - Authentication
  - Event validation
  - Event normalization
  - Event processing
  - Storage abstraction
      |
      v
Database
(To be selected)
```

The Go SDK will capture reporting events and send them asynchronously to the central reporting service. The service will validate and process the events before passing them to the storage layer.

The database implementation will remain independent of the SDK and gRPC API.

## 3. Components

### 3.1 Go Reporting SDK

A reusable Go package that can be imported into any Go service.

Responsibilities:

- Provide a simple configuration similar in concept to Bugsnag.
- Capture information, warning, and error events.
- Support error details and stack traces where available.
- Include service name, environment, application version, timestamp, and custom attributes.
- Add events to a bounded in-memory queue.
- Send events asynchronously in batches using gRPC.
- Provide `Flush` and `Close` methods for graceful shutdown.
- Apply RPC timeouts and bounded retry behavior.
- Avoid blocking normal application request processing while delivering reports.

Illustrative API:

```go
reporting.Init(reporting.Config{
    APIKey:      "...",
    Endpoint:    "reporting.internal:443",
    Service:     "booking-api",
    Environment: "production",
})

reporting.Info("Booking request received")

reporting.Warn("Third-party response was slow")

reporting.Error(err, reporting.Fields{
    "operation": "create_booking",
})
```

This API is an example. Exact method names and configuration fields can be finalized during implementation.

### 3.2 Central Reporting Service

A standalone Go service exposing a gRPC API for the Go SDK.

Responsibilities:

- Authenticate clients using an API key or an appropriate service identity.
- Validate event fields, batch size, and payload size.
- Receive batches of reporting events.
- Normalize events into a common internal representation.
- Pass validated events to a storage-independent processing layer.
- Return clear acceptance or rejection results where practical.
- Expose health checks and service metrics.
- Support graceful shutdown.

The gRPC handlers should not contain database-specific logic directly.

### 3.3 Shared Protocol Definition

Maintain the gRPC contract in a separate protocol module, such as `reporting-proto`.

This module will contain:

- Protocol Buffers definitions.
- Event and severity definitions.
- Batch request and response messages.
- Generated Go client and server types.

Both the SDK and reporting service should use the same versioned contract to avoid maintaining separate event definitions.

### 3.4 Storage Abstraction

The database has not yet been selected. Persistence should be implemented behind an interface so the database can be introduced or changed without modifying the SDK's public API or the gRPC contract.

Illustrative interface:

```go
type EventProcessor interface {
    Process(ctx context.Context, events []*Event) error
}
```

The final interface should match the processing requirements.

Initially, the service can validate events and use a simple processor for development and testing. Once the database is selected, a persistent implementation can be added behind the same abstraction.

Before production use, the team must decide whether processing and persistence happen synchronously or asynchronously and when the service acknowledges an event batch.

## 4. Event Structure

Each reporting event should contain the information needed to identify and investigate an issue.

Proposed fields:

| Field | Purpose |
|---|---|
| `event_id` | Unique identifier for the event |
| `service` | Name of the reporting application or service |
| `environment` | Environment, such as development, staging, or production |
| `severity` | Information, warning, or error |
| `message` | Human-readable event message |
| `error_type` | Error type, when applicable |
| `stack_trace` | Stack trace, when available |
| `timestamp` | Time when the event occurred |
| `attributes` | Additional diagnostic metadata |

The event structure should remain small enough for efficient batch transmission. Field limits and maximum event sizes should be defined before production deployment.

## 5. Delivery and Reliability

The SDK should use asynchronous delivery so reporting does not normally delay application work.

Initial configurable limits should include:

- Maximum in-memory queue size.
- Maximum number of events per batch.
- Maximum batch payload size.
- Batch flush interval.
- RPC deadline.
- Retry limit and exponential backoff with jitter.

Actual values should be selected through load testing and operational requirements rather than treated as fixed requirements in this proposal.

### Important delivery considerations

- **In-memory queue:** Events still queued when the application crashes may be lost.
- **Server acknowledgement:** If the service acknowledges events before durable persistence, a server crash can cause accepted events to be lost.
- **Retries:** Retrying a request can result in duplicate delivery. Stable event IDs and server-side deduplication should be considered.
- **Queue overflow:** The SDK needs a defined policy when its queue is full, such as dropping events and incrementing a dropped-event metric.
- **Graceful shutdown:** `Flush` and `Close` should use bounded timeouts so shutdown cannot hang indefinitely.

The system should explicitly document its delivery guarantee. Stronger durability mechanisms can be evaluated if required by the business.

## 6. Security and Data Handling

The following controls should be considered part of the production design:

- Use TLS for client-to-service communication.
- Authenticate reporting clients and restrict credentials to the intended service or project.
- Enforce request size limits, batch limits, rate limits, and input validation.
- Redact passwords, tokens, cookies, authorization headers, and other sensitive values before transmission or persistence where possible.
- Avoid collecting unnecessary personal data.
- Define log retention and access policies before storing production events.
- Do not allow client-supplied metadata to overwrite trusted server-side identity fields.
- Keep database credentials and other privileged secrets out of the SDK configuration exposed to application code.

## 7. Initial Scope and Exclusions

### Included

- Shared event and Protocol Buffers definitions.
- Reusable Go Reporting SDK.
- Central Go gRPC reporting service.
- Event validation and client authentication.
- Bounded client queue and batched asynchronous delivery.
- Storage-independent event processing.
- Health checks and basic operational metrics.
- Graceful shutdown.
- Tests for successful delivery, service unavailability, retries, queue overflow, invalid payloads, and shutdown behavior.

### Excluded

- Distributed tracing, trace IDs, and span IDs.
- Dashboard or searchable user interface.
- Alerting and notification workflows.
- Final database selection and advanced retention/search architecture.
- Durable client-side storage or message broker integration unless required by the initial reliability requirements.

## 8. Suggested Implementation Order

1. Agree on event fields, severity levels, authentication, and delivery semantics.
2. Define and version the Protocol Buffers contract.
3. Implement the central gRPC service with validation and a storage-independent processor.
4. Implement the Go SDK with bounded queueing, batching, retries, `Flush`, and `Close`.
5. Add integration tests and failure-path tests.
6. Run load tests and choose production queue, batch, and timeout defaults.
7. Select and implement the storage backend based on retention, search, throughput, and operational requirements.
8. Review the production security, monitoring, and operational requirements before rollout.

## 9. Decisions for Review 

The following decisions should be confirmed before production implementation:

- Which event fields and metadata are required?
- What is the expected event volume and maximum event size?
- Is best-effort delivery acceptable initially, or is durable delivery required?
- When should the service acknowledge a batch: after validation, after enqueueing, or after durable persistence?
- Which authentication mechanism should be used between Go services and the reporting service?
- Which database best meets retention, search, throughput, cost, and operational needs?
- What retention period and redaction rules are required?
- Which operational metrics and alerts are needed to detect dropped events, queue pressure, and reporting-service failures?

## 10. Recommendation

Proceed with the Go Reporting SDK and central gRPC reporting service first.

Keep event processing independent of the transport and database layers. Define the event contract and delivery guarantees before implementation, and select the storage backend based on actual retention, search, throughput, and operational requirements.

This approach provides a focused starting point for centralized application logging and error reporting while keeping the implementation maintainable and testable.
