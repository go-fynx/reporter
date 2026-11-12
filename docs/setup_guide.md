# Setup guide: run the reporter service and report from your Go service

This guide walks through two jobs:

1. [Install and run the reporter service](#part-1-install-and-run-the-reporter-service) on a server, using `go install`.
2. [Report errors and events from your Go service](#part-2-report-from-your-go-service) with the Go SDK, `reporter-go`.

Do part 1 first, or get the service's URL and an API key for your project from whoever runs it. The reference guides, [service.md](service.md) and [sdk.md](sdk.md), cover every setting and edge case.

## Before you start

You need Go 1.27 or newer; check with `go version`.

The repository, `github.com/go-fynx/reporter`, is public. `go install`, `go get` and `git clone` need no credentials or extra Go settings.

## Part 1: Install and run the reporter service

### What the server needs

- **Go 1.27 or newer**, but only to build the binary, which has no runtime dependencies.
- **MongoDB**: a replica set in production. See [service.md › MongoDB](service.md#mongodb) for the database users to create.
- **A TLS certificate and key** for the hostname clients will use, such as `reporter.internal`.
- **systemd**, to run the binary as a service.
- **An open port** (8443 by default) that the reporting services can reach.

To try the service before setting up MongoDB and TLS, see [Trying it without MongoDB or TLS](#trying-it-without-mongodb-or-tls).

### Step 1: Install the binary

**From a release tag.** One command, with nothing to clone:

```sh
VERSION=v0.1.0
CGO_ENABLED=0 go install -trimpath -ldflags "-s -w -X main.version=$VERSION" \
  github.com/go-fynx/reporter/cmd/reporter@$VERSION
```

> [!IMPORTANT]
> This works only for versions released as described in [For maintainers](#for-maintainers-making-go-install-version-work). For any other version, it fails with `The go.mod file for the module providing named packages contains one or more replace directives`. In that case, install from a clone.

**From a clone.** This works for any branch, tag or commit:

```sh
git clone https://github.com/go-fynx/reporter.git
cd reporter
git checkout main   # or a tag
CGO_ENABLED=0 go install -trimpath -ldflags "-s -w -X main.version=$(git describe --tags --always)" ./cmd/reporter
```

What the flags do:
- `CGO_ENABLED=0` builds a static binary.
- `-trimpath -s -w` keep local paths and debug data out of the binary.
- `-X main.version=...` sets what `reporter version` prints. Without it, it prints `dev`.

`go install` puts the binary in `$GOBIN`, or in `~/go/bin` if `GOBIN` isn't set. Copy it to the path the systemd unit uses:

```sh
BIN=$(go env GOBIN); BIN=${BIN:-$(go env GOPATH)/bin}
sudo install -m 0755 "$BIN/reporter" /usr/local/bin/reporter
reporter version
```

You also need two files from the repository's `deploy/` directory: the systemd unit and the example configuration. In a clone, they're in `./deploy`. After a `go install` from a release tag, they're in Go's module cache:

```sh
SRC=$(go env GOMODCACHE)/github.com/go-fynx/reporter@$VERSION   # or the clone's directory
ls "$SRC/deploy"   # config.dev.json  config.example.json  reporter.service
```

> [!TIP]
> If the server has no Go, run `make build` in a clone on another Linux machine and copy `bin/reporter` over. If the server's architecture differs, set it in the same command: `GOOS=linux GOARCH=arm64 make build`.

### Step 2: Create the service user and directories

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin reporter
sudo install -d -m 0750 -o root -g reporter /etc/reporter /etc/reporter/tls
sudo install -m 0640 -o root -g reporter server.crt server.key /etc/reporter/tls/
```

`server.crt` must contain the full certificate chain.

### Step 3: Create an API key for each project

```sh
reporter keygen -project booking
```

The command prints two things:
- **The API key.** It's shown only once. Store it in your secrets manager and give it to the services that report for this project; all of them share it. They use it in [Part 2](#part-2-report-from-your-go-service).
- **A config entry** with the key's SHA-256 hash, which goes into `api_keys` in the next step. The service never stores the key itself.

### Step 4: Write the configuration

Any field you leave out takes its default, so a production config can be this short:

```json
{
  "environment": "production",
  "server": {
    "listen_address": ":8443",
    "tls": {
      "cert_file": "/etc/reporter/tls/server.crt",
      "key_file": "/etc/reporter/tls/server.key"
    }
  },
  "api_keys": [
    {"id": "booking-20261112", "project": "booking", "sha256": "<sha256 from reporter keygen>"}
  ],
  "storage": {
    "mongodb": {
      "uri": "mongodb://reporter:<password>@mongo1:27017,mongo2:27017,mongo3:27017/?replicaSet=rs0&authSource=reporter"
    }
  }
}
```

To set limits, rate limits, retention or redaction, start from `$SRC/deploy/config.example.json` instead, which has every field. The [configuration table](service.md#configuration) explains each one.

The file contains the MongoDB password, so install it readable only by root and the service. Then validate it as the service user, which also confirms the service can read it:

```sh
sudo install -m 0640 -o root -g reporter config.json /etc/reporter/config.json
sudo -u reporter /usr/local/bin/reporter check-config -config /etc/reporter/config.json
# /etc/reporter/config.json is valid: environment production, listening on :8443 with TLS, storage mongodb, 1 API key(s)
```

### Step 5: Start the service

To watch it start, run it in the foreground first. Ctrl-C stops it.

```sh
sudo -u reporter /usr/local/bin/reporter serve -config /etc/reporter/config.json
```

Then run it under systemd, which starts it at boot and restarts it if it fails:

```sh
sudo cp "$SRC/deploy/reporter.service" /etc/systemd/system/reporter.service
sudo systemctl daemon-reload
sudo systemctl enable --now reporter
systemctl status reporter
journalctl -u reporter -f   # logs, as JSON lines
```

To listen on a port below 1024, such as 443, uncomment `AmbientCapabilities` in the unit file.

### Step 6: Check that it works

```sh
curl -s https://reporter.internal:8443/healthz   # "ok": the process is running
curl -s https://reporter.internal:8443/readyz    # "ok": MongoDB is reachable and events are accepted
```

Send a test event with the API key from step 3:

```sh
curl -sS https://reporter.internal:8443/reporter.v1.ReporterService/Notify \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $REPORTER_API_KEY" \
  -d '{"source": {"service": "smoke-test"}, "events": [{"eventId": "'"$(uuidgen)"'", "timestamp": "'"$(date -u +%FT%TZ)"'", "severity": "SEVERITY_INFO", "message": "hello"}]}'
# {"acceptedCount":1}
```

The event is now in MongoDB: `db.events.find({service: "smoke-test"})`. If the certificate comes from a private certificate authority, add `--cacert <ca file>` to the `curl` commands.

### Upgrades and changes

- **Upgrading:** repeat step 1 with the new version, then run `sudo systemctl restart reporter`. In-flight requests finish before the old process exits.
- **Config changes, new API keys and renewed certificates:** run `check-config`, then restart. The service reads its files only at startup.
- **Rotating or revoking a key:** see [service.md › API keys](service.md#api-keys).

### Trying it without MongoDB or TLS

For a quick trial on a laptop or test machine, use this config:
- `devlog` storage writes events to the service's log instead of storing them;
- `insecure` serves plain HTTP.

The service refuses both settings when `environment` is `production`.

```json
{
  "environment": "development",
  "server": {"listen_address": "127.0.0.1:8080", "insecure": true},
  "api_keys": [{"id": "local-dev", "project": "local", "sha256": "7e9f8fd111802be56c379d597842e29b2cebd35ff2133d431a49fa556a18704e"}],
  "storage": {"type": "devlog"}
}
```

```sh
reporter serve -config trial.json
```

This config accepts the API key `dev-key` for the project `local`. Each event the service receives appears in its log as a line containing `"msg":"event"`.

## Part 2: Report from your Go service

From the operator of the reporter service, you need its URL (such as `https://reporter.internal:8443`) and your project's API key.

### Step 1: Add the SDK

Add the module to your service:

```sh
go get github.com/go-fynx/reporter/reporter-go@latest
```

SDK releases are tagged `reporter-go/vX.Y.Z`. To pin one, use `@vX.Y.Z`. Until the first release is tagged, `@latest` takes the newest commit on `main`.

### Step 2: Call Configure first in main

```go
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	reporter "github.com/go-fynx/reporter/reporter-go"
)

var version = "dev" // set at build time with -ldflags "-X main.version=..."

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reporter.Configure(reporter.Configuration{
		Service:         "booking-api",
		AppVersion:      version,
		ProjectPackages: []string{"main*", "github.com/acme/booking-api/**"}, // your packages, highlighted in stack traces
		MainContext:     ctx,                                                // on SIGTERM, send the events still queued
	})

	// ... the rest of your program
}
```

Set the API key, URL and environment in each deployment's environment. Then the same binary runs everywhere, and the key stays out of your code:

```sh
REPORTER_API_KEY=<your project's key>
REPORTER_ENDPOINT=https://reporter.internal:8443
REPORTER_ENVIRONMENT=production   # or staging, development, ...
```

Every `Configuration` field has a matching `REPORTER_*` variable. A value set in code takes precedence. For the full list, see [sdk.md › Configuration](sdk.md#configuration).

**Why `Configure` must come first.** On Linux and macOS, `Configure` starts crash reporting by running your program a second time as a monitor process, so `ps` shows two processes. That second copy runs `main` until it reaches `Configure`, so any code before it runs twice. Only side-effect-free lines, like creating the signal context above, belong before it.

### Step 3: Report errors and events

```go
if err := chargeCard(ctx, booking); err != nil {
	reporter.Error(err, reporter.Fields{"booking_id": booking.ID, "amount": booking.Amount})
	return err
}

reporter.Info("booking created", reporter.Fields{"booking_id": booking.ID})
reporter.Warn("payment provider slow", reporter.Fields{"took": elapsed})
```

| Call | Severity | Stack trace |
|---|---|---|
| `reporter.Error(err, ...)` | error | yes |
| `reporter.Notify(err, ...)` | warning | yes |
| `reporter.Warn(msg, ...)` | warning | no |
| `reporter.Info(msg, ...)` | info | no |

You can pass these optional arguments in any order:
- **`reporter.Fields{...}`:** attributes you can search for later.
- **`reporter.SeverityError`, `SeverityWarning` or `SeverityInfo`:** overrides the severity.
- **`reporter.ErrorClass{Name: "PaymentError"}`:** overrides the error type. By default, it's the error's Go type, such as `*fs.PathError`.

**Sensitive data.** Before an event is sent, the SDK replaces the value of any field whose key contains `password`, `passwd`, `secret`, `token`, `authorization`, `cookie`, `api_key`, `apikey` or `credential` (ignoring case) with `[FILTERED]`. Messages and error texts aren't filtered, so keep secrets and personal data out of them.

**Wrapped errors.** For an error wrapped with `fmt.Errorf("...: %w", err)`, every error in the chain is sent as a cause.

**Shared fields.** To add the same fields to every event from one part of your code, create a notifier:

```go
billing := reporter.New(reporter.Fields{"component": "billing"})
billing.Error(err)
```

**Calls don't block.** Events are queued and sent in the background. A call returns an error only when nothing will be sent: the error is nil, the SDK isn't configured, or it's shutting down.

### Step 4: Catch panics

| Where the panic happens | What to do |
|---|---|
| Anywhere, crashing the program | Nothing. The crash monitor reports it as an unhandled error. Your program's output and exit code don't change. |
| In an HTTP handler | Wrap your handler: `http.ListenAndServe(addr, reporter.Handler(mux))`. net/http recovers panics in handlers itself, so the program doesn't crash and the monitor never sees them. |
| In a goroutine or job that should keep running | Put `defer reporter.Recover()` at its top. The panic is reported as a handled warning and stopped. |
| Somewhere you want reported, and still crashing | Put `defer reporter.AutoNotify()` at its top. |

`Recover` and `AutoNotify` only work when deferred directly, as shown.

If your router recovers panics itself, for example gin's `Recovery` middleware, `reporter.Handler` never sees them. In that case, call `reporter.Error` from the router's recovery hook.

### Step 5: Send queued events on shutdown

Events still in the queue when the program exits are lost. With `MainContext` set as in step 2, cancelling the context (by SIGTERM or Ctrl-C) stops new events and sends the queued ones in the background. Give that a moment before `main` returns, for example by finishing your server's graceful shutdown first.

Short-lived programs, such as command-line tools and cron jobs, should set `REPORTER_SYNCHRONOUS=1` or `Synchronous: true`. Each call then sends its event before it returns.

### Step 6: Check that it works

Run a local service as in [Trying it without MongoDB or TLS](#trying-it-without-mongodb-or-tls), then start your service against it:

```sh
REPORTER_API_KEY=dev-key REPORTER_ENDPOINT=http://127.0.0.1:8080 REPORTER_ENVIRONMENT=development go run .
```

Each event appears in the reporter service's log.

In production:
- **Counts:** `reporter.Stats()` returns how many events were sent, failed, dropped and rejected. Export these counts to your metrics to catch lost events.
- **SDK problems** are logged through the standard `log` package, with the prefix `reporter:`.

### If the service uses a private certificate authority

If the reporter service's certificate comes from an internal certificate authority (CA), sending fails with `x509: certificate signed by unknown authority`. There are two fixes:

- **Trust the CA on each machine.** On Debian or Ubuntu, copy the CA certificate to `/usr/local/share/ca-certificates/` and run `sudo update-ca-certificates`. No code change is needed.
- **Trust the CA in code.** gRPC needs HTTP/2, so enable it on the transport:

  ```go
  caPEM, err := os.ReadFile("/etc/ssl/internal-ca.crt")
  if err != nil {
  	log.Fatal(err)
  }
  roots, err := x509.SystemCertPool()
  if err != nil {
  	roots = x509.NewCertPool()
  }
  roots.AppendCertsFromPEM(caPEM)
  protocols := new(http.Protocols)
  protocols.SetHTTP2(true)

  reporter.Configure(reporter.Configuration{
  	Transport: &http.Transport{
  		Proxy:           http.ProxyFromEnvironment,
  		TLSClientConfig: &tls.Config{RootCAs: roots},
  		Protocols:       protocols,
  	},
  	// ... the other fields from step 2
  })
  ```

## Troubleshooting

| What you see | Cause |
|---|---|
| `go install ...@vX.Y.Z` fails with `contains one or more replace directives` | That version wasn't released for `go install`. Install from a clone. |
| `reporter: not configured, events will not be sent: missing APIKey` | `REPORTER_API_KEY` isn't set in your service's environment. |
| `reporter: failed to send event ...: unauthenticated: invalid API key` | The key's hash isn't in the service's `api_keys`, or the service wasn't restarted after the key was added. |
| `reporter: failed to send event ...: unavailable: ... certificate signed by unknown authority` | The certificate comes from a private CA. See [the section above](#if-the-service-uses-a-private-certificate-authority). |
| `reporter: failed to send event ...: unavailable: ... connection refused`, or a timeout | `REPORTER_ENDPOINT` is wrong, or a firewall blocks the service's port. |
| No events arrive, and nothing is logged | `EnabledEnvironments` (`REPORTER_ENABLED_ENVIRONMENTS`) doesn't include this environment. |
| The reporter service doesn't start | Run `journalctl -u reporter -e`, then see [service.md › Troubleshooting](service.md#troubleshooting). |

## For maintainers: making `go install …@version` work

`go install pkg@version` refuses any module whose `go.mod` has a `replace` directive. The service's `go.mod` currently replaces the SDK with `./reporter-go`. To release a version that installs with one command, make the service require a tagged SDK version, and use a Go workspace to keep local development on `./reporter-go`:

1. Tag and push the SDK. The tag starts with the module's directory:
   ```sh
   git tag reporter-go/v0.1.0
   git push origin reporter-go/v0.1.0
   ```
2. Make the service require that tag, and create a workspace for local development:
   ```sh
   go mod edit -dropreplace=github.com/go-fynx/reporter/reporter-go \
               -require=github.com/go-fynx/reporter/reporter-go@v0.1.0
   go mod tidy
   go work init . ./reporter-go
   make lint test
   ```
   Commit `go.mod`, `go.sum` and `go.work`. With `go.work`, builds and tests in a clone still compile `./reporter-go`, just as the `replace` made them do. `go install …@version` ignores `go.work`.
3. Tag and push the service:
   ```sh
   git tag v0.1.0
   git push origin v0.1.0
   ```
4. On a machine without a clone, check that `go install github.com/go-fynx/reporter/cmd/reporter@v0.1.0` works.

To release changes to the SDK later:
1. Release the SDK first, as in step 1, with a new version.
2. Update the service to that version:
   ```sh
   GOWORK=off go get github.com/go-fynx/reporter/reporter-go@vX.Y.Z
   GOWORK=off go mod tidy
   ```
3. Tag the service, as in step 3.
