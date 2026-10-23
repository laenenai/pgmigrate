package pgmigrate_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/laenenai/pgmigrate"
)

// Example_callerSuppliedReconcilers reproduces, against this module's public
// surface and nothing else, the two reconcilers audit has today: the hash
// and range partitions of its event table, and its append-only privilege
// set. It is the evidence for the charter's load-bearing claim — that the
// reconciler contract is expressive enough for a real consumer's declarative
// state without that consumer's shape leaking in here — and the evidence is
// checked by the compiler rather than asserted in a doc comment.
//
// Nothing in it needs a database, so it has no Output comment: Go compiles
// an example without one and does not run it, which is exactly the
// guarantee wanted.
//
// Read it for what is absent. This module has no notion of a partition, a
// modulus, a month, a window, an append-only table or a writable head row.
// All of that is in the closure, where it belongs, and the only things it
// borrows are generic: ExecIdempotent's savepoint discipline, EnsureRole and
// AssertPrivileges.
func Example_callerSuppliedReconcilers() {
	// The consumer's own configuration and its own sentinel, neither of
	// which this module knows anything about.
	const (
		schema  = "audit"
		appRole = "audit_app"

		hashPartitions = 64
		monthsBehind   = 1
		monthsAhead    = 12
	)
	errInvalidConfig := errors.New("audit: invalid configuration")

	now := func() time.Time { return time.Now().UTC() }
	monthOf := func(t time.Time) time.Time {
		u := t.UTC()
		return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	}

	// Layer 2: the partitions. The modulus is fixed the first time and
	// refuses to change afterwards, because changing it redistributes every
	// tenant's rows — a data migration, not a configuration edit. The
	// refusal is returned as the consumer's own sentinel, and Up wraps it
	// with %w and no sentinel of its own, so errors.Is against
	// errInvalidConfig still matches at the call site.
	partitions := pgmigrate.Reconciler{
		Name: "partitions",
		Reconcile: func(ctx context.Context, tx pgmigrate.Tx) error {
			var existing int
			err := tx.QueryRow(ctx,
				`SELECT count(*) FROM pg_inherits WHERE inhparent = ($1 || '.event')::regclass`,
				schema,
			).Scan(&existing)
			if err != nil {
				return fmt.Errorf("count hash partitions: %w", err)
			}

			switch {
			case existing == 0:
				for i := range hashPartitions {
					stmt := fmt.Sprintf(
						`CREATE TABLE IF NOT EXISTS %s.event_h%d PARTITION OF %s.event `+
							`FOR VALUES WITH (MODULUS %d, REMAINDER %d) PARTITION BY RANGE (recorded_at)`,
						schema, i, schema, hashPartitions, i)
					if err := pgmigrate.ExecIdempotent(ctx, tx, stmt); err != nil {
						return fmt.Errorf("create hash partition %d: %w", i, err)
					}
				}
			case existing != hashPartitions:
				return fmt.Errorf("%w: event already has %d hash partitions but HashPartitions is %d",
					errInvalidConfig, existing, hashPartitions)
			}

			// The monthly window inside each hash partition, extended by
			// every Up — which is how a library-applied migration model
			// keeps the window ahead of the present.
			start := monthOf(now()).AddDate(0, -monthsBehind, 0)
			for i := range hashPartitions {
				deflt := fmt.Sprintf(
					`CREATE TABLE IF NOT EXISTS %s.event_h%d_default PARTITION OF %s.event_h%d DEFAULT`,
					schema, i, schema, i)
				if err := pgmigrate.ExecIdempotent(ctx, tx, deflt); err != nil {
					return fmt.Errorf("create default partition of hash %d: %w", i, err)
				}
				for n := range monthsBehind + monthsAhead + 1 {
					month := start.AddDate(0, n, 0)
					stmt := fmt.Sprintf(
						`CREATE TABLE IF NOT EXISTS %s.event_h%d_%s PARTITION OF %s.event_h%d `+
							`FOR VALUES FROM ('%s') TO ('%s')`,
						schema, i, month.Format("200601"), schema, i,
						month.Format("2006-01-02 15:04:05+00"),
						month.AddDate(0, 1, 0).Format("2006-01-02 15:04:05+00"))
					if err := pgmigrate.ExecIdempotent(ctx, tx, stmt); err != nil {
						return fmt.Errorf("create range partition %s of hash %d: %w", month.Format("200601"), i, err)
					}
				}
			}
			return nil
		},
	}

	// Layer 3: the privileges that are the actual append-only mechanism.
	// The role name is configuration precisely because the REVOKE names it,
	// which is why this cannot be a versioned migration: a deployment that
	// renamed the role would have the migration skipped and come up with no
	// separation at all while the migrator reported everything applied.
	privileges := pgmigrate.Reconciler{
		Name: "privileges",
		Reconcile: func(ctx context.Context, tx pgmigrate.Tx) error {
			if err := pgmigrate.EnsureRole(ctx, tx, appRole); err != nil {
				return err
			}
			return pgmigrate.AssertPrivileges(ctx, tx, pgmigrate.Privileges{
				Schema:      schema,
				Role:        appRole,
				SchemaUsage: true,
				Tables: []pgmigrate.TablePrivileges{
					{
						// The history: append and read, and nothing else.
						Table: "event",
						Grant: []pgmigrate.Privilege{pgmigrate.PrivilegeSelect, pgmigrate.PrivilegeInsert},
						Revoke: []pgmigrate.Privilege{
							pgmigrate.PrivilegeUpdate, pgmigrate.PrivilegeDelete, pgmigrate.PrivilegeTruncate,
							pgmigrate.PrivilegeReferences, pgmigrate.PrivilegeTrigger,
						},
					},
					{
						// The head row is the one writable surface: an
						// append has to move the head. DELETE stays
						// revoked, because deleting a head row would let an
						// appender restart a chain at sequence one.
						Table: "chain_head",
						Grant: []pgmigrate.Privilege{
							pgmigrate.PrivilegeSelect, pgmigrate.PrivilegeInsert, pgmigrate.PrivilegeUpdate,
						},
						Revoke: []pgmigrate.Privilege{
							pgmigrate.PrivilegeDelete, pgmigrate.PrivilegeTruncate,
							pgmigrate.PrivilegeReferences, pgmigrate.PrivilegeTrigger,
						},
					},
				},
				RevokeFromPublic: true,
			})
		},
	}

	// What the consumer's own Migrate function is, in full. The pool it
	// passes is a distinct, short-lived migration pool on the owner role:
	// opened here, closed when this returns. Runtime work uses a separate
	// pool on appRole, which does not possess these rights at all.
	_ = func(ctx context.Context, migrationPool pgmigrate.DB) error {
		return pgmigrate.Up(ctx, migrationPool, pgmigrate.Config{
			Schema:      schema,
			Identity:    "github.com/laenenai/audit/internal/migrate",
			Migrations:  nil, // the consumer's own embed.FS
			Reconcilers: []pgmigrate.Reconciler{partitions, privileges},
		})
	}
}
