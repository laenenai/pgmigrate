// Package testdb is the harness every database-backed test in this module
// runs on: Start gives a test its own uniquely-named, isolated Postgres
// database, so tests run in parallel with no shared state and nothing to
// truncate between them.
//
// # This is staged duplication, not divergence
//
// The shared harness belongs in tenancy, because tenant scoping and
// row-level security are what it exists to test. tenancy does not exist
// yet, and cas/internal/testdb and audit/internal/testdb are each keeping
// their own copy until the post-alpha swap. This package is the next copy of
// the same agreed pattern — same image, same per-test database, same
// TEST_DATABASE_URL override, same testing.Short() gate — and it converges
// on tenancy's harness the moment that module lands. It is deliberately
// shaped to make that swap mechanical: Start's signature is the whole of the
// contract.
//
// It differs from the other copies in one way, and only because this module
// differs: there is no default migrator, because this module has no
// migrations of its own. Start hands back an empty database, which is
// exactly what a test of a migrator needs.
//
// Duplicating test code for one release is a cost worth paying to avoid
// creating a module before a second consumer needs it. Duplicating
// production code would not be.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// postgresImage is the container image used when TEST_DATABASE_URL is not
// set. It is pgvector/pgvector:pg18, not stock postgres:18 — vector columns
// exist elsewhere in this system and the stock image lacks the extension, so
// a harness built on it passes locally and fails where it matters. This
// module has no vector columns of its own; matching the image is about every
// module's tests running on the same Postgres as the deployment.
const postgresImage = "pgvector/pgvector:pg18"

// adminConnectTimeout bounds every administrative connection Start makes: to
// create or drop a database, and to open the per-test pool.
const adminConnectTimeout = 10 * time.Second

// DB is an isolated, empty database created for a single test by Start.
type DB struct {
	pool *pgxpool.Pool
	name string
}

// Pool returns the connection pool opened against this test's database. It
// connects as the container's superuser, which is the migration identity
// this module's own tests need: Up must be able to create a schema, create
// roles and grant on them.
//
// A test asserting anything about what a *non*-owner may do has to switch
// roles explicitly, because a superuser bypasses privileges and row-level
// security unconditionally, and a test that did not switch would be proving
// nothing.
func (d *DB) Pool() *pgxpool.Pool { return d.pool }

// Name is this test's database name, for an error message that has to say
// which database it was talking about.
func (d *DB) Name() string { return d.name }

var (
	containerOnce sync.Once
	containerDSN  string
	containerErr  error
)

// Start creates a uniquely-named, empty Postgres database and registers a
// t.Cleanup that closes the pool and drops it. Each call gets its own
// database, so tests using Start may run in parallel without interfering
// with one another; there is no shared database and no Truncate step because
// there is nothing shared to reset.
//
// When TEST_DATABASE_URL is set, Start connects to that server and still
// creates a unique database on it rather than reusing a shared one.
// Otherwise it starts a pgvector/pgvector:pg18 container via
// testcontainers-go once per test binary run (not once per test) and shares
// that single container across every call to Start in the run.
//
// Start skips the test under testing.Short() — this is the module's one
// chokepoint for that convention, so every database test inherits it
// without its author needing a gate of their own. -short wins even when
// TEST_DATABASE_URL is set: a reachable server is not a reason to run, since
// the convention is "no databases in the inner loop", period. The skip
// happens before any container or database work, so a skipped test pays no
// setup cost.
func Start(t *testing.T) *DB {
	t.Helper()

	if testing.Short() {
		t.Skip("testdb: skipping database-backed test in -short mode; run `mise run test-all` for the full suite")
	}

	ctx := context.Background()
	serverDSN := adminDSN(ctx, t)
	dbName := newDatabaseName(t)

	adminConn, err := pgx.Connect(ctx, serverDSN)
	if err != nil {
		t.Fatalf("testdb: connect to admin database: %v", err)
	}
	defer func() { _ = adminConn.Close(ctx) }()

	if _, err := adminConn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize()); err != nil {
		t.Fatalf("testdb: create database %s: %v", dbName, err)
	}

	// The drop is registered now, immediately after the database exists,
	// rather than after the pool is opened below: opening can fail via
	// t.Fatalf, which never returns (it calls runtime.Goexit), so a Cleanup
	// registered after it would simply never run on that path — orphaning
	// the database permanently, which is especially damaging against a
	// long-lived TEST_DATABASE_URL server where nothing else ever drops it.
	// pool is filled in below; this closure closes over the variable itself,
	// so at test end it sees whatever pool holds by then, and closes it
	// first so the drop does not find the database still in use.
	var pool *pgxpool.Pool
	t.Cleanup(func() {
		if pool != nil {
			pool.Close()
		}
		dropDatabase(serverDSN, dbName, t)
	})

	testDSN, err := withDatabaseName(serverDSN, dbName)
	if err != nil {
		t.Fatalf("testdb: build dsn for database %s: %v", dbName, err)
	}

	poolCfg, err := pgxpool.ParseConfig(testDSN)
	if err != nil {
		t.Fatalf("testdb: parse dsn for database %s: %v", dbName, err)
	}
	poolCfg.ConnConfig.ConnectTimeout = adminConnectTimeout

	pool, err = pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("testdb: open pool for database %s: %v", dbName, err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("testdb: ping database %s: %v", dbName, err)
	}

	return &DB{pool: pool, name: dbName}
}

// dropDatabase connects to serverDSN and drops name, forcibly disconnecting
// any session still attached (Start's own pool was just closed, but
// pgxpool's close is not guaranteed to have torn down every socket on the
// server side yet). It logs rather than fails the test: best-effort cleanup
// is preferable to masking the test's real result.
func dropDatabase(serverDSN, name string, t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), adminConnectTimeout)
	defer cancel()

	conn, err := pgx.Connect(ctx, serverDSN)
	if err != nil {
		t.Logf("testdb: cleanup: connect to drop database %s: %v", name, err)
		return
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
		t.Logf("testdb: cleanup: drop database %s: %v", name, err)
	}
}

// adminDSN returns the DSN of a Postgres server to create test databases
// on: TEST_DATABASE_URL when set, otherwise a container started once per
// test binary run via testcontainers-go.
func adminDSN(ctx context.Context, t *testing.T) string {
	t.Helper()

	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		return dsn
	}

	containerOnce.Do(func() {
		containerDSN, containerErr = startContainer(ctx)
	})
	if containerErr != nil {
		t.Fatalf("testdb: start postgres container: %v", containerErr)
	}
	return containerDSN
}

func startContainer(ctx context.Context) (string, error) {
	const (
		user     = "postgres"
		password = "postgres"
		database = "postgres"
	)

	container, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase(database),
		tcpostgres.WithUsername(user),
		tcpostgres.WithPassword(password),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return "", fmt.Errorf("run postgres container: %w", err)
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return "", fmt.Errorf("container connection string: %w", err)
	}
	return dsn, nil
}

// newDatabaseName returns a database name unique enough to run in parallel
// across the whole test binary: 80 bits of randomness rendered as lowercase
// hex, since Postgres folds unquoted identifiers to lowercase and this
// harness has no reason to fight that.
//
// crypto/rand rather than a ULID library because this module's dependency
// list is the shortest in the set and a unique string is not worth adding to
// it; crypto/rand.Read cannot fail in Go 1.24 and later, and the error is
// checked anyway so that a future change of mind about that is a test
// failure rather than a silent collision.
func newDatabaseName(t *testing.T) string {
	t.Helper()

	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("testdb: mint a unique database name: %v", err)
	}
	return "testdb_" + hex.EncodeToString(raw[:])
}

// withDatabaseName returns dsn with its path replaced by name, so the
// returned DSN points at the newly created per-test database on the same
// server dsn names.
func withDatabaseName(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	u.Path = "/" + name
	return u.String(), nil
}
