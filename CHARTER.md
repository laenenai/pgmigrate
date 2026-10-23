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
| Lock key = CRC32(IEEE) of the package import path | `1000744357` | `1966699558` |
| Poll period / max retries | `1` / `300` | `1` / `300` |
| `Up` + `Status` as the surface | yes | yes |
| Own schema with its own version table inside it | yes | yes |

Same design, same constants, the same reasoning written out twice in the doc
comments, and keys differing only because the import paths do. Roughly 370
lines of generic machinery, on its way to being duplicated eight times.

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

- `Up(ctx, pool, Config) error` and `Status(ctx, pool) ([]Applied, error)`,
  taking a narrow interface rather than a dependency on `tenancy` — so a
  consumer that does not use the tenant model does not acquire it.
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
