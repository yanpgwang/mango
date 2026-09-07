package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// These golden tests assert exact JSON wire shapes that the SDK's typed structs
// abstract away — specifically that a persisted event is a flat top-level tagged
// union (no "payload" wrapper, no "sequence"/"seq"/"after_seq"), and that the
// error envelope matches {"type":"error","error":{...}}.

func rawRequest(t *testing.T, method, url, body string) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, url, r)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func TestGolden_EventIsFlatTaggedUnion(t *testing.T) {
	client, ts := sdkClientAndServer(t)
	ctx := context.Background()
	agent := mustAgent(t, client, "opus", "sys")
	env := mustEnv(t, ts.URL)
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: env,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	base := ts.URL

	status, body := rawRequest(t, "POST", base+"/v1/sessions/"+session.ID+"/events",
		`{"events":[{"type":"user.message","content":[{"type":"text","text":"hi"}]}]}`)
	if status != 200 {
		t.Fatalf("send status %d: %s", status, body)
	}

	var sent struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("decode send response: %v", err)
	}
	if len(sent.Data) != 1 {
		t.Fatalf("send returned %d events", len(sent.Data))
	}
	ev := sent.Data[0]

	// Top-level tagged union: type + content at the top level, no wrapper.
	if ev["type"] != "user.message" {
		t.Fatalf("event type = %v", ev["type"])
	}
	if _, ok := ev["content"]; !ok {
		t.Fatal("content must be a top-level field on the event")
	}
	if _, ok := ev["id"]; !ok {
		t.Fatal("event must carry a top-level id")
	}
	// Internal / forbidden keys must never surface.
	for _, forbidden := range []string{"payload", "sequence", "seq", "after_seq", "session_id"} {
		if _, ok := ev[forbidden]; ok {
			t.Errorf("event exposed forbidden key %q: %v", forbidden, ev)
		}
	}
	// content[] is an array of typed blocks.
	blocks, ok := ev["content"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("content is not a single-block array: %v", ev["content"])
	}
	block := blocks[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "hi" {
		t.Fatalf("content block = %v", block)
	}

	// Same shape on the list endpoint, and next_page present with no prev_page.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st, lb := rawRequest(t, "GET", base+"/v1/sessions/"+session.ID+"/events", "")
		if st != 200 {
			t.Fatalf("list status %d: %s", st, lb)
		}
		var listed map[string]any
		if err := json.Unmarshal(lb, &listed); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		if _, ok := listed["next_page"]; !ok {
			t.Fatalf("list envelope missing next_page: %s", lb)
		}
		if _, ok := listed["prev_page"]; ok {
			t.Fatalf("events list must not expose prev_page: %s", lb)
		}
		data := listed["data"].([]any)
		var userMsg map[string]any
		for _, raw := range data {
			e := raw.(map[string]any)
			if e["type"] == "user.message" {
				userMsg = e
			}
		}
		if userMsg != nil {
			for _, forbidden := range []string{"payload", "sequence", "seq", "after_seq"} {
				if _, ok := userMsg[forbidden]; ok {
					t.Errorf("listed event exposed forbidden key %q", forbidden)
				}
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("never observed the user.message in list output")
}

func TestGolden_FileDocumentReferenceRemainsPublicAndFlat(t *testing.T) {
	client, ts := sdkClientAndServer(t)
	ctx := context.Background()
	agent := mustAgent(t, client, "opus", "sys")
	env := mustEnv(t, ts.URL)
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent: anthropic.BetaSessionNewParamsAgentUnion{
			OfString: anthropic.String(agent.ID),
		},
		EnvironmentID: env,
	})
	if err != nil {
		t.Fatal(err)
	}
	status, body := rawRequest(
		t, http.MethodPost, ts.URL+"/v1/sessions/"+session.ID+"/events",
		`{"events":[{"type":"user.message","content":[{"type":"document",`+
			`"title":"Notes","source":{"type":"file","file_id":"file_notes"}}]}]}`,
	)
	if status != http.StatusOK {
		t.Fatalf("send status %d: %s", status, body)
	}
	var sent struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &sent); err != nil || len(sent.Data) != 1 {
		t.Fatalf("decode send response: %v (%s)", err, body)
	}
	event := sent.Data[0]
	blocks, ok := event["content"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("public content = %#v", event["content"])
	}
	block := blocks[0].(map[string]any)
	source := block["source"].(map[string]any)
	if event["type"] != "user.message" || block["type"] != "document" ||
		block["title"] != "Notes" || source["type"] != "file" ||
		source["file_id"] != "file_notes" {
		t.Fatalf("public File event = %#v", event)
	}
	for key := range event {
		if strings.HasPrefix(key, "__") {
			t.Fatalf("public event leaked private key %q: %#v", key, event)
		}
	}
}

func TestGolden_RejectsServerOnlyEventType(t *testing.T) {
	client, ts := sdkClientAndServer(t)
	ctx := context.Background()
	agent := mustAgent(t, client, "opus", "sys")
	env := mustEnv(t, ts.URL)
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: env,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	base := ts.URL

	// A server-emitted type must be rejected when submitted by a client.
	status, body := rawRequest(t, "POST", base+"/v1/sessions/"+session.ID+"/events",
		`{"events":[{"type":"agent.message","content":[{"type":"text","text":"nope"}]}]}`)
	if status != 400 {
		t.Fatalf("expected 400 for server-only event type, got %d: %s", status, body)
	}
	assertErrorEnvelope(t, body, "invalid_request_error")

	// An unknown type must also be rejected.
	status, body = rawRequest(t, "POST", base+"/v1/sessions/"+session.ID+"/events",
		`{"events":[{"type":"user.bogus"}]}`)
	if status != 400 {
		t.Fatalf("expected 400 for unknown event type, got %d: %s", status, body)
	}
	assertErrorEnvelope(t, body, "invalid_request_error")
}

func TestGolden_ErrorEnvelopeShape(t *testing.T) {
	_, ts := sdkClientAndServer(t)
	base := ts.URL
	// A missing bearer token returns the standard authentication envelope.
	req, _ := http.NewRequestWithContext(context.Background(), "GET", base+"/v1/agents", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, resp.Body)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing bearer token: status %d: %s", resp.StatusCode, body)
	}
	assertErrorEnvelope(t, body, "authentication_error")
}

func TestGolden_BodyLimitRejects(t *testing.T) {
	_, ts := sdkClientAndServer(t)
	base := ts.URL
	// A body larger than the 32 MiB limit. Content-Length is set automatically
	// from the buffer length.
	big := bytes.NewBuffer(make([]byte, (32<<20)+1))
	req, _ := http.NewRequestWithContext(context.Background(), "POST", base+"/v1/agents", big)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, resp.Body)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 413 {
		t.Fatalf("expected 413 for oversized body, got %d: %s", resp.StatusCode, body)
	}
	assertErrorEnvelope(t, body, "request_too_large")
}

func TestGolden_ChunkedBodyLimitRejects(t *testing.T) {
	_, ts := sdkClientAndServer(t)
	base := ts.URL

	// A valid JSON string forces the decoder to consume the full unknown-length
	// body, exercising MaxBytesReader rather than the Content-Length fast path.
	bodyReader := io.MultiReader(
		strings.NewReader(`{"name":"`),
		io.LimitReader(zeroReader{}, (32<<20)+1),
		strings.NewReader(`","model":"claude-opus-4-8"}`),
	)
	req, _ := http.NewRequestWithContext(context.Background(), "POST", base+"/v1/agents", bodyReader)
	req.ContentLength = -1
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, resp.Body)
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 413 {
		t.Fatalf("expected 413 for chunked oversized body, got %d: %s", resp.StatusCode, out)
	}
	assertErrorEnvelope(t, out, "request_too_large")
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func assertErrorEnvelope(t *testing.T, body []byte, wantType string) {
	t.Helper()
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode error envelope: %v (%s)", err, body)
	}
	if env.Type != "error" {
		t.Fatalf("top-level type = %q, want error: %s", env.Type, body)
	}
	if env.Error.Type != wantType {
		t.Fatalf("error.type = %q, want %q: %s", env.Error.Type, wantType, body)
	}
	if env.Error.Message == "" {
		t.Fatalf("error.message empty: %s", body)
	}
}
