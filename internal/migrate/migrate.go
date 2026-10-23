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
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/laenenai/pgmigrate/internal/domain"
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

// Up applies cfg to db: the caller's embedded migrations, then the caller's
// reconcilers. See the package doc comment for why those are separate layers
// and for what db must and must not be.
//
// Up is safe to call concurrently. Calling it against an already-current
// database applies nothing new, re-runs every reconciler, and returns nil.
func Up(ctx context.Context, db DB, cfg Config) error {
	resolved, err := cfg.resolve()
	if err != nil {
		return err
	}

	if err := ensureSchema(ctx, db, resolved); err != nil {
		return err
	}

	provider, release, err := newProvider(db, resolved)
	if err != nil {
		return err
	}
	defer release()

	if _, err := provider.Up(ctx); err != nil {
		return classify("apply migrations", err)
	}

	return reconcile(ctx, db, resolved)
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
func Status(ctx context.Context, db DB, cfg Config) ([]Applied, error) {
	resolved, err := cfg.resolve()
	if err != nil {
		return nil, err
	}

	// Status creates the schema too, for the same reason Up does: goose
	// cannot read a version table it has not been able to create, and that
	// table lives in the schema. A status read against a database this
	// caller has never migrated is a legitimate thing to ask for — it is
	// how an operator finds out that everything is pending.
	if err := ensureSchema(ctx, db, resolved); err != nil {
		return nil, err
	}

	provider, release, err := newProvider(db, resolved)
	if err != nil {
		return nil, err
	}
	defer release()

	statuses, err := provider.Status(ctx)
	if err != nil {
		return nil, classify("status", err)
	}

	applied := make([]Applied, 0, len(statuses))
	for _, s := range statuses {
		applied = append(applied, Applied{
			Version:   s.Source.Version,
			Name:      path.Base(s.Source.Path),
			Applied:   s.State == goose.StateApplied,
			AppliedAt: s.AppliedAt,
		})
	}
	return applied, nil
}

// ensureSchema creates the caller's schema if it is not there yet, because
// the version table lives inside it and goose has to create that table
// before it can run the migration that would otherwise create the schema.
//
// This is the ordering problem the charter names: the schema must exist
// before the migrator runs. The alternative is to put the version table in
// public, which is available immediately and shared immediately — not an
// option for a library that must not share a version table with its
// consumer's own goose sequence.
//
// CREATE SCHEMA IF NOT EXISTS still loses a race against a concurrent
// identical statement — both sessions can see "not there" before either
// commits, and this runs before the advisory lock is taken, because the lock
// is taken by goose on goose's own connection — so duplicate_schema is
// caught and treated as success.
func ensureSchema(ctx context.Context, db DB, cfg resolvedConfig) error {
	_, err := db.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{cfg.schema}.Sanitize())
	switch {
	case err == nil, isDuplicateObject(err):
		return nil
	default:
		return classify("create schema "+cfg.schema, err)
	}
}

// newProvider builds a goose Provider over db, configured with the
// session-level advisory lock described on LockKey, plus a release function
// that disposes of the database/sql handle it needed.
//
// goose checks out exactly one *sql.Conn for any one call to Up or Status
// and runs the session lock, the bookkeeping check and every migration
// statement on that same connection. That is what makes a session-scoped
// advisory lock actually serialize the work it is held around; taking the
// lock on a connection of our own and letting goose use a different one
// would protect a session nothing else touches.
func newProvider(db DB, cfg resolvedConfig) (*goose.Provider, func(), error) {
	sqlDB, release, err := openStdDB(db)
	if err != nil {
		return nil, nil, err
	}

	provider, err := buildProvider(sqlDB, cfg)
	if err != nil {
		release()
		return nil, nil, err
	}
	return provider, release, nil
}

func buildProvider(sqlDB *sql.DB, cfg resolvedConfig) (*goose.Provider, error) {
	locker, err := lock.NewPostgresSessionLocker(
		lock.WithLockID(cfg.lockKey),
		lock.WithLockTimeout(cfg.pollSeconds, cfg.maxRetries),
	)
	if err != nil {
		// Only reachable if resolve let through arguments goose rejects,
		// which is a defect in this module rather than a runtime condition
		// — but a configuration-shaped one either way.
		return nil, fmt.Errorf("%w: pgmigrate: build session locker: %w", domain.ErrInvalidConfig, err)
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, cfg.migrations,
		goose.WithSessionLocker(locker),
		goose.WithTableName(cfg.qualifiedVersionTable()),
	)
	if err != nil {
		// Reachable if the caller's embedded migration set is malformed or
		// empty — a defect in the caller's binary, not in the database.
		return nil, fmt.Errorf("%w: pgmigrate: build goose provider for schema %s: %w", domain.ErrInvalidConfig, cfg.schema, err)
	}
	return provider, nil
}

// classify maps an error out of goose or pgx onto this module's taxonomy, so
// nothing here returns a bare driver error across its boundary.
//
// Two failure modes are "reachable in principle, just not right now", and
// therefore domain.ErrUnavailable — worth a caller retrying:
//
//   - the database could not be reached at all, which pgx reports as a
//     *pgconn.ConnectError anywhere in the chain;
//   - the advisory lock could not be acquired within its budget, meaning
//     another process is still migrating. goose exports no sentinel for
//     this: its exhausted-retries error is the fixed, unexported string
//     "failed to acquire lock", which is what is matched below. That is
//     pinned to the goose version in go.mod, and the conformance suite
//     exercises it through real contention, so an upgrade that reworded it
//     fails a test instead of silently misclassifying a contended lock as a
//     permanent defect.
//
// Everything else means the caller's embedded schema does not fit this
// database — a rejected statement, a version mismatch between the embedded
// set and the version table — which is a deployment defect, so
// domain.ErrInvalidConfig.
//
// The underlying error stays wrapped either way. Unlike a data-plane adapter,
// nothing here has seen a row, so there is no payload to leak and an
// operator gets to read what Postgres actually said.
func classify(op string, err error) error {
	var connectErr *pgconn.ConnectError
	switch {
	case errors.As(err, &connectErr):
		return fmt.Errorf("%w: pgmigrate: %s: could not reach the database: %w", domain.ErrUnavailable, op, err)
	case strings.Contains(err.Error(), "failed to acquire lock"):
		return fmt.Errorf("%w: pgmigrate: %s: timed out waiting for another migration to finish: %w", domain.ErrUnavailable, op, err)
	default:
		return fmt.Errorf("%w: pgmigrate: %s: %w", domain.ErrInvalidConfig, op, err)
	}
}
