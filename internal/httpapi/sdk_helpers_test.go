package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// mustEnv creates a fixture with a raw request so Session SDK tests remain
// focused on the Session operation under test. Environment SDK lifecycle
// coverage lives in TestSDK_EnvironmentLifecycle.
func mustEnv(t *testing.T, serverURL string) string {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), "POST",
		serverURL+"/v1/environments",
		bytes.NewBufferString(`{"name":"e","config":{"type":"self_hosted"}}`))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	defer closeTestResource(t, resp.Body)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("create environment status %d: %s", resp.StatusCode, body)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode environment: %v", err)
	}
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatalf("environment id empty: %s", body)
	}
	return id
}

// assertAPIStatus asserts that err is an SDK API error with the given HTTP
// status code.
func assertAPIStatus(t *testing.T, err error, want int) {
	t.Helper()
	var apierr *anthropic.Error
	if !errors.As(err, &apierr) {
		t.Fatalf("expected *anthropic.Error, got %T: %v", err, err)
	}
	if apierr.StatusCode != want {
		t.Fatalf("API error status = %d, want %d", apierr.StatusCode, want)
	}
}

// assertRawObjectHasFields protects against a subtle SDK-test false positive:
// response decoding is intentionally lenient, so an api:"required" field may be
// absent without making the SDK call fail. Presence must be asserted from the
// original response JSON as well as from typed values.
func assertRawObjectHasFields(t *testing.T, raw string, fields ...string) {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		t.Fatalf("decode raw SDK response: %v (%s)", err, raw)
	}
	for _, field := range fields {
		if _, ok := object[field]; !ok {
			t.Errorf("raw SDK response missing field %q: %s", field, raw)
		}
	}
}
