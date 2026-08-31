package mango

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(Config{BaseURL: server.URL + "/proxy", APIKey: "test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestAllOpenAPIOperationsHaveNamedMethods(t *testing.T) {
	data, err := os.ReadFile("../operations.json")
	if os.IsNotExist(err) {
		t.Skip("contract coverage runs in repository checkout")
	}
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Operations []struct{ ID, Method, Path string }
	}
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatal(err)
	}
	if len(Operations) != len(contract.Operations) {
		t.Fatalf("operations: got %d, want %d", len(Operations), len(contract.Operations))
	}
	typ := reflect.TypeOf(&Client{})
	metadata := make(map[string]Operation)
	for _, operation := range Operations {
		if _, exists := metadata[operation.ID]; exists {
			t.Fatalf("duplicate %s", operation.ID)
		}
		metadata[operation.ID] = operation
		name := strings.ToUpper(operation.ID[:1]) + operation.ID[1:]
		if _, ok := typ.MethodByName(name); !ok {
			t.Errorf("missing typed method %s", name)
		}
	}
	for _, want := range contract.Operations {
		got := metadata[want.ID]
		if got.Method != want.Method || got.Path != want.Path {
			t.Errorf("%s: got %#v, want %#v", want.ID, got, want)
		}
	}
}

func TestBaseURLPathAuthAndRepeatedQuery(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/proxy/v1/sessions/a%2Fb%3Fc%23d/events" {
			t.Errorf("path %s", r.URL.EscapedPath())
		}
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("missing auth")
		}
		if got := r.URL.Query()["types[]"]; !reflect.DeepEqual(got, []string{"agent.message", "session.status_idle"}) {
			t.Errorf("types %v", got)
		}
		if r.URL.Query().Get("limit") != "0" {
			t.Error("explicit zero query was omitted")
		}
		fmt.Fprint(w, `{"data":[],"next_page":null}`)
	})
	_, err := client.ListSessionEvents(context.Background(), "a/b?c#d", ListSessionEventsParams{Types: Some([]CoreSessionEventType{CoreSessionEventTypeAgentMessage, CoreSessionEventTypeSessionStatusIdle}), Limit: Some(int64(0))})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTypedCreateAndNullableUpdate(t *testing.T) {
	var calls int
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" {
			t.Error(r.Method)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if calls == 1 {
			if string(body["model"]) != `"model-test"` {
				t.Errorf("model union %s", body["model"])
			}
			if string(body["system"]) != `"Be helpful"` {
				t.Errorf("system %s", body["system"])
			}
			if _, exists := body["description"]; exists {
				t.Error("unset description was included")
			}
		} else {
			if string(body["description"]) != "null" {
				t.Errorf("nullable description %s", body["description"])
			}
			if _, exists := body["name"]; exists {
				t.Error("unset name included")
			}
		}
		fmt.Fprint(w, `{"id":"agent_1","name":"test","multiagent":null}`)
	})
	agent, err := client.CreateAgent(context.Background(), AgentCreateRequest{Name: "test", Model: ModelInput{String: Ptr("model-test")}, System: SomePtr("Be helpful")})
	if err != nil || agent.ID != "agent_1" {
		t.Fatalf("agent %#v, %v", agent, err)
	}
	_, err = client.UpdateAgent(context.Background(), agent.ID, AgentUpdateRequest{Description: Null[*string]()})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTypedAPIErrorAndNoWriteRetry(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("request-id", "req-header")
		w.WriteHeader(503)
		fmt.Fprint(w, `{"type":"error","error":{"type":"overloaded_error","message":"try later"},"request_id":"req-body"}`)
	})
	_, err := client.CreateAgent(context.Background(), AgentCreateRequest{Name: "test", Model: ModelInput{String: Ptr("m")}})
	var apiError *APIError
	if !errors.As(err, &apiError) {
		t.Fatalf("not API error: %v", err)
	}
	if apiError.StatusCode != 503 || apiError.RequestID != "req-header" || apiError.Type != "overloaded_error" || apiError.Message != "try later" {
		t.Fatalf("%+v", apiError)
	}
	if calls.Load() != 1 {
		t.Fatalf("write retried %d times", calls.Load())
	}
}

func TestRedirectsDoNotForwardCredentials(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer target.Close()
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	_, err := client.GetAgent(context.Background(), "a")
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != 307 {
		t.Fatalf("redirect result %v", err)
	}
	if leaked.Load() {
		t.Fatal("followed redirect")
	}
}

func TestPublicRoutesDoNotSendBearer(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("public route sent credential")
		}
		fmt.Fprint(w, "ok")
	})
	if err := client.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.Readiness(context.Background()); err != nil {
		t.Fatal(err)
	}
	download, err := client.OpenAPI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer download.Close()
	content, _ := io.ReadAll(download)
	if string(content) != "ok" {
		t.Errorf("%q", content)
	}
}

func TestContextAndFiniteTimeout(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	client.requestTimeout = 20 * time.Millisecond
	_, err := client.GetAgent(context.Background(), "a")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.GetAgent(ctx, "a")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel %v", err)
	}
}

func TestMultipartFilesAndSkillPaths(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		reader, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			return
		}
		var contents []string
		var dispositions []string
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Error(err)
				return
			}
			data, _ := io.ReadAll(part)
			contents = append(contents, string(data))
			dispositions = append(dispositions, part.Header.Get("Content-Disposition"))
		}
		if strings.HasSuffix(r.URL.Path, "/skills") {
			if !reflect.DeepEqual(contents, []string{"Skill title", "# Instructions", "data"}) {
				t.Errorf("skill content %v", contents)
			}
			if !strings.Contains(dispositions[2], `filename="references/input.txt"`) {
				t.Errorf("skill path lost: %s", dispositions[2])
			}
			fmt.Fprint(w, `{"id":"skill_1"}`)
		} else {
			if !reflect.DeepEqual(contents, []string{"test\x00bytes"}) {
				t.Errorf("file %v", contents)
			}
			fmt.Fprint(w, `{"id":"file_1"}`)
		}
	})
	file, err := client.UploadFile(context.Background(), FileUploadRequest{File: Upload{Filename: "file.bin", Reader: strings.NewReader("test\x00bytes")}})
	if err != nil || file.ID != "file_1" {
		t.Fatalf("file %#v %v", file, err)
	}
	skill, err := client.CreateSkill(context.Background(), SkillUploadRequest{DisplayTitle: Some("Skill title"), Files: []Upload{{Filename: "SKILL.md", Reader: strings.NewReader("# Instructions")}, {Filename: "references/input.txt", Reader: strings.NewReader("data")}}})
	if err != nil || skill.ID != "skill_1" {
		t.Fatalf("skill %#v %v", skill, err)
	}
}

func TestStreamingDownloadPreservesHeaders(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="x.bin"`)
		fmt.Fprint(w, "\x00\xffbinary")
	})
	download, err := client.DownloadFile(context.Background(), "file_1")
	if err != nil {
		t.Fatal(err)
	}
	defer download.Close()
	data, err := io.ReadAll(download)
	if err != nil || string(data) != "\x00\xffbinary" {
		t.Fatalf("download %q %v", data, err)
	}
	if download.Header.Get("Content-Disposition") == "" {
		t.Error("lost filename")
	}
}

func TestMultipartProducerStopsWhenRequestPipeCloses(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	done := make(chan error, 1)
	go func() {
		err := writeMultipart(multipart.NewWriter(writer), []multipartPart{{name: "file", upload: &Upload{Filename: "large.bin", Reader: strings.NewReader(strings.Repeat("x", 1<<20))}}})
		_ = writer.CloseWithError(err)
		done <- err
	}()
	buffer := make([]byte, 16)
	if _, err := io.ReadFull(reader, buffer); err != nil {
		t.Fatal(err)
	}
	// The request path closes this pipe both on an early HTTP response and on
	// cancellation. Ordinary source readers must not leave the producer blocked.
	_ = reader.CloseWithError(context.Canceled)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("producer error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("multipart producer leaked after pipe close")
	}
}

func TestMultipartEarlyErrorAndCancellation(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("unused") != "" {
			t.Error("unexpected query")
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		fmt.Fprint(w, `{"error":{"type":"request_too_large","message":"too large"}}`)
	})
	_, err := client.UploadFile(context.Background(), FileUploadRequest{File: Upload{Filename: "large.bin", Reader: strings.NewReader(strings.Repeat("x", 1<<20))}})
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != 413 {
		t.Fatalf("early rejection: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.UploadFile(ctx, FileUploadRequest{File: Upload{Filename: "large.bin", Reader: strings.NewReader(strings.Repeat("x", 1<<20))}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("upload cancellation: %v", err)
	}
}

func TestInvalidBaseURLsAndDotSegments(t *testing.T) {
	for _, endpoint := range []string{"", "/local", "file:///path", "https://user:password@example.com", "https://example.com?x=1", "https://example.com/#x"} {
		if _, err := New(Config{BaseURL: endpoint}); err == nil {
			t.Errorf("accepted %q", endpoint)
		}
	}
	if escapePath("..") != "%2E%2E" || escapePath(".") != "%2E" {
		t.Fatal("dot segments not escaped")
	}
}
