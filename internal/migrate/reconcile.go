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

import "context"

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
func ExecIdempotent(_ context.Context, _ Tx, _ string) error {
	return errNotImplemented
}
