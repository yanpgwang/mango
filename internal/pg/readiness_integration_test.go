package pg

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/httpapi"
	mango "github.com/yanpgwang/mango/sdk/go"
)

func TestReadinessHTTPPostgresRecovery(t *testing.T) {
	store := testStoreWithMaxConns(t, 1)
	handler := httpapi.NewServer(httpapi.Deps{Readiness: store.Readiness}, httpapi.Config{RequireAuth: true}).Handler()
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := mango.New(mango.Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	assertStatus := func(want int) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if response.Code != want {
			t.Fatalf("readyz = %d %s, want %d", response.Code, response.Body.String(), want)
		}
		err := client.System.Readiness(context.Background())
		if want == 200 {
			if err != nil {
				t.Fatal(err)
			}
		} else {
			var apiError *mango.APIError
			if !errors.As(err, &apiError) || apiError.StatusCode != 503 || apiError.Type != "api_error" || apiError.RequestID == "" {
				t.Fatalf("SDK error = %v", err)
			}
		}
	}
	assertStatus(200)
	if _, err := store.pool.Exec(context.Background(), "SET default_transaction_read_only = on"); err != nil {
		t.Fatal(err)
	}
	assertStatus(503)
	if err := client.System.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(context.Background(), "SET default_transaction_read_only = off"); err != nil {
		t.Fatal(err)
	}
	assertStatus(200)

	// Exhaust only this test's pool. Other service tests keep their connections.
	conn, err := store.pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx))
	if response.Code != 503 {
		t.Fatalf("exhausted pool = %d", response.Code)
	}
	conn.Release()
	assertStatus(200)
}

func TestReadinessClosedPool(t *testing.T) {
	if baseURL() == "" {
		t.Skip("MANGO_TEST_DATABASE_URL not set")
	}
	pool, err := Pool(context.Background(), baseURL())
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{pool: pool}
	pool.Close()
	if err := store.Readiness(context.Background()); err == nil {
		t.Fatal("closed pool reported ready")
	}
}
