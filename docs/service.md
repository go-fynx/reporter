# Reporter service: operations guide

How to build, configure, deploy and run the reporter service. For the API that clients call, see [api.md](api.md).

## What it does

The service receives log and error events over one port. It accepts gRPC from the reporter-go SDK, and plain HTTP POSTs with JSON from services in other languages. For each request it:
1. checks the API key;
2. applies the project's rate limit;
3. validates each event, rejecting invalid ones individually;
4. removes sensitive attribute values;
5. stores the events in MongoDB.

A request succeeds only after MongoDB has written its events. The service keeps no state of its own, so you can run several instances behind a load balancer.

## Build

You need Go 1.27 and `make`.

```sh
make tools   # once: installs buf, protoc-gen-go, protoc-gen-connect-go and staticcheck into ./bin
make build   # produces bin/reporter, a static binary with no runtime dependencies
```

Without `make`:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(git describe --tags --always)" -o bin/reporter ./cmd/reporter
```

## Commands

| Command | Purpose |
|---|---|
| `reporter serve -config <file>` | Start the service |
| `reporter check-config -config <file>` | Validate a config file and print a summary; exits 1 on errors |
| `reporter keygen -project <name> [-id <name>]` | Create an API key and print the entry for the config file |
| `reporter version` | Print the build version |

## Configuration

All configuration is in one JSON file, passed with `-config`.
- **Defaults:** any field you leave out takes the default below.
- **Unknown fields are an error,** so typos are caught at startup.
- **Durations** are strings such as `"5s"`, `"2m"` or `"24h"`.
- **Examples:**
  - [deploy/config.example.json](../deploy/config.example.json) is a production example with every field.
  - [deploy/config.dev.json](../deploy/config.dev.json) is for local development.

The file can contain the MongoDB password. Make it readable only by the service, for example mode `0640` owned by `root:reporter`. The service logs a warning at startup if every user on the machine can read it.

| Field | Default | Meaning |
|---|---|---|
| `environment` | `production` | Environment of the reporter service itself. `production` refuses `server.insecure` and the `devlog` storage. |
| `server.listen_address` | `:8443` | Address and port to listen on |
| `server.tls.cert_file`, `server.tls.key_file` | — | PEM certificate (with its chain) and private key. Required unless `server.insecure` is true. |
| `server.insecure` | `false` | Serve plaintext HTTP/1.1 and HTTP/2 (h2c). Local development only. |
| `server.read_header_timeout` | `5s` | Time allowed to receive request headers |
| `server.read_timeout` | `30s` | Time allowed to read a whole request |
| `server.write_timeout` | `30s` | Time allowed to write a response |
| `server.idle_timeout` | `2m` | How long idle keep-alive connections stay open |
| `server.shutdown_timeout` | `30s` | How long in-flight requests get to finish on shutdown |
| `limits.max_request_bytes` | `1048576` | Largest request accepted (1 MiB, like Bugsnag). Larger ones get 429. |
| `limits.max_events_per_request` | `100` | |
| `limits.max_in_flight_requests` | `256` | Concurrent requests; more are refused immediately with 503 |
| `limits.max_attributes` | `100` | Per event |
| `limits.max_attribute_key_length` | `128` | Bytes |
| `limits.max_causes` | `32` | Per event |
| `limits.max_stack_frames` | `256` | Per stack |
| `limits.max_clock_skew` | `24h` | How far in the future an event's timestamp may be |
| `limits.processing_timeout` | `10s` | Longest a request may wait for MongoDB |
| `rate_limit.events_per_second` | `100` | Per project, on each service instance. `0` turns rate limiting off. |
| `rate_limit.burst` | `1000` | Events a project may send at once. Must be at least `max_events_per_request`. |
| `redaction.filters` | password, passwd, secret, token, authorization, cookie, api_key, apikey, credential | An attribute whose key contains any of these (ignoring case) has its value replaced with `[FILTERED]` before it is stored |
| `api_keys[]` | — (at least one) | `{id, project, sha256}`; see [API keys](#api-keys) |
| `storage.type` | `mongodb` | `mongodb`, or `devlog` to only log events (development) |
| `storage.mongodb.uri` | — | MongoDB connection string |
| `storage.mongodb.database` | `reporter` | |
| `storage.mongodb.collection` | `events` | |
| `storage.mongodb.write_concern` | `majority` | `majority` or a number of nodes. Writes always wait for the journal. |
| `storage.mongodb.connect_timeout` | `10s` | Also bounds how long startup waits for MongoDB |
| `storage.mongodb.max_pool_size` | `100` | Connections per service instance |
| `storage.retention_days.error` / `.warning` / `.info` | `90` / `90` / `14` | How long events are kept |
| `log.level` | `info` | `debug`, `info`, `warn` or `error` |

Validate a file before deploying it:

```sh
reporter check-config -config /etc/reporter/config.json
```

The service reads its configuration only at startup. Restart it to apply changes.

## API keys

Each project gets its own key. The service stores only the key's SHA-256 hash.

```sh
reporter keygen -project booking
```

1. Give the printed key to the services that report for the project. For the Go SDK, that's its `APIKey` setting or `REPORTER_API_KEY`; see [sdk.md](sdk.md).
2. Add the printed entry to `api_keys`.
3. Restart the service.

The key is shown only once. If it's lost, create a new one.

- **Rotate** a key:
  1. Add a new entry for the same project.
  2. Restart the service.
  3. Switch the clients to the new key.
  4. Remove the old entry and restart again.
  Both keys work in between.
- **Revoke** a key: remove its entry and restart.

## TLS

Point `server.tls` at a PEM certificate and private key. The certificate must be valid for the hostname clients use, for example `reporter.internal`.
- **Protocols:** TLS 1.2 and newer, with HTTP/2 negotiated automatically, which gRPC needs.
- **Renewing:** replace the files and restart.
- **Service user:** needs read access to both files.

## MongoDB

### Production

Use a replica set. The default `majority` write concern means a successful response survives the loss of one MongoDB node. Give the service its own user, plus a read-only user for engineers who query events:

```js
use reporter
db.createUser({user: "reporter", pwd: passwordPrompt(), roles: [{role: "readWrite", db: "reporter"}]})
db.createUser({user: "engineer", pwd: passwordPrompt(), roles: [{role: "read", db: "reporter"}]})
```

Connection string, with the user created in the `reporter` database:

```
mongodb://reporter:<password>@mongo1:27017,mongo2:27017,mongo3:27017/?replicaSet=rs0&authSource=reporter
```

### Local development

The service works with a standalone `mongod`. To behave like production, run it as a single-node replica set:

1. Add this to `/etc/mongod.conf`:
   ```yaml
   replication:
     replSetName: rs0
   ```
2. Restart the server and initialise the replica set once:
   ```sh
   sudo systemctl restart mongod
   mongosh --eval 'rs.initiate({_id: "rs0", members: [{_id: 0, host: "127.0.0.1:27017"}]})'
   ```

Then use `mongodb://127.0.0.1:27017/?replicaSet=rs0`.

### What the service creates

At startup it creates these indexes on the events collection. Creating them again on later startups is harmless.

| Index | Purpose |
|---|---|
| `project_event_id` (unique) | Stores each event once; a resent event is ignored |
| `project_service_environment_severity_timestamp` | Listing a service's recent events |
| `attributes_k_v` | Finding events by attribute |
| `expire_at_ttl` | Deletes events when their retention ends |

**Documents.** Each event is one document:
- **Identity and source:** `event_id`, `project`, `service`, `environment`, `app_version`, `hostname`, `os`, `runtime_version`, `sdk_name`, `sdk_version`.
- **Times:** `timestamp` (client clock), `received_at` (server clock), `expire_at`.
- **Error details:** `severity` (`info`, `warning`, `error`), `unhandled`, `message`, `error_type`, `stack`, `causes`.
- **Attributes:** stored as a list of `{k, v}` pairs.

**Retention.** `expire_at` is `received_at` plus the retention for the event's severity.
- MongoDB deletes expired documents in a background pass that runs about every 60 seconds.
- Changing `retention_days` only affects events stored afterwards.

### Reading events

There is no UI yet. Query with `mongosh` or Compass using a read-only user:

```js
use reporter

// Latest errors for one service
db.events.find({project: "booking", service: "booking-api", environment: "production", severity: "error"})
  .sort({timestamp: -1}).limit(20)

// Errors per type in the last 24 hours
db.events.aggregate([
  {$match: {project: "booking", severity: "error", timestamp: {$gte: new Date(Date.now() - 24 * 3600 * 1000)}}},
  {$group: {_id: "$error_type", count: {$sum: 1}}},
  {$sort: {count: -1}}
])

// Events with a given attribute
db.events.find({project: "booking", attributes: {$elemMatch: {k: "invoice_id", v: "inv_123"}}})
```

## Running with systemd

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin reporter
sudo install -m 0755 bin/reporter /usr/local/bin/reporter
sudo install -d -m 0750 -o root -g reporter /etc/reporter /etc/reporter/tls
sudo install -m 0640 -o root -g reporter config.json /etc/reporter/config.json
sudo install -m 0640 -o root -g reporter server.crt server.key /etc/reporter/tls/
reporter check-config -config /etc/reporter/config.json
sudo cp deploy/reporter.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now reporter
```

The unit runs the service as the `reporter` user with a read-only view of the system. It also gives the service 45 seconds to stop, longer than the default `shutdown_timeout` of 30 seconds.

Logs are JSON lines on standard output, collected by the journal:

```sh
journalctl -u reporter -f
```

## Health checks and monitoring

| Path | Meaning |
|---|---|
| `GET /healthz` | 200 while the process runs. Use for liveness. |
| `GET /readyz` | 200 when the service is accepting events and MongoDB is reachable; 503 otherwise, including during shutdown. Use for load balancer checks. |

There are no metrics yet; Prometheus is planned. Until then, watch the logs:

| Log message | Level | Meaning |
|---|---|---|
| `reporter service started` | info | Startup finished; includes address, TLS, environment and storage |
| `call failed` | warn | A request was refused; `code` says why (`unauthenticated`, `invalid_argument`, `resource_exhausted`, `unavailable`) |
| `events rejected` | warn | Some events in a request were invalid; includes project, service, counts and the first reason |
| `rate limit exceeded` | warn | A project went over `rate_limit` |
| `storing events failed` | error | MongoDB refused or timed out; clients got 503 |
| `readiness check failed` | warn | `/readyz` could not reach MongoDB |
| `panic while handling a call` | error | A bug; includes a stack trace. The service keeps running. |

Set `log.level` to `debug` to log every accepted request.

## Shutdown

On SIGTERM or SIGINT the service:
1. switches `/readyz` to 503;
2. stops accepting new connections;
3. waits up to `server.shutdown_timeout` for in-flight requests to finish;
4. closes the MongoDB connection.

It exits 0 after a clean shutdown, or 1 if requests had to be cut off.

## Behaviour to be aware of

- **Rate limits are per instance.** With two instances, a project can send up to twice `events_per_second` in total.
- **Over-limit requests are refused, not queued.** When `max_in_flight_requests` is reached, new requests get 503 straight away, which bounds memory use.
- **No retries in the v1 SDK.** It doesn't retry, so events that fail during a restart or a MongoDB outage are dropped on the client side, as with Bugsnag. Other clients may resend; duplicates are ignored.
- **Clock skew.** Client clocks are trusted for `timestamp`, up to `max_clock_skew` into the future. `received_at` always comes from the server.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `invalid JSON: json: unknown field "..."` at startup | A typo in the config file |
| `set server.tls, or server.insecure for local development` | Neither TLS nor insecure mode is configured |
| `mongodb is not reachable` at startup | Wrong URI, credentials or network; the service waits `connect_timeout` before giving up |
| `load TLS certificate` at startup | Wrong path, or the service user cannot read the files |
| Clients get 401 | The key is not in `api_keys`, the entry's hash doesn't match, or the service wasn't restarted after the change |
| Clients get 429 | The project is over its rate limit (see the `rate limit exceeded` log), or requests exceed 1 MiB |
| Clients get 503 `server is busy` | `max_in_flight_requests` reached; MongoDB is probably slow |
| Clients get 503 `events could not be stored` | MongoDB is failing; check `/readyz` and the `storing events failed` log |

## Development

```sh
make run    # builds and serves deploy/config.dev.json on 127.0.0.1:8080, plaintext, local MongoDB
```

The development config accepts the API key `dev-key` for project `local`:

```sh
curl -sS http://127.0.0.1:8080/reporter.v1.ReporterService/Notify \
  -H 'Content-Type: application/json' -H 'Authorization: Bearer dev-key' \
  -d '{"source": {"service": "demo"}, "events": [{"eventId": "'"$(uuidgen)"'", "timestamp": "'"$(date -u +%FT%TZ)"'", "severity": "SEVERITY_INFO", "message": "hello"}]}'
```

**Tests:**
- `make test` runs the unit tests.
- `make test-integration` runs everything, including the MongoDB tests. It needs `REPORTER_TEST_MONGO_URI`, for example `mongodb://127.0.0.1:27017`. Each test uses a throwaway database and drops it afterwards.
- The race detector needs a C compiler. Without one, run `make test TESTFLAGS=`.
- After changing `reporter.proto`, run `make generate` and commit the generated code.
