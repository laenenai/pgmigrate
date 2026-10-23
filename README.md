# pgmigrate

[![ci](https://github.com/laenenai/pgmigrate/actions/workflows/ci.yml/badge.svg)](https://github.com/laenenai/pgmigrate/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/laenenai/pgmigrate.svg)](https://pkg.go.dev/github.com/laenenai/pgmigrate)

Applies a library's embedded migrations into a Postgres schema that library
owns — idempotently, safely under concurrency — and reconciles the declarative
state migrations cannot express.

> **Status: implemented, pre-release.** Extracted from the two independently
> written implementations in `audit` and `iam`. The conformance suite in
> `pgmigratetest` runs against a real Postgres; `audit` and `iam` have not
> yet been switched over to it.

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

```go
err := pgmigrate.Up(ctx, migrationPool, pgmigrate.Config{
    Schema:     "example",
    Identity:   "github.com/laenenai/example/internal/migrate",
    Migrations: migrations.FS,
    Reconcilers: []pgmigrate.Reconciler{{
        Name: "privileges",
        Reconcile: func(ctx context.Context, tx pgmigrate.Tx) error {
            if err := pgmigrate.EnsureRole(ctx, tx, appRole); err != nil {
                return err
            }
            return pgmigrate.AssertPrivileges(ctx, tx, pgmigrate.Privileges{
                Schema: "example", Role: appRole, SchemaUsage: true,
                Tables: []pgmigrate.TablePrivileges{{
                    Table:  "widget",
                    Grant:  []pgmigrate.Privilege{pgmigrate.PrivilegeSelect, pgmigrate.PrivilegeInsert},
                    Revoke: []pgmigrate.Privilege{pgmigrate.PrivilegeUpdate, pgmigrate.PrivilegeDelete},
                }},
                RevokeFromPublic: true,
            })
        },
    }},
})
```

`DB` is two methods, `Exec` and `Begin`, which `*pgxpool.Pool`, `*pgx.Conn`
and `pgx.Tx` all satisfy with no wrapper at the call site. goose additionally
needs a `database/sql` handle, which pgx's own interfaces cannot express, so a
handle that is not a `*pgxpool.Pool` implements `StdDBOpener` to say how to
produce one.

**Reconcilers are supplied by the caller, not built in.** This module owns
*when and how* reconciliation runs — inside the lock, in one transaction,
after the versioned migrations, on every `Up` — and knows nothing about what is
being reconciled. That line is what keeps it from accumulating every
consumer's partitioning strategy. `audit` reconciles hash and range partitions
and an append-only privilege set; `iam` reconciles neither, and pays nothing
for the phase. `Example_callerSuppliedReconcilers` in `example_test.go`
reproduces both of `audit`'s against this module's public surface, so the
claim that the contract is expressive enough is checked by the compiler.

The declarative helpers the module does provide are generic: `EnsureRole`
ensures a `NOLOGIN` role with no cluster-wide attributes exists,
`AssertPrivileges` asserts exactly one grant set, and `ExecIdempotent` is the
savepoint discipline every reconciler that creates anything needs.

## The migration pool is not the runtime pool

The handle passed to `Up` holds the migration rights. Runtime work uses a
separate pool on the application role. They are different credentials, not two
states of one session: `SET ROLE` is reversible with `RESET ROLE`, so one
owner connection narrowing itself makes privilege separation per-session and
dependent-on-remembering, and forgetting to set it is a silent escalation with
no trace. Two pools make it structural — the runtime credential does not
possess the mutating rights at all. Open the migration pool, migrate, close
it; migration is a startup activity, and a long-lived owner pool in the
process is a standing escalation target for no benefit.

## What it does not do

Connection pooling or RLS session context; any consumer's actual migrations;
moving a tenant between databases, which is a different thing entirely; query
building.

## Testing

`mise run test` is the inner loop: `go test ./... -short`, no containers and no
databases. `mise run test-all` runs everything, including the conformance suite
against a `pgvector/pgvector:pg18` container started by `testcontainers-go`.
Set `TEST_DATABASE_URL` to use a running server instead; each test still gets
its own uniquely named database.

A consumer runs the suite against its own handle:

```go
func TestPgmigrateConformance(t *testing.T) {
    pgmigratetest.RunMigrateSuite(t, func(t *testing.T) pgmigrate.DB {
        return myHarness.Start(t).Pool()
    })
}
```

The suite, not the `DB` interface and not the doc comments, is the
authoritative description of the contract's edge-case behaviour.

## Documentation

- [`CHARTER.md`](CHARTER.md) — including why it is named `pgmigrate` and not `migrate`
- `api.txt` — the public surface as a golden file, diffed in CI

## License

Apache-2.0. See [`LICENSE`](LICENSE).
