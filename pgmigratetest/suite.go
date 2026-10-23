// Package pgmigratetest is the conformance suite for pgmigrate: the
// authoritative description of the contract's edge-case behaviour, which
// neither the doc comments nor the DB interface can be.
//
// It is a sibling package rather than a _test.go file for two forced
// reasons. Identifiers in _test.go files are not importable from outside
// their package, and this suite must be callable from another package and
// from another module — a consumer adopting pgmigrate runs it against its
// own handle, which is what makes "adopted" testable instead of asserted.
// And a non-_test.go file that imports testing would make testing a
// dependency of the production package, so every consumer's binary would
// link it and inherit its flag registrations.
//
// What the suite is parameterised over is where a database comes from. There
// is one implementation of the migration machinery and there will only ever
// be one; what varies between consumers is the handle, so the factory
// returns a pgmigrate.DB over a freshly created, empty database and the
// suite proves that handle is adequate to migrate through.
//
// The migrations the suite applies are its own — two trivial ones built at
// run time for whichever schema a subtest is using. A suite that needed the
// caller's migrations would be testing the caller.
package pgmigratetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/laenenai/pgmigrate"
)

// errReconcilerRefused is the sentinel the failing reconciler in
// ReconcilerFailureAbortsUp returns. It is declared here, in the suite,
// deliberately: the contract is that Up wraps a reconciler's error with %w
// and adds no sentinel of its own, so a caller's own error class survives to
// the call site. A reconciler that refuses a configuration — an attempt to
// change a hash modulus that is fixed at creation, say — is the caller's
// judgement to express and this module's job to deliver intact.
var errReconcilerRefused = errors.New("pgmigratetest: the reconciler refused this configuration")

// roleSuffix makes the role names this suite creates unique per process.
// Roles are cluster-wide while pgmigrate's advisory lock is per-database, so
// two test binaries sharing one TEST_DATABASE_URL server would otherwise be
// asserting privileges about each other's role.
var roleSuffix = func() string {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "fixed"
	}
	return hex.EncodeToString(raw[:])
}()

// RunMigrateSuite runs the contract every pgmigrate deployment must satisfy.
//
// newDB is called once per subtest, receiving that subtest's own *testing.T,
// and must return a handle to a freshly created, empty database — no schema,
// no version table, nothing applied. A harness that needs per-test setup and
// teardown registers it with t.Cleanup inside newDB, so every subtest gets
// isolated state.
//
// The handle must connect as a role that can create schemas, create roles
// and grant on them: that is the migration identity, and it is deliberately
// not the identity an application runs as. See pgmigrate's package doc
// comment on the two-pool model.
func RunMigrateSuite(t *testing.T, newDB func(t *testing.T) pgmigrate.DB) {
	t.Helper()

	t.Run("MigrationsAppliedOnceAndIdempotentOnRerun", func(t *testing.T) {
		db := newDB(t)
		ctx := context.Background()
		cfg := suiteConfig("widgets")

		if err := pgmigrate.Up(ctx, db, cfg); err != nil {
			t.Fatalf("first Up: %v", err)
		}

		applied, err := pgmigrate.Status(ctx, db, cfg)
		if err != nil {
			t.Fatalf("Status after the first Up: %v", err)
		}
		assertAllApplied(t, applied)

		// A second Up against an already-current database must apply
		// nothing new and still return nil. "Idempotent" here means the
		// whole of Up, not only goose: the reconcile phase runs again by
		// design, and the proof that the versioned layer did not is that
		// no AppliedAt moved.
		if err := pgmigrate.Up(ctx, db, cfg); err != nil {
			t.Fatalf("second Up: %v", err)
		}

		again, err := pgmigrate.Status(ctx, db, cfg)
		if err != nil {
			t.Fatalf("Status after the second Up: %v", err)
		}
		if len(again) != len(applied) {
			t.Fatalf("the second Up changed the migration count: %d, want %d", len(again), len(applied))
		}
		for i := range applied {
			if !again[i].AppliedAt.Equal(applied[i].AppliedAt) {
				t.Errorf("migration %d AppliedAt moved on an idempotent second Up: %v -> %v",
					applied[i].Version, applied[i].AppliedAt, again[i].AppliedAt)
			}
		}

		// Belt and braces: the DDL itself ran exactly once. CREATE INDEX
		// has no IF NOT EXISTS guard in the suite's second migration, so a
		// double-applied migration would have failed outright rather than
		// leaving two indexes — this also checks that nothing swallowed
		// such a failure.
		if n := scalar[int](ctx, t, db, `
			SELECT count(*) FROM pg_class
			WHERE relname = 'widget_by_name' AND relnamespace = $1::regnamespace
		`, "widgets"); n != 1 {
			t.Fatalf("index widgets.widget_by_name exists %d times, want exactly 1", n)
		}
	})

	t.Run("VersionTableLivesInTheCallersSchemaAndNotPublic", func(t *testing.T) {
		db := newDB(t)
		ctx := context.Background()
		cfg := suiteConfig("widgets")

		if err := pgmigrate.Up(ctx, db, cfg); err != nil {
			t.Fatalf("Up: %v", err)
		}

		if n := scalar[int](ctx, t, db, `
			SELECT count(*) FROM pg_tables WHERE schemaname = $1 AND tablename = $2
		`, "widgets", pgmigrate.DefaultVersionTable); n != 1 {
			t.Fatalf("widgets.%s exists %d times, want exactly 1", pgmigrate.DefaultVersionTable, n)
		}

		if n := scalar[int](ctx, t, db, `
			SELECT count(*) FROM pg_tables WHERE schemaname = 'public'
		`); n != 0 {
			t.Fatalf("migrating left %d tables in public: a consumer's own goose sequence lives there, "+
				"and sharing it is the collision a schema per caller exists to prevent", n)
		}

		// Nor anything else of this caller's, outside its own schema.
		strays := strings.TrimSpace(scalar[string](ctx, t, db, `
			SELECT coalesce(string_agg(schemaname || '.' || tablename, ', ' ORDER BY 1), '')
			FROM pg_tables
			WHERE schemaname NOT IN ($1, 'pg_catalog', 'information_schema')
		`, "widgets"))
		if strays != "" {
			t.Fatalf("migrating left tables outside schema widgets: %s", strays)
		}
	})

	t.Run("TwoCallersSchemasDoNotCollide", func(t *testing.T) {
		db := newDB(t)
		ctx := context.Background()
		alpha := suiteConfig("alpha")
		beta := suiteConfig("beta")

		// The whole point of deriving the key from the caller's identity:
		// two callers migrating two schemas in one database have no reason
		// to wait for each other.
		if pgmigrate.LockKey(alpha.Identity) == pgmigrate.LockKey(beta.Identity) {
			t.Fatalf("two identities produced the same lock key %d", pgmigrate.LockKey(alpha.Identity))
		}

		if err := pgmigrate.Up(ctx, db, alpha); err != nil {
			t.Fatalf("Up for alpha: %v", err)
		}
		if err := pgmigrate.Up(ctx, db, beta); err != nil {
			t.Fatalf("Up for beta: %v", err)
		}

		for _, schema := range []string{"alpha", "beta"} {
			if n := scalar[int](ctx, t, db, `
				SELECT count(*) FROM pg_tables WHERE schemaname = $1 AND tablename = $2
			`, schema, pgmigrate.DefaultVersionTable); n != 1 {
				t.Errorf("%s.%s exists %d times, want exactly 1", schema, pgmigrate.DefaultVersionTable, n)
			}
			if n := scalar[int](ctx, t, db, `
				SELECT count(*) FROM pg_tables WHERE schemaname = $1 AND tablename = 'widget'
			`, schema); n != 1 {
				t.Errorf("%s.widget exists %d times, want exactly 1", schema, n)
			}
		}

		// Each sequence is independent: alpha's version table records
		// alpha's migrations and knows nothing of beta's.
		for _, cfg := range []pgmigrate.Config{alpha, beta} {
			applied, err := pgmigrate.Status(ctx, db, cfg)
			if err != nil {
				t.Fatalf("Status for %s: %v", cfg.Schema, err)
			}
			assertAllApplied(t, applied)
			if len(applied) != migrationCount {
				t.Errorf("Status for %s reported %d migrations, want %d", cfg.Schema, len(applied), migrationCount)
			}
		}
	})

	t.Run("ConcurrentCallersDoNotDoubleApply", func(t *testing.T) {
		db := newDB(t)
		ctx := context.Background()
		cfg := suiteConfig("widgets")

		const callers = 8
		var reconciled sync.WaitGroup
		errs := make([]error, callers)
		for i := range callers {
			reconciled.Add(1)
			go func() {
				defer reconciled.Done()
				errs[i] = pgmigrate.Up(ctx, db, cfg)
			}()
		}
		reconciled.Wait()

		for i, err := range errs {
			if err != nil {
				t.Errorf("caller %d: Up returned %v", i, err)
			}
		}

		applied, err := pgmigrate.Status(ctx, db, cfg)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		assertAllApplied(t, applied)

		// One row per migration in the version table, not eight: a lock
		// that did not serialize would show up here even if every caller
		// reported success.
		if n := scalar[int](ctx, t, db, `SELECT count(*) FROM widgets.goose_db_version WHERE version_id > 0`); n != migrationCount {
			t.Fatalf("widgets.goose_db_version holds %d applied rows, want %d", n, migrationCount)
		}
		if n := scalar[int](ctx, t, db, `
			SELECT count(*) FROM pg_class
			WHERE relname = 'widget_by_name' AND relnamespace = 'widgets'::regnamespace
		`); n != 1 {
			t.Fatalf("index widgets.widget_by_name exists %d times after concurrent Up, want exactly 1", n)
		}
	})

	t.Run("ReconcilersRunInsideTheLockAndOnEveryUp", func(t *testing.T) {
		db := newDB(t)
		ctx := context.Background()

		runs := 0
		lockWasFree := false
		cfg := suiteConfig("widgets", pgmigrate.Reconciler{
			Name: "marker",
			Reconcile: func(ctx context.Context, tx pgmigrate.Tx) error {
				runs++

				// Inside the lock means another session cannot take the
				// same key. The probe uses a transaction-level try so that
				// the connection it borrows from the pool is not left
				// holding a session lock if the assertion fails.
				key := pgmigrate.LockKey(suiteIdentity("widgets"))
				if free := scalar[bool](ctx, t, db, `SELECT pg_try_advisory_xact_lock($1)`, key); free {
					lockWasFree = true
				}

				return pgmigrate.ExecIdempotent(ctx, tx,
					`CREATE TABLE IF NOT EXISTS widgets.marker (run_count int NOT NULL)`)
			},
		})

		if err := pgmigrate.Up(ctx, db, cfg); err != nil {
			t.Fatalf("first Up: %v", err)
		}
		if err := pgmigrate.Up(ctx, db, cfg); err != nil {
			t.Fatalf("second Up: %v", err)
		}
		if err := pgmigrate.Up(ctx, db, cfg); err != nil {
			t.Fatalf("third Up: %v", err)
		}

		// Every Up, not once: declarative state is asserted on every run,
		// which is the whole reason it is not a versioned migration.
		if runs != 3 {
			t.Errorf("the reconciler ran %d times across three Ups, want 3", runs)
		}
		if lockWasFree {
			t.Error("another session could take the advisory lock while a reconciler was running: " +
				"the reconcile phase is not inside the lock, so two deployments could reconcile against each other")
		}
		if n := scalar[int](ctx, t, db, `
			SELECT count(*) FROM pg_tables WHERE schemaname = 'widgets' AND tablename = 'marker'
		`); n != 1 {
			t.Errorf("the reconciler's table exists %d times, want exactly 1", n)
		}

		// And nothing recorded the reconcile phase as a version: there is
		// no "pending" state for something asserted every time.
		if n := scalar[int](ctx, t, db, `SELECT count(*) FROM widgets.goose_db_version WHERE version_id > 0`); n != migrationCount {
			t.Errorf("widgets.goose_db_version holds %d applied rows, want %d: the reconcile phase recorded a version", n, migrationCount)
		}
	})

	t.Run("ReconcilerFailureAbortsUpAndIsNeverRecordedAsApplied", func(t *testing.T) {
		db := newDB(t)
		ctx := context.Background()

		marker := pgmigrate.Reconciler{
			Name: "marker",
			Reconcile: func(ctx context.Context, tx pgmigrate.Tx) error {
				return pgmigrate.ExecIdempotent(ctx, tx,
					`CREATE TABLE IF NOT EXISTS widgets.marker (id int)`)
			},
		}
		refuse := pgmigrate.Reconciler{
			Name:      "refuse",
			Reconcile: func(context.Context, pgmigrate.Tx) error { return errReconcilerRefused },
		}

		err := pgmigrate.Up(ctx, db, suiteConfig("widgets", marker, refuse))
		if !errors.Is(err, errReconcilerRefused) {
			t.Fatalf("Up with a refusing reconciler error = %v, want it to wrap the reconciler's own sentinel", err)
		}
		if !strings.Contains(err.Error(), "refuse") {
			t.Errorf("Up's error does not name the reconciler that failed: %v", err)
		}

		// Every reconciler in one Up shares one transaction, so the
		// marker's work is rolled back with the refusal rather than left
		// behind half-reconciled.
		if n := scalar[int](ctx, t, db, `
			SELECT count(*) FROM pg_tables WHERE schemaname = 'widgets' AND tablename = 'marker'
		`); n != 0 {
			t.Errorf("a reconciler that ran before the failing one left %d tables behind: the reconcile phase is not one transaction", n)
		}

		// The reconcile phase records no version, ever — which is what
		// makes the failure recoverable: the next Up runs the reconcilers
		// again rather than finding them "applied" and skipping them.
		//
		// Worth being precise about what this does and does not claim. The
		// versioned migrations ran before the reconcilers and are recorded,
		// because they genuinely ran: goose commits each migration as it
		// applies it, and a migration that ran and was not recorded would
		// be re-applied on the next Up and fail. What must not exist is a
		// version row standing for the reconcile phase.
		if n := scalar[int](ctx, t, db, `SELECT count(*) FROM widgets.goose_db_version WHERE version_id > 0`); n != migrationCount {
			t.Fatalf("widgets.goose_db_version holds %d applied rows after a failed reconcile, want %d", n, migrationCount)
		}

		if err := pgmigrate.Up(ctx, db, suiteConfig("widgets", marker)); err != nil {
			t.Fatalf("Up after fixing the reconciler: %v", err)
		}
		if n := scalar[int](ctx, t, db, `
			SELECT count(*) FROM pg_tables WHERE schemaname = 'widgets' AND tablename = 'marker'
		`); n != 1 {
			t.Errorf("the reconciler did not re-run after a previous Up failed: marker table count = %d, want 1", n)
		}
	})

	t.Run("StatusReportsPendingBeforeUp", func(t *testing.T) {
		db := newDB(t)
		ctx := context.Background()
		cfg := suiteConfig("widgets")

		applied, err := pgmigrate.Status(ctx, db, cfg)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if len(applied) != migrationCount {
			t.Fatalf("Status reported %d migrations, want the %d in the embedded set", len(applied), migrationCount)
		}
		for _, m := range applied {
			if m.Applied {
				t.Errorf("migration %d (%s) reported applied before Up ran", m.Version, m.Name)
			}
			if !m.AppliedAt.IsZero() {
				t.Errorf("migration %d (%s) has a non-zero AppliedAt before Up ran", m.Version, m.Name)
			}
		}

		// Status creates the schema and lets goose create its bookkeeping
		// table — it cannot read a version table that does not exist — but
		// it applies nothing.
		if n := scalar[int](ctx, t, db, `
			SELECT count(*) FROM pg_tables WHERE schemaname = 'widgets' AND tablename = 'widget'
		`); n != 0 {
			t.Errorf("Status applied a migration: widgets.widget exists")
		}
	})

	t.Run("InvalidConfigMigratesNothing", func(t *testing.T) {
		db := newDB(t)
		ctx := context.Background()

		valid := suiteConfig("widgets")
		invalid := map[string]pgmigrate.Config{
			"no_schema":            {Identity: valid.Identity, Migrations: valid.Migrations},
			"no_identity":          {Schema: "widgets", Migrations: valid.Migrations},
			"no_migrations":        {Schema: "widgets", Identity: valid.Identity},
			"schema_not_plain":     {Schema: "widgets; DROP SCHEMA public", Identity: valid.Identity, Migrations: valid.Migrations},
			"schema_leading_digit": {Schema: "1widgets", Identity: valid.Identity, Migrations: valid.Migrations},
			"negative_lock_wait":   {Schema: "widgets", Identity: valid.Identity, Migrations: valid.Migrations, LockMaxRetries: -1},
			"unnamed_reconciler": {
				Schema: "widgets", Identity: valid.Identity, Migrations: valid.Migrations,
				Reconcilers: []pgmigrate.Reconciler{{Reconcile: func(context.Context, pgmigrate.Tx) error { return nil }}},
			},
			"nil_reconciler": {
				Schema: "widgets", Identity: valid.Identity, Migrations: valid.Migrations,
				Reconcilers: []pgmigrate.Reconciler{{Name: "nil"}},
			},
		}
		for name, cfg := range invalid {
			t.Run(name, func(t *testing.T) {
				if err := pgmigrate.Up(ctx, db, cfg); !errors.Is(err, pgmigrate.ErrInvalidConfig) {
					t.Errorf("Up error = %v, want ErrInvalidConfig", err)
				}
				if _, err := pgmigrate.Status(ctx, db, cfg); !errors.Is(err, pgmigrate.ErrInvalidConfig) {
					t.Errorf("Status error = %v, want ErrInvalidConfig", err)
				}
			})
		}

		// A rejected configuration must not have half-migrated anything —
		// which is why the configuration is resolved before anything
		// touches the database.
		if n := scalar[int](ctx, t, db, `SELECT count(*) FROM pg_namespace WHERE nspname = 'widgets'`); n != 0 {
			t.Fatalf("a rejected configuration created schema widgets")
		}
	})

	t.Run("DeclarativeHelpersAssertRatherThanMigrate", func(t *testing.T) {
		db := newDB(t)
		ctx := context.Background()

		role := "pgmigratetest_app_" + roleSuffix
		privileges := func(role string) pgmigrate.Reconciler {
			return pgmigrate.Reconciler{
				Name: "privileges",
				Reconcile: func(ctx context.Context, tx pgmigrate.Tx) error {
					if err := pgmigrate.EnsureRole(ctx, tx, role); err != nil {
						return err
					}
					return pgmigrate.AssertPrivileges(ctx, tx, pgmigrate.Privileges{
						Schema:      "widgets",
						Role:        role,
						SchemaUsage: true,
						Tables: []pgmigrate.TablePrivileges{{
							Table: "widget",
							Grant: []pgmigrate.Privilege{pgmigrate.PrivilegeSelect, pgmigrate.PrivilegeInsert},
							Revoke: []pgmigrate.Privilege{
								pgmigrate.PrivilegeUpdate, pgmigrate.PrivilegeDelete, pgmigrate.PrivilegeTruncate,
								pgmigrate.PrivilegeReferences, pgmigrate.PrivilegeTrigger,
							},
						}},
						RevokeFromPublic: true,
					})
				},
			}
		}

		if err := pgmigrate.Up(ctx, db, suiteConfig("widgets", privileges(role))); err != nil {
			t.Fatalf("Up: %v", err)
		}

		// EnsureRole's whole contract: the role exists, it cannot log in,
		// and it carries nothing cluster-wide that would make every grant
		// below it decorative.
		var canLogin, superuser, bypassRLS bool
		row := scalar[string](ctx, t, db, `
			SELECT rolcanlogin || ',' || rolsuper || ',' || rolbypassrls FROM pg_roles WHERE rolname = $1
		`, role)
		if _, err := fmt.Sscanf(strings.ReplaceAll(row, ",", " "), "%t %t %t", &canLogin, &superuser, &bypassRLS); err != nil {
			t.Fatalf("read role attributes of %s from %q: %v", role, row, err)
		}
		if canLogin {
			t.Errorf("role %s can log in: EnsureRole creates a grantee, not a connection identity", role)
		}
		if superuser {
			t.Errorf("role %s is a superuser, which bypasses privileges and row security unconditionally", role)
		}
		if bypassRLS {
			t.Errorf("role %s has BYPASSRLS, which makes every row-security policy decorative", role)
		}

		assertPrivileges(ctx, t, db, role, "widgets.widget", map[pgmigrate.Privilege]bool{
			pgmigrate.PrivilegeSelect: true, pgmigrate.PrivilegeInsert: true,
			pgmigrate.PrivilegeUpdate: false, pgmigrate.PrivilegeDelete: false,
			pgmigrate.PrivilegeTruncate: false, pgmigrate.PrivilegeReferences: false,
			pgmigrate.PrivilegeTrigger: false,
		})

		// Asserting rather than migrating, which is the failure this whole
		// phase exists to prevent: a privilege granted out of band — by
		// hand, or by an earlier configuration — is taken away again by the
		// next Up. A versioned migration would have recorded itself as
		// applied and left the grant standing.
		exec(ctx, t, db, `GRANT UPDATE, DELETE ON widgets.widget TO `+quoteIdent(role))
		assertPrivileges(ctx, t, db, role, "widgets.widget", map[pgmigrate.Privilege]bool{
			pgmigrate.PrivilegeUpdate: true, pgmigrate.PrivilegeDelete: true,
		})

		if err := pgmigrate.Up(ctx, db, suiteConfig("widgets", privileges(role))); err != nil {
			t.Fatalf("second Up: %v", err)
		}
		assertPrivileges(ctx, t, db, role, "widgets.widget", map[pgmigrate.Privilege]bool{
			pgmigrate.PrivilegeSelect: true, pgmigrate.PrivilegeInsert: true,
			pgmigrate.PrivilegeUpdate: false, pgmigrate.PrivilegeDelete: false,
		})

		// And a renamed role gets its own, independently correct set rather
		// than a no-op because the privilege step was already recorded as
		// applied. This is the charter's silent failure, stated as a test.
		renamed := role + "_renamed"
		if err := pgmigrate.Up(ctx, db, suiteConfig("widgets", privileges(renamed))); err != nil {
			t.Fatalf("Up with a renamed role: %v", err)
		}
		assertPrivileges(ctx, t, db, renamed, "widgets.widget", map[pgmigrate.Privilege]bool{
			pgmigrate.PrivilegeSelect: true, pgmigrate.PrivilegeInsert: true,
			pgmigrate.PrivilegeUpdate: false, pgmigrate.PrivilegeDelete: false,
		})
	})

	t.Run("WaitingForAnotherMigratorIsUnavailable", func(t *testing.T) {
		db := newDB(t)
		ctx := context.Background()

		cfg := suiteConfig("widgets")
		// One poll of one second and one retry, so the exhausted-budget
		// path happens in about a second instead of five minutes. That the
		// budget is configuration rather than a package-level test hook is
		// what makes this reachable from another module at all.
		cfg.LockPollSeconds = 1
		cfg.LockMaxRetries = 1

		// Hold the key from another session, as a second deployment
		// mid-migration would. A transaction-level lock is released by the
		// rollback below whatever happens in between.
		holder, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the lock-holding transaction: %v", err)
		}
		defer func() { _ = holder.Rollback(ctx) }()
		if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, pgmigrate.LockKey(cfg.Identity)); err != nil {
			t.Fatalf("take the advisory lock: %v", err)
		}

		if err := pgmigrate.Up(ctx, db, cfg); !errors.Is(err, pgmigrate.ErrUnavailable) {
			t.Fatalf("Up while another migrator holds the lock error = %v, want ErrUnavailable: "+
				"waiting for another deployment is the one condition here worth retrying", err)
		}
	})
}

// migrationCount is how many migrations suiteMigrations builds.
const migrationCount = 2

// suiteIdentity is the module identity the suite migrates under for a given
// schema. It is distinct per schema so that TwoCallersSchemasDoNotCollide is
// genuinely two callers, with two lock keys, rather than one caller writing
// twice.
func suiteIdentity(schema string) string {
	return "github.com/laenenai/pgmigrate/pgmigratetest/" + schema
}

// suiteConfig is the configuration the suite migrates with.
func suiteConfig(schema string, reconcilers ...pgmigrate.Reconciler) pgmigrate.Config {
	return pgmigrate.Config{
		Schema:      schema,
		Identity:    suiteIdentity(schema),
		Migrations:  suiteMigrations(schema),
		Reconcilers: reconcilers,
	}
}

// suiteMigrations builds the suite's own two migrations for schema.
//
// They are generated rather than embedded because the schema is a parameter
// — two callers in one database is one of the things being tested, and goose
// has no notion of a default schema to interpolate for us. goose does not
// checksum migration sources, so generating them is safe; what it keys on is
// the version number in the filename.
//
// The second migration's CREATE INDEX deliberately has no IF NOT EXISTS: a
// migration that got applied twice must fail loudly, so that a test for
// "applied once" is testing the lock rather than the guard.
func suiteMigrations(schema string) fs.FS {
	q := quoteIdent(schema)
	return fstest.MapFS{
		"00001_widget.sql": &fstest.MapFile{Data: []byte(
			"-- +goose Up\n" +
				"CREATE TABLE " + q + ".widget (\n" +
				"    id   bigint PRIMARY KEY,\n" +
				"    name text   NOT NULL\n" +
				");\n" +
				"\n-- +goose Down\n" +
				"DROP TABLE " + q + ".widget;\n",
		)},
		"00002_widget_index.sql": &fstest.MapFile{Data: []byte(
			"-- +goose Up\n" +
				"CREATE INDEX widget_by_name ON " + q + ".widget (name);\n" +
				"\n-- +goose Down\n" +
				"DROP INDEX " + q + ".widget_by_name;\n",
		)},
	}
}

// assertAllApplied fails t unless every migration reported is applied with a
// time to show for it.
func assertAllApplied(t *testing.T, applied []pgmigrate.Applied) {
	t.Helper()

	if len(applied) == 0 {
		t.Fatal("no migrations reported at all: the embedded set is empty")
	}
	for _, m := range applied {
		if !m.Applied {
			t.Errorf("migration %d (%s) is not applied", m.Version, m.Name)
		}
		if m.AppliedAt.IsZero() {
			t.Errorf("migration %d (%s) is applied but has a zero AppliedAt", m.Version, m.Name)
		}
	}
}

// assertPrivileges checks what role actually holds on table, privilege by
// privilege.
//
// It reads the grant table rather than trying a statement and seeing it
// fail, because that is the only way to see that a privilege is *absent*
// rather than merely unused: a test that attempted an UPDATE and saw it
// refused could not tell a revoke from a trigger.
func assertPrivileges(ctx context.Context, t *testing.T, db pgmigrate.DB, role, table string, want map[pgmigrate.Privilege]bool) {
	t.Helper()

	for privilege, expected := range want {
		held := scalar[bool](ctx, t, db, `SELECT has_table_privilege($1, $2, $3)`, role, table, string(privilege))
		if held != expected {
			t.Errorf("role %s holds %s on %s = %v, want %v", role, privilege, table, held, expected)
		}
	}
}

// scalar runs a one-row, one-column query through db and returns the value.
//
// It goes through Begin rather than wanting a Query method on pgmigrate.DB,
// because the production interface lists what the module itself uses and
// nothing more. A transaction gives the suite the full pgx surface without
// widening that.
func scalar[T any](ctx context.Context, t *testing.T, db pgmigrate.DB, query string, args ...any) T {
	t.Helper()

	var zero T

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin a transaction to read %q: %v", query, err)
		return zero
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var out T
	if err := tx.QueryRow(ctx, query, args...).Scan(&out); err != nil {
		t.Fatalf("read %q: %v", query, err)
		return zero
	}
	return out
}

// exec runs one statement through db, for a step that puts the database into
// the state a subtest wants to see corrected.
func exec(ctx context.Context, t *testing.T, db pgmigrate.DB, stmt string) {
	t.Helper()

	if _, err := db.Exec(ctx, stmt); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

// quoteIdent renders s as a double-quoted SQL identifier. The suite builds
// its own identifiers from its own constants, so this is about the generated
// SQL being well-formed rather than about safety.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
