package temporal_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
)

var integrationSchemaSeq int

// integrationStore provisions an isolated PostgreSQL schema, runs the embedded
// migrations into it, and returns a pg.Store bound to it plus a cleanup func. It
// mirrors the pg package's own test harness but lives in the external test
// package, using only pg's exported API.
func integrationStore(t *testing.T, url string) (*pg.Store, func()) {
	t.Helper()
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	integrationSchemaSeq++
	schema := "itest_" + sanitizeSchema(t.Name()) + "_" + itoaInt(integrationSchemaSeq)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
		pool.Close()
		t.Skipf("cannot create schema (database unreachable?): %v", err)
	}
	if err := pg.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	store := pg.NewDefaultWorkspaceStore(pool, domain.NewRandomIDGen(), realClock{})
	cleanup := func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		pool.Close()
	}
	return store, cleanup
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func sanitizeSchema(s string) string {
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

func itoaInt(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func startIntegrationRuntime(
	t *testing.T,
	parent context.Context,
	runtime *temporalpkg.Runtime,
) func() {
	t.Helper()
	if err := runtime.Worker.Start(); err != nil {
		t.Fatalf("start integration worker: %v", err)
	}
	relayCtx, cancelRelay := context.WithCancel(parent)
	relayDone := make(chan error, 1)
	go func() { relayDone <- runtime.Relay.Run(relayCtx) }()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancelRelay()
			runtime.Worker.Stop()
			select {
			case err := <-relayDone:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("stop integration relay: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("integration relay did not stop")
			}
		})
	}
}

func waitForIntegrationCompletion(
	t *testing.T,
	store *pg.Store,
	sessionID string,
	timeout time.Duration,
) []domain.Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		events, err := store.EventsAfter(context.Background(), sessionID, 0, 300)
		if err != nil {
			t.Fatalf("list completed integration events: %v", err)
		}
		if failure, ok := firstFailureEvent(events); ok {
			t.Fatalf("integration failed with %s: %#v", failure.Type, failure.Payload)
		}
		if hasType(events, domain.EvAgentMessage) && latestIdleReason(events) == "end_turn" {
			return events
		}
		time.Sleep(100 * time.Millisecond)
	}
	events, _ := store.EventsAfter(context.Background(), sessionID, 0, 300)
	t.Fatalf("timed out waiting for integration completion; events=%s", typeList(events))
	return nil
}

func latestIdleReason(events []domain.Event) string {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type != domain.EvSessionStatusIdle {
			continue
		}
		stopReason, _ := events[index].Payload["stop_reason"].(map[string]any)
		reason, _ := stopReason["type"].(string)
		return reason
	}
	return ""
}
