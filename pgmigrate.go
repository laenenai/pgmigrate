// Package pgmigrate applies a library's own embedded migrations into a
// Postgres schema that library owns — idempotently and safely under
// concurrency — and reconciles the declarative state that migrations cannot
// express.
//
// It exists because two increments built it twice. On 2026-10-23 the iam and
// audit modules were implemented in parallel by independent agents that
// could not see each other's work, and both produced the same machinery: a
// session-level advisory lock around Up, a lock key that is the CRC32 (IEEE)
// checksum of the migration package's import path, the same poll period and
// retry count, the same Up-and-Status surface, and a schema per module with
// its own version table inside it. Same design, same constants, the same
// reasoning written out twice, and keys differing only because the import
// paths did.
//
// # Three things in that are easy to get wrong, and one fails silently
//
//  1. Versioned versus reconciled. Putting privilege grants in a versioned
//     migration is a trap: the migration records itself as applied, so a
//     deployment that later renames its application role comes up with no
//     privilege separation at all while the migrator reports everything
//     applied. Declarative state — roles, grants, revokes, partitions — must
//     be asserted on every run, not migrated once.
//  2. The schema must exist before the migrator runs, because the version
//     table lives in the schema and the migrator needs to create it first.
//     Up creates it.
//  3. The advisory lock key must derive from the library's identity, so that
//     N processes starting against one database serialize instead of racing,
//     and so a library's lock cannot collide with its consumer's own
//     migrations in the same cluster. A hand-chosen constant collides
//     eventually.
//
// # Reconcilers are supplied by the caller, not built in
//
// This module owns when and how reconciliation runs — inside the lock, in
// one transaction, after the versioned migrations, on every Up — and knows
// nothing about what is being reconciled. audit reconciles hash and range
// partitions and an append-only privilege set; iam reconciles neither.
// Neither of those moved in here, and that line is what keeps this module
// from accumulating every consumer's partitioning strategy.
//
// The declarative helpers it does provide are generic: EnsureRole ensures a
// NOLOGIN role exists, AssertPrivileges asserts exactly one grant set, and
// ExecIdempotent is the savepoint discipline every reconciler that creates
// anything needs.
//
// # The pool handed to Up is a migration pool
//
// It holds the migration rights. Runtime work uses a separate pool on the
// application role. The two are different credentials, not two states of one
// session: SET ROLE is reversible with RESET ROLE, so one owner connection
// narrowing itself makes privilege separation per-session and
// dependent-on-remembering, and forgetting to set it is a silent
// escalation. Two pools make it structural — the runtime credential does not
// possess the mutating rights at all. Open the migration pool, migrate,
// close it; migration is a startup activity, and a long-lived owner pool in
// the process is a standing escalation target for no benefit.
//
// # What it does not do
//
// Connection pooling, handle lifecycle or row-level-security session
// context; any consumer's actual migrations; moving a tenant between pools,
// which is a different thing entirely; query building; any domain concept.
//
// # Where the contract is written down
//
// The conformance suite in pgmigratetest, not this doc comment and not the
// DB interface, is the authoritative description of the edge-case
// behaviour: that migrations are applied once and are idempotent on re-run,
// that the version table lands in the caller's schema and not in public,
// that two callers' schemas do not collide, that concurrent callers do not
// double-apply, that reconcilers run inside the lock and on every Up, that a
// reconciler failure aborts Up without being recorded as applied, and that
// Status reports pending before Up. A consumer runs it against its own
// handle.
//
// This file carries the module's aliases, sentinel errors, exported
// constants and entry points — and no logic. Every function here is a
// one-line forward to an internal package; the reason they are here at all
// is that Go's internal rule makes anything not re-exported here
// unreachable from outside the module, and a facade that hides what the
// module is for is not a facade.
package pgmigrate

import (
	"context"

	"github.com/laenenai/pgmigrate/internal/domain"
	"github.com/laenenai/pgmigrate/internal/migrate"
)

type (
	// Config is everything a caller supplies: its schema, its module
	// identity, its embedded migration FS and its reconcilers. See
	// migrate.Config.
	Config = migrate.Config
	// Applied describes one migration's state as the version table knows
	// it. See migrate.Applied.
	Applied = migrate.Applied

	// DB is the narrow Postgres handle Up and Status migrate through, which
	// a *pgxpool.Pool satisfies without a wrapper. See migrate.DB.
	DB = migrate.DB
	// StdDBOpener is the one method a DB that is not a *pgxpool.Pool owes
	// goose. See migrate.StdDBOpener.
	StdDBOpener = migrate.StdDBOpener
	// Tx is the transaction a Reconciler runs in. See migrate.Tx.
	Tx = migrate.Tx

	// Reconciler is one caller-supplied assertion about declarative state,
	// run inside the lock on every Up. See migrate.Reconciler for the
	// contract it must satisfy.
	Reconciler = migrate.Reconciler

	// Privilege is one Postgres table privilege, from a closed set. See
	// migrate.Privilege.
	Privilege = migrate.Privilege
	// TablePrivileges is one table's complete privilege assertion. See
	// migrate.TablePrivileges.
	TablePrivileges = migrate.TablePrivileges
	// Privileges is a declarative statement of what one role may do in one
	// schema. See migrate.Privileges.
	Privileges = migrate.Privileges
)

// The module's error taxonomy: two sentinels, split by the one question a
// caller can act on — retry, or fix the deployment.
var (
	// ErrInvalidConfig means the module was wired up wrongly and retrying
	// will not help. See domain.ErrInvalidConfig.
	ErrInvalidConfig = domain.ErrInvalidConfig
	// ErrUnavailable means the database was not reachable or not usable
	// just now, and a retry is worth making. See domain.ErrUnavailable.
	ErrUnavailable = domain.ErrUnavailable
)

// The table privileges AssertPrivileges accepts.
const (
	PrivilegeSelect     = migrate.PrivilegeSelect
	PrivilegeInsert     = migrate.PrivilegeInsert
	PrivilegeUpdate     = migrate.PrivilegeUpdate
	PrivilegeDelete     = migrate.PrivilegeDelete
	PrivilegeTruncate   = migrate.PrivilegeTruncate
	PrivilegeReferences = migrate.PrivilegeReferences
	PrivilegeTrigger    = migrate.PrivilegeTrigger
)

// Defaults a Config leaves to this module.
const (
	// DefaultVersionTable is the unqualified name of the version table
	// inside the caller's schema.
	DefaultVersionTable = migrate.DefaultVersionTable
	// DefaultLockPollSeconds and DefaultLockMaxRetries bound the wait for
	// the advisory lock: both extracted implementations chose these.
	DefaultLockPollSeconds = migrate.DefaultLockPollSeconds
	DefaultLockMaxRetries  = migrate.DefaultLockMaxRetries
)

// Up applies cfg to db: the caller's embedded migrations, then the caller's
// reconcilers. See migrate.Up.
func Up(ctx context.Context, db DB, cfg Config) error {
	return migrate.Up(ctx, db, cfg)
}

// Status reports the state of every migration embedded in cfg.Migrations.
// See migrate.Status.
func Status(ctx context.Context, db DB, cfg Config) ([]Applied, error) {
	return migrate.Status(ctx, db, cfg)
}

// LockKey returns the advisory lock key derived from a module identity: the
// CRC32 (IEEE) checksum of it. See migrate.LockKey.
func LockKey(identity string) int64 { return migrate.LockKey(identity) }

// IsPlainIdentifier reports whether s is safe to use as a SQL identifier.
// See migrate.IsPlainIdentifier.
func IsPlainIdentifier(s string) bool { return migrate.IsPlainIdentifier(s) }

// EnsureRole ensures a NOLOGIN role with no cluster-wide attributes exists.
// See migrate.EnsureRole.
func EnsureRole(ctx context.Context, tx Tx, role string) error {
	return migrate.EnsureRole(ctx, tx, role)
}

// AssertPrivileges asserts exactly the grant set p describes. See
// migrate.AssertPrivileges.
func AssertPrivileges(ctx context.Context, tx Tx, p Privileges) error {
	return migrate.AssertPrivileges(ctx, tx, p)
}

// ExecIdempotent runs a CREATE inside a savepoint, treating a
// duplicate-object error as success. See migrate.ExecIdempotent.
func ExecIdempotent(ctx context.Context, tx Tx, stmt string) error {
	return migrate.ExecIdempotent(ctx, tx, stmt)
}
