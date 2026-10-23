// This file is the generic half of the reconcile phase: the contract a
// caller's declarative assertion satisfies, and the one piece of plumbing
// every such assertion turns out to need.
//
// What is deliberately absent is any example of one. The two
// implementations this module was extracted from reconcile hash and range
// partitions, and an append-only privilege set, respectively; neither moved
// here. A module that owned a partitioning strategy would own the next
// consumer's too.

package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/laenenai/pgmigrate/internal/domain"
)

// Reconciler is one caller-supplied assertion about declarative state, run
// inside the advisory lock, after the versioned migrations, on every Up.
//
// The contract a reconciler must satisfy:
//
//   - It must be idempotent. It runs on every Up, including the thousandth
//     Up against an unchanged database, and that is the point: the state of
//     the database is then a function of the configuration the deployment is
//     running with rather than of the order in which configurations were
//     tried. GRANT, REVOKE and CREATE ... IF NOT EXISTS are idempotent
//     already; see ExecIdempotent for the case they do not cover.
//
//   - It must do all its work through tx, and must not commit or roll it
//     back. Every reconciler in one Up shares one transaction, so a failure
//     anywhere leaves the database exactly as the migrations left it rather
//     than half-reconciled. Postgres makes DDL transactional, which is what
//     makes that possible.
//
//   - It may assume the versioned migrations have been applied, because they
//     have; reconcilers run after them, which is why a reconciler can name
//     a table a migration created.
//
//   - It may return its own error type or sentinel. Up wraps a reconciler's
//     error with %w and adds no sentinel of its own, so errors.Is against
//     the caller's own sentinel still matches at the call site. A reconciler
//     that refuses a configuration — an attempt to change a hash modulus
//     that is fixed at creation, say — is the caller's judgement to express
//     and this module's job to deliver intact.
type Reconciler struct {
	// Name identifies this reconciler in the error Up returns when it
	// fails. Required: an unattributable failure in a caller-supplied phase
	// is the one thing a generic machine cannot diagnose for the caller.
	Name string

	// Reconcile asserts the state. Required.
	Reconcile func(ctx context.Context, tx Tx) error
}

// ExecIdempotent runs a CREATE that is meant to be a no-op when the object
// already exists, inside a savepoint, and treats a duplicate-object error as
// success.
//
// Two things make this necessary rather than convenient, and both were found
// the hard way:
//
//   - CREATE ... IF NOT EXISTS still loses a race against a concurrent
//     identical statement, because both sessions can see "not there" before
//     either commits. The advisory lock serializes one caller's migrations
//     against another instance of the *same* caller, and roles are
//     cluster-wide while the lock is per-database, so two test databases on
//     one cluster race to create the same role. CREATE ROLE has no IF NOT
//     EXISTS form at all.
//
//   - In Postgres a failed statement aborts the whole transaction, so "try
//     it and ignore the duplicate" is only available to a caller that can
//     roll back to just before the attempt. The savepoint is the point.
//
// It is exported because it is the plumbing every reconciler that creates
// anything needs, and re-deriving the savepoint discipline in each consumer
// is how one of them comes to poison its own transaction.
func ExecIdempotent(ctx context.Context, tx Tx, stmt string) error {
	nested, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: pgmigrate: open savepoint: %w", domain.ErrUnavailable, err)
	}

	if _, err := nested.Exec(ctx, stmt); err != nil {
		_ = nested.Rollback(ctx)
		if isDuplicateObject(err) {
			return nil
		}
		return err
	}
	if err := nested.Commit(ctx); err != nil {
		return fmt.Errorf("%w: pgmigrate: release savepoint: %w", domain.ErrUnavailable, err)
	}
	return nil
}

// reconcile runs every reconciler inside one transaction holding the same
// advisory lock goose used for the versioned migrations, so the whole of Up
// is serialized against another process doing the same thing. DDL is
// transactional in Postgres, so a failure anywhere in here leaves the
// database exactly as the migrations left it rather than half-reconciled.
//
// pg_advisory_xact_lock blocks rather than polling, and is released by the
// commit or the rollback. That is the right shape here: this transaction is
// short, there is nothing useful to do while waiting, and goose's polling
// locker has already established that nobody else is mid-migration — this
// lock is held against the *next* process to arrive.
//
// A caller with no reconcilers opens no transaction and takes no lock. That
// is not an optimisation: one of the two implementations this module was
// extracted from reconciles nothing at all, and a module that made it pay
// for a phase it does not use would be the wrong shape for half its
// consumers.
func reconcile(ctx context.Context, db DB, cfg resolvedConfig) error {
	if len(cfg.reconcilers) == 0 {
		return nil
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return unavailable("begin reconciliation", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", cfg.lockKey); err != nil {
		return unavailable("take the reconciliation lock", err)
	}

	for _, r := range cfg.reconcilers {
		// The reconciler's error is wrapped with %w and nothing else: no
		// sentinel of this module's is added, so errors.Is against the
		// caller's own sentinel still matches at the call site. A
		// reconciler refusing a configuration is the caller's judgement to
		// express and this module's job to deliver intact.
		if err := r.Reconcile(ctx, tx); err != nil {
			return fmt.Errorf("pgmigrate: reconcile %q in schema %s: %w", r.Name, cfg.schema, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return unavailable("commit reconciliation", err)
	}
	return nil
}

// isDuplicateObject reports whether err is Postgres complaining that
// something we asked it to create already exists. Every CREATE this module
// issues, and every one a well-written reconciler issues, is idempotent by
// intent, and the IF NOT EXISTS form still loses a race against a concurrent
// identical statement — so these codes are success rather than failure.
func isDuplicateObject(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "42P06", // duplicate_schema
		"42P07", // duplicate_table
		"42710", // duplicate_object: a role, a policy
		"23505": // unique_violation, which is how a lost CREATE ROLE race surfaces
		return true
	default:
		return false
	}
}

// unavailable wraps err as domain.ErrUnavailable, with op naming the step
// that failed. Everything it is used for is a transaction this module opened
// against a database it had already reached, so a failure is the database
// becoming unreachable or unusable mid-run rather than a schema that does
// not fit.
func unavailable(op string, err error) error {
	return fmt.Errorf("%w: pgmigrate: %s: %w", domain.ErrUnavailable, op, err)
}
