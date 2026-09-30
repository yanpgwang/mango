// Package pg provides the authoritative control-plane persistence: a pgx pool,
// embedded goose migrations, and stores built on sqlc-generated queries.
package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:generate sqlc generate -f ../../sqlc.yaml

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
