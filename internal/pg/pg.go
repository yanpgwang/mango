// Package pg provides the authoritative control-plane persistence: a pgx pool,
// embedded goose migrations, and stores built on sqlc-generated queries.
package pg

import (
	"context"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:generate sqlc generate -f ../../sqlc.yaml

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Pool opens a pgx connection pool for the given PostgreSQL URL. The caller owns
// the returned pool and must Close it.
func Pool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("pg: parse database url: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pg: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pg: ping: %w", err)
	}
	return pool, nil
}

// Migrate applies all embedded goose migrations up to the latest version. It is
// idempotent: goose records applied versions and skips them on the next run.
// goose uses database/sql, so the pool's config is borrowed through the pgx
// stdlib adapter and the temporary *sql.DB is closed before returning (it does
// not close the caller's pool).
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("pg: set goose dialect: %w", err)
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()
	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		return fmt.Errorf("pg: run migrations: %w", err)
	}
	return nil
}
