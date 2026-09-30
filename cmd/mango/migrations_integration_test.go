package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Startup must not initialize an empty database. Only the explicit migration
// role may create the schema, and retrying it must preserve operator data.
func TestMigrationOperatorCLI(t *testing.T) {
	databaseURL := os.Getenv("MANGO_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("MANGO_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	operatorURL, err := url.Parse(databaseURL)
	if err != nil || (operatorURL.Scheme != "postgres" && operatorURL.Scheme != "postgresql") {
		t.Fatal("operator CLI test requires a PostgreSQL URL")
	}
	schema := fmt.Sprintf("test_migration_cli_%d", time.Now().UnixNano())
	query := operatorURL.Query()
	query.Set("search_path", schema)
	operatorURL.RawQuery = query.Encode()
	pool, err := pgxpool.New(ctx, operatorURL.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	run := func(arguments ...string) (string, error) {
		t.Helper()
		command := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestMigrationCLIProcess$", "--"}, arguments...)...)
		command.Env = append(os.Environ(), "MANGO_TEST_MIGRATION_CLI=1", envDatabaseURL+"="+operatorURL.String())
		out, err := command.CombinedOutput()
		return string(out), err
	}
	assertEmpty := func() {
		t.Helper()
		var tables int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_tables WHERE schemaname = current_schema()").Scan(&tables); err != nil || tables != 0 {
			t.Fatalf("startup changed empty schema: tables=%d, err=%v", tables, err)
		}
	}
	for _, arguments := range [][]string{{"serve"}, {"orchestrate"}, {"workspace", "list"}, {"api-key", "list", "-workspace", "wrkspc_default"}} {
		out, err := run(arguments...)
		if err == nil || !strings.Contains(out, "mango migrate") {
			t.Fatalf("%s should reject an empty schema with migration guidance (exit error=%v)", strings.Join(arguments, " "), err)
		}
		assertEmpty()
	}
	if _, err := run("migrate", "unexpected-argument"); err == nil {
		t.Fatal("migrate accepted a positional argument")
	}
	assertEmpty()
	// Two first-run jobs must serialize instead of racing to create tables or
	// bootstrap rows. Each uses an independent process and connection pool.
	migrationErrors := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			_, err := run("migrate")
			migrationErrors <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-migrationErrors; err != nil {
			t.Fatalf("concurrent explicit migration failed: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE workspaces SET name = 'Preserved' WHERE id = 'wrkspc_default'"); err != nil {
		t.Fatal(err)
	}
	if _, err := run("migrate"); err != nil {
		t.Fatalf("repeated migration failed: %v", err)
	}
	out, err := run("workspace", "list")
	if err != nil || !strings.Contains(out, "wrkspc_default\tPreserved\t") {
		t.Fatalf("operator command did not read the preserved Workspace: %v", err)
	}
	// A different checkout must fail closed without trying to downgrade or
	// rewriting the migration ledger.
	if _, err := pool.Exec(ctx, "INSERT INTO goose_db_version (version_id, is_applied) VALUES (99, true)"); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"serve"}, {"orchestrate"}, {"workspace", "list"}, {"api-key", "list", "-workspace", "wrkspc_default"}, {"migrate"}} {
		out, err := run(arguments...)
		if err == nil || !strings.Contains(out, "schema version 99") {
			t.Fatalf("%s did not reject an unsupported schema (exit error=%v)", strings.Join(arguments, " "), err)
		}
	}
	var rows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM goose_db_version").Scan(&rows); err != nil || rows != 3 {
		t.Fatalf("unsupported schema ledger changed: rows=%d, err=%v", rows, err)
	}
}

func TestMigrationCLIProcess(t *testing.T) {
	if os.Getenv("MANGO_TEST_MIGRATION_CLI") != "1" {
		return
	}
	for i, value := range os.Args {
		if value == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
	t.Fatal("missing CLI arguments")
}
