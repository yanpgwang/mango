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

// Execute the exact files rendered as snippets by Fumadocs. These use the real
// router and wire validation with test-only state/model behavior, not a durable
// service deployment or real model. They must observe output, not just a 200.
func TestDocumentationSDKQuickstart(t *testing.T) {
	if os.Getenv("MANGO_TEST_SDK") != "1" {
		t.Skip("run make sdk-conformance after make sdk-install")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, example := range []struct {
		name, dir string
		args      []string
	}{
		{"go", "sdk/go", []string{"go", "run", "./examples/quickstart"}},
		{"python", "sdk/python", []string{filepath.Join(root, "sdk/python/.venv/bin/python"), "examples/quickstart.py"}},
		{"typescript", "sdk/typescript", []string{"node", "--experimental-strip-types", "examples/quickstart.ts"}},
		{"http", ".", []string{"bash", "examples/sdk-quickstart.sh"}},
	} {
		t.Run(example.name, func(t *testing.T) {
			handler := newTestHandler(t, Config{RequireAuth: true}, false)
			const key = "docs-quickstart-test-key"
			var mu sync.Mutex
			seen := map[string]int{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !isPublicRequest(r) && r.Header.Get("Authorization") != "Bearer "+key {
					t.Error("documentation example must authenticate to Mango")
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				if r.Header.Get("anthropic-beta") != "" || r.Header.Get("anthropic-version") != "" {
					t.Error("documentation example sent vendor headers")
				}
				mu.Lock()
				seen[r.Method+" "+r.URL.Path]++
				mu.Unlock()
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			command := exec.CommandContext(ctx, example.args[0], example.args[1:]...)
			command.Dir = filepath.Join(root, example.dir)
			for _, entry := range os.Environ() {
				name, _, _ := strings.Cut(entry, "=")
				if strings.HasPrefix(name, "MANGO_") || strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "OPENAI_") {
					continue
				}
				command.Env = append(command.Env, entry)
			}
			command.Env = append(command.Env, "MANGO_BASE_URL="+server.URL, "MANGO_API_KEY="+key)
			output, err := command.CombinedOutput()
			if strings.Contains(string(output), key) {
				t.Fatal("documentation example exposed its credential")
			}
			if err != nil {
				t.Fatalf("quickstart failed: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), "Quickstart completed") {
				t.Fatalf("quickstart did not finish: %s", output)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, route := range []string{"POST /v1/agents", "POST /v1/environments", "POST /v1/sessions"} {
				if seen[route] != 1 {
					t.Errorf("expected exactly one %s, got %d", route, seen[route])
				}
			}
			var deletedSessions, deletedEnvironments, archivedAgents int
			for route, count := range seen {
				if strings.HasPrefix(route, "DELETE /v1/sessions/") {
					deletedSessions += count
				}
				if strings.HasPrefix(route, "DELETE /v1/environments/") {
					deletedEnvironments += count
				}
				if strings.HasPrefix(route, "POST /v1/agents/") && strings.HasSuffix(route, "/archive") {
					archivedAgents += count
				}
			}
			if deletedSessions != 1 || deletedEnvironments != 1 || archivedAgents != 1 {
				t.Errorf("quickstart must clean up its own resources: sessions=%d environments=%d agents=%d", deletedSessions, deletedEnvironments, archivedAgents)
			}
		})
	}
}
