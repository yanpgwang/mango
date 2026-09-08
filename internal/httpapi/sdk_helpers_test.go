package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	mango "github.com/yanpgwang/mango/sdk/go"
)

// Record the actual response bytes before SDK decoding. This keeps assertions
// about secret redaction and null fields independent of the generated types.
func recordedSDKClient(t *testing.T, baseURL string) (*mango.Client, func() string) {
	t.Helper()
	transport := &sdkResponseTransport{}
	client, err := mango.New(mango.Config{BaseURL: baseURL, APIKey: "sk-test", HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	return client, func() string {
		transport.mu.Lock()
		defer transport.mu.Unlock()
		return transport.body
	}
}

type sdkResponseTransport struct {
	mu   sync.Mutex
	body string
}

func (t *sdkResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := http.DefaultTransport.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		return response, nil
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	t.mu.Lock()
	t.body = string(body)
	t.mu.Unlock()
	return response, nil
}

func rawJSONField(t *testing.T, raw, key string) string {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		t.Fatal(err)
	}
	return string(object[key])
}

// mustEnv creates a fixture with a raw request so Session SDK tests remain
// focused on the Session operation under test. Environment lifecycle assertions
// remain in the raw HTTP suite.
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
	var apierr *mango.APIError
	if !errors.As(err, &apierr) {
		t.Fatalf("expected *mango.APIError, got %T: %v", err, err)
	}
	if apierr.StatusCode != want {
		t.Fatalf("API error status = %d, want %d", apierr.StatusCode, want)
	}
}

// assertRawObjectHasFields protects against a subtle SDK-test false positive:
// response decoding is intentionally lenient, so a required response field may be
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
