package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// This tier runs independently packaged SDKs through Mango's real HTTP router,
// validation, DTOs and event projection. Repositories and model execution use
// the raw HTTP suite's test-only fakes, not PostgreSQL or a live provider.
func TestFirstPartySDKHTTPConformance(t *testing.T) {
	if os.Getenv("MANGO_TEST_SDK") != "1" {
		t.Skip("run make sdk-conformance after make sdk-install")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, language := range []struct {
		name string
		args []string
	}{
		{"go", []string{"go", "run", "./examples/conformance"}},
		{"python", []string{filepath.Join(root, "sdk", "python", ".venv", "bin", "python"), "examples/conformance.py"}},
		{"typescript", []string{"node", "examples/conformance.mjs"}},
	} {
		t.Run(language.name, func(t *testing.T) {
			handler := newTestHandler(t, Config{RequireAuth: true}, false)
			const key = "sdk-http-conformance-key"
			var mu sync.Mutex
			seen := map[string]int{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !isPublicRequest(r) && r.Header.Get("Authorization") != "Bearer "+key {
					t.Error("SDK did not send the expected bearer credential")
					http.Error(w, "invalid test credential", http.StatusUnauthorized)
					return
				}
				if r.Header.Get("anthropic-version") != "" || r.Header.Get("anthropic-beta") != "" {
					t.Error("first-party SDK sent a vendor-specific header")
				}
				mu.Lock()
				seen[r.Method+" "+r.URL.Path]++
				mu.Unlock()
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			command := exec.CommandContext(ctx, language.args[0], language.args[1:]...)
			command.Dir = filepath.Join(root, "sdk", language.name)
			// SDK tests only need the synthetic local key, never model or
			// operator credentials inherited from the developer's shell.
			for _, entry := range os.Environ() {
				name, _, _ := strings.Cut(entry, "=")
				if strings.HasPrefix(name, "MANGO_") || strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "OPENAI_") {
					continue
				}
				command.Env = append(command.Env, entry)
			}
			command.Env = append(command.Env, "MANGO_SDK_TEST_URL="+server.URL, "MANGO_SDK_TEST_KEY="+key)
			output, err := command.CombinedOutput()
			if strings.Contains(string(output), key) {
				t.Fatal("SDK conformance process exposed its credential")
			}
			if err != nil {
				t.Fatalf("SDK conformance: %v\n%s", err, output)
			}
			t.Logf("%s", output)
			mu.Lock()
			defer mu.Unlock()
			for _, route := range []string{"GET /healthz", "POST /v1/agents", "GET /v1/agents", "POST /v1/environments", "POST /v1/sessions"} {
				if seen[route] == 0 {
					t.Errorf("conformance executable did not exercise %s", route)
				}
			}
		})
	}
}
