// This file declares the handle Up and Status work through, and the one
// bridge goose forces on it.

package migrate

import (
	"context"
	"database/sql"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the narrow Postgres handle Up and Status migrate through: the two
// methods this module uses, and nothing else.
//
// It is an interface rather than a *pgxpool.Pool so that a consumer which
// manages connections some other way — a tenancy control-plane handle, a
// single *pgx.Conn in a test, a transaction a caller already holds — can be
// migrated without this module acquiring a dependency on whatever is
// managing them. *pgxpool.Pool, *pgx.Conn and pgx.Tx all satisfy it as
// declared, with no wrapper at the call site.
//
// The methods are pgx-shaped because goose's Postgres dialect is, and
// because a module that applies DDL and hands a transaction to a
// caller-supplied reconciler has no way to be driver-agnostic that is worth
// the indirection. What this interface buys is independence from a
// *connection manager*, which is the dependency that would otherwise travel
// with it.
type DB interface {
	// Exec runs one statement. Up uses it for exactly one thing: creating
	// the schema before goose runs. See ensureSchema.
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	// Begin starts the transaction the reconcile phase runs in, which is
	// also the transaction that holds the advisory lock while it does.
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Tx is the transaction a Reconciler is handed: pgx.Tx, aliased so that the
// reconciler contract can be read without chasing an import, and so that a
// reconciler can open a savepoint with Begin (which is what makes
// ExecIdempotent possible) without this module re-declaring half of pgx.
type Tx = pgx.Tx

// StdDBOpener is the one thing goose needs that pgx's own interfaces cannot
// express: a database/sql handle. goose.NewProvider takes a *sql.DB, checks
// out exactly one *sql.Conn from it per call, and runs the session lock, the
// bookkeeping check and every migration statement on that one connection —
// which is what makes a session-scoped advisory lock actually serialize the
// work it is held around.
//
// A DB that is a *pgxpool.Pool needs no help: pgx ships
// stdlib.OpenDBFromPool for exactly this, and Up uses it. Any other DB
// implementation says how to produce a database/sql handle over the same
// database by implementing this interface. The returned release function is
// called when the migration finishes, and must not close the underlying
// handle the caller owns — stdlib.OpenDBFromPool documents that closing the
// *sql.DB it returns never closes the pool behind it, and an implementation
// of this method is expected to preserve that property.
type StdDBOpener interface {
	StdDB() (db *sql.DB, release func(), err error)
}
