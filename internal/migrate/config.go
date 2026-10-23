// This file is the whole of what a caller supplies, and the validation that
// turns it into something the rest of the package can use without checking
// anything twice.

package migrate

import (
	"fmt"
	"hash/crc32"
	"io/fs"

	"github.com/laenenai/pgmigrate/internal/domain"
)

// DefaultVersionTable is the unqualified name of the table goose records
// applied versions in. It is goose's own default name, kept rather than
// renamed per consumer: the table lives inside the caller's own schema, so
// two consumers' version tables are already distinct objects and a
// distinguishing prefix would only be noise.
//
// It is worth being explicit about what this replaces. Before this module
// existed, one of the two implementations put its version table in public
// under a hand-prefixed name (public.goose_iam_db_version) precisely to
// dodge the bootstrap ordering problem described on ensureSchema. That works
// for exactly one library and then collides with the consumer's own goose
// sequence, which is the thing a schema per module exists to prevent.
const DefaultVersionTable = "goose_db_version"

// DefaultLockPollSeconds and DefaultLockMaxRetries bound how long Up and
// Status wait for the advisory lock before giving up as
// domain.ErrUnavailable. goose's session locker polls pg_try_advisory_lock
// rather than blocking, so these are the poll interval in whole seconds (1
// is the library's minimum) and the number of retries after the first
// attempt — about five minutes.
//
// Both extracted implementations chose 1 and 300 independently, which is why
// these are the defaults rather than a number this module picked.
const (
	DefaultLockPollSeconds = 1
	DefaultLockMaxRetries  = 300
)

// Config is everything a caller supplies to Up and Status. Schema, Identity
// and Migrations are required; the rest has a default a deployment has no
// reason to change unless it has a reason.
type Config struct {
	// Schema is the Postgres schema this caller owns. Up creates it before
	// goose runs and puts the version table inside it, so nothing this
	// caller migrates lands in public and two consumers of one database
	// never share a version table.
	//
	// Required, and it must be a plain identifier: letters, digits and
	// underscore, not starting with a digit. There is no default, because a
	// default would be a schema name nobody chose.
	Schema string

	// Identity is the import path of the package that owns Migrations —
	// "github.com/laenenai/example/internal/migrate", say. The advisory lock
	// key is the CRC32 (IEEE) checksum of it; see LockKey.
	//
	// It is an input rather than a constant in this module because the
	// identity being hashed is the *caller's*, not this module's: two
	// consumers migrating two schemas in one database have no reason to wait
	// for each other, and a key derived from pgmigrate's own import path
	// would make them.
	//
	// Required. Whatever a caller passes, it must never change: changing it
	// would let an old and a new binary migrate concurrently against each
	// other during a rolling deploy, which is the race the lock exists to
	// prevent. It is a string a caller passes rather than something read
	// out of the build so that it can be held still across a module path
	// rename; see LockKey.
	Identity string

	// Migrations is the caller's embedded goose migration set — an
	// embed.FS whose root holds NNNNN_name.sql files. Required.
	//
	// Embedded rather than read from disk because the deployment model this
	// module serves is a single binary that migrates itself at startup: a
	// library whose tables live in its consumer's database cannot ask that
	// consumer to run a migration tool.
	Migrations fs.FS

	// Reconcilers are the caller's declarative assertions, run inside the
	// lock, in one transaction, after the versioned migrations, on every
	// Up. This module owns when and how they run and knows nothing about
	// what they reconcile. Optional: a caller with no declarative state
	// passes none and no reconcile transaction is opened.
	//
	// See Reconciler for the contract, and the package doc comment for why
	// declarative state must not be a versioned migration.
	Reconcilers []Reconciler

	// VersionTable is the unqualified name of goose's bookkeeping table
	// inside Schema. Zero means DefaultVersionTable. A deployment adopting
	// this module over an existing goose sequence sets it to the name that
	// sequence already uses.
	VersionTable string

	// LockPollSeconds and LockMaxRetries bound the wait for the advisory
	// lock. Zero means DefaultLockPollSeconds / DefaultLockMaxRetries; a
	// negative value is a configuration error. One second is goose's
	// minimum poll interval.
	//
	// These are configuration rather than package-level variables with a
	// test hook, which is how one of the two extracted implementations
	// reached the timeout path. A conformance suite has to be able to make
	// that path happen in a second, from another module, through the public
	// surface.
	LockPollSeconds int
	LockMaxRetries  int
}

// LockKey returns the Postgres advisory lock key derived from a module
// identity: the CRC32 (IEEE) checksum of it, widened to int64.
//
//	LockKey("github.com/laenenai/audit/internal/migrate") == 1367485358
//	LockKey("github.com/laenenai/iam/internal/migrate")   == 3346129644
//
// Both extracted implementations computed exactly this, independently, and
// hardcoded the result as a constant. The unit test for this function pins
// the two values above, so that the extraction cannot silently move either
// consumer's lock.
//
// CRC32 is not a security property here and does not need to be: the key
// only has to be stable for one identity and unlikely to equal a key some
// unrelated thing in the same cluster picked. A hand-chosen constant
// collides eventually; a checksum of the import path collides only if two
// import paths do.
//
// # A module path rename changes the key
//
// This is the one operational consequence of deriving the key from the
// import path, and it is worth knowing before a path migration rather than
// during one. Two builds of the same consumer either side of a rename hash
// two different identities, so they take two different advisory locks and do
// not serialize against each other — if both run against one database during
// a rollout, they can migrate concurrently, which is the race the lock
// exists to prevent.
//
// The two examples above are themselves evidence: these keys were 1966699558
// and 1000744357 while the modules lived under a "laenen-ai" path, and
// renaming the organisation moved both.
//
// It is still the right trade, because the alternative is a hand-chosen
// constant that collides eventually and silently. A deployment renaming its
// module path and unwilling to accept the window pins Identity to the old
// path for one release — Identity is a string a caller passes, not something
// this module reads from the build, precisely so that it can be held still
// while the import path moves.
//
// It is exported because the key is operational surface — it is what an
// operator sees in pg_locks while a deployment is migrating, and what a
// test asserts against to prove something ran inside the lock.
func LockKey(identity string) int64 {
	return int64(crc32.ChecksumIEEE([]byte(identity)))
}

// resolvedConfig is Config with every default filled in and every value
// checked, so nothing downstream of resolve has to remember which fields
// were optional.
type resolvedConfig struct {
	schema       string
	identity     string
	versionTable string
	migrations   fs.FS
	reconcilers  []Reconciler
	lockKey      int64
	pollSeconds  uint64
	maxRetries   uint64
}

// qualifiedVersionTable is the version table as goose must be told about it:
// inside the caller's schema, which is the whole cross-module decision.
func (c resolvedConfig) qualifiedVersionTable() string {
	return c.schema + "." + c.versionTable
}

// resolve validates cfg and fills in its defaults. It is called first in
// both Up and Status, before anything touches the database, so a rejected
// configuration cannot have half-migrated anything.
func (c Config) resolve() (resolvedConfig, error) {
	out := resolvedConfig{
		schema:       c.Schema,
		identity:     c.Identity,
		versionTable: c.VersionTable,
		migrations:   c.Migrations,
		reconcilers:  c.Reconcilers,
		pollSeconds:  uint64(DefaultLockPollSeconds),
		maxRetries:   uint64(DefaultLockMaxRetries),
	}

	if out.schema == "" {
		return resolvedConfig{}, fmt.Errorf("%w: pgmigrate: Schema is required: it is the schema the version table lives in, and there is no sensible default for it", domain.ErrInvalidConfig)
	}
	if !IsPlainIdentifier(out.schema) {
		return resolvedConfig{}, fmt.Errorf("%w: pgmigrate: Schema %q is not a plain identifier: letters, digits and underscore, not starting with a digit", domain.ErrInvalidConfig, out.schema)
	}

	if out.identity == "" {
		return resolvedConfig{}, fmt.Errorf("%w: pgmigrate: Identity is required: the advisory lock key is derived from it, and a module with no identity would share a key with every other", domain.ErrInvalidConfig)
	}
	out.lockKey = LockKey(out.identity)

	if out.migrations == nil {
		return resolvedConfig{}, fmt.Errorf("%w: pgmigrate: Migrations is nil: there is nothing to apply", domain.ErrInvalidConfig)
	}

	if out.versionTable == "" {
		out.versionTable = DefaultVersionTable
	}
	if !IsPlainIdentifier(out.versionTable) {
		return resolvedConfig{}, fmt.Errorf("%w: pgmigrate: VersionTable %q is not a plain identifier", domain.ErrInvalidConfig, out.versionTable)
	}

	for i, r := range out.reconcilers {
		if r.Name == "" {
			return resolvedConfig{}, fmt.Errorf("%w: pgmigrate: Reconcilers[%d] has no Name: the name is what an error names, and an unnamed failure in a caller-supplied phase is unattributable", domain.ErrInvalidConfig, i)
		}
		if r.Reconcile == nil {
			return resolvedConfig{}, fmt.Errorf("%w: pgmigrate: Reconcilers[%d] (%s) has a nil Reconcile function", domain.ErrInvalidConfig, i, r.Name)
		}
	}

	if c.LockPollSeconds < 0 || c.LockMaxRetries < 0 {
		return resolvedConfig{}, fmt.Errorf("%w: pgmigrate: LockPollSeconds (%d) and LockMaxRetries (%d) must not be negative", domain.ErrInvalidConfig, c.LockPollSeconds, c.LockMaxRetries)
	}
	if c.LockPollSeconds > 0 {
		out.pollSeconds = uint64(c.LockPollSeconds)
	}
	if c.LockMaxRetries > 0 {
		out.maxRetries = uint64(c.LockMaxRetries)
	}

	return out, nil
}

// IsPlainIdentifier reports whether s is safe to use as an unquoted-shaped
// SQL identifier: ASCII letters, digits and underscore, not starting with a
// digit, and not empty.
//
// Every identifier this module interpolates is also run through
// pgx.Identifier.Sanitize, which quotes and escapes it. This check is a
// second, narrower gate in front of that, applied to the identifiers that
// come from configuration — and GRANT, REVOKE and CREATE ROLE cannot take a
// name as a bound parameter, so those names are the values that have to
// reach Postgres as SQL text. A value in that position gets more than one
// reason to be safe.
//
// It is exported because a caller writing a reconciler is building DDL from
// its own configuration and needs the same gate; re-deriving it is how the
// two come to disagree about what is plain.
func IsPlainIdentifier(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return len(s) > 0
}
