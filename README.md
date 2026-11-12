# reporter

An internal replacement for Bugsnag: applications send log and error events to a central service, which stores them in MongoDB.

| Part | Path | Status |
|---|---|---|
| Reporter service | repo root (`cmd/reporter`, `internal/`) | Ready to deploy |
| Contract (`.proto` and generated code) | [reporter-go/proto](reporter-go/proto/reporter/v1/reporter.proto) | Done |
| Go SDK `reporter-go` | [reporter-go](reporter-go) | Ready to use |

- **Reporting from a Go service:** [docs/sdk.md](docs/sdk.md)
- **Using the service from another language:** [docs/api.md](docs/api.md)
- **Deploying and running the service:** [docs/service.md](docs/service.md)
- **Design and decisions:** [docs/implementation_plan.md](docs/implementation_plan.md)

## Quick start (local development)

You need Go 1.27, `make` and a local MongoDB.

```sh
make tools   # once
make run     # serves on 127.0.0.1:8080 with deploy/config.dev.json
```

Send an event with the development key `dev-key`:

```sh
curl -sS http://127.0.0.1:8080/reporter.v1.ReporterService/Notify \
  -H 'Content-Type: application/json' -H 'Authorization: Bearer dev-key' \
  -d '{"source": {"service": "demo"}, "events": [{"eventId": "'"$(uuidgen)"'", "timestamp": "'"$(date -u +%FT%TZ)"'", "severity": "SEVERITY_INFO", "message": "hello"}]}'
```

Then look at it with `mongosh reporter_dev --eval 'db.events.find()'`.

## Layout

```
cmd/reporter/                  service binary: serve, check-config, keygen, version
internal/app/                  HTTP server, TLS, health checks, interceptors, graceful shutdown
internal/config/               JSON configuration: defaults and validation
internal/auth/                 API keys and per-project rate limits
internal/ingest/               the Notify RPC: conversion, validation, per-event rejections
internal/event/                internal event type, validation rules, redaction
internal/storage/mongostore/   MongoDB storage: documents, indexes, retention
internal/processor/            in-memory (tests) and log-only (development) storage, shared tests
reporter-go/                   Go SDK (package reporter) and the contract under proto/
deploy/                        example configs and a systemd unit
```
