package pg

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate is the explicit schema-writing operation. A PostgreSQL advisory lock
// serializes concurrent migration jobs. Closing the provider releases only its
// database/sql adapter, never the caller's pgx pool.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	provider, err := migrationProvider(ctx, pool)
	if err != nil {
		return err
	}
	defer func() { _ = provider.Close() }()
	if err := checkSchema(ctx, pool, provider.ListSources(), true); err != nil {
		return err
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("pg: run migrations: %w", err)
	}
	return checkSchema(ctx, pool, provider.ListSources(), false)
}

// CheckSchema reads the migration ledger and requires exactly this binary's
// applied versions. It performs no DDL, seed writes, or ledger initialization.
// It checks migration versions, not arbitrary manual schema modifications.
func CheckSchema(ctx context.Context, pool *pgxpool.Pool) error {
	provider, err := migrationProvider(ctx, pool)
	if err != nil {
		return err
	}
	defer func() { _ = provider.Close() }()
	return checkSchema(ctx, pool, provider.ListSources(), false)
}

func migrationProvider(ctx context.Context, pool *pgxpool.Pool) (*goose.Provider, error) {
	files, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("pg: embedded migrations: %w", err)
	}
	// Separate schemas are independent Mango installations (and isolated test
	// fixtures). Serialize jobs for the same effective schema, not all jobs in
	// a database. PostgreSQL advisory locks are already database-scoped.
	var lockID int64
	if err := pool.QueryRow(ctx, `SELECT hashtextextended('mango:migrations:' || current_schema(), 0)`).Scan(&lockID); err != nil {
		return nil, fmt.Errorf("pg: resolve migration schema: %w", err)
	}
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(lockID))
	if err != nil {
		return nil, fmt.Errorf("pg: migration lock: %w", err)
	}
	db := stdlib.OpenDBFromPool(pool)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files,
		goose.WithDisableGlobalRegistry(true), goose.WithSessionLocker(locker))
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pg: migration provider: %w", err)
	}
	return provider, nil
}

func checkSchema(ctx context.Context, pool *pgxpool.Pool, sources []*goose.Source, allowPending bool) error {
	// Down migrations may append a false record (the original Goose API) or
	// delete the applied record (the provider API). Only the latest record for
	// each version governs whether it is currently applied.
	rows, err := pool.Query(ctx, `
		SELECT version_id FROM (
			SELECT DISTINCT ON (version_id) version_id, is_applied
			FROM goose_db_version ORDER BY version_id, id DESC
		) versions WHERE is_applied AND version_id <> 0 ORDER BY version_id`)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			if allowPending {
				return nil
			}
			return errors.New("pg: schema is not initialized; run mango migrate before starting this command")
		}
		return fmt.Errorf("pg: read schema versions: %w", err)
	}
	defer rows.Close()
	expected := make(map[int64]bool, len(sources))
	for _, source := range sources {
		expected[source.Version] = false
	}
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return fmt.Errorf("pg: read schema version: %w", err)
		}
		if _, known := expected[version]; !known {
			return fmt.Errorf("pg: applied schema version %d is not supported by this binary; use a database and binary from the same checkout or release", version)
		}
		expected[version] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("pg: read schema versions: %w", err)
	}
	if !allowPending {
		for _, source := range sources {
			if !expected[source.Version] {
				return fmt.Errorf("pg: schema migration %d is pending; run mango migrate before starting this command", source.Version)
			}
		}
	}
	return nil
}
