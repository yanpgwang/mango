package pg

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
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
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDBFromPool(store.pool)
	defer func() { _ = db.Close() }()
	if err := goose.DownToContext(ctx, db, "migrations", 0); err != nil {
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
