# pgmigrate

[![ci](https://github.com/laenenai/pgmigrate/actions/workflows/ci.yml/badge.svg)](https://github.com/laenenai/pgmigrate/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/laenenai/pgmigrate.svg)](https://pkg.go.dev/github.com/laenenai/pgmigrate)

Applies a library's embedded migrations into a Postgres schema that library
owns — idempotently, safely under concurrency — and reconciles the declarative
state migrations cannot express.

> **Status: design only.** This repository currently contains the charter and
> the boundary. There is no implementation yet, and no release. It is an
> extraction of two independently written implementations, pending.

## The problem it solves

A library whose tables live in its *consumer's* database cannot use the
consumer's migration sequence: two independent migration owners collide on a
single version table. So each library owns a Postgres schema and keeps its
version table inside it, and applies its own migrations from its own embedded
files.

Three things in that are easy to get wrong, and one of them fails silently:

1. **Versioned versus reconciled.** Putting privilege grants in a *versioned*
   migration is a trap: the migration records itself as applied, so a
   deployment that later renames its application role comes up with **no
   privilege separation at all while the migrator reports everything applied**.
   Declarative state — roles, grants, revokes, partitions — must be asserted on
   every run, not migrated once.
2. **The schema must exist before the migrator runs**, because the version
   table lives in the schema and the migrator needs to create it.
3. **The advisory lock key** must derive from the library's identity, so
   concurrent starts serialize and a library's lock cannot collide with its
   consumer's own migrations in the same cluster.

## Shape

`Up` and `Status`, taking a narrow database interface rather than a dependency
on any connection-management library. The caller supplies its schema name, its
embedded migration files, and its module identity.

**Reconcilers are supplied by the caller, not built in.** This module owns
*when and how* reconciliation runs — inside the lock, idempotently, after the
versioned migrations — and knows nothing about what is being reconciled. That
line is what keeps it from accumulating every consumer's partitioning strategy.

## What it does not do

Connection pooling or RLS session context; any consumer's actual migrations;
moving a tenant between databases, which is a different thing entirely; query
building.

## Documentation

- [`CHARTER.md`](CHARTER.md) — including why it is named `pgmigrate` and not `migrate`
- `api.txt` — the public surface as a golden file, diffed in CI

## License

Apache-2.0. See [`LICENSE`](LICENSE).
