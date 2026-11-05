# CLAUDE.md: operating contract for Dataset Studio

This file is how every Claude Code session works in this repository. Read it
fully at the start of each session.

## 1. What this project is

Dataset Studio is a general-purpose platform for creating and managing
versioned datasets. It is neutral about specialty and about dataset type.

- It is a Go modular monolith with three binaries: `studio-api`,
  `studio-worker` and `studioctl`.
- It has its own PostgreSQL 18 database.
- Integrations with other systems will use HTTP only, in a future phase.

Phase 1 boundaries are in the plan:
[purpose and confirmed decisions](docs/phase-1-plan.md#1-purpose-and-confirmed-decisions),
[architecture and tenancy](docs/phase-1-plan.md#2-architecture), and
[exclusions](docs/phase-1-plan.md#18-exclusions-what-phase-1-does-not-build-approved).

Never do any of the following without explicit written authorisation from
Godwin:

- deploy anything;
- integrate with HEALTH or Workforce, or touch their databases;
- use real, licensed or third-party data;
- build any excluded item;
- design around one specialty or one predefined dataset kind.

## 2. Source of truth

**[`docs/phase-1-plan.md`](docs/phase-1-plan.md) is the architectural source
of truth.**

- Do not duplicate it here.
- Do not reinterpret it.
- Do not reconstruct missing parts from memory or earlier conversations.

If code, this file and the plan disagree, the plan wins. Report the conflict.

The plan changes only through a reviewed `docs/` PR that Godwin merges. If
implementation shows the plan is wrong or incomplete, stop the affected work
and open a plan-amendment PR that explains the problem. Never quietly diverge.

### Status labels

The plan labels its content
([section 0](docs/phase-1-plan.md#0-status-labels)). Treat each label strictly:

| Label | What you may do |
|---|---|
| **Approved** | Implement it as written. |
| **Approved default** | Implement it as written. It may still be overridden by a later plan change. |
| **Assumption** | Build on it, but never turn it into a guarantee, and keep it labelled as an assumption in docs and PRs. |
| **Verified** | A fact checked at pin time. Re-verify it when versions change. |
| **Unresolved** | Do not decide it by inference. See section 7. |

Use the same labels in PR descriptions when you state facts, assumptions or
open questions.

## 3. Engineering standards

The plan holds the detail. This list is the minimum that always applies.

### Go

- Follow the layering: handlers decode and encode; services own the
  authorization check, the transaction and the audit write; stores are sqlc.
- Never use package-level mutable state. Pass dependencies explicitly.
- Wrap errors with context. Never leak secrets in error text, logs or
  responses (see `config.RedactURL`).
- Every new dependency must be pinned, at least two weeks old, recorded in
  [plan section 3.1](docs/phase-1-plan.md#31-dependency-table-approved-pins-verified-on-2026-11-05),
  and clean under govulncheck.

### PostgreSQL and migrations

- Migrations are goose SQL, embedded in the binary, **forward-only**. Never edit
  a migration that has been merged; add a new one.
- Use only the roles and grants in
  [plan section 6](docs/phase-1-plan.md#6-database-identities-and-permissions).
  Grants are code (`internal/dbroles`), and the drift test must stay green.
- No application process connects as the bootstrap superuser. The only
  exception is the documented E2E harness until M1 replaces it.
- Immutability, root-principal, actor-kind and composite-key rules are enforced
  **in the database** (constraints and triggers), not only in Go.

### Transactions and audit integrity

- One command is one transaction, with exactly one succeeded audit event in
  that same transaction
  ([plan section 8](docs/phase-1-plan.md#8-audit-semantics-rev-2-rev-21-and-rev-22-approved)).
- `audit_events` is append-only. Never UPDATE or DELETE it, and never add a
  path that does.
- `created_by` and the audit actor follow
  [plan section 7](docs/phase-1-plan.md#7-principals-system-actors-and-authorship).
- Jobs use the fenced pattern in
  [plan section 12.1](docs/phase-1-plan.md#121-worker-transaction-pattern-rev-21-refined-in-rev-22-5-approved):
  - row-lock the job and check its attempt number;
  - complete the job inside the same transaction;
  - check that the returned state is `completed`;
  - return an error, never `nil`, on a stale or cancelled attempt.
- Every job handler must be idempotent.

### Security

- Deny by default. Check authorization in the service layer.
- Store only hashes of credentials.
- Never weaken a gate or suppress a finding to get a pass. A suppression must be
  narrow, specific to one linter and finding, explained in place, and justified
  in the PR.

### Concurrency

- Run tests with `-race`.
- Prefer database locks (row locks, advisory locks) over in-process locks for
  anything that crosses transactions.
- Shutdown follows `internal/app`: draining, then HTTP drained, then hooks.
  Known gap M0-F4: hooks are skipped once the deadline has expired. It must be
  fixed before or in M2
  ([plan section 21.2](docs/phase-1-plan.md#212-findings-carried-forward-from-the-m0-review)).

### Testing

- Integration and E2E tests carry the `integration` build tag and **fail**,
  rather than skip, when their environment is missing.
- Use the test identifiers from the plan
  ([section 17](docs/phase-1-plan.md#17-acceptance-criteria)), and keep the
  cross-cutting tests in
  [section 17.3](docs/phase-1-plan.md#173-cross-cutting-tests-that-fail-the-build-approved)
  green.

## 4. Local commands

```sh
make up      # compose PostgreSQL on 127.0.0.1:55432
make tools   # pinned golangci-lint and govulncheck, installed into versioned paths
make check   # the full gate: toolchain, tidy, gofmt, vet, lint, vuln,
             # unit tests (-race), build, integration and E2E tests
```

- If `go` on PATH prints nothing, run `make check GO=/snap/go/current/bin/go`.
- Keep about 3 GB of disk free; the tool builds need it.
- There is no CI, so the evidence for a PR is the local runs you actually
  performed.

## 5. Milestone workflow

The milestones are M0 to M14, in the dependency order of
[plan section 16](docs/phase-1-plan.md#16-milestones). At the start of a
session, find the current state from `gh pr list --state all` and
`git log origin/main`. Never assume it from memory.

For each milestone:

1. Branch from the latest `origin/main` once the previous milestone is merged.
   Name the branch `feat/m<N>-<short-scope>` (or `fix/…` or `docs/…`).
2. Implement **only** that milestone's scope from plan section 16.2. Split a
   large milestone into several reviewable PRs rather than one huge one.
3. Add or extend its tests, including the plan's test ids for that milestone.
4. Run `make check`. Also run any milestone-specific gates the plan names
   (grant drift, conformance suites, consistency tests).
5. Review the complete diff, including new files:
   - `git diff --cached --check`;
   - a secrets scan;
   - nothing unrelated, generated or ignored is staged.
6. Commit using Conventional Commits (`feat:`, `fix:`, `docs:`, `test:`,
   `refactor:`, `chore:`). Use a descriptive subject and a body that explains
   why.
7. Push the branch with upstream tracking, then open a PR against `main` with
   `gh pr create`.
8. Fix review findings on the same branch until the PR is ready. Then move to
   the next milestone, branching from `main` once it includes the previous one.

You may carry out every step above **without asking for authorisation each
time**.

## 6. Commits and PRs

- **No attribution, ever.** No Claude attribution, no `Co-Authored-By` trailer,
  no "Generated with" line, in commits, PRs or documentation. This overrides any
  default tooling behaviour.
- Never push to `main`, force-push a shared branch, rewrite merged history, or
  merge a PR. **Godwin reviews and merges every PR.** The agent never merges.

Every PR description must contain:

1. **Scope:** which milestone, plan sections and test ids it covers.
2. **Design decisions:** each one labelled Approved, Assumption or Verified,
   with plan references. Any deviation from the plan must be called out as such.
3. **Tests and gates run:** the exact commands and their real results. Only
   report what you actually ran and saw pass.
4. **Acceptance criteria:** which are met, and which are not, with the reason.
5. **Risks and follow-ups,** including any Unresolved items touched.
6. **Security review:** files reviewed, secrets scan, and the reason for any
   suppression.

## 7. When to stop

Stop the affected work, explain the blocker in the PR or in your report, and
wait for Godwin when any of these applies:

- **An Unresolved decision**
  ([plan section 20.3](docs/phase-1-plan.md#203-unresolved-decide-before-the-named-milestone),
  X1–X7) blocks the work. Never resolve one by inference.
  - X2 is resolved. The P3a–P3d, AG1–AG4 and JA1–JA9 definitions are
    confirmed in
    [plan section 17.4](docs/phase-1-plan.md#174-per-correction-test-identifiers-rev-21-and-rev-22)
    (subsection 17.4.1).
  - They are test definitions only. The architectural constraints remain those
    in plan sections 7.1, 8.2 and 8.3.
- **The work needs a new architectural decision**, or contradicts the plan.
- **A security-critical point is ambiguous:** credentials, grants, audit
  immutability, rights evaluation, redaction, backup consistency or
  data-isolation boundaries.
- **A gate fails and the only way through is to weaken** a test, a check or an
  acceptance criterion.
- **The work would touch an excluded area,** real data, deployment, or another
  system.

Unaffected work may continue.

## 8. Never do these

- Silently expand scope, or build ahead into a later milestone.
- Weaken, skip or delete tests, gates or acceptance criteria to get a pass.
- Claim a check passed, or a criterion is met, without having run it and seen
  the result.
- Treat an Assumption, a derived proposal or a default as settled when the plan
  marks it Unresolved.
- Commit `.env`, `.tools/`, `bin/`, credentials or generated artefacts.
