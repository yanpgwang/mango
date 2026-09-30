package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg"
	"github.com/yanpgwang/mango/internal/workspace"
)

// Exercise the actual operator command parser and output, keeping issued
// secrets in memory rather than exposing them in test logs.
func TestEnvironmentKeyOperatorCLI(t *testing.T) {
	databaseURL := os.Getenv("MANGO_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("MANGO_TEST_DATABASE_URL not set")
	}
	ctx := workspace.WithScope(context.Background(), workspace.DefaultID)
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("test_environment_key_cli_%d", time.Now().UnixNano())
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	// ConnString retains the original input; encode the child process's isolated
	// search_path explicitly instead of assuming RuntimeParams changes persist.
	operatorURL, err := url.Parse(databaseURL)
	if err != nil || (operatorURL.Scheme != "postgres" && operatorURL.Scheme != "postgresql") {
		t.Fatal("operator CLI test requires a PostgreSQL URL")
	}
	query := operatorURL.Query()
	query.Set("search_path", schema)
	operatorURL.RawQuery = query.Encode()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
	if err := pg.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := pg.NewStore(pool, domain.NewRandomIDGen(), realClock{})
	now := time.Now().UTC()
	if err := pg.NewEnvironmentRepository(store).Put(ctx, domain.Environment{ID: "env_operator", Name: "operator", ConfigType: "self_hosted", Config: map[string]any{"type": "self_hosted"}, Metadata: map[string]any{}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	run := func(arguments ...string) string {
		t.Helper()
		command := exec.Command(os.Args[0], append([]string{"-test.run=^TestAPIKeyCLIProcess$", "--"}, arguments...)...)
		command.Env = append(os.Environ(), "MANGO_TEST_API_KEY_CLI=1", envDatabaseURL+"="+operatorURL.String())
		out, err := command.Output()
		if err != nil {
			t.Fatalf("operator command failed: %v", err)
		}
		return string(out)
	}
	created := run("api-key", "create", "-workspace", workspace.DefaultID, "-environment", "env_operator", "-label", "pool-a")
	fields := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(created), "\n") {
		key, value, _ := strings.Cut(line, "\t")
		fields[key] = value
	}
	if fields["environment"] != "env_operator" || fields["workspace"] != workspace.DefaultID || fields["id"] == "" || fields["api_key"] == "" {
		t.Fatal("create did not return key identity, scope, and one-time secret")
	}
	var storedInSchema bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+schema+".api_keys WHERE id=$1)", fields["id"]).Scan(&storedInSchema); err != nil || !storedInSchema {
		t.Fatalf("CLI did not write the isolated schema: %v", err)
	}

	if _, scope, err := store.AuthenticateEnvironmentKey(ctx, fields["api_key"]); err != nil || scope.EnvironmentID != "env_operator" {
		t.Fatalf("issued key scope invalid: %v", err)
	}
	listing := run("api-key", "list", "-workspace", workspace.DefaultID)
	if !strings.Contains(listing, fields["id"]+"\tpool-a\tactive\t") || !strings.Contains(listing, "\tenv_operator\n") || strings.Contains(listing, fields["api_key"]) {
		t.Fatal("listing omitted scope/status or disclosed secret")
	}
	for range 2 {
		run("api-key", "revoke", "-id", fields["id"])
	}
	if _, _, err := store.AuthenticateEnvironmentKey(ctx, fields["api_key"]); !errors.Is(err, workspace.ErrInvalidEnvironmentKey) {
		t.Fatalf("operator revocation did not revoke key: %v", err)
	}
	listing = run("api-key", "list", "-workspace", workspace.DefaultID)
	if !strings.Contains(listing, "\tpool-a\trevoked\t") || strings.Contains(listing, fields["api_key"]) {
		t.Fatal("listing did not show revoked status or disclosed secret")
	}
}

func TestAPIKeyCLIProcess(t *testing.T) {
	if os.Getenv("MANGO_TEST_API_KEY_CLI") != "1" {
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
