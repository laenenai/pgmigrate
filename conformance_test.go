package pgmigrate_test

import (
	"testing"

	"github.com/laenenai/pgmigrate"
	"github.com/laenenai/pgmigrate/internal/testdb"
	"github.com/laenenai/pgmigrate/pgmigratetest"
)

// TestConformance runs this module's own conformance suite against a
// *pgxpool.Pool, which is the handle both consumers this module was
// extracted from already hold.
//
// It is the same function a consumer calls. That is the point of the suite
// living in a sibling package: tenancy will run it against its own
// control-plane handle, and a handle that cannot pass it is a handle that
// cannot be migrated through, which is a thing worth finding out in that
// module's own test run rather than in a deployment.
//
// The factory is called once per subtest, with that subtest's own *T, so
// every subtest gets a freshly created, empty database and the subtests are
// independent. testdb.Start registers its own cleanup and skips under
// -short, so this test needs no gate of its own.
func TestConformance(t *testing.T) {
	t.Parallel()

	pgmigratetest.RunMigrateSuite(t, func(t *testing.T) pgmigrate.DB {
		return testdb.Start(t).Pool()
	})
}
