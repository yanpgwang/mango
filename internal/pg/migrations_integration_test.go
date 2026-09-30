package pg

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yanpgwang/mango/internal/app"
)

// Reapplying initialization must preserve both application data and the
// bootstrap Workspace; it must not replay seed writes over operator changes.
func TestMigrateReapplyPreservesSessionAndWorkspace(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sesn_migration_reapply")
	if _, err := store.CreateSession(ctx, session, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET name = 'Renamed' WHERE id = 'wrkspc_default'`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, store.pool); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSession(ctx, session.ID)
	if err != nil || got.ID != session.ID {
		t.Fatalf("Session after reapply = %+v, %v", got, err)
	}
	threads, err := store.ListSessionThreads(ctx, session.ID, app.SessionThreadListQuery{Limit: 10})
	if err != nil || len(threads) != 1 {
		t.Fatalf("Session Threads after reapply = %+v, %v", threads, err)
	}
	var name string
	if err := store.pool.QueryRow(ctx, `SELECT name FROM workspaces WHERE id = 'wrkspc_default'`).Scan(&name); err != nil || name != "Renamed" {
		t.Fatalf("Workspace after reapply = %q, %v", name, err)
	}
}

// The development schema can be removed and initialized again without leaving
// foreign keys or application tables behind, even after a Session is created.
func TestMigrateDownAndReinitialize(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if _, err := store.CreateSession(ctx, newSession("sesn_migration_down"), nil); err != nil {
		t.Fatal(err)
	}
	provider, err := migrationProvider(ctx, store.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = provider.Close() }()
	if _, err := provider.DownTo(ctx, 0); err != nil {
		t.Fatal(err)
	}
	var tables int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = current_schema() AND tablename <> 'goose_db_version'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("application tables after Down = %d, %v", tables, err)
	}
	if err := Migrate(ctx, store.pool); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := store.pool.QueryRow(ctx, `SELECT name FROM workspaces WHERE id = 'wrkspc_default'`).Scan(&name); err != nil || name != "Default Workspace" {
		t.Fatalf("bootstrap Workspace after reinitialization = %q, %v", name, err)
	}
	if _, err := store.CreateSession(ctx, newSession("sesn_migration_reinitialized"), nil); err != nil {
		t.Fatalf("create Session after reinitialization: %v", err)
	}
}

// Even read-only connections can validate an initialized schema. Startup must
// never try to initialize the migration ledger as a side effect of checking it.
func TestCheckSchemaReadOnlyAndRolledBack(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	config := store.pool.Config()
	config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := CheckSchema(ctx, pool); err != nil {
		t.Fatalf("read-only schema check: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (1, false)`); err != nil {
		t.Fatal(err)
	}
	if err := CheckSchema(ctx, pool); err == nil || !strings.Contains(err.Error(), "mango migrate") {
		t.Fatalf("rolled-back baseline was accepted: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (1, true)`); err != nil {
		t.Fatal(err)
	}
	if err := CheckSchema(ctx, pool); err != nil {
		t.Fatalf("reapplied baseline was rejected: %v", err)
	}
}
