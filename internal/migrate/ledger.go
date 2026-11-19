package migrate

// This file is the shared-ledger half of the module: one table, outside every
// module schema, with a row per module and version. It implements goose's
// Store interface directly rather than a custom Querier because the table's
// shape is not goose's default shape: there is no is_applied column, and the
// module is part of every key.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3/database"
)

// sharedStore is goose's bookkeeping for one module inside the shared table.
// It is created per Up/Status call; it holds no connection and no state beyond
// the two validated identifiers and the sanitized table name.
type sharedStore struct {
	schema string
	module string
	table  string
}

var (
	_ database.Store         = (*sharedStore)(nil)
	_ database.StoreExtender = (*sharedStore)(nil)
)

func newSharedStore(schema, module string) *sharedStore {
	return &sharedStore{
		schema: schema,
		module: module,
		table:  pgx.Identifier{schema, DefaultSharedVersionTable}.Sanitize(),
	}
}

// Tablename is the qualified table goose reports in its errors and uses for
// its own existence fallback.
func (s *sharedStore) Tablename() string { return s.table }

// physicalTableExists asks the catalog, not the module row. It is the check
// used before DDL: a table that exists is never recreated, so a pre-created
// ledger needs no CREATE privilege.
func (s *sharedStore) physicalTableExists(ctx context.Context, db database.DBTxConn) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx,
		`SELECT to_regclass($1) IS NOT NULL`,
		s.schema+"."+DefaultSharedVersionTable).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check shared ledger table: %w", err)
	}
	return exists, nil
}

// TableExists is module-aware: the table being there is not enough, because
// goose needs this module's zero row before Provider.Up will accept the
// ledger as initialized. Returning false when the table exists but the zero
// row does not makes goose call CreateVersionTable (a no-op for an existing
// table) and then Insert(0) — a DML-only path.
func (s *sharedStore) TableExists(ctx context.Context, db database.DBTxConn) (bool, error) {
	table, err := s.physicalTableExists(ctx, db)
	if err != nil || !table {
		return false, err
	}
	var zero bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM `+s.table+` WHERE module = $1 AND version = 0)`,
		s.module).Scan(&zero); err != nil {
		return false, fmt.Errorf("check shared ledger zero row: %w", err)
	}
	return zero, nil
}

// CreateVersionTable creates the table only when it is physically absent. The
// schema is created only when it is absent too, for the same privilege reason
// as ensureSchemaNamed: CREATE ... IF NOT EXISTS checks the CREATE privilege
// even when the object exists.
func (s *sharedStore) CreateVersionTable(ctx context.Context, db database.DBTxConn) error {
	table, err := s.physicalTableExists(ctx, db)
	if err != nil {
		return err
	}
	if table {
		return nil
	}

	var schemaExists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`,
		s.schema).Scan(&schemaExists); err != nil {
		return fmt.Errorf("check shared ledger schema: %w", err)
	}
	if !schemaExists {
		if _, err := db.ExecContext(ctx,
			`CREATE SCHEMA `+pgx.Identifier{s.schema}.Sanitize()); err != nil {
			return fmt.Errorf("create shared ledger schema: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+s.table+` (
		module     text        NOT NULL,
		version    bigint      NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (module, version)
	)`); err != nil {
		return fmt.Errorf("create shared ledger table: %w", err)
	}
	return nil
}

func (s *sharedStore) Insert(ctx context.Context, db database.DBTxConn, req database.InsertRequest) error {
	if _, err := db.ExecContext(ctx,
		`INSERT INTO `+s.table+` (module, version) VALUES ($1, $2)
		 ON CONFLICT (module, version) DO NOTHING`,
		s.module, req.Version); err != nil {
		return fmt.Errorf("insert shared ledger version %d: %w", req.Version, err)
	}
	return nil
}

func (s *sharedStore) Delete(ctx context.Context, db database.DBTxConn, version int64) error {
	if _, err := db.ExecContext(ctx,
		`DELETE FROM `+s.table+` WHERE module = $1 AND version = $2`,
		s.module, version); err != nil {
		return fmt.Errorf("delete shared ledger version %d: %w", version, err)
	}
	return nil
}

func (s *sharedStore) GetMigration(ctx context.Context, db database.DBTxConn, version int64) (*database.GetMigrationResult, error) {
	var appliedAt time.Time
	err := db.QueryRowContext(ctx,
		`SELECT applied_at FROM `+s.table+` WHERE module = $1 AND version = $2`,
		s.module, version).Scan(&appliedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %d", database.ErrVersionNotFound, version)
	}
	if err != nil {
		return nil, fmt.Errorf("get shared ledger version %d: %w", version, err)
	}
	return &database.GetMigrationResult{Timestamp: appliedAt, IsApplied: true}, nil
}

func (s *sharedStore) GetLatestVersion(ctx context.Context, db database.DBTxConn) (int64, error) {
	var latest sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT max(version) FROM `+s.table+` WHERE module = $1`,
		s.module).Scan(&latest); err != nil {
		return -1, fmt.Errorf("get latest shared ledger version: %w", err)
	}
	if !latest.Valid {
		return -1, fmt.Errorf("latest %w", database.ErrVersionNotFound)
	}
	return latest.Int64, nil
}

func (s *sharedStore) ListMigrations(ctx context.Context, db database.DBTxConn) ([]*database.ListMigrationsResult, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT version FROM `+s.table+` WHERE module = $1 ORDER BY version DESC`,
		s.module)
	if err != nil {
		return nil, fmt.Errorf("list shared ledger migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var migrations []*database.ListMigrationsResult
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan shared ledger migration: %w", err)
		}
		migrations = append(migrations, &database.ListMigrationsResult{
			Version:   version,
			IsApplied: true,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate shared ledger migrations: %w", err)
	}
	return migrations, nil
}
