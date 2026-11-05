# Local development

Dataset Studio runs locally only. There are no deployment targets.

## Prerequisites

| Tool | Version | Notes |
|---|---|---|
| Go | 1.27.x (go.mod pins `toolchain go1.27.3`) | The race detector needs a C compiler (`gcc`). |
| Docker + Compose v2 | Docker 29+, Compose 5+ | Runs PostgreSQL 18 on `127.0.0.1:55432`. |
| git, bash, make | any recent | |
| Free disk space | about 3 GB | Building the pinned lint tools needs roughly 1.5 GB of Go build cache. |

If `go` on your PATH is a wrapper that prints nothing (seen with some snap
installs), pass the real binary to every target:

```sh
make check GO=/snap/go/current/bin/go
```

`make toolchain-check` fails loudly in that case instead of letting later
gates "pass" without running.

## First run

```sh
make env      # creates .env from .env.example (any make target also does this)
make up       # starts PostgreSQL and waits until it is healthy
make tools    # installs pinned golangci-lint v2.15.0 and govulncheck v1.9.0 into .tools/bin
make check    # every M0 gate
```

`.env` is git-ignored and holds local-only values. Keep
`STUDIO_PG_BOOTSTRAP_PASSWORD` URL-safe: the Makefile embeds it in the
test-harness URL.

## Database identities in M0

The compose container has one superuser, `studio_bootstrap`. It is used only
by the test harness, through `STUDIO_TEST_DATABASE_URL`. The Makefile derives
that URL from `.env` and passes it only to the `make test-integration` test
command; it is not exported to any other recipe.

**Exception until M1:** the E2E tests in `test/e2e` start `studio-api` and
`studio-worker` with this superuser URL, because no less-privileged role exists
yet. M1 creates per-component roles and moves the E2E tests onto them. Outside
the tests, application processes never connect as the superuser.
`STUDIO_DATABASE_URL` stays empty until M1, so `/readyz` reports no database
check in local runs.

## Running the binaries

```sh
make run-api      # studio-api on http://127.0.0.1:8480
make run-worker   # studio-worker (idle in M0)
bin/studioctl version
bin/studioctl config check
```

| Endpoint | Meaning |
|---|---|
| `GET /healthz` | Liveness: 200 whenever the process is serving HTTP. |
| `GET /readyz` | Readiness: 200 only if every dependency check passes and the process is not draining. Otherwise 503. Error details are logged, never returned. |
| anything else | 404 as `application/problem+json` (RFC 9457). |

Every response carries `X-Request-ID`. A well-formed inbound value is kept;
anything else is replaced.

On SIGINT or SIGTERM, a process shuts down in this order, all within
`STUDIO_SHUTDOWN_TIMEOUT`:
1. readiness switches to `draining`;
2. HTTP stops accepting new connections and drains in-flight requests;
3. shutdown hooks run in reverse registration order.

It exits 0 on a clean stop.

## Configuration

| Variable | Default | |
|---|---|---|
| `STUDIO_LOG_LEVEL` | `info` | debug, info, warn, error |
| `STUDIO_LOG_FORMAT` | `json` | json, text |
| `STUDIO_HTTP_ADDR` | `127.0.0.1:8480` | Listens on loopback only by default. |
| `STUDIO_DATABASE_URL` | empty | Optional in M0. |
| `STUDIO_SHUTDOWN_TIMEOUT` | `20s` | One deadline shared by every shutdown step. |
| `STUDIO_READINESS_TIMEOUT` | `2s` | Per check. |
| `STUDIO_HTTP_{READ_HEADER,READ,WRITE,IDLE}_TIMEOUT` | 5s / 30s / 30s / 120s | |

Invalid configuration lists every problem at once. Passwords are never
printed, whether they appear in the URL's userinfo or as `password` or
`sslpassword` query parameters.

## Exit codes

| Process | Code | Meaning |
|---|---|---|
| `studio-api`, `studio-worker` | 0 | Clean shutdown after SIGINT or SIGTERM. |
| | 1 | Runtime failure: database pool could not be created, listen failed, the HTTP server failed, or shutdown finished with errors (for example the drain deadline expired). |
| | 2 | Invalid configuration (`STUDIO_*` variables). |
| `studioctl config check` | 0 | Configuration valid; effective settings printed with secrets redacted. |
| | 1 | Configuration invalid; every problem listed on stderr. |
| `studioctl version`, `studioctl help` | 0 | Success. `version` exits 1 only if writing its output fails. |
| `studioctl` (no command, unknown command, bad arguments) | 2 | Usage error; usage printed on stderr. |

## Tests

| Command | What it runs |
|---|---|
| `make test` | Unit tests with `-race`. No external services needed. |
| `make test-integration` | Builds `bin/`, then runs the `integration`-tagged tests: database ping against the compose PostgreSQL, plus end-to-end tests that start the real binaries, probe the endpoints and send SIGTERM. Missing environment variables **fail** these tests rather than skipping them. |
| `make check` | toolchain → tidy → gofmt → vet → lint → govulncheck → unit → build → integration/e2e |

## Stopping and resetting

```sh
make down                     # stop; data is kept
make db-destroy CONFIRM=yes   # stop and delete the local data volume
```
