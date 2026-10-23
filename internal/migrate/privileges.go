// This file holds the two declarative helpers that are generic: ensure a
// NOLOGIN role exists, and assert exactly this grant set. They are helpers
// for a caller's reconciler, not a layer of this module — nothing calls
// them unless a caller does.
//
// They were extracted from the append-only privilege set one of the two
// implementations reconciles, with everything specific to it dropped: it
// named its own two tables, decided that one of them is the single writable
// surface and that the other must never be deletable from, and left its leaf
// partitions unreachable on purpose. Every one of those is a statement about
// an audit log, and none of them belongs here. What is left is the shape:
// a role, a schema, and per table the privileges it holds and the ones it
// must not.
//
// # Asserting, not migrating
//
// The whole value of these being helpers for the reconcile phase rather
// than something a caller writes into a versioned migration is that a
// versioned migration records itself as applied. A deployment that later
// renames its application role would then have its privilege migration
// skipped and come up with no privilege separation at all, while the
// migrator reported everything applied. GRANT and REVOKE are idempotent, so
// there is nothing to gain from running them once and nothing to lose by
// running them every time.
//
// A REVOKE of a privilege that was never granted is not redundant, either.
// It is what makes the intent explicit, what overrides a privilege granted
// by hand or by an earlier configuration, and what a deployment's
// enforcement check reads back to decide whether it has any separation at
// all.

package migrate

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/laenenai/pgmigrate/internal/domain"
)

// Privilege is one Postgres table privilege. The set is closed on purpose:
// a privilege word reaches Postgres as SQL text, because GRANT cannot take
// one as a bound parameter, so the only values that can appear there are
// the ones named here.
type Privilege string

// The table privileges Postgres defines. MAINTAIN (Postgres 17 and later)
// is deliberately absent: nothing has needed it, and a closed set earns its
// keep by being added to on purpose.
const (
	PrivilegeSelect     Privilege = "SELECT"
	PrivilegeInsert     Privilege = "INSERT"
	PrivilegeUpdate     Privilege = "UPDATE"
	PrivilegeDelete     Privilege = "DELETE"
	PrivilegeTruncate   Privilege = "TRUNCATE"
	PrivilegeReferences Privilege = "REFERENCES"
	PrivilegeTrigger    Privilege = "TRIGGER"
)

// TablePrivileges is the complete assertion for one table: what the role
// holds, and what it must not.
type TablePrivileges struct {
	// Table is the table's unqualified name, inside Privileges.Schema. It
	// must be a plain identifier.
	Table string
	// Grant is granted to the role. Revoke is revoked from it. A privilege
	// in neither list is left alone, which is the one place this assertion
	// is not total — Postgres offers no "revoke everything except these"
	// that does not also revoke what was just granted, so a caller that
	// wants a privilege definitely absent names it in Revoke.
	Grant  []Privilege
	Revoke []Privilege
}

// Privileges is a complete, declarative statement of what one role may do
// in one schema, for AssertPrivileges to assert on every Up.
//
// It is one value in one place so that "what can this role do here" is
// answerable by reading a literal rather than by auditing a schema.
type Privileges struct {
	// Schema is the schema the tables live in. Required, and a plain
	// identifier.
	Schema string
	// Role is the role the privileges are asserted for. Required, and a
	// plain identifier — see IsPlainIdentifier for why this one in
	// particular is checked twice.
	Role string
	// SchemaUsage grants USAGE on Schema to Role. Without it every table
	// privilege below is unreachable, so it is the usual answer; a caller
	// asserting privileges in a schema some other grant already covers
	// leaves it false.
	SchemaUsage bool
	// Tables are the per-table assertions, applied in order.
	Tables []TablePrivileges
	// RevokeFromPublic additionally revokes ALL on the schema and on every
	// named table from PUBLIC.
	//
	// PUBLIC holds no table privileges by default, but "by default" is a
	// property of how a table was created rather than a guarantee about how
	// it is now, and a privilege held by PUBLIC would make every revoke
	// above it decorative.
	RevokeFromPublic bool
}

// EnsureRole creates role if it does not already exist, as NOLOGIN and with
// nothing cluster-wide: no superuser, no createdb, no createrole, no
// inheritance, and explicitly NOBYPASSRLS — a role that bypasses row-level
// security would make a schema's policies decorative.
//
// NOLOGIN because the role this creates is a grantee, not a connection
// identity: it exists so that privileges can be asserted against a name.
// A deployment that connects as it — which is the two-pool model this
// module documents, and the one it recommends — gives it LOGIN and a
// credential out of band, through whatever manages its secrets. Nothing
// here depends on which, and nothing here should: a migration that could
// mint a login credential would be a migration that could mint one for
// itself.
//
// Roles are cluster-wide rather than per-database while the advisory lock is
// per-database, so two databases on one cluster can race to create the same
// role at the same moment. A guard that checks pg_roles first still loses
// that race, so the duplicate is caught rather than prevented; see
// ExecIdempotent.
func EnsureRole(ctx context.Context, tx Tx, role string) error {
	if !IsPlainIdentifier(role) {
		return fmt.Errorf("%w: pgmigrate: role %q is not a plain identifier: letters, digits and underscore, not starting with a digit", domain.ErrInvalidConfig, role)
	}

	stmt := "CREATE ROLE " + pgx.Identifier{role}.Sanitize() +
		" NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOLOGIN NOBYPASSRLS"

	if err := ExecIdempotent(ctx, tx, stmt); err != nil {
		return fmt.Errorf("%w: pgmigrate: create role %s: %w", domain.ErrInvalidConfig, role, err)
	}
	return nil
}

// AssertPrivileges applies p: USAGE on the schema if asked, then every
// table's grants and revokes in order, then the revokes from PUBLIC. It is
// meant to be called from a Reconciler, so that it runs on every Up.
//
// It does not create the role. A caller that wants the role created calls
// EnsureRole first, in the same reconciler; keeping them separate is what
// lets a deployment whose roles are provisioned elsewhere — by its cloud
// provider's IAM integration, say — assert privileges without this module
// trying to own role creation too.
func AssertPrivileges(ctx context.Context, tx Tx, p Privileges) error {
	statements, err := privilegeStatements(p)
	if err != nil {
		return err
	}

	for _, stmt := range statements {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%w: pgmigrate: assert privileges for role %s in schema %s: %w", domain.ErrInvalidConfig, p.Role, p.Schema, err)
		}
	}
	return nil
}

// privilegeStatements renders p as the ordered list of GRANT and REVOKE
// statements that assert it, or reports why p cannot be rendered.
//
// It is one list built in one place so that "what can this role do here" is
// answerable by reading the output rather than by auditing a schema, and so
// that the rendering is unit-testable without a database — which matters,
// because every identifier and every privilege word in it reaches Postgres
// as SQL text. GRANT and REVOKE accept neither as a bound parameter.
//
// The order is deliberate: schema usage first, because without it every
// table privilege below is unreachable; then each table's grants before its
// revokes, so that a privilege named in both ends up absent rather than
// present; then PUBLIC last, because a revoke from PUBLIC can only ever
// narrow what the statements before it established.
func privilegeStatements(p Privileges) ([]string, error) {
	if !IsPlainIdentifier(p.Schema) {
		return nil, fmt.Errorf("%w: pgmigrate: Privileges.Schema %q is not a plain identifier", domain.ErrInvalidConfig, p.Schema)
	}
	if !IsPlainIdentifier(p.Role) {
		return nil, fmt.Errorf("%w: pgmigrate: Privileges.Role %q is not a plain identifier", domain.ErrInvalidConfig, p.Role)
	}

	schema := pgx.Identifier{p.Schema}.Sanitize()
	role := pgx.Identifier{p.Role}.Sanitize()

	var out []string
	if p.SchemaUsage {
		out = append(out, "GRANT USAGE ON SCHEMA "+schema+" TO "+role)
	}

	for _, table := range p.Tables {
		if !IsPlainIdentifier(table.Table) {
			return nil, fmt.Errorf("%w: pgmigrate: Privileges.Tables names %q, which is not a plain identifier", domain.ErrInvalidConfig, table.Table)
		}
		qualified := pgx.Identifier{p.Schema, table.Table}.Sanitize()

		granted, err := privilegeList(table.Grant)
		if err != nil {
			return nil, err
		}
		revoked, err := privilegeList(table.Revoke)
		if err != nil {
			return nil, err
		}

		if granted != "" {
			out = append(out, "GRANT "+granted+" ON "+qualified+" TO "+role)
		}
		if revoked != "" {
			out = append(out, "REVOKE "+revoked+" ON "+qualified+" FROM "+role)
		}
	}

	if p.RevokeFromPublic {
		for _, table := range p.Tables {
			out = append(out, "REVOKE ALL ON "+pgx.Identifier{p.Schema, table.Table}.Sanitize()+" FROM PUBLIC")
		}
		out = append(out, "REVOKE ALL ON SCHEMA "+schema+" FROM PUBLIC")
	}

	return out, nil
}

// privilegeList renders privileges as a comma-separated SQL list, rejecting
// anything outside the closed set. The check is the reason the set is
// closed: these words are interpolated, so the only values that may appear
// are the ones this package named.
func privilegeList(privileges []Privilege) (string, error) {
	if len(privileges) == 0 {
		return "", nil
	}

	words := make([]string, 0, len(privileges))
	for _, privilege := range privileges {
		if !privilege.valid() {
			return "", fmt.Errorf("%w: pgmigrate: %q is not a table privilege this module will interpolate; use the Privilege constants", domain.ErrInvalidConfig, string(privilege))
		}
		words = append(words, string(privilege))
	}
	return strings.Join(words, ", "), nil
}

// valid reports whether p is one of the constants declared above.
func (p Privilege) valid() bool {
	switch p {
	case PrivilegeSelect, PrivilegeInsert, PrivilegeUpdate,
		PrivilegeDelete, PrivilegeTruncate, PrivilegeReferences, PrivilegeTrigger:
		return true
	default:
		return false
	}
}
