package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSessionsConnectUsesHTTPWithoutDatabase(t *testing.T) {
	t.Setenv("MANGO_API_KEY", "local-test-key")
	t.Setenv("MANGO_DATABASE_URL", "invalid:client-command-must-not-open-a-database")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-test-key" {
			t.Error("missing API key")
		}
		switch r.URL.Path {
		case "/v1/sessions/sesn_test":
			_, _ = io.WriteString(w, `{"id":"sesn_test","status":"terminated"}`)
		case "/v1/sessions/sesn_test/events":
			_, _ = io.WriteString(w, `{"data":[{"id":"sevt_test","type":"agent.message","content":[{"type":"text","text":"saved answer"}],"processed_at":null}],"next_page":null}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	// A flag overrides the environment's base URL, and flags follow the Session ID.
	t.Setenv("MANGO_BASE_URL", "http://invalid.local")
	var output, diagnostics bytes.Buffer
	err := sessionsCommand(context.Background(), []string{"connect", "sesn_test", "-base-url", server.URL}, io.NopCloser(strings.NewReader("must not send\n")), &output, &diagnostics)
	if err != nil || !strings.Contains(output.String(), "saved answer") {
		t.Fatalf("connect: %v, output=%s", err, &output)
	}
}

func TestSessionsConnectHelpAndValidation(t *testing.T) {
	t.Setenv("MANGO_API_KEY", "")
	for _, args := range [][]string{{"connect", "-h"}, {"connect", "sesn_test", "-h"}} {
		var out bytes.Buffer
		if err := sessionsCommand(context.Background(), args, nil, io.Discard, &out); err != nil || !strings.Contains(out.String(), "read-only") {
			t.Fatalf("help: %v, output=%s", err, &out)
		}
	}
	for _, args := range [][]string{{}, {"connect"}, {"list"}, {"connect", "sesn_test"}, {"connect", "sesn_test", "extra"}, {"connect", "sesn_test", "-api-key", "do-not-accept"}} {
		if err := sessionsCommand(context.Background(), args, nil, io.Discard, io.Discard); err == nil {
			t.Fatalf("invalid command accepted: %v", args)
		}
	}
}
