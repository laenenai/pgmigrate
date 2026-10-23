package migrate

import (
	"context"
	"errors"
	"testing"
	"testing/fstest"

	"github.com/laenenai/pgmigrate/internal/domain"
)

// TestLockKey pins the two keys the extracted implementations use. Both
// agents computed crc32.ChecksumIEEE over their own migration package's
// import path and hardcoded the result; this module takes the identity as
// input and computes the same thing, so adoption must not move either
// consumer's lock.
//
// The third and fourth cases record what those keys were under the
// modules' previous "laenen-ai" organisation path, which is the whole of
// the evidence for the warning on LockKey: renaming a module path moves its
// lock key, so two builds either side of a rename do not serialize against
// each other.
func TestLockKey(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		identity string
		want     int64
	}{
		"audit":               {"github.com/laenenai/audit/internal/migrate", 1367485358},
		"iam":                 {"github.com/laenenai/iam/internal/migrate", 3346129644},
		"audit_before_rename": {"github.com/laenen-ai/audit/internal/migrate", 1966699558},
		"iam_before_rename":   {"github.com/laenen-ai/iam/internal/migrate", 1000744357},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := LockKey(c.identity); got != c.want {
				t.Fatalf("LockKey(%q) = %d, want %d", c.identity, got, c.want)
			}
		})
	}

	// Two identities that differ at all must not share a lock: two callers
	// migrating two schemas in one database have no reason to wait for
	// each other.
	if LockKey("a/b") == LockKey("a/c") {
		t.Fatal("two distinct identities produced the same lock key")
	}
	// And the key must be non-negative, because a caller may well print it
	// next to what pg_locks reports.
	if LockKey("") < 0 {
		t.Fatal("LockKey returned a negative key")
	}
}

func TestConfigResolveFillsDefaults(t *testing.T) {
	t.Parallel()

	got, err := Config{
		Schema:     "widgets",
		Identity:   "example.com/widgets",
		Migrations: fstest.MapFS{},
	}.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got.versionTable != DefaultVersionTable {
		t.Errorf("versionTable = %q, want %q", got.versionTable, DefaultVersionTable)
	}
	if want := "widgets." + DefaultVersionTable; got.qualifiedVersionTable() != want {
		t.Errorf("qualifiedVersionTable() = %q, want %q: the version table must live inside the caller's schema", got.qualifiedVersionTable(), want)
	}
	if got.pollSeconds != DefaultLockPollSeconds {
		t.Errorf("pollSeconds = %d, want %d", got.pollSeconds, DefaultLockPollSeconds)
	}
	if got.maxRetries != DefaultLockMaxRetries {
		t.Errorf("maxRetries = %d, want %d", got.maxRetries, DefaultLockMaxRetries)
	}
	if got.lockKey != LockKey("example.com/widgets") {
		t.Errorf("lockKey = %d, want the key derived from Identity", got.lockKey)
	}
}

func TestConfigResolveRejects(t *testing.T) {
	t.Parallel()

	valid := Config{Schema: "widgets", Identity: "example.com/widgets", Migrations: fstest.MapFS{}}
	noop := func(context.Context, Tx) error { return nil }

	cases := map[string]Config{
		"empty":                {},
		"no_schema":            {Identity: valid.Identity, Migrations: valid.Migrations},
		"no_identity":          {Schema: valid.Schema, Migrations: valid.Migrations},
		"no_migrations":        {Schema: valid.Schema, Identity: valid.Identity},
		"schema_injection":     {Schema: `widgets"; DROP SCHEMA public`, Identity: valid.Identity, Migrations: valid.Migrations},
		"schema_leading_digit": {Schema: "1widgets", Identity: valid.Identity, Migrations: valid.Migrations},
		"schema_dotted":        {Schema: "public.widgets", Identity: valid.Identity, Migrations: valid.Migrations},
		"version_table_dotted": {Schema: valid.Schema, Identity: valid.Identity, Migrations: valid.Migrations, VersionTable: "public.v"},
		"negative_poll":        {Schema: valid.Schema, Identity: valid.Identity, Migrations: valid.Migrations, LockPollSeconds: -1},
		"negative_retries":     {Schema: valid.Schema, Identity: valid.Identity, Migrations: valid.Migrations, LockMaxRetries: -1},
		"unnamed_reconciler": {
			Schema: valid.Schema, Identity: valid.Identity, Migrations: valid.Migrations,
			Reconcilers: []Reconciler{{Reconcile: noop}},
		},
		"nil_reconcile_func": {
			Schema: valid.Schema, Identity: valid.Identity, Migrations: valid.Migrations,
			Reconcilers: []Reconciler{{Name: "named"}},
		},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := cfg.resolve(); !errors.Is(err, domain.ErrInvalidConfig) {
				t.Fatalf("resolve error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestIsPlainIdentifier(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"audit":               true,
		"a":                   true,
		"_private":            true,
		"schema_1":            true,
		"MixedCase":           true,
		"":                    false,
		"1leading":            false,
		"has space":           false,
		"has-hyphen":          false,
		"has.dot":             false,
		`has"quote`:           false,
		"drop; DROP SCHEMA x": false,
		"unicodé":             false,
	}
	for in, want := range cases {
		if got := IsPlainIdentifier(in); got != want {
			t.Errorf("IsPlainIdentifier(%q) = %v, want %v", in, got, want)
		}
	}
}
