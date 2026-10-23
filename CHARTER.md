# Charter: `pgmigrate`

**It does this one thing:** Apply a module's own embedded migrations into a
Postgres schema that module owns, idempotently and safely under concurrency,
and reconcile the declarative state that migrations cannot express.

**It must not contain:** Connection pooling, handle lifecycle or RLS session
context (`tenancy`). Any module's actual migrations. Tenant-to-pool migration,
which is a different thing entirely (`tenancy`, CAS §14.1). Query building. Any
domain concept.

## Why it exists: two agents built it twice, identically

On 2026-10-23 the `iam` and `audit` increments were built in parallel by
independent agents that could not see each other's work. Both produced the same
machinery:

| | `iam` | `audit` |
|---|---|---|
| Session-level advisory lock around `Up` | yes | yes |
| Lock key = CRC32(IEEE) of the package import path | `3346129644` | `1367485358` |
| Poll period / max retries | `1` / `300` | `1` / `300` |
| `Up` + `Status` as the surface | yes | yes |
| Own schema with its own version table inside it | yes | yes |

Same design, same constants, the same reasoning written out twice in the doc
comments, and keys differing only because the import paths do. (Those key
values are the ones after the 2026-10-23 module-path correction; the extraction
recomputed them and pinned all four in `TestLockKey`.)

**The extraction found that `iam` contributed nothing `audit` did not already
have, which is itself the finding** — the two converge almost line for line on
the failure taxonomy, on `Applied`, on handing goose the pool so the session
lock and the migrations land on one connection, and on the `1`/`300` poll
constants. Where they differ, `iam` is worse in exactly one place, and it is
this module's reason for existing: `iam` put its version table in
`public.goose_iam_db_version`, because goose must create its bookkeeping table
before migration 00001 creates the schema. `audit` answers that by creating the
schema itself first. One of the two had to be right and they had both reasoned
it out independently.

The "roughly 370 lines" estimate above was wrong: the generic production core
came to ~1,120 lines including doc comments, plus a 690-line conformance
suite.

That is the "second consumer" rule satisfied by evidence rather than by
anticipation.

## And it has behaviour that can be silently wrong

Which is the test a module has to pass (see `CONVENTIONS.md`). Three specific
ways, each discovered rather than imagined:

1. **The versioned-versus-reconciled split.** `audit` found that putting
   privilege grants in a *versioned* migration is a trap: the migration records
   itself as applied, so a deployment that later renames its application role
   comes up with **no privilege separation at all while the migrator reports
   everything applied.** Declarative state — roles, grants, revokes, partitions
   — must be *asserted on every run*, not migrated once. Six modules
   re-deriving that will get it wrong somewhere, and the failure is silent.
2. **The schema must exist before the migrator runs**, because the version
   table lives in the schema and the migrator needs to create it. CAS sidestepped
   this by putting its version table in `public`, which is exactly what is not
   available to a module that must not share a version table with another.
3. **The advisory lock key.** Derived from the module's identity so N processes
   starting against one database serialize instead of racing, and so a module's
   lock cannot collide with its consumer's own migrations in the same cluster.
   A hand-chosen constant collides eventually.

## Shape

- `Up(ctx, db, Config) error` and **`Status(ctx, db, Config)`** — corrected
  2026-10-23. The original `Status(ctx, pool)` was inherited from `audit` and
  `iam`, where the schema, version table, embedded files and lock key were
  compile-time constants of the caller's own package. Once the caller supplies
  them, `Status` needs the same `Config` as `Up`.
- **`DB` is `Exec` + `Begin`, not a purely narrow interface, and that is
  forced.** goose structurally requires a `*sql.DB`, and the only pgx bridge to
  one takes a concrete `*pgxpool.Pool`; building a `database/sql` driver over a
  narrow interface would mean a second connection manager inside the module
  whose charter forbids owning one. So: `Exec` + `Begin` is satisfied by
  `*pgxpool.Pool`, `*pgx.Conn` and `pgx.Tx` with no wrapper, plus an optional
  `StdDBOpener` that a non-pgxpool handle — `tenancy`'s, later — implements to
  supply the one thing it owes. A `DB` that is neither is refused with a typed
  error naming the method.
- **A reconciler failure cannot un-record migrations that genuinely ran**, and
  the charter's own ordering is why: reconcilers run *after* the versioned
  migrations, and goose commits each migration as it applies it. The achievable
  contract, which is the right one: the reconcile phase records **no version of
  its own**, its whole transaction rolls back so no partial declarative state
  survives, and the next `Up` re-runs every reconciler. That is what makes the
  failure recoverable.
- **The pool handed to `Up` is a distinct, short-lived migration pool holding
  the migration rights** (owner's decision, 2026-10-23). Runtime work uses a
  separate per-tenant scoped pool on the application role, which `tenancy`
  provides. The two are different credentials, not two states of one session:
  `SET ROLE` is reversible via `RESET ROLE`, so a single owner connection
  switching roles makes privilege separation per-session and
  dependent-on-remembering, and forgetting to set is a silent escalation. Two
  pools make it structural — the runtime credential does not possess the
  mutating rights at all. Open the migration pool, migrate, close it; a
  long-lived owner pool in the process is a standing escalation target for no
  benefit, since migration is a startup activity.
- The caller supplies: its schema name, its embedded migration FS, and its
  module identity (from which the lock key is derived).
- **Reconcilers are supplied by the caller, not built in.** `audit` reconciles
  hash and range partitions and an append-only privilege set; `iam` reconciles
  neither. This module owns *when and how* reconciliation runs — inside the
  lock, idempotently, after the versioned migrations — and knows nothing about
  what is being reconciled. The declarative helpers it does provide (ensure a
  `NOLOGIN` role exists; assert exactly this grant set) are generic.
- goose is the engine, per the standing preference for industry standards and
  `tenancy`'s decision that it is goose everywhere. CAS's bespoke 499-line
  migrator is **retired**, not extracted.

## Naming

`migrate` was rejected: CAS §14.1 and `tenancy` both use "migration" for moving
a *tenant between pools*, so a module called `migrate` would be the first place
anyone looked for something it does not contain. It would also read as a fork of
`golang-migrate/migrate`. `schema` was rejected because kgs already uses the
word for its ontology and for registered authorization schemas.

The `pg` prefix here disambiguates a responsibility rather than scoping a
bucket — the distinction that kept `pg` itself from becoming this module's name.
It matches the house style for Postgres-specific code: `pglog`, `pgregistry`,
`cas/internal/pg`.

## Adoption

| Consumer | State |
|---|---|
| audit | extraction pending — `internal/migrate`, ~1,600 lines including tests, the more advanced of the two |
| iam | extraction pending — `internal/migrate`, 210 lines |
| tenancy | pending — needed for its own registry tables |
| cas | pending — its bespoke migrator is retired rather than extracted |
| acre | pending — adopts from birth |

## Provenance

Raised by the owner 2026-10-23 on seeing the duplication, after the narrower
question of whether `pglog` was shareable (it is not — it implements `audit`'s
own port with `audit`'s chain semantics, and is the module rather than a
candidate for extraction from it).
