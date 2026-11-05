# Dataset Studio

A general-purpose platform for creating and managing versioned datasets. It is
neutral about specialty and about dataset type: dataset kinds, record schemas,
vocabularies, validation rules and review profiles are versioned registry
content, not code.

**Status:** Phase 1, milestone M0 (repository foundation). There are no
domain features yet.

- Go modular monolith with three binaries: `studio-api`, `studio-worker`, `studioctl`
- Its own PostgreSQL 18 database. It never accesses other systems' databases;
  integrations will use HTTP only.
- Local development and testing only.

See [docs/local-dev.md](docs/local-dev.md) to get started:

```sh
make up && make tools && make check
```
