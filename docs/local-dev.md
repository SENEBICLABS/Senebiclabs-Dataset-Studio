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
make env       # creates .env from .env.example (any make target also does this)
make up        # starts PostgreSQL and waits until it is healthy
make db-setup  # creates the database roles and the studio database, then migrates
make tools     # installs pinned golangci-lint v2.15.0, govulncheck v1.9.0 and sqlc v1.31.1 into .tools/bin
               # (sqlc compiles a C parser and needs about 2 GB of free disk)
make check     # every gate
```

`.env` is git-ignored and holds local-only values. Keep every password in it
URL-safe (letters, digits, `-`, `_`): the Makefile embeds them in connection
URLs. If your `.env` predates M1, copy the `STUDIO_DB_*_PASSWORD` lines from
`.env.example`; until then the Makefile uses the same local placeholders.

## Database identities

Plan section 6 defines the roles; `deploy/bootstrap/roles.sql` creates them and
`internal/dbroles` holds every privilege as code.

| Role | Login | Used by |
|---|---|---|
| `studio_bootstrap` (compose superuser) | yes | `studioctl db bootstrap` and the test harness's setup only |
| `studio_owner` | no | owns the `studio` schema and every object |
| `studio_migrator` | yes | `studioctl migrate`; acts as the owner only through `SET ROLE` |
| `studio_app` | yes | `studio-api` |
| `studio_worker` | yes | `studio-worker` |
| `studio_backup` | yes | backups (M7); reads everything, writes nothing |
| `studio_readonly` | yes | optional local debugging; no credential columns |

`make db-setup` runs two steps:

1. `studioctl db bootstrap` (as the superuser): creates or updates the roles,
   sets their passwords as SCRAM-SHA-256 verifiers (the plaintext never
   reaches the server), and creates the `studio` database owned by
   `studio_owner`, with `CONNECT` for the login roles only.
2. `studioctl migrate` (as `studio_migrator`): creates the schema, applies the
   embedded forward-only migrations, then re-applies the grants map. Every run
   resets privileges to exactly that map.

Both steps are idempotent. `bin/studioctl migrate status` prints the current
and latest versions.

No application process connects as the superuser. To give the API a database
check locally, set in `.env`:

```sh
STUDIO_DATABASE_URL=postgres://studio_app:<STUDIO_DB_APP_PASSWORD>@127.0.0.1:55432/studio?sslmode=disable
STUDIO_AUDIT_HMAC_KEY=<at least 32 random characters, local only>
```

`STUDIO_AUDIT_HMAC_KEY` is required whenever `STUDIO_DATABASE_URL` is set: it
keys the HMAC recorded for failed logins with unknown identifiers (plan 8.1),
so the raw identifier is never stored. It is never logged or printed.

**PG\* environment variables:** pgx, like libpq, falls back to `PGHOST`,
`PGUSER`, `PGPASSWORD`, `PGSSLMODE` and `~/.pgpass` for anything a connection
URL leaves out. Always give complete URLs (host, user, database, sslmode) and
make sure no stray `PG*` variables are set in the shell that runs the
binaries.

## Authentication (M3)

The first administrator exists only through the CLI (plan 14). The password is
read from stdin, never from flags or the environment:

```sh
STUDIO_DATABASE_URL=postgres://studio_app:...@127.0.0.1:55432/studio?sslmode=disable \
  bin/studioctl create-admin --email admin@example.test --name "Local Admin" < password.txt
```

| Endpoint | Behaviour |
|---|---|
| `POST /v1/auth/login` | `{"email", "password"}` → `{"token", "token_type": "Bearer", "expires_at", "principal_id"}`. Every credential failure returns the same 401 body. Rate limited per account (5/min) and per IP (20/min); over the limit returns 429 with `Retry-After`. |
| `POST /v1/auth/logout` | Revokes the calling session (204). API keys are revoked instead, not logged out. |
| `GET /v1/me` | The authenticated principal, including its `global_role`. |

Sessions last 12 hours and end earlier after `STUDIO_SESSION_IDLE_TIMEOUT`
without use. API keys have the form `dsk_<prefix>_<secret>`. Only SHA-256
hashes of tokens and keys, and argon2id hashes of passwords, are stored.
Unauthenticated requests get 401 with `WWW-Authenticate: Bearer` and are not
audited.

## Authorization and administration (M4)

Authorization is deny by default. The role-to-permission matrix of plan
section 14 lives in `internal/authz`. Services check it before reading
anything, so a refused request gets 403 even for an id that does not exist.
Every 403 is recorded as a `denied` audit event: events are aggregated per
actor, action and target for 60 seconds and flushed at shutdown.

Global roles are `platform_admin`, `registry_admin` and `member`. The role is
read on every request, so a change takes effect at once. Project roles
arrive with projects in M10.

All of these endpoints need `platform_admin` ("Manage users and service
accounts"):

| Endpoint | Behaviour |
|---|---|
| `GET /v1/users` | Cursor-paginated (`?limit=1..200&cursor=`). Responses never include password hashes. |
| `POST /v1/users` | `{"email", "display_name", "password", "global_role"?}`; the role defaults to `member`. Returns 409 if the email is taken. |
| `GET /v1/users/{id}` | One user. |
| `PATCH /v1/users/{id}` | Any of `display_name`, `global_role`, `password`. A password change revokes the user's open sessions. The email cannot be changed. |
| `POST /v1/users/{id}:disable` | Disables the user and revokes its sessions; its API keys stop working at once. |
| `GET /v1/service-accounts`, `POST /v1/service-accounts` | `{"name", "description"?, "global_role"?}`. Service accounts have no password and use API keys. |
| `POST /v1/principals/{id}/api-keys` | `{"expires_at": RFC 3339}` → the key, shown once. Works for users and service accounts, never system principals. |
| `GET /v1/principals/{id}/api-keys` | Key metadata only: no secret, no hash. |
| `DELETE /v1/api-keys/{id}` | Permanent revocation (204). |

## Running the binaries

```sh
make run-api      # studio-api on http://127.0.0.1:8480
make run-worker   # studio-worker (idle: no job kinds yet)
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
| `STUDIO_DATABASE_URL` | empty | Optional; the `studio_app` URL (see Database identities). |
| `STUDIO_SHUTDOWN_TIMEOUT` | `20s` | One deadline shared by every shutdown step. |
| `STUDIO_READINESS_TIMEOUT` | `2s` | Per check. |
| `STUDIO_SESSION_IDLE_TIMEOUT` | `30m` | Ends sessions unused for this long; they also expire 12 hours after login. |
| `STUDIO_AUDIT_HMAC_KEY` | empty | Required (≥ 32 characters) when `STUDIO_DATABASE_URL` is set. Secret. |
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
| `studioctl db bootstrap`, `studioctl migrate [up\|status]`, `studioctl create-admin` | 0 | Success. |
| | 1 | Missing or invalid settings, connection failure, or a failed step. |
| `studioctl version`, `studioctl help` | 0 | Success. `version` exits 1 only if writing its output fails. |
| `studioctl` (no command, unknown command, bad arguments) | 2 | Usage error; usage printed on stderr. |

## Tests

| Command | What it runs |
|---|---|
| `make test` | Unit tests with `-race`. No external services needed. |
| `make test-integration` | Builds `bin/`, then runs the `integration`-tagged tests. The harness (`internal/testdb`) bootstraps the roles, migrates one template database as `studio_migrator`, and clones a fresh database per test; tests log in as each role. This covers the M1 schema and grant tests in `test/integration` and the end-to-end tests that start the real binaries as `studio_app` and `studio_worker`. Missing environment variables **fail** these tests rather than skipping them. |
| `make check` | toolchain → tidy → gofmt → vet → lint → govulncheck → unit → build → integration/e2e |

## Stopping and resetting

```sh
make down                     # stop; data is kept
make db-destroy CONFIRM=yes   # stop and delete the local data volume
```
