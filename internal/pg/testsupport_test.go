package pg

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yanpgwang/mango/internal/domain"
)

// testDatabaseURLEnv names the connection string for the integration test
// database. When unset the PostgreSQL-backed tests skip, so `go test ./...`
// passes with no local stack. The local dev stack (deployments/local) exposes a
// suitable database at the documented default.
const testDatabaseURLEnv = "MANGO_TEST_DATABASE_URL"

// baseURL returns the configured admin/base database URL, or "" when the tests
// should skip. It defaults to the local dev stack's connection string when the
// stack is reachable via the standard env var.
func baseURL() string {
	return os.Getenv(testDatabaseURLEnv)
}

// testStore provisions an isolated PostgreSQL schema for one test, runs
// migrations into it, and returns a Store bound to it. It skips the test when no
// test database is configured. Each test gets its own schema so parallel tests
// never collide, and the schema is dropped on cleanup.
func testStore(t *testing.T) *Store {
	return testStoreWithOptions(t, 0)
}

func testStoreWithMaxConns(t *testing.T, maxConns int32) *Store {
	return testStoreWithOptions(t, maxConns)
}

func testStoreWithOptions(t *testing.T, maxConns int32) *Store {
	t.Helper()
	url := baseURL()
	if url == "" {
		t.Skipf("%s not set; skipping PostgreSQL integration test", testDatabaseURLEnv)
	}
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	// Put randomness before the readable name so PostgreSQL's 63-byte identifier
	// truncation cannot erase it. Independent test processes may run the same case.
	schema := "test_" + domain.NewRandomIDGen().NewID("")[:12] + "_" + sanitize(t.Name())
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.ConnConfig.RuntimeParams["application_name"] = schema
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
		pool.Close()
		t.Skipf("cannot create schema (database unreachable?): %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		pool.Close()
	})
	return NewDefaultWorkspaceStore(pool, &seqIDGen{}, fixedClock{})
}

// sanitize keeps only characters safe for a schema identifier.
func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// seqIDGen produces deterministic, monotonically increasing ids per prefix so
// test assertions can predict event ids where needed.
type seqIDGen struct {
	mu sync.Mutex
	n  int64
}

func (g *seqIDGen) NewID(prefix string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return prefix + itoa(g.n)
}

// fixedClock advances by a fixed step per call so created_at ordering is stable
// and distinct without depending on wall-clock resolution.
type fixedClock struct{}

var clockSeq atomic.Int64

func (fixedClock) Now() time.Time {
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	return base.Add(time.Duration(clockSeq.Add(1)) * time.Millisecond)
}

// newSession builds a minimal valid session for admission tests.
func newSession(id string) domain.Session {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	return domain.Session{
		ID:            id,
		AgentID:       "agent_1",
		AgentVersion:  1,
		EnvironmentID: "env_1",
		Status:        domain.StatusIdle,
		Metadata:      map[string]any{},
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}
