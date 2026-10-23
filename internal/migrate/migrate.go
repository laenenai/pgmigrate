// Package migrate applies a caller's embedded schema migrations into a
// Postgres schema that caller owns, and reconciles the declarative state
// migrations cannot express. It is the only package in this module that
// imports goose; consumers see Up, Status, Applied, Config, DB and
// Reconciler through the facade.
//
// # A schema per caller, with its own version table inside it
//
// A library whose tables live in its *consumer's* database cannot use that
// consumer's migration sequence: two independent migration owners writing
// one database collide on goose's single version table. So each library
// owns a Postgres schema and keeps its version table inside it —
// Config.Schema and Config.Schema + "." + Config.VersionTable. Independent
// sequences, no shared ordering, no collision. A schema per library is also
// the unit privilege separation needs.
//
// The schema is created by Up itself, before goose runs, rather than only by
// the caller's migration 00001. goose has to be able to create its
// bookkeeping table before any migration has run, and that table lives in
// the schema — so the schema has to exist first. The alternative, which one
// of the two extracted implementations took and which this module
// deliberately does not offer, is to put the version table in public, where
// it is immediately available and immediately shared with whatever else is
// migrating that database.
//
// # What Up applies, and in which of three layers
//
// Up is three things in sequence, and which layer a piece of DDL belongs in
// is decided by one question: can a later deployment legitimately change it?
//
//  1. The fixed schema, as goose migrations (Config.Migrations). Tables,
//     the partitioning strategy, triggers, row-level security policies,
//     indexes. Versioned, applied once, never changed. These are the
//     decisions that cannot be revisited — converting a populated table to
//     a partitioned one is an outage.
//
//  2. and 3. The declarative state, reconciled on every Up
//     (Config.Reconcilers). Roles, grants, revokes, partition windows:
//     anything whose correct value is a function of the configuration this
//     deployment is running with, rather than of the order in which
//     configurations were tried.
//
// Layer 1 is this module's. Layers 2 and 3 are the caller's, and that split
// is the charter's load-bearing line: this module owns *when and how*
// reconciliation runs — inside the lock, in one transaction, after the
// versioned migrations, on every run — and knows nothing about what is being
// reconciled. The alternative is a module that accumulates every consumer's
// partitioning strategy.
//
// Reconciling rather than migrating declarative state is not a style
// preference; it was discovered. A versioned migration records itself as
// applied, so a deployment that later renames its application role has its
// privilege migration silently skipped and comes up with no privilege
// separation at all while the migrator reports everything applied. That
// failure is silent, which is why it is the module's responsibility rather
// than each consumer's to get the phase ordering right.
//
// # The pool handed to Up is a migration pool, not the runtime pool
//
// Up's DB must connect as a role that can create objects in — and ideally
// owns — Config.Schema, and that can GRANT to and REVOKE from whatever
// roles the caller's reconcilers name. That is the owner identity, and it is
// deliberately not the identity the application runs as. Runtime work uses a
// separate pool on the application role.
//
// The two are different credentials, not two states of one session. SET ROLE
// is the mechanism that looks like it would do, and it must not be used for
// this: it is reversible with RESET ROLE, so a single owner connection that
// narrows itself makes privilege separation per-session and
// dependent-on-remembering, and forgetting to set it is a silent escalation
// with no trace. Two pools make the separation structural — the runtime
// credential does not possess the mutating rights at all, so there is
// nothing to forget and nothing to reset.
//
// The migration pool is also short-lived: open it, migrate, close it.
// Migration is a startup activity, and a long-lived owner pool in the
// process is a standing escalation target for no benefit.
//
// # Concurrency
//
// Up and Status are safe to call concurrently, from goroutines or from
// separate processes against the same database. goose's migrations are
// serialized by a session-level advisory lock on LockKey(Config.Identity),
// and the reconcile transaction takes the same key as a transaction-level
// lock for its own work. Calling Up against an already-current database
// applies nothing new, re-runs every reconciler, and returns nil.
//
// # Why there is no sqlc here
//
// CONVENTIONS.md makes sqlc the default and names one exception: SQL
// composed at run time. This package is entirely that exception. Every
// statement it issues is built from configuration — a schema name, a version
// table name, a role name — or reads a system catalog, which sqlc cannot
// resolve against a schema directory that contains no pg_catalog. There is
// no fixed query here for a generator to generate, and this module has no
// migrations of its own for one to generate against.
package migrate

import (
	"context"
	"errors"
	"time"
)

// Applied describes one migration's state as goose's bookkeeping table knows
// it, for an operator-facing status command to print.
type Applied struct {
	// Version is the migration's leading version number, e.g. 1 for
	// 00001_init.sql.
	Version int64
	// Name is the migration's filename, e.g. "00001_init.sql".
	Name string
	// Applied reports whether this migration has been applied. AppliedAt is
	// the zero time when it has not.
	Applied   bool
	AppliedAt time.Time
}

// errNotImplemented is the placeholder the behaviour-bearing entry points
// return until the implementation lands. The conformance suite precedes the
// implementation, so this commit is the contract plus the suite that holds
// it, and every database-backed subtest fails against it by design.
var errNotImplemented = errors.New("pgmigrate: not implemented")

// Up applies cfg to db: the caller's embedded migrations, then the caller's
// reconcilers. See the package doc comment for why those are separate layers
// and for what db must and must not be.
//
// Up is safe to call concurrently. Calling it against an already-current
// database applies nothing new, re-runs every reconciler, and returns nil.
func Up(_ context.Context, _ DB, _ Config) error {
	return errNotImplemented
}

// Status reports the state of every migration embedded in cfg.Migrations:
// applied ones with the time they were applied, and pending ones not yet
// run. It takes the same advisory lock as Up for the duration of the call,
// so a status read reflects a consistent view rather than racing a
// concurrent Up.
//
// It reports layer 1 only. Reconcilers have no "pending" state to report:
// they are asserted on every Up, so "is the declarative state correct" is
// answered by reading the database against the running configuration, never
// by a version number. That is the whole reason they are not versioned.
//
// Status takes a Config where the charter's sketch took only a handle. It
// has to: the schema, the version table, the embedded set and the lock key
// are all the caller's now, and in the two implementations this module was
// extracted from they were compile-time constants of the caller's own
// package.
func Status(_ context.Context, _ DB, _ Config) ([]Applied, error) {
	return nil, errNotImplemented
}
