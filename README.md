# Dataset Studio

A general-purpose platform for creating and managing versioned datasets. It is
neutral about specialty and about dataset type: dataset kinds, record schemas,
vocabularies, validation rules and review profiles are versioned registry
content, not code.

**Status:** Phase 1, milestone M1 (database foundation: roles, migrations,
grants as code, system principals). There are no domain features yet. The
architectural source of truth is [docs/phase-1-plan.md](docs/phase-1-plan.md).

- Go modular monolith with three binaries: `studio-api`, `studio-worker`, `studioctl`
- Its own PostgreSQL 18 database. It never accesses other systems' databases;
  integrations will use HTTP only.
- Local development and testing only.

See [docs/local-dev.md](docs/local-dev.md) to get started:

```sh
make up && make tools && make check
```
