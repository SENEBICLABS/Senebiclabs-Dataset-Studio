# Dataset Studio: Phase 1 Implementation Plan (Revision 2.2)

This is the canonical Phase 1 plan. It consolidates the approved planning
record into one document:

| Revision | Content | Approval status in the record |
|---|---|---|
| 1 | Initial Phase 1 architecture, entities, API, security, milestones | Broadly approved, with revision requests |
| 2 | Records and releases promoted to required scope; tenancy assumption; slot-based schema extensions; backup consistency; database identities; audit semantics; dependency pins | Approved in principle |
| 2.1 | Eight targeted corrections plus a River v0.49.0 transaction-behaviour inspection | Approved in principle |
| 2.2 | Five narrow clarifications (root principal, actor kind, job-execution audit constraints, denied-event aggregation, River fence tests) | Requested by the project owner and incorporated before M0 was authorised |
| 2.2 amendment (X2) | Test definitions P3a–P3d, AG1–AG4 and JA1–JA9 confirmed (section 17.4.1). No constraint changes. | Confirmed by Godwin, 2026-11-05 |

Where a later revision changed an earlier one, this document states the final
position and notes what it superseded. Editorial changes made while
consolidating, such as renaming colliding test identifiers, are listed in
section 22. Nothing here adds requirements beyond the record. Anything the
record does not settle is marked **Unresolved** (section 20).

## 0. Status labels

| Label | Meaning |
|---|---|
| **Approved** | Explicitly decided in the planning record. |
| **Approved default** | Proposed with a default. The record states that approving the plan approves the default unless flagged, and no objection was recorded. |
| **Assumption** | Stated as an assumption. It holds until changed. |
| **Unresolved** | Not settled by the record. It must be decided before the affected milestone. |
| **Verified** | A fact checked during planning (tool versions, library source, or a schema experiment). |

---

## 1. Purpose and confirmed decisions

Dataset Studio is a general-purpose platform for creating and managing datasets.

**Confirmed architecture decisions (Approved):**

1. It is a general-purpose dataset creation and management platform.
2. It is not designed around oncology or any other medical specialty.
3. It is not designed around one predefined dataset type.
4. It is a separate Go repository.
5. It owns an independent PostgreSQL database, completely separate from
   HEALTH's Supabase database.
6. HEALTH remains responsible for clinical review, consensus and adjudication.
7. Workforce remains responsible for clinician operations.
8. Integration with HEALTH happens through HTTP APIs, never direct database
   access, and in a future phase only.
9. Phase 1 is local development and verification only. Deployment is out of
   scope.

**Required architectural properties (Approved):**

- multiple dataset kinds and versioned record schemas;
- specialty-neutral and multi-specialty projects;
- versioned vocabularies and extensions;
- different source types and evidence structures;
- configurable validation rules;
- versioned review profiles;
- reusable ingestion, extraction, generation and packaging infrastructure;
- immutable record revisions and dataset versions;
- rights management and provenance;
- room to expand without a foundational redesign.

**Further constraints (Approved):**

- No primary dataset kind is selected.
- Phase 1 does not require an initial real dataset.
- At least two structurally different synthetic record schemas are used in
  tests to prove extensibility. They are test fixtures, not product
  commitments.
- No Claude attribution, co-author trailers or generated-by attribution
  appears in commits or engineering documentation.

---

## 2. Architecture

### 2.1 Shape (Approved)

```
               ┌────────────── studio-api (Go, net/http) ───────────────┐
 HTTP/JSON ──► │ middleware: request-id → authn → body limits → recover  │
               │ handlers → services (authz + tx + audit) → stores (sqlc) │
               └───────┬───────────────────────┬────────────────────────┘
                       │                       │
              PostgreSQL (own instance)   blob.Store (fs | s3*)
                       ▲                       ▲
               ┌───────┴── studio-worker ──────┴──┐   * optional
               │ job queue in Postgres (River)     │
               └───────────────────────────────────┘
 studioctl (CLI): migrate · create-admin · backup · restore · verify
```

- **Modular monolith.** One Go module with three binaries: `studio-api`,
  `studio-worker` and `studioctl`. Domain packages live under `internal/`.
- **Layering.**
  - Handlers only decode requests and encode responses.
  - Services own the authorization check, the transaction and the audit write.
  - Stores are generated SQL (sqlc).
- **One transaction per mutation.** Every mutation and its audit event commit
  together. Jobs are enqueued inside that same transaction.
- **Registry content is data, not code.** Dataset kinds, record schemas and the
  other registry kinds are versioned JSON documents, each checked by a Go
  *kind handler*. A new kind is one handler plus one meta-schema, with no table
  changes.
- **No specialty or domain appears as a column, enum or code path.** A specialty
  is a concept in a versioned vocabulary, attached to projects as zero or more
  tags. Multi-specialty means several tags.
- **Assumption:** Phase 1 is API plus CLI only, with no web UI.
- **Assumption:** Phase 1 uses synthetic data only, with no PHI, PII or
  licensed third-party content.

### 2.2 Tenancy (Assumption T1, Approved)

> Phase 1 is a single-organisation, internal installation.
> - There is no organisation or workspace entity and no tenant id column.
> - Projects are the only access-control boundary.
> - All principals belong to the one organisation.
> - External parties never log in. They receive released datasets in later
>   phases, through mechanisms not designed here.

No multi-tenant infrastructure is built. The future extension points are:

| Extension point | How it stays open |
|---|---|
| Top-level ownership | Every project-scoped row can be reached through `projects`. A future `workspace_id` would go on `projects`, non-global `registry_entries`, `sources` and `principals` only. |
| Registry scope | `registry_entries.project_id` is null for global entries. Workspace scope would be one more nullable column plus a resolution rule: project, then workspace, then global. |
| Authorization | `authz.Principal` carries a `Scopes` slice, which holds a single implicit scope today. Policies take a scope argument from the start. |
| API | Resource ids are globally unique UUIDv7 values, so paths need no tenant prefix. A workspace could later arrive as a header or path prefix without changing ids. |
| Manifests | Every manifest records `installation_id`, a random UUID created at bootstrap. |
| Blob dedupe | Content-addressed keys would create a cross-tenant "does this file exist" side channel. Future work: per-workspace key salting or dedupe within a tenant only. Documented, not built. |

---

## 3. Technology and dependency decisions

### 3.1 Dependency table (Approved; pins Verified on 2026-11-05)

**Pinning rule:** the newest release that is at least about two weeks old at
pin time.

| Component | Decision | Pin | Notes |
|---|---|---|---|
| Go toolchain | `go 1.27.0`, `toolchain go1.27.3` | 1.27.3 | River and goose need Go 1.26 or later. |
| PostgreSQL | `postgres:18.6-trixie`, pinned by digest | 18.6, `sha256:f4b419e350fd98d4a8429603295208976dea4c8fb8326d818d346d3e29f1db6f` | 19 was only at rc1 when pinned. Version 18 provides `uuidv7()`, `pg_read_all_data` and MAINTAIN (17 and later). |
| Driver | `github.com/jackc/pgx/v5` | v5.11.0 | River 0.49 and goose 3.29 also require v5.11.0. |
| Indirect, security pin | `golang.org/x/text` | v0.41.0 | Fixes GO-2026-6629 and GO-2026-5970, reached through pgx. Pinned in M0. |
| Indirect, required by the line above | `golang.org/x/sync` | v0.22.0 | Raised automatically by x/text v0.41.0's go.mod. |
| Migrations | `github.com/pressly/goose/v3`, SQL files embedded in the binary, forward-only | v3.29.0 | |
| Job queue | River `github.com/riverqueue/river` and `riverdriver/riverpgxv5` | v0.49.0 | MPL-2.0, pre-1.0. Wrapped behind `internal/jobs.Queue`. River's SQL is carried in our own goose migrations. |
| Query generation | `github.com/sqlc-dev/sqlc`, through a `tool` directive in a separate `tools/go.mod` | v1.31.1 | Generated code is committed. |
| JSON Schema | `github.com/santhosh-tekuri/jsonschema/v6`, draft 2020-12, remote `$ref` disabled | v6.0.3 | |
| Rules | `cel.dev/cel-go` | v0.32.0 | The module path moved from `github.com/google/cel-go`. |
| Canonical JSON | `github.com/gowebpki/jcs` (RFC 8785) | v1.0.2 | |
| Crypto | `golang.org/x/crypto` (argon2id) | v0.58.0 | |
| Rate limiting | `golang.org/x/time/rate` | v0.16.0 | |
| UUIDs | `github.com/google/uuid` (`NewV7`) | v1.6.0 | The app generates ids. The database `uuidv7()` is a fallback default. |
| Lint | golangci-lint, pinned binary in a versioned tool path | v2.15.0 | |
| Vulnerabilities | govulncheck | v1.9.0 | |
| S3 (optional O3) | `aws-sdk-go-v2/service/s3` | Pinned when O3 starts | The local S3 emulator choice is **Unresolved** (section 20). |
| Standard library | `net/http` pattern routing, `log/slog`, `archive/tar`, `crypto/sha256` | n/a | |
| Tests | Standard library, plus a template-database clone harness against the compose PostgreSQL | n/a | No testcontainers. |

### 3.2 River versus an in-house queue (Approved: River v0.49.0)

| Criterion | River | In-house (`SKIP LOCKED`) |
|---|---|---|
| Transactional enqueue with pgx | yes | yes |
| Job completion, domain change and audit in one transaction | yes (`JobCompleteTx`) | yes |
| Retries, backoff, unique jobs, periodic jobs, rescuer, discarded state | built in and tested | about 800 to 1,200 lines plus tests |
| Schema and grant control | through pinned SQL in our migrations | full |
| Risks | pre-1.0 churn; MPL-2.0 licence review | our own concurrency bugs |

The exit route is `internal/jobs.Queue` plus its conformance suite, which makes
a replacement roughly a two to three day job. The MPL-2.0 licence sign-off is
an approved default (U1).

### 3.3 River v0.49.0 behaviour (Verified from source in Rev 2.1)

| # | Finding | Consequence |
|---|---|---|
| R1 | `JobCompleteTx` marks a job completed inside the caller's transaction and rolls back with it. | Atomic job completion, domain change and audit is supported. |
| R2 | The completion update matches only `state = 'running'`, with no attempt check. | A stale worker could complete another attempt's job, so we need our own fence (section 12). |
| R3 | When the job is not running, the completion query returns the unchanged row, and `JobCompleteTx` returns **no error**. | The caller must check that the returned `State == completed`. |
| R4 | v0.49.0 removed internal savepoints. Callers must roll back on any River error. | `WithTx` aborts the whole transaction on any River error. |
| R5 | `InsertTx` jobs are invisible until commit. The insert uses `INSERT … ON CONFLICT (unique_key) DO UPDATE SET kind = … RETURNING`, `nextval(river_job_id_seq)`, `river_job_state_in_bitmask()` and `pg_notify`. | The API role needs SELECT, INSERT and UPDATE(kind) on `river_job`, USAGE on the sequence, and EXECUTE on the function. |
| R6 | Defaults are `JobTimeout` 1 minute and `RescueStuckJobsAfter` 1 hour. A rescued job becomes `retryable`. | The fence is required, and long work is split into chunks. |
| R7 | The cleaner deletes finished jobs. The reindexer runs `REINDEX`. | The worker needs DELETE and MAINTAIN on `river_job`. Set `CompletedJobRetentionPeriod` to 30 days. Audit stores `job_id` as a plain value with no foreign key. |
| R8 | River's PostgreSQL tables are `river_migration`, `river_job`, `river_leader` (unlogged), `river_queue` and `river_notification`, plus the type `river_job_state`. | This is the exact set the grants must cover. |
| R9 | v0.48.0 fixed the rescuer overwriting jobs that completed concurrently. | Included in the v0.49.0 pin. |

Also verified in the Rev 2.2 clarifications:

- The rescuer's `JobRescueMany` is a plain
  `UPDATE … WHERE state = 'running' AND attempted_at < horizon`. It waits on a
  row lock held by our fence and then matches nothing.
- Fetching (`JobGetAvailable`) uses `FOR UPDATE SKIP LOCKED`, but only on
  `available` jobs.

---

## 4. Repository structure (Approved; M0 parts as built)

```
cmd/            studio-api/  studio-worker/  studioctl/
internal/
  app/                    lifecycle and ordered shutdown            (M0, built)
  config/                 environment configuration and redaction   (M0, built)
  version/                build identity                            (M0, built)
  platform/
    db/                   pool and readiness ping; WithTx in M1     (M0, built)
    httpx/                problem+json, request id, recover, access log (M0, built)
    health/               /healthz and /readyz                      (M0, built)
    logging/              slog setup                                (M0, built)
  principals/  auth/  authz/  audit/
  registry/  (canonical/, jsonschema/, resolve/, kinds/{datasetkind,recordschema,
              vocabulary,extension,ruleset,reviewprofile,sourcetype})
  projects/  datasets/  records/  releases/
  sources/  rights/  provenance/
  blob/  (ledger/, fsstore/, s3store/ optional, conformance/)
  jobs/  backup/  dbroles/
migrations/                goose SQL, embedded
db/queries/*.sql  sqlc.yaml
tools/go.mod               sqlc pinned through a tool directive
api/openapi.yaml           hand-maintained and contract-tested
testdata/fixtures/         registry fixtures, records, expected manifests
test/integration/  test/e2e/
deploy/compose.yaml        local PostgreSQL only
deploy/bootstrap/          superuser-only SQL: database and roles (M1)
docs/                      local-dev.md, this plan, ADRs
Makefile  .golangci.yml  go.mod  .env.example
```

Revision 1 also listed `platform/ids` and `platform/clock`. These remain
planned helpers and are not yet built.

---

## 5. Database model (final, after Rev 2.1 and 2.2)

**Common rules (Approved):**

- Ids are UUIDv7, generated by the app, with the database `uuidv7()` as a
  default.
- Every table has `created_at` and `created_by uuid NOT NULL REFERENCES
  principals`, except the exempt tables listed in section 7.1.
- **Immutable (I):** a trigger rejects UPDATE and DELETE, and the role has no
  grant for them.
- **Append-only (A):** INSERT only.

### 5.1 Identity

| Table | Columns | Notes |
|---|---|---|
| `principals` | id, kind, handle (unique), status, created_by | `kind` uses the shared enum (`user`, `service_account`, `system`, `anonymous`) and rejects `anonymous`. `UNIQUE (id, kind)`. Root-principal constraints are in section 7.1. A trigger forbids updating id, handle, kind or created_by. |
| `users` | principal_id (PK, FK), kind (FK with principal_id), email (citext, unique), display_name, password_hash, global_role | A CHECK forbids `kind = 'system'`. |
| `service_accounts` | principal_id (PK, FK), kind (FK with principal_id), description, global_role | A CHECK forbids `kind = 'system'`. |
| `sessions` | id, principal_id, token_hash, expires_at, revoked_at, last_seen_at, ip, user_agent | Exempt from `created_by`. |
| `api_keys` | id, principal_id, prefix, key_hash, expires_at, revoked_at | |
| `installation` | singleton row: installation_id, created_at, created_by | Written by migration 00002 as `system:migration`. |
| `idempotency_keys` | principal_id, key, request_sha256, response_status, response_body (capped at 64 KiB), expires_at | Added in Rev 2.1. |

### 5.2 Audit

`audit_events` (A) has these columns:

- id, occurred_at, tx_id, action, `action_class`
  (`command`, `job_execution`, `security`), outcome
  (`succeeded`, `denied`, `failed`);
- actor_principal_id, actor_kind, on_behalf_of_principal_id, auth_method;
- target_type, target_id, project_id, dataset_id, request_id;
- job_id, job_attempt, job_trigger (`command`, `periodic`);
- causation_event_id, causation_outcome;
- details jsonb, details_version.

Its constraints are in section 8.

### 5.3 Blob storage (Rev 2.1 #7 supersedes the Rev 2 `blob_objects.state` design)

| Table | Columns | Notes |
|---|---|---|
| `blob_uploads` | id, state (`staging`, `promoted`, `abandoned`, `deduplicated`), staging_key, sha256, size, expires_at, created_by | Tracks uploads in progress. |
| `blob_objects` (I) | id, sha256 (unique), size_bytes, media_type, created_at, created_by | Holds committed blobs only, with no state column. Every blob reference is a foreign key to this table. |
| `blob_health_events` (A) | blob_object_id, kind (`missing`, `corrupt`), detected_at, details | |

### 5.4 Registry

| Table | Columns | Notes |
|---|---|---|
| `registry_entries` | kind, namespace, name, project_id (null means global) | unique (kind, namespace, name) |
| `registry_versions` | entry_id, major, minor, patch, prerelease, status (`draft`, `published`, `deprecated`, `retired`), content jsonb, content_sha256, meta_schema_version, changelog, published_at/by, deprecated_at/by, retired_at/by | unique (entry_id, version). Content is frozen on publish. |
| `registry_version_refs` | from_version_id, to_version_id, role | Real foreign keys. Inserted only at publish. |
| `vocabulary_concepts` | vocabulary_version_id, code, display, parent_code, attributes jsonb | Concepts are rows, so large vocabularies scale. |
| `resolved_schemas` (I) | id, effective_schema jsonb, sha256 (unique), base_version_id, extension_version_ids (sorted), resolver_version | Exempt from `created_by` (derived cache). |
| `validation_engines` (I) | id, sha256 (unique), fingerprint jsonb | Added in Rev 2.1 #6. |

### 5.5 Projects and datasets (Rev 2 moved configuration from project to dataset)

| Table | Columns | Notes |
|---|---|---|
| `projects` | slug, name, description, status | No specialty or type column. |
| `project_members` | project_id, principal_id, role | |
| `project_tags` | project_id, vocabulary_version_id, concept_code | Zero or more tags. |
| `datasets` | id, project_id, slug (unique per project), name, dataset_kind_entry_id, status | `UNIQUE (id, project_id)`. A multi-kind project is several datasets. |
| `dataset_configurations` | id, dataset_id, seq, status (`draft`, `active`, `superseded`), resolved_schema_id, activated_at/by | One active configuration per dataset (partial unique index). Immutable once active. `UNIQUE (id, dataset_id)` and `UNIQUE (id, resolved_schema_id)`. |
| `dataset_configuration_items` | configuration_id, role, registry_version_id | Role counts: `dataset_kind` exactly 1, `record_schema` exactly 1, `extension` any number, `validation_ruleset` any number, `review_profile` 0 or 1, `source_type` any number. |

### 5.6 Sources, rights and provenance

| Table | Columns | Notes |
|---|---|---|
| `sources` | source_type_version_id, title, external_ref, metadata jsonb, status | Metadata is validated against the source type. |
| `project_sources` | project_id, source_id, purpose, linked_at/by, unlinked_at/by | Unlinking is a soft update. |
| `source_artifacts` (I, except `verified_at` and `verify_status`) | id, source_id, blob_object_id, original_filename | |
| `rights_statements` (A) | source_id, seq, basis, license_ref, holder, permitted_purposes, denied_purposes, restrictions, jurisdiction, effective_from/to, evidence_artifact_id, status, supersedes_id, created_at | `created_at` is set with `clock_timestamp()` while holding the `RIGHTS_EPOCH` shared lock (section 9.3). |
| `provenance_edges` (A) | subject_type/id, relation, object_type/id, activity jsonb | W3C PROV relations. |

Rights purposes are concepts in a registry vocabulary.
`rights.Evaluate(source, purpose, as_of)` returns `allow`, `deny` or
`unknown`, and **unknown counts as deny**.

### 5.7 Records (required, Approved in Rev 2)

| Table | Columns | Notes |
|---|---|---|
| `records` | id, dataset_id, external_key (unique per dataset, nullable), current_revision_id, current_seq | `UNIQUE (id, dataset_id)`. Only `current_*` can change, under optimistic concurrency. |
| `record_revisions` (I) | id, record_id, dataset_id, seq, parent_revision_id, operation (`create`, `update`, `withdraw`), configuration_id, resolved_schema_id, content jsonb (null for withdraw), content_sha256, validation_status, validation_report, validation_engine_id, audit_event_id | Unique (record_id, seq). Content is capped at 1 MiB. |
| `record_evidence` (I) | revision_id, source_artifact_id, locator jsonb, role | The locator is validated against the source type's locator schema. |
| `record_attachments` (I) | revision_id, name, blob_object_id | |

### 5.8 Releases (required, Approved in Rev 2; lifecycle from Rev 2.1 #2)

| Table | Columns | Notes |
|---|---|---|
| `dataset_versions` | id, dataset_id, version (semver), status (`draft`, `freezing`, `frozen`, `retracted`), configuration_id, intended_purposes, selection, previous_version_id, current_attempt_id, rights_as_of, manifest_blob_id, manifest_sha256, frozen_at/by, bundle_status (`pending`, `building`, `ready`, `failed`), bundle_blob_id, bundle_sha256, bundle_built_at, bundle_error, retracted_at/by, retraction_reason | `UNIQUE (id, dataset_id)`. `bundle_sha256` can be set only once. |
| `dataset_version_members` | dataset_version_id, dataset_id, record_id, record_revision_id, revision_operation | PK (dataset_version_id, record_id). Immutable once the parent is no longer a draft. |
| `dataset_version_freeze_attempts` | id, dataset_version_id, attempt_no, requested_by, rights_as_of, engine_fingerprint_id, outcome (`running`, `passed`, `failed`, `cancelled`, `aborted`), cursor, counters, report_blob_id, finished_at | |
| `dataset_version_member_checks` | attempt_id, dataset_version_id, record_id, validation_engine_id, report_sha256, report (inline when there are warnings or errors) | |
| `dataset_version_rights_checks` | attempt_id, source_id, purpose, decision, statement_ids | |

### 5.9 Relational integrity (Rev 2.1 #5, Approved)

| Constraint | Prevents |
|---|---|
| `record_revisions FK (record_id, dataset_id) → records(id, dataset_id)` | A revision in a dataset its record is not in. |
| `record_revisions UNIQUE (id, record_id)` and `UNIQUE (id, record_id, operation)` | (Targets for the keys below.) |
| `record_revisions FK (parent_revision_id, record_id) → record_revisions(id, record_id)` | A parent revision from another record. |
| `record_revisions FK (configuration_id, dataset_id) → dataset_configurations(id, dataset_id)` | A configuration from another dataset. |
| `record_revisions FK (configuration_id, resolved_schema_id) → dataset_configurations(id, resolved_schema_id)` | A resolved schema the configuration did not produce. |
| `records FK (current_revision_id, id) → record_revisions(id, record_id) DEFERRABLE INITIALLY DEFERRED` | A current pointer to another record's revision. |
| `dataset_versions FK (configuration_id, dataset_id) → dataset_configurations(id, dataset_id)` | A release using another dataset's configuration. |
| `dataset_version_members FK (dataset_version_id, dataset_id)`, `FK (record_id, dataset_id)`, `FK (record_revision_id, record_id, revision_operation)`, `CHECK (revision_operation <> 'withdraw')`, PK (version, record) | Cross-dataset members, a mismatched revision and record, tombstone members, or two revisions of one record. |
| `dataset_version_member_checks FK (dataset_version_id, record_id) → dataset_version_members` | A check row for a non-member. |
| Trigger on `record_evidence` insert | An artifact whose source is not linked to the dataset's project. |

Foreign keys use `MATCH SIMPLE`, so a null parent stays allowed.

### 5.10 Jobs

River's tables (R8) are carried as goose migrations at the pinned version.

---

## 6. Database identities and permissions

### 6.1 Identities (Approved in Rev 2; superuser name as built in M0)

| Role | Login | Used by | Privileges |
|---|---|---|---|
| bootstrap superuser (`studio_bootstrap` in the M0 compose; Rev 2 wrote `postgres`) | yes | `deploy/bootstrap` SQL and `studioctl db bootstrap` / `db create-empty` only | Creates the database and roles. Never used by applications. |
| `studio_owner` | no | Owns the schema and every object | Owns triggers and functions, so no login role can disable them. |
| `studio_migrator` | yes | `studioctl migrate`, the pg_restore step of `studioctl restore`, applying grants | Uses `SET ROLE studio_owner` (member, NOINHERIT). |
| `studio_app` | yes | `studio-api`, plus the `studioctl` subcommands that write as the app | The matrix in section 6.3. |
| `studio_worker` | yes | `studio-worker`, `studioctl jobs retry` | The matrix in section 6.3. No access to `users.password_hash`, `sessions`, `api_keys` or `idempotency_keys` beyond the sweep. |
| `studio_backup` | yes | `studioctl backup` and verification, plus the containerised `pg_dump` | `pg_read_all_data`, `pg_export_snapshot`, advisory locks. No writes. |
| `studio_readonly` | yes (optional) | Local debugging | SELECT, excluding credential tables. |

**Rules:**

- **Grants as code.** `internal/dbroles` produces idempotent GRANT and REVOKE
  SQL from one table→role→privilege map. It is applied after every migration
  and every restore.
- **Drift test.** A test compares the database catalog with that map.
- **Defaults.** PUBLIC privileges are revoked by default, and `search_path` is
  pinned per role.
- **Timeouts.** `statement_timeout` and `idle_in_transaction_session_timeout`
  are set per role:

  | Role | Statement | Idle in transaction |
  |---|---|---|
  | app | 15 s | 30 s |
  | worker | 5 min | 1 min |
  | backup | none | none |

- `SELECT … FOR SHARE` needs UPDATE privilege, so registry status races are
  prevented with advisory locks instead (section 9.4).

### 6.2 Subcommand roles (Approved in Rev 2.1 #4)

| Role | `studioctl` subcommands |
|---|---|
| app | `create-admin`, the restore marker event, the write step of `verify-restore` |
| worker | `jobs retry` (moved out of the API) |
| backup | `backup`, verification reads |
| migrator | `migrate`, `restore` |

### 6.3 Endpoint and job permission matrix (Approved in Rev 2.1 #4)

Key: **S** select, **I** insert, **U(cols)** update those columns only,
**D** delete. "Trigger-guarded" means a trigger limits the change to a draft
or allowed state. Every row also has **I** on `audit_events`, except
unaudited reads.

**API role (`studio_app`)**

| Endpoint group | Operations |
|---|---|
| Every authenticated request | S sessions, api_keys, principals(id, kind, status); U sessions(last_seen_at), at most once a minute; S/I idempotency_keys on POST |
| `auth/login` | S users(principal_id, email, password_hash, status); I sessions |
| `auth/logout`, key revoke | U sessions(revoked_at), api_keys(revoked_at) |
| Users and service accounts (admin) | I principals, users, service_accounts, api_keys; U users(display_name, global_role, password_hash), principals(status) |
| Registry drafts, publish, status changes | S/I registry_entries, registry_versions, registry_version_refs (publish only); U registry_versions(content, content_sha256, changelog, status, published_at/by, deprecated_at/by, retired_at/by), trigger-guarded; I/D vocabulary_concepts, draft only |
| Registry reads, resolve, dry-run validate | S registry tables, vocabulary_concepts, resolved_schemas |
| Projects, members, tags | I/U projects(name, description, status); I/D project_members, U(role); I/D project_tags |
| Datasets and configurations | I/U datasets(name, status); I dataset_configurations; I/D dataset_configuration_items (draft only); U dataset_configurations(status, resolved_schema_id, activated_at/by); I resolved_schemas `ON CONFLICT DO NOTHING` |
| Sources and links | I/U sources(title, external_ref, metadata, status); I project_sources, U(unlinked_at, unlinked_by) |
| Artifact upload | I/U blob_uploads(state, sha256, size, promoted_at); I blob_objects `ON CONFLICT DO NOTHING`; I source_artifacts, provenance_edges; I river_job |
| Artifact download (audited) | S source_artifacts, blob_objects |
| Rights | I rights_statements; S for evaluation |
| Records and batch | S configuration, registry and artifact tables; I records, record_revisions, record_evidence, record_attachments, provenance_edges, blob_uploads, blob_objects; U records(current_revision_id, current_seq, status) |
| Dataset versions (draft) | I dataset_versions, U(version, intended_purposes, selection); I/D dataset_version_members (draft only) |
| Freeze request, cancel, retract, rebuild | U dataset_versions(status, current_attempt_id, rights_as_of, retracted_at/by, retraction_reason, bundle_status), trigger-guarded; I/U freeze_attempts(outcome, finished_at); I river_job |
| Downloads, diff, reads | S of the relevant tables (downloads are audited) |
| Audit query, job list | S audit_events; S river_job |
| River insert (R5) | S, I, U(kind) river_job; USAGE river_job_id_seq; EXECUTE river_job_state_in_bitmask; USAGE type river_job_state |

**Worker role (`studio_worker`)**

| Job | Operations |
|---|---|
| Every job (fence, R1–R3) | S, U river_job; I audit_events |
| `artifact.verify_checksum` | S source_artifacts, blob_objects; U source_artifacts(verified_at, verify_status) |
| `blob.sweep` | S/U blob_uploads(state); S blob_objects |
| `blob.audit` | S blob_objects; I blob_health_events |
| `release.verify_members` | S members, records, record_revisions, record_evidence, registry tables, vocabulary_concepts, resolved_schemas, validation_engines; I member_checks, validation_engines `ON CONFLICT DO NOTHING`; U freeze_attempts(cursor, counters) |
| `release.verify_rights` | S rights_statements, sources, source_artifacts, record_evidence; I rights_checks; U freeze_attempts(cursor, counters) |
| `release.finalize` | S the above; I blob_uploads, blob_objects; U dataset_versions(status, manifest_blob_id, manifest_sha256, frozen_at, frozen_by, bundle_status); U freeze_attempts(outcome, finished_at, report_blob_id) |
| `release.bundle` | S release inputs; I blob_uploads, blob_objects; U dataset_versions(bundle_status, bundle_blob_id, bundle_sha256, bundle_built_at, bundle_error) |
| `release.reconcile` | S dataset_versions, freeze_attempts, river_job; U dataset_versions(status), freeze_attempts(outcome, finished_at) |
| `idempotency.sweep` | S(expires_at), D idempotency_keys |
| River client internals (R7, R8) | S/I/U/D river_job, river_queue, river_leader, river_notification; S river_migration; USAGE on River sequences and type; EXECUTE on River functions; MAINTAIN on river_job |

**Explicitly withheld:**

- **Worker:** users, sessions, api_keys and idempotency_keys (beyond the
  sweep); project, membership and registry writes.
- **API:** DELETE on river_job; UPDATE on river_job beyond `kind`; DELETE on
  any immutable table; UPDATE on audit_events.
- **Both:** TRUNCATE, DDL, REFERENCES.
- **Migrator:** not used at runtime.
- **Backup:** no writes.

**Unresolved:** the `blob.audit` row above is inferred from its described
effects (Rev 2.1 #7). The Rev 2.1 matrix did not list `blob.audit`
explicitly. Confirm it during M5.

---

## 7. Principals, system actors and authorship

### 7.1 Root principal and exemptions (Rev 2.1 #1 and Rev 2.2 #1, Approved)

- **The root principal is `system:migration`.** It is the only row whose
  `created_by` points to itself. The constraints are written as equivalences,
  so they hold in both directions:
  - `CHECK ((handle = 'system:migration') = (created_by = id))`: the root must
    reference itself, and no other row may.
  - `CHECK ((handle = 'system:migration') = (id = '00000000-0000-7000-8000-000000000001'))`
    with `UNIQUE (handle)`: exactly one root, at a fixed id.
  - `CHECK (handle <> 'system:migration' OR kind = 'system')`.
  - A trigger forbids updating id, handle, kind or created_by.
- **Seeded system principals.** Migration `00002` seeds `system:migration`,
  `system:cli` and `system:worker`, each with `created_by = system:migration`.
  `system:bootstrap` from Rev 2 was removed.
- **System principals cannot authenticate.** Composite foreign keys on
  `(principal_id, kind)` plus CHECKs keep them out of `users` and
  `service_accounts`.
- **The bootstrap SQL creates no rows.** It creates only the database and the
  roles. The `installation` row comes from migration `00002`, and a restore
  carries it over through the dump.
- **Tables exempt from `created_by`:**
  - `audit_events`: its actor columns are the record;
  - `sessions`: `principal_id` is the creator;
  - `resolved_schemas`: a derived cache;
  - `river_*` and goose bookkeeping tables.

  A catalog test enforces this list.
- **Anonymous actor.** `audit_events` has the CHECK
  `(actor_kind = 'anonymous') = (actor_principal_id IS NULL)`, and
  `anonymous` is allowed only for `auth.login_failed`. Unauthenticated
  requests to protected endpoints return 401 and are not audited.

### 7.2 Executing versus accountable principal (Rev 2.1 #8, Approved)

| Context | `created_by` and audit actor | `on_behalf_of_principal_id` | Decision columns (`published_by`, `frozen_by`, …) |
|---|---|---|---|
| API request | The authenticated user or service account | null | The same principal. |
| Job execution | `system:worker` | The initiating principal | The initiating principal (for example `frozen_by = freeze_attempts.requested_by`). |
| `studioctl` | `system:cli` (with `details.os_user` and `hostname`) | null | `system:cli` |
| Migration seed | `system:migration` | null | n/a |
| Failed login | n/a | null | n/a |

- `created_by` and the audit actor are always equal for rows written by the
  same command.
- `on_behalf_of` is allowed only when `actor_kind = 'system'`.
- There is no impersonation in Phase 1.

---

## 8. Audit semantics (Rev 2, Rev 2.1 and Rev 2.2, Approved)

### 8.1 What counts as an auditable mutation

One command, meaning one service operation that commits a change to domain or
security state, writes exactly one `outcome = succeeded` event in the same
transaction.

| Situation | Rule |
|---|---|
| Single-entity command | One event. |
| Multi-entity command (activation, freeze) | One event, with secondary changes in `details.affected[]`. |
| Batch (records, concept load, member set) | One event with `{count, payload_sha256, first_id, last_id}`. Created rows carry `audit_event_id`. All-or-nothing. |
| Rolled-back transaction | No event, only a log line with `request_id`. |
| Authorization denied (403) | A `denied` event written out of band (aggregated, section 8.3). |
| Authentication | Login, logout, session and key changes, and password changes are normal events. A failed login is a `failed` event: the actor is the existing user's principal, or anonymous with `details.identifier_hmac`. |
| Background jobs | See section 8.2. |
| Idempotent replay | No new event. |
| Reads | Not audited, except data egress: `artifact.content_read`, `dataset_version.manifest_read` and `dataset_version.bundle_read` (approved default U3). |
| Dry-run validate | Not audited. |
| Excluded technical writes | `sessions.last_seen_at`, River bookkeeping, staging-upload cleanup (except one `blob.pending_swept` event per sweep). |
| Migrations | Schema changes are tracked by goose. Seeded rows get one `system.seeded` event. |
| Restore | One `system.restored` event after verification. |

**Enforcement:**

- Actions come from a catalog in code, each with a versioned `details` struct.
- A route-driven completeness test covers every mutating route.
- A job-driven completeness test covers every job kind.

### 8.2 Actor-kind and job-execution constraints (Rev 2.2 #2 and #3, Approved)

**Actor kind:**

- `audit_events` has the composite foreign key
  `(actor_principal_id, actor_kind) → principals(id, kind)`, using
  `MATCH SIMPLE` so it is unchecked when the actor is null for anonymous.
- A principal's kind can never change. Both the trigger and the foreign key
  enforce this.

**Job execution:**

- `(action_class = 'job_execution') = (job_id IS NOT NULL)`.
- `job_id`, `job_attempt` and `job_trigger` are all set or all null, and
  `job_attempt >= 1`.
- `(action_class = 'job_execution') = (actor_principal_id = <system:worker id>)`.
- `job_trigger = 'command'` requires `causation_event_id` and
  `on_behalf_of_principal_id`. `periodic` forbids both.
- **Causation:**
  - a self-referencing foreign key
    `(causation_event_id, causation_outcome) → audit_events(id, outcome)`,
    with `causation_outcome = 'succeeded'` and `UNIQUE (id, outcome)` on
    `audit_events`;
  - an insert trigger requires the causing event's `details.enqueued_jobs` to
    contain this `job_id`, the causing event to have occurred earlier, and
    `on_behalf_of` to be inherited from it.
- There is a unique index on `job_id` where `action_class = 'job_execution'`.
- A job that enqueues another job writes an event, because enqueueing is a
  state change. For a chunked job, the causing event of chunk *n+1* is chunk
  *n*'s event.

### 8.3 Denied-event aggregation (Rev 2.1 #4 and Rev 2.2 #4, Approved)

- Denied and failed events are aggregated in memory per API process, because
  `audit_events` is append-only.
- They are grouped in 60-second windows by (actor or identifier HMAC, action,
  target, outcome).
- A window is flushed when it closes, or early once there are 10,000 distinct
  keys.
- **Graceful shutdown:** the flush runs as a shutdown hook after HTTP has
  drained, with at most 5 seconds of the shutdown deadline. If it fails, each
  pending aggregate is written as an ERROR log record named
  `audit_aggregate_unflushed`.
- **This is best-effort.** A hard crash (SIGKILL, out of memory) loses at most
  one window of *denied and failed* events. This is accepted. Succeeded events
  are written transactionally and are never affected.
- **Prerequisite carried from the M0 review (finding M0-F4):** the M0
  lifecycle currently skips hooks once the shutdown deadline has expired. It
  must call hooks even after the deadline so the fallback logging can run.
  Fix this before or in M2.

---

## 9. Registry and schema versioning

### 9.1 Kinds and lifecycle (Approved)

**Kinds:** `dataset_kind`, `record_schema`, `extension`, `vocabulary`,
`validation_ruleset`, `review_profile` and `source_type`. Each kind handler
implements `Kind`, `MetaSchema`, `Validate`, `ExtractRefs` and
`CheckCompatibility`.

- **Identity.** A version is `kind/namespace/name@MAJOR.MINOR.PATCH[-pre]`.
  Content is canonicalised with RFC 8785 and hashed with SHA-256.
- **Lifecycle.**
  - A draft is editable with `If-Match`.
  - Publishing validates against the meta-schema and the kind's own checks,
    resolves references to published versions, and freezes the content.
- **Pinning.** Pins are always to exact version ids. A reference may be
  authored as a semver range, but it resolves to an exact version at publish.
- **Compatibility.**
  - The publisher declares the version bump.
  - For `record_schema` and `extension`, a conservative checker refuses a
    declared bump that is smaller than what it detects. Adding an optional
    property is minor; everything else is major.
  - A full semantic diff is optional work (O6).
- **Validation.**
  - Level 1 is JSON Schema, from the resolved schema.
  - Level 2 is CEL rulesets, each rule `{id, severity, applies_to,
    cel_expression, message}`, with a cost budget and timeout.
  - Fields bound to a vocabulary are checked against its concepts.
  - The report shape is `{valid, errors[], warnings[], rule_versions[]}`.
- **Review profiles** are stored, versioned and validated in Phase 1, but not
  executed.

### 9.2 Schema extensions: slots, not `allOf` (Rev 2, Approved)

**Experiment (Verified with Python `jsonschema` 4.19.0, draft 2020-12; repeated
in Go as test E1 in M9):**

- With `allOf[closed base, extension]`, a valid extended record is rejected.
- An open base accepts misspelled keys.
- A base with its own `unevaluatedProperties: false` inside `allOf` rejects
  extension fields.
- An extension can silently narrow base fields through `allOf`.
- Closed nested items fail the same way at every level.
- Slot materialisation behaves correctly in every case tested, including
  nested slots.

**Strategy:**

1. **The base declares slots.** It marks extension points with
   `x-studio-extension-point` on a closed `extensions` object, at the root or
   nested (including inside array items). Bases should be closed everywhere.
2. **An extension targets slots.** It declares
   `targets: [{schema, range, point, fragment}]`. Its data key is the
   extension's `namespace.name`, without a version.
3. **The resolver materialises one effective schema.**
   - It inserts fragments into their slots in sorted namespace order.
   - It moves each extension's `$defs` under `$defs["ext:<ns.name>"]` and
     rewrites its references.
   - It canonicalises and hashes the result and stores it in
     `resolved_schemas`.
   - Validation then runs plain JSON Schema on that one document.
4. **Publish checks.**
   - Targets must exist in the targeted base range.
   - No `$ref` may leave the fragment or reach outside the registry.
   - Fragments cannot declare extension points.
   - A configuration cannot bind two versions of the same extension.
5. **Activation checks.**
   - The target range must include the bound base version.
   - The point must exist in that exact version.
   - A binding may be marked `required`.
6. **Rules.** Extension CEL rules may read base fields. That is the only way an
   extension can constrain base data, and such rules are namespaced.
7. **Compatibility.**
   - Adding a slot is minor; removing or moving one is major.
   - Adding an optional property to an extension fragment is minor; anything
     else is major.

**Tests E1–E11:**

| Test | Expectation |
|---|---|
| E1 | The `allOf` failures reproduce in Go. |
| E2 | A closed root slot plus an extension. |
| E3 | Nested slots inside array items. |
| E4 | Two extensions coexist; the resolved hash doesn't depend on binding order. |
| E5 | A missing point is refused at publish; a range mismatch is refused at activation. |
| E6 | A fragment cannot redefine a base property. |
| E7 | Optional versus required bindings. |
| E8 | Pins survive a base v2. |
| E9 | Extension CEL rules work. |
| E10 | Remote `$ref` is refused, and internal `$defs` refs are rewritten. |
| E11 | Golden-file determinism. |

### 9.3 Deprecated and retired versions (Rev 2.1 #3, Approved)

| Operation | published | deprecated | retired |
|---|---|---|---|
| New `registry_version_refs` | ✓ | ✗ | ✗ |
| Add to a draft configuration | ✓ | Only if carried forward from the dataset's active configuration | ✗ |
| Activate a configuration containing it | ✓ | Only if carried forward | ✗ |
| Write revisions under an active configuration pinning it | ✓ | ✓, with a `registry.deprecated_pin` warning | ✗ |
| Request or finalize a freeze pinning it | ✓ | ✓, with the warning recorded in the manifest | ✗ |
| Read, resolve, dry-run, existing releases, bundle rebuild, restore | ✓ | ✓ | ✓ |

**Status changes:**

- published ⇄ deprecated (reversible);
- published or deprecated → retired (final).

Retiring needs `acknowledge_dependents: <count>`, which must match the current
count. It does not need zero dependents.

### 9.4 Race-free status changes (Approved)

- Status changes take `pg_advisory_xact_lock(registry_version_key(id))`.
- Revision writes, freeze requests and freeze finalization take the shared form
  of that lock for each pinned version.
- Triggers on configuration items, activation and revision inserts act as the
  backstop.

### 9.5 Reproducibility metadata (Rev 2.1 #6, Approved)

`validation_engines` stores a fingerprint containing:

- `studio_version` and `go_version`;
- `resolver_version` (an integer bumped on any change to resolver output);
- the canonicalizer module version;
- jsonschema (module version, draft, `format` enforcement setting, regex
  engine);
- cel (module version, sha256 of the environment declaration);
- `validator_version` and `bundle_format_version`.

Module versions come from `runtime/debug.ReadBuildInfo`.

- **One engine per freeze attempt.** A fingerprint change between chunks aborts
  the attempt.
- **Determinism.** The CEL environment excludes non-deterministic functions.
- **Reports.**
  - Every member check stores `report_sha256`.
  - The full report is stored inline when it has warnings or errors.
  - Failed attempts keep a full report blob.
- **Manifest and bundle** include the engines, `validation/summary.json` and
  `validation/reports.jsonl`. This is bundle format version 2.
- **Rebuilds** never re-validate.
- `studioctl verify-release --revalidate` is diagnostic only.

---

## 10. Records and dataset versions

### 10.1 Records (Rev 2, Approved)

- **Create.** Validates against the dataset's active configuration (resolved
  schema, rulesets, vocabularies, evidence locators) and stores revision 1
  with its validation report.
- **Update.** `If-Match: <current_revision_id>`; a mismatch returns 412.
- **Withdraw.** Writes a tombstone revision. Records are never deleted.
- **Batch.** Up to 1,000 records, all-or-nothing (approved default U6).
- **Configuration changes.** Existing revisions keep their pins. There is no
  automatic content migration.
- **Invalid revisions.** They may be stored, and are blocked at release
  (approved default U2).

### 10.2 Freeze lifecycle (Rev 2.1 #2 supersedes the Rev 2 single-transaction freeze)

**Status:** `draft → freezing → frozen → retracted`, with
`freezing → draft` on failure or cancellation. Bundle readiness is a separate
field.

| Step | Who | Effect |
|---|---|---|
| `POST …:freeze` | API, short transaction | Takes the exclusive `RIGHTS_EPOCH` advisory lock, then sets `rights_as_of := clock_timestamp()`. Creates a freeze attempt and sets status `freezing`, which makes members immutable. Enqueues `release.verify_members` and writes the audit event. |
| `release.verify_members` | Worker, one transaction per 500 members | Writes member checks (`ON CONFLICT DO NOTHING`), advances the cursor and re-enqueues itself. The last chunk enqueues `release.verify_rights`. |
| `release.verify_rights` | Worker, one transaction per chunk of sources | Writes rights checks evaluated as of `rights_as_of`. |
| `release.finalize` | Worker, short transaction | Checks every result and re-checks registry pins under advisory locks. Writes the manifest blob, sets status `frozen` and `bundle_status = pending`, and enqueues `release.bundle`. If any check failed, the status returns to `draft`, the attempt is marked `failed`, and the report is stored. |
| `release.bundle` | Worker | Builds the bundle outside any transaction. `bundle_status` moves `pending → building → ready`, or ends `failed` with `bundle_error`. |
| `POST …:rebuild-bundle` | API | Allowed only when the bundle is `failed`. |
| `POST …:cancel-freeze` | API | `freezing → draft`. The attempt becomes `cancelled`. |
| `release.reconcile` (every 5 minutes) | Worker | A `freezing` version whose attempt has no live job is aborted back to draft. |

- **Released** means `frozen` and `bundle_status = ready`. Bundle downloads
  return 409 until then.
- **Deterministic rights.**
  - Rights inserts take `pg_advisory_xact_lock_shared(RIGHTS_EPOCH)` and then
    set `created_at := clock_timestamp()`.
  - Freeze takes the exclusive lock before reading the clock.
  - So every statement with `created_at ≤ rights_as_of` has committed by then,
    and every later statement gets a later timestamp.
- **Retries.**
  - Chunks are idempotent.
  - Each chunk retries up to 10 times.
  - A discarded chunk leads to an aborted attempt.
  - A new freeze request creates attempt *n+1*, and earlier attempts are kept.
- **Approved default:** release jobs have a 2-minute job timeout.

**Freeze gates:**

- every member is re-validated against the version's configuration;
- rights evaluate to `allow` for every evidence artifact's source and every
  intended purpose (unknown counts as deny);
- registry pins are not retired;
- every referenced blob is committed.

### 10.3 Manifest and bundle (Rev 2, plus Rev 2.1 #6, Approved)

**Manifest** (RFC 8785 canonical JSON):

- `installation_id`, dataset, version, previous version, `frozen_at`;
- every pinned registry version with its content sha256, plus the resolved
  schema sha256;
- members sorted by record id, with revision id and content sha256;
- evidence artifacts with sha256;
- a rights snapshot;
- a validation digest;
- the engines;
- the bundle format version.

**Bundle** (approved default U4):

- Uncompressed tar.
- Sorted entries, mtime set to `frozen_at`, uid and gid 0, mode 0644, PAX
  headers.
- Contents: `manifest.json`, `records.jsonl`, `evidence.jsonl`,
  `schemas/<sha>.json`, registry documents, vocabularies, `rights.json` and the
  validation files.
- Artifact bytes are excluded by default; an optional flag includes them,
  gated on rights.

**Reproducibility:** rebuilding a bundle gives identical bytes, including after
a backup and restore.

### 10.4 Version numbering and retraction (Approved)

- Semver chosen by the user, with a diff endpoint to help choose (approved
  default U5).
- Retraction sets the status and records a reason. Content never changes.

---

## 11. Blob storage and recovery (Rev 2.1 #7, Approved)

**Upload protocol:**

1. **Transaction 1:** insert `blob_uploads` with state `staging`, and commit.
2. Stream to `staging/<upload_id>`, hashing as it goes, and fsync.
3. **Transaction 2:**
   - take `pg_advisory_xact_lock(blob_key(sha256))`;
   - if a `blob_objects` row with that sha256 exists, verify the final file,
     delete the staging file and mark the upload `deduplicated`;
   - otherwise rename staging → `sha256/ab/cd/<hash>`, fsync the directory,
     insert `blob_objects` and mark the upload `promoted`;
   - insert the referencing rows and the audit event;
   - commit.

**Recovery** is done by `blob.sweep`, every 15 minutes. It try-locks the
exclusive `BACKUP_GC` lock and skips its run if a backup holds it.

| Crash point | Recovery |
|---|---|
| Before transaction 1 commits | Staging files older than 24 hours with no row are deleted. |
| During streaming | After `expires_at` (24 hours): delete the file and mark the upload `abandoned`. Write one audit event per sweep. |
| After the rename, before transaction 2 commits | A final-path file with no row and mtime older than 24 hours is deleted only after taking the per-sha lock and re-checking for a row. A re-upload of the same content within the grace period reuses the file. |
| Row exists but the file is missing or corrupt | Not self-healing. `blob.audit` and `verify-restore` detect it and write `blob_health_events`, and `/readyz` fails. The fix is restoring from backup. |

Committed blobs are immutable and are never deleted in Phase 1.

**Tests B1–B8:** crash injection at each point, the sweeper racing an
uploader, the backup lock, concurrent dedupe, missing-file detection, and
backup test C1 re-run on this protocol.

---

## 12. Jobs and the River fence

### 12.1 Worker transaction pattern (Rev 2.1, refined in Rev 2.2 #5, Approved)

```
BEGIN
  SELECT state, attempt, metadata FROM river_job WHERE id = $job FOR UPDATE   -- fence
  require state = 'running' AND attempt = job.Attempt
          AND NOT (metadata ? 'cancel_attempted_at')
      -- otherwise ROLLBACK and return ErrStaleAttempt / a cancellation error
  domain writes + audit_events INSERT (job_id, job_attempt, causation ...)
  j := river.JobCompleteTx[*riverpgxv5.Driver](ctx, tx, job)
  require err == nil AND j.State == completed                                -- R3
COMMIT
```

**A stale or cancelled attempt must return an error, never `nil`.** Returning
`nil` would let River's completer mark *another* worker's running attempt as
completed, and the job would never run (R2).

**Every job handler must be idempotent.** A test runs each one twice.

### 12.2 Tests (J1–J10, with the Rev 2.2 refinements)

| Test | Proves |
|---|---|
| J1 | The commit path: domain row, audit event and completion all visible. |
| J2 | A failure injected after `JobCompleteTx` but before commit: none of the three visible; the retry produces exactly one audit event. |
| J3 | The same with a panic. |
| J4a | Rescued and re-fetched before the fence. The rescue uses River's rescue query, and a second client fetches attempt 2. Both orderings are tested: B commits first, or A is released first. Exactly one domain effect, and the job eventually completes. |
| J4b | A rescue attempt while the fence is held is blocked (checked with `pg_blocking_pids`). After commit it updates zero rows, and the state stays `completed`. |
| J4c | Shows that returning `nil` on a stale attempt loses the job. Documents why the rule exists. |
| J5a | Library level, no fence: after an out-of-band state change, `JobCompleteTx` returns the row with no error (proves R3). |
| J5b | With the fence: the post-completion state check, exercised by fault injection that skips the fence. |
| J5c | Cancellation: `cancel_attempted_at` is detected at the fence. |
| J6 | A job enqueued in a rolled-back transaction never runs; one enqueued in a committed transaction runs once. |
| J7 | A River error inside a domain transaction leads to a full rollback (R4). |
| J8 | Both roles run their River operations with only their generated grants. |
| J9 | Killing the worker before commit leads the rescuer to re-run the job, with exactly one audit event. |
| J10 | The unique index rejects a duplicate `job_execution` audit event. |

**Unresolved:** whether River's rescue query can be called directly through
its driver (`JobRescueMany`). Verify it in M6. If it can't, the test runs the
same SQL text copied from v0.49.0.

---

## 13. Backup and restore (Rev 2, adapted by Rev 2.1 #7, Approved)

**Backup** (`studioctl backup`, as `studio_backup`):

1. Take `pg_advisory_lock_shared(BACKUP_GC)`. Open a
   `REPEATABLE READ READ ONLY` transaction and call `pg_export_snapshot()`.
2. In that snapshot, read every `blob_objects` row, the row counts, the goose
   version, the newest audit event id, the registry digest and the release
   manifest hashes.
3. Run `pg_dump -Fc --snapshot=<id>` from the PostgreSQL 18 container. The host
   `pg_dump` is version 14 and is not used.
4. Copy every listed blob, re-hashing each one. A mismatch aborts the backup.
5. Write `manifest.json`, then the `COMPLETE` marker last.

**Restore:**

1. Verify every hash before touching anything. Refuse a non-empty target, or a
   backup from a newer schema version.
2. Create an empty database as the bootstrap superuser.
3. Run `pg_restore --no-owner --role=studio_owner --exit-on-error
   --single-transaction` as the migrator.
4. Re-apply grants, then copy the blobs, verifying them.
5. Run `verify-restore`: counts, no dangling blob references, blob hashes,
   registry hashes, and manifest hashes.
6. Write the `system.restored` event.

**Tests C1–C10:**

| Test | Checks |
|---|---|
| C1 | Concurrent writes during backup, repeated 20 times. |
| C2 | An upload still in `staging` at snapshot time is absent from the manifest and unreferenced after restore. Reconciled to the Rev 2.1 #7 staging design. |
| C3 | A failed blob copy leaves no `COMPLETE` marker. |
| C4 | A corrupt or missing blob is caught by the restore pre-check. |
| C5 | Tampering is detected. |
| C6 | Restoring into a non-empty target is refused. |
| C7 | A backup from a newer schema is refused. |
| C8 | The sweep is blocked by the backup lock. |
| C9 | Bundle rebuilds are byte-identical after restore. |
| C10 | A reference to a nonexistent blob is rejected by the database. Reconciled: Rev 2's "pending blob" trigger was replaced by a plain foreign key. |

---

## 14. Security (Approved)

**Authentication:**

- Local accounts with argon2id password hashing.
- Opaque 256-bit session tokens sent as Bearer tokens, stored as SHA-256
  hashes, expiring after 12 hours plus an idle timeout, and revocable.
- API keys of the form `dsk_<prefix>_<secret>`, stored as hashes, with expiry
  and revocation.
- Login rate-limited per account and IP, with uniform error messages.
- The first admin is created only through `studioctl create-admin`. There are
  no default credentials.
- OIDC is deferred (optional O4).

**Authorization:**

- Deny by default.
- Role-to-permission mapping lives in code.
- Checks run in the service layer.
- A route × role matrix test covers every route.

| Permission | platform_admin | registry_admin | owner | maintainer | contributor | viewer |
|---|---|---|---|---|---|---|
| Manage users and service accounts | ✓ | | | | | |
| Publish or draft global registry versions | ✓ | ✓ | | | | |
| Project-private registry entries | ✓ | | ✓ | ✓ | | |
| Read registry | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Create project | ✓ | | any member (approved default U7) | | | |
| Manage members, archive | ✓ | | ✓ | | | |
| Configurations: edit / activate | ✓ | | ✓ / ✓ | ✓ / ✗ | | |
| Sources, artifacts, links | ✓ | | ✓ | ✓ | ✓ | |
| Record rights statements | ✓ | | ✓ | ✓ | | |
| Read project data | ✓ | | ✓ | ✓ | ✓ | ✓ |
| Read audit: project / global | ✓ / ✓ | | ✓ / ✗ | | | |

**Rev 1 did not decide how the new Rev 2 operations map to roles.** The
matrix above is the Rev 1 matrix. Rev 2 added records, revisions, batch,
dataset versions, freeze, cancel, retract and rebuild, but nothing in the
record maps them to roles. This is **Unresolved** (section 20). Decide it
before M12.

**Data protection:**

- Content-hash blob keys only.
- Uploads are streamed, size-capped and have their media type sniffed.
- Logs never contain tokens, password fields or record content.
- Secrets come only from the environment.
- Remote `$ref` is disabled, and CEL has a cost limit.
- `govulncheck` and golangci-lint (with gosec) run in `make check`.

**Display redaction (M0, as built):**

- `config.RedactURL` masks the userinfo password and the values of the
  `password` and `sslpassword` query parameters.
- A key/value DSN is fully redacted.
- The connection string used by pgx is not modified.

TLS and encryption at rest are deployment concerns and outside Phase 1.

---

## 15. API (`/v1`; Rev 1, plus Rev 2 additions, minus the Rev 2.1 removal)

**Conventions:**

- JSON. Errors use RFC 9457 problem+json.
- Cursor pagination.
- `ETag` and `If-Match` for drafts and records.
- `Idempotency-Key` accepted on uploads and publish actions.
- 1 MiB JSON body limit.

| Area | Endpoints |
|---|---|
| Ops | `GET /healthz`, `GET /readyz` (no auth; built in M0) |
| Auth | `POST /v1/auth/login`, `POST /v1/auth/logout`, `GET /v1/me` |
| Users | `GET/POST /v1/users`, `GET/PATCH /v1/users/{id}`, `POST /v1/users/{id}:disable` |
| Service accounts and keys | `GET/POST /v1/service-accounts`, `POST/GET /v1/{principal}/api-keys`, `DELETE /v1/api-keys/{id}` |
| Registry | `GET /v1/registry/kinds`; `GET/POST /v1/registry/entries?kind=`; `GET /v1/registry/entries/{id}`; `GET/POST /v1/registry/entries/{id}/versions`; `GET/PATCH /v1/registry/versions/{vid}`; `POST …/{vid}:publish`, `:deprecate`, `:retire`; `GET …/{vid}/refs`, `…/dependents`, `…/resolved`; `POST …/{vid}:validate` |
| Vocabularies | `GET /v1/registry/versions/{vid}/concepts?q=`, `PUT …/concepts` (draft only) |
| Projects | `GET/POST /v1/projects`, `GET/PATCH /v1/projects/{id}`, `POST …:archive`, `GET/PUT/DELETE /v1/projects/{id}/members/{principal}`, `GET/PUT /v1/projects/{id}/tags` |
| Datasets | `GET/POST /v1/projects/{id}/datasets`, `GET/PATCH /v1/datasets/{id}`, `POST …:archive` |
| Configurations | `GET/POST /v1/datasets/{id}/configurations`, `PATCH …/{cid}`, `POST …/{cid}:activate`, `GET /v1/datasets/{id}/configuration`, `GET /v1/resolved-schemas/{id}` |
| Validation (dry run) | `POST /v1/datasets/{id}/validate` |
| Sources | `GET/POST /v1/sources`, `GET/PATCH /v1/sources/{id}`, `POST /v1/sources/{id}/artifacts`, `GET …/artifacts`, `GET /v1/artifacts/{aid}/content`, `POST/DELETE /v1/projects/{id}/sources/{sid}` |
| Rights | `GET/POST /v1/sources/{id}/rights`, `POST /v1/rights/{rid}:supersede`, `GET /v1/sources/{id}/rights:evaluate?purpose=&at=` |
| Provenance | `GET /v1/provenance?subject_type=&subject_id=` |
| Records | `GET/POST /v1/datasets/{id}/records`, `POST /v1/datasets/{id}/records:batch`, `GET /v1/records/{id}`, `GET/POST /v1/records/{id}/revisions`, `GET /v1/revisions/{rid}`, `POST /v1/records/{id}:withdraw` |
| Releases | `GET/POST /v1/datasets/{id}/versions`, `GET /v1/dataset-versions/{vid}`, `PUT/DELETE …/{vid}/members`, `POST …:freeze`, `:cancel-freeze`, `:retract`, `:rebuild-bundle`, `GET …/manifest`, `GET …/bundle`, `GET …/diff?against={vid}` |
| Audit | `GET /v1/audit-events?…` |
| Jobs | `GET /v1/jobs`, `GET /v1/jobs/{id}`. Retry moved to `studioctl jobs retry` in Rev 2.1. |

`DELETE /v1/projects/{id}/sources/{sid}` performs a soft unlink
(`unlinked_at`) in line with the Rev 2.1 permission matrix.

---

## 16. Milestones

### 16.1 Dependencies (Approved)

```
M0 ─► M1 ─► M2 ─► M3 ─► M4 ─┬────────────► M8 ─► M9 ─► M10 ─► M11 ─► M12 ─► M13 ─► M14
            │               │                             ▲       ▲
            ├─► M5 ─────────┼─► M7 (backup core) ─────────┘       │
            └─► M6 ─────────┘                                      │
                                    (M7 consistency tests extended in M13)
```

M5 and M6 depend only on M1 and M2, and can run alongside M3 and M4.

### 16.2 Milestone scope and outcomes

| # | Milestone | Depends on | Outcome and tests |
|---|---|---|---|
| **M0** | Skeleton: config, logging, health and readiness, Makefile, compose (pinned digest), lint and vulnerability scan, local-dev docs | none | **Complete**: commit `92445c6`, branch `feat/dataset-studio-m0`. The acceptance criteria are in section 17.1. |
| M1 | Bootstrap SQL, the six roles, goose, grants as code, `principals` with system actors (section 7.1), `installation`, `idempotency_keys`, test harness | M0 | Grant-drift test; each role is refused what it should be; migrations run from empty. Tests P1–P4, G2, G3. |
| M2 | Audit core: writer, action catalog, out-of-transaction denied and failed writer with aggregation, actor-kind and job-execution constraints, lifecycle hook fix (M0-F4) | M1 | A rolled-back transaction leaves no event; UPDATE and DELETE on audit rows fail. Tests P5, A3–A5, AG1–AG4, JA1–JA9. |
| M3 | Authentication, including failed-login audit with HMAC | M2 | Lifecycle tests; only hashes are stored. |
| M4 | Authorization, route × role matrix, denied events | M3 | The matrix is complete, and 403s are audited. |
| M5 | Blob store, `blob_uploads` staging and promotion protocol, `blob_objects` ledger, sweep and audit jobs, `blob_health_events` | M2 | Store conformance suite; B1–B7. |
| M6 | Jobs: River behind an interface, the fence pattern, River grants, unique job audit index, idempotency harness | M2 | Job conformance suite; J1–J10. |
| M7 | Backup, restore and verify-restore | M5, M6 | C1–C8 and C10 on the existing tables; B8; P6. |
| M8 | Registry core, canonical hashing, `record_schema` and `dataset_kind` handlers, fixtures A and B, status rules and advisory locks | M4 | Publish and freeze work; wrong bumps rejected; D1, D2, D7. |
| M9 | Remaining kinds, slot resolver, `resolved_schemas`, CEL rulesets, source-type locator schemas, `validation_engines` | M8 | E1–E11; a third kind added with no migration; V6. |
| M10 | Projects, tags, datasets, dataset configurations | M9 | Multi-kind project; no tags works; two domain tags works; D1 activation path. |
| M11 | Sources, artifacts on the ledger, rights with the `RIGHTS_EPOCH` lock, provenance | M10, M5, M6 | Unknown rights evaluate to deny; supersession; checksum job. |
| M12 | Records, revisions, evidence, attachments, batch, composite keys | M11 | Immutability; 412 on stale If-Match; batch atomicity; RI1–RI9; D3, D6. |
| M13 | Dataset versions, freeze state machine, attempts and checks tables, release jobs, manifest v2, deterministic bundle, diff, retraction | M12, M7 | F1–F8 (freeze), D4, D5, V1–V5, A1, A2, C9. |
| M14 | Hardening: end-to-end walkthrough for both fixtures (registry through release, then backup, restore and rebuild), docs | all | The acceptance criteria in section 17.2. |

**Optional work, only after M14** (not decided; section 20):

- O3: S3 adapter.
- O4: OIDC.
- O5: audit hash chain.
- O6: semantic schema diff.
- O7: generated clients.
- Partial-success batches.

O1 (records) and O2 (dataset versions) from Rev 1 were promoted to required
scope in Rev 2.

---

## 17. Acceptance criteria

### 17.1 M0 (final; met at commit `92445c6`)

1. On a clean clone, `make up && make tools && make check` passes.
2. Three binaries build with version and commit injected; `studioctl version`
   prints JSON.
3. `/healthz` returns 200. `/readyz` returns 200 when ready, and 503 when the
   database is unreachable or the server is draining, without leaking error
   details.
4. Unknown routes return RFC 9457 problem JSON. `X-Request-ID` is on every
   response, and a malformed inbound value is replaced.
5. A panic returns a 500 problem response. The panic value is logged and never
   sent to the client.
6. SIGTERM shuts down in order (draining, HTTP drained, hooks in reverse),
   within one deadline, and exits 0. In-flight requests complete.
7. Invalid configuration lists every problem. Passwords never appear in
   output or logs, whether in userinfo or as query parameters.
8. Logs are JSON with a `component` field. Query strings and headers are not
   logged.
9. Compose runs PostgreSQL 18.6 pinned by digest, bound to loopback, and
   healthy. A test asserts major version 18.
10. Integration tests fail rather than skip when their environment is missing.
11. `make toolchain-check` rejects a Go command that prints nothing.
12. Lint and the vulnerability scan are clean at the pinned versions.
13. There is no domain code and no migrations, and no application uses the
    superuser outside the documented E2E-harness exception.

### 17.2 Phase 1 (Approved in Rev 2)

- **AC1.** M0 through M14 are complete on a clean clone using only
  `docs/local-dev.md`. `make check` passes, including `-race`, lint and
  govulncheck.
- **AC2.** For each of the two structurally different fixtures: registry
  publish → dataset configuration with an extension → records with evidence
  from two source types → revisions → freeze → manifest and bundle. None of it
  uses fixture-specific code.
- **AC3.** A frozen dataset version rebuilds byte for byte from the database,
  including after backup and restore (C9).
- **AC4.** Freeze is blocked by an invalid member, by unknown or denied rights
  for a stated purpose, and by a retired pin.
- **AC5.** E1–E11 pass.
- **AC6.** C1–C10 pass. Restore refuses every incomplete or tampered backup.
- **AC7.** Each database role is denied what its spec forbids.
- **AC8.** Every `created_by` and audit actor resolves to a principal, and
  system actors cannot authenticate.
- **AC9.** The audit semantics in section 8 hold under test.
- **AC10.** Nothing requires choosing a primary dataset kind or specialty.

### 17.3 Cross-cutting tests that fail the build (Approved)

1. Authorization matrix.
2. Audit completeness, for routes and job kinds.
3. Immutability through raw SQL.
4. OpenAPI contract: registered routes equal documented paths.
5. Neutrality guard: no specialty or dataset-type identifiers and no
   dataset-kind enums in non-test code.
6. Extensibility: fixture A, fixture B and a third kind, with zero migrations.
7. Grant drift.
8. Every job kind is idempotent.
9. Deterministic outputs: resolver, manifest and bundle golden hashes.

### 17.4 Per-correction test identifiers (Rev 2.1 and Rev 2.2)

| Area | Tests | Definition status |
|---|---|---|
| Principals | P1–P6 | Defined in Rev 2.1 #1 (migrate from empty; catalog `created_by` test; non-root self-reference rejected; system user rejected; anonymous constraint; restore keeps `installation_id`). P3 is extended by P3b below. |
| Root principal | P3a–P3d | **Confirmed (X2).** Defined below. |
| Job-execution audit | JA1–JA9 | **Confirmed (X2).** Defined below. |
| Aggregation | AG1–AG4 | **Confirmed (X2).** Defined below. |
| Freeze | F1–F8 | Defined in Rev 2.1 #2. |
| Status rules | D1–D7 | Defined in Rev 2.1 #3. |
| Grants | G1–G5 | Defined in Rev 2.1 #4. |
| Relational integrity | RI1–RI9 | Defined in Rev 2.1 #5 (one raw-SQL violation per constraint, plus a positive case). Renamed from R1–R9; see section 22. |
| Reproducibility | V1–V6 | Defined in Rev 2.1 #6. |
| Blobs | B1–B8 | Defined in Rev 2.1 #7. |
| Actor and authorship | A1–A5 | Defined in Rev 2.1 #8. |
| Jobs | J1–J10 (J4a–c, J5a–c) | Defined in Rev 2.1 and Rev 2.2 #5. |
| Extensions | E1–E11 | Defined in Rev 2. |
| Backup | C1–C10 | Defined in Rev 2, reconciled in Rev 2.1. |

#### 17.4.1 Confirmed test definitions for X2

Godwin confirmed these on 2026-11-05.

**Constraints versus tests.** These are *test definitions*. They state how
already approved constraints are proven, and they add no new constraint. The
architectural constraints stay exactly as written in section 7.1 (root
principal), section 8.2 (job-execution audit) and section 8.3 (denied-event
aggregation). If a test and a constraint ever disagree, the constraint wins and
the test is corrected.

**Milestone placement** is unchanged from section 16.2: P3a–P3d in M1;
AG1–AG4 and JA1–JA9 in M2. Any JA test that needs real River jobs may move to
M6.

**Root principal (section 7.1)**

Each test asserts that the database itself rejects the violation, through a
constraint or trigger and using raw SQL, not only the Go code.

| Test | Asserts |
|---|---|
| P3a | The root principal (`handle = 'system:migration'`) must reference itself and be of kind `system`. Inserting or seeding it with `created_by ≠ id`, or with any other kind, is rejected. |
| P3b | No non-root principal may reference itself. This **extends the existing P3 test** (Rev 2.1 #1) with the equivalence direction from Rev 2.2, rather than adding a duplicate. |
| P3c | There is exactly one root, at the fixed id `00000000-0000-7000-8000-000000000001`. A second `system:migration` row, a root at any other id, and a non-root row at the root id are all rejected. |
| P3d | After insert, updating `id`, `handle`, `kind` or `created_by` on any principal, including the root, is rejected. |

**Denied-event aggregation (section 8.3)**

| Test | Asserts |
|---|---|
| AG1 | On graceful shutdown, the aggregator flush runs as a shutdown hook after HTTP has drained, and writes every pending aggregate to `audit_events`. |
| AG2 | If the shutdown flush fails (for example, the database is unavailable), each pending aggregate is written as an ERROR log record named `audit_aggregate_unflushed`, with its full fields, and no aggregate is silently dropped. |
| AG3 | Reaching 10,000 distinct aggregation keys triggers an early flush before the 60-second window closes. |
| AG4 | After a hard crash (SIGKILL), loss is bounded to at most one aggregation window of *denied and failed* events. Events from earlier, already-flushed windows are present, and succeeded events, which are transactional, are unaffected. |

**Job-execution audit (section 8.2)**

There is one test per constraint, in the order they appear in section 8.2.
Each inserts a row that violates exactly that constraint, and asserts it is
rejected.

| Test | Constraint (section 8.2) | Violation asserted to fail |
|---|---|---|
| JA1 | `(action_class = 'job_execution') = (job_id IS NOT NULL)` | A `job_execution` event without `job_id`, and a non-job event with `job_id`. |
| JA2 | `job_id`, `job_attempt` and `job_trigger` are all set or all null | Any partial combination. |
| JA3 | `job_attempt >= 1` | `job_attempt = 0`. |
| JA4 | `(action_class = 'job_execution') = (actor_principal_id = <system:worker id>)` | A job event by any other actor, and a non-job event by `system:worker`. |
| JA5 | `job_trigger = 'command'` requires `causation_event_id` and `on_behalf_of_principal_id` | A command-triggered event missing either one. |
| JA6 | `job_trigger = 'periodic'` forbids `causation_event_id` and `on_behalf_of_principal_id` | A periodic event with either one set. |
| JA7 | Causation foreign key `(causation_event_id, causation_outcome) → audit_events(id, outcome)`, with `causation_outcome = 'succeeded'` | A causation pointing to a nonexistent event, or to a `denied` or `failed` event. |
| JA8 | The causation insert trigger | Three sub-cases, each must fail: the causing event's `details.enqueued_jobs` does not contain this `job_id`; the causing event did not occur earlier; `on_behalf_of` is not inherited from the causing event. |
| JA9 | Unique index on `job_id` where `action_class = 'job_execution'` | A second `job_execution` event for the same `job_id`. |

---

## 18. Exclusions: what Phase 1 does not build (Approved)

- document extraction or OCR;
- LLM generation;
- HEALTH integration of any kind;
- Workforce integration;
- clinician review, or executing review profiles;
- customer licensing, catalogs, publication or external distribution of
  releases;
- deployment, TLS or KMS;
- a web UI;
- multi-tenancy;
- record content migrations;
- partial-success imports;
- deleting committed blobs, or retention policies;
- OIDC, S3 and the audit hash chain, unless the optional items are approved;
- real, licensed or third-party data and vocabularies;
- any predefined dataset kind or specialty.

---

## 19. Risks (Approved)

| Risk | Mitigation |
|---|---|
| A generic JSONB registry gives weaker database guarantees. | Meta-schemas, references as real foreign keys, triggers, content hashes. |
| JSON Schema plus CEL may not express some future rules. | Rulesets are a registry kind, so another engine can be added later. |
| Slot extensions cannot narrow base fields as profiles do. | Explicit extension CEL rules now; a future `profile` kind. |
| Schema compatibility is hard to get right. | A conservative checker now; semantic diff deferred (O6). |
| Large licensed vocabularies. | Concepts stored as rows; rights statements on vocabularies later. |
| The rights model may be legally too simple. | Unknown counts as deny. **Assumption:** legal review is needed before any real data. |
| Re-validating at freeze is costly. | Chunked jobs, plus skipping re-validation when the schema and rules are identical; benchmark at 100,000 records (F1). |
| Uncompressed bundles are large. | Acceptable locally. |
| Tar or gzip output could change across Go versions. | Uncompressed tar with explicit headers, golden hashes, and the bundle format version in the manifest. |
| River is pre-1.0. | Pinned, behind an interface, with a conformance suite and an exit route. |
| Stored invalid revisions could accumulate. | The release gate blocks them, and the status is filterable. |
| Denied and failed audit writes can be lost. | Documented as best-effort; succeeded events are transactional. |
| Backup snapshot coupling. | Wrapped in `studioctl`; C1 runs under concurrent load. |
| Toolchain PATH quirk; host `pg_dump` 14. | The `toolchain-check` guard; dump and restore run in the container. |
| A Go library differs from the Python schema experiment. | E1 re-proves the behaviour in Go before the resolver is built. |
| The local disk is close to full (observed during M0). | Keep around 3 GB free for tool builds. |

---

## 20. Decision register

### 20.1 Approved defaults (accepted with the plan; no objection recorded)

| # | Decision | Default |
|---|---|---|
| U1 | River MPL-2.0 licence | Approved for use. A formal licence sign-off is not recorded. |
| U2 | Storing invalid revisions | Allowed; blocked at release. |
| U3 | Audited reads | Data egress only. |
| U4 | Bundle format | Uncompressed tar; artifact bytes excluded by default. |
| U5 | Version numbering | Semver chosen by the user. |
| U6 | Batches | All-or-nothing, capped at 1,000. |
| U7 | Project creation | Any member. |
| Rev 2.1 | Deprecation reversible, retirement final; 24-hour blob grace periods; 30-day job retention; release chunks of 500; 2-minute release job timeout; job retry moved to `studioctl`. | As stated. |
| Rev 1 | Modular monolith; stdlib HTTP; sqlc; goose forward-only; argon2id; opaque tokens; code-defined roles; deny by default. | As stated. |

### 20.2 Assumptions

- **T1:** single-organisation installation.
- No web UI in Phase 1.
- Synthetic data only.
- External parties receive released datasets later and never log in.
- Legal review is needed before any real data.
- PostgreSQL 17 or later is required for MAINTAIN; satisfied by the 18.6 pin.
- Single region, internal use, run locally.
- HEALTH's future integration will authenticate as a Dataset Studio service
  account.
- Workforce does not talk to Dataset Studio directly in Phase 1.

### 20.3 Unresolved (decide before the named milestone)

| # | Item | Needed by |
|---|---|---|
| X1 | Role mapping for the Rev 2 record, revision, batch, dataset-version and freeze operations (section 14) | M12 |
| X2 | **Resolved 2026-11-05.** Definitions of tests P3a–P3d, AG1–AG4 and JA1–JA9 confirmed by Godwin; recorded in section 17.4.1. Milestone placement stays as in section 16.2 (JA tests that need real River jobs may move to M6). | — |
| X3 | Whether `JobRescueMany` can be called through River's driver for J4a | M6 |
| X4 | `blob.audit` permissions (section 6.3) | M5 |
| X5 | Whether any optional item (O3–O7, partial-success batches) enters Phase 1 | After M14 |
| X6 | Local S3 emulator for O3 | O3 only |
| X7 | Formal sign-off of the MPL-2.0 licence (U1) | Before any distribution |

---

## 21. Implementation record

### 21.1 Decisions made while implementing M0

- **Module path:** `github.com/SENEBICLABS/Senebiclabs-Dataset-Studio`, matching
  the remote's casing.
- **Ports:** PostgreSQL on `127.0.0.1:55432` and the API on `127.0.0.1:8480`,
  because 5432 and 8080 were already in use on the development machine.
- **Superuser name:** the compose superuser is `studio_bootstrap`. The
  application's `STUDIO_DATABASE_URL` stays empty until M1.
- **Exit codes:**
  - services: 0 clean stop, 1 runtime or shutdown failure, 2 invalid
    configuration;
  - `studioctl config check`: 0 valid, 1 invalid;
  - `studioctl` usage errors: 2.
- **Credential scoping:** Makefile credentials are scoped to the recipes that
  need them, and tools install into versioned directories.

### 21.2 Findings carried forward from the M0 review

| Id | Severity | Finding | Target |
|---|---|---|---|
| M0-F4 | Low | Lifecycle skips shutdown hooks after the deadline expires. | Fix before or in M2 (section 8.3). |
| M0-F5 | Low | A second SIGINT during shutdown does not force exit. | Opportunistic. |
| M0-F7 | Info | pgx reads `PG*` environment variables and `~/.pgpass` as defaults. | Document or isolate in M1. |
| M0-F11 | Nit | Unused `health.Service.CheckNames`; `Recover` writes a second header after a partial response; compose `restart: unless-stopped`; `freeAddr` port race in the e2e tests. | Opportunistic. |

---

## 22. Consolidation notes (editorial, no change in requirements)

- **Relational-integrity tests renamed.** Rev 2.1 used R1–R9 for both the River
  findings and these tests. The River findings keep R1–R9; the tests become
  RI1–RI9.
- **Freeze tests versus M0 review findings.** Rev 2.1 freeze tests F1–F8 are
  distinct from the M0 review findings, which are prefixed M0-F.
- **Superseded designs**, recorded once in their final form:
  - Rev 1 milestone numbering (M0–M11) was replaced by Rev 2's M0–M14.
  - Rev 1 project-level configuration moved to dataset level in Rev 2.
  - Rev 2's `blob_objects.state` and pending-blob trigger were replaced by
    `blob_uploads` in Rev 2.1.
  - Rev 2's single-transaction freeze was replaced by the asynchronous
    lifecycle in Rev 2.1.
  - `system:bootstrap` was removed in Rev 2.1.
  - The jobs retry endpoint moved to `studioctl` in Rev 2.1.
  - `UPDATE`-based audit collapsing was replaced by in-memory aggregation in
    Rev 2.1.
  - Backup tests C2 and C10 were reconciled to the staging-upload design.
- **Approval record.** Rev 2.2 was requested as "narrow clarifications" and
  incorporated before M0 was authorised. No separate written approval of the
  Rev 2.2 text appears in the record.
