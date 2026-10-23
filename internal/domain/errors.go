// Package domain holds this module's value objects and its complete error
// taxonomy. It imports nothing of this module's own and nothing that does
// I/O, so the sentinels a consumer branches on are reachable without pulling
// in goose, pgx or a container runtime.
//
// There are two sentinels, and the split between them is the one question a
// caller can actually act on: retry, or fix the deployment. Both extracted
// implementations arrived at exactly this pair, with the same wording in
// their doc comments.
package domain

import "errors"

// ErrInvalidConfig means the module was wired up wrongly, and no amount of
// retrying will help: a missing schema name, a schema name that is not a
// plain identifier, a nil migration FS, a migration Postgres rejected, or a
// version table that disagrees with the embedded set. It is the "fix the
// deployment" sentinel, as distinct from ErrUnavailable's "try again".
//
// A migration that does not fit the database it was pointed at is
// configuration rather than storage: the binary and the database disagree
// about which schema this is, and that is resolved by shipping a different
// binary or pointing it somewhere else.
var ErrInvalidConfig = errors.New("pgmigrate: invalid configuration")

// ErrUnavailable means the database is reachable in principle but was not
// reachable, or not usable, just now: a refused connection, or an advisory
// lock still held by another process that is still migrating. It is the one
// sentinel in this module that invites a retry.
//
// Waiting for another migrator and failing to reach the server at all are
// deliberately the same sentinel, because the caller's decision is the same
// in both cases — back off and try again — and a caller that distinguished
// them would be deciding that one of the two is worth giving up on.
var ErrUnavailable = errors.New("pgmigrate: database unavailable")
