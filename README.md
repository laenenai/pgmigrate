# pgmigrate

Applies a module's embedded migrations into a Postgres schema that module owns,
idempotently and safely under concurrency — and reconciles the declarative
state migrations cannot express.

Exists because `iam` and `audit` independently built the same machinery on the
same day, down to the same constants, and because getting the
versioned-versus-reconciled split wrong fails **silently**: the migrator
reports everything applied while privilege separation is absent.

**Not in scope:** connection pooling or RLS session context (`tenancy`), any
module's actual migrations, tenant-to-pool migration (a different thing),
query building.

See [CHARTER.md](CHARTER.md).
