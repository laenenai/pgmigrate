package migrate

import (
	"errors"
	"strings"
	"testing"

	"github.com/laenenai/pgmigrate/internal/domain"
)

// TestPrivilegeStatements pins the rendering, because every identifier and
// every privilege word in it reaches Postgres as SQL text — GRANT and
// REVOKE accept neither as a bound parameter — and because the order is
// load-bearing rather than incidental.
func TestPrivilegeStatements(t *testing.T) {
	t.Parallel()

	got, err := privilegeStatements(Privileges{
		Schema:      "widgets",
		Role:        "widget_app",
		SchemaUsage: true,
		Tables: []TablePrivileges{
			{
				Table:  "widget",
				Grant:  []Privilege{PrivilegeSelect, PrivilegeInsert},
				Revoke: []Privilege{PrivilegeUpdate, PrivilegeDelete, PrivilegeTruncate},
			},
			{
				Table: "widget_head",
				Grant: []Privilege{PrivilegeSelect, PrivilegeInsert, PrivilegeUpdate},
			},
		},
		RevokeFromPublic: true,
	})
	if err != nil {
		t.Fatalf("privilegeStatements: %v", err)
	}

	want := []string{
		`GRANT USAGE ON SCHEMA "widgets" TO "widget_app"`,
		`GRANT SELECT, INSERT ON "widgets"."widget" TO "widget_app"`,
		`REVOKE UPDATE, DELETE, TRUNCATE ON "widgets"."widget" FROM "widget_app"`,
		`GRANT SELECT, INSERT, UPDATE ON "widgets"."widget_head" TO "widget_app"`,
		`REVOKE ALL ON "widgets"."widget" FROM PUBLIC`,
		`REVOKE ALL ON "widgets"."widget_head" FROM PUBLIC`,
		`REVOKE ALL ON SCHEMA "widgets" FROM PUBLIC`,
	}
	if len(got) != len(want) {
		t.Fatalf("rendered %d statements, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("statement %d:\n got %s\nwant %s", i, got[i], want[i])
		}
	}
}

// A table with no revokes renders no REVOKE, and a table with neither
// renders nothing at all: an empty assertion is not an error, it is a
// caller saying "this table's privileges are somebody else's business".
func TestPrivilegeStatementsOmitsEmptyLists(t *testing.T) {
	t.Parallel()

	got, err := privilegeStatements(Privileges{
		Schema: "widgets",
		Role:   "widget_app",
		Tables: []TablePrivileges{{Table: "widget"}},
	})
	if err != nil {
		t.Fatalf("privilegeStatements: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("rendered %d statements for an empty assertion, want none:\n%s", len(got), strings.Join(got, "\n"))
	}
}

func TestPrivilegeStatementsRejects(t *testing.T) {
	t.Parallel()

	valid := Privileges{Schema: "widgets", Role: "widget_app"}

	cases := map[string]Privileges{
		"no_schema":        {Role: valid.Role},
		"no_role":          {Schema: valid.Schema},
		"schema_not_plain": {Schema: `widgets"; DROP SCHEMA public --`, Role: valid.Role},
		"role_not_plain":   {Schema: valid.Schema, Role: `app" WITH SUPERUSER --`},
		"table_not_plain": {
			Schema: valid.Schema, Role: valid.Role,
			Tables: []TablePrivileges{{Table: `widget"; DROP TABLE x --`, Grant: []Privilege{PrivilegeSelect}}},
		},
		// The closed set of privileges is the point: a privilege word is
		// interpolated, so an arbitrary one would be arbitrary SQL in a
		// position nothing can quote.
		"privilege_not_in_the_closed_set": {
			Schema: valid.Schema, Role: valid.Role,
			Tables: []TablePrivileges{{Table: "widget", Grant: []Privilege{"ALL PRIVILEGES"}}},
		},
		"privilege_is_injection": {
			Schema: valid.Schema, Role: valid.Role,
			Tables: []TablePrivileges{{Table: "widget", Revoke: []Privilege{`SELECT ON pg_authid TO PUBLIC; --`}}},
		},
		"privilege_lowercase": {
			Schema: valid.Schema, Role: valid.Role,
			Tables: []TablePrivileges{{Table: "widget", Grant: []Privilege{"select"}}},
		},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := privilegeStatements(p); !errors.Is(err, domain.ErrInvalidConfig) {
				t.Fatalf("privilegeStatements error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}
