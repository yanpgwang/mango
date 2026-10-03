package mango

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestToolRunnerPreparesFullMCPOutputFromPagedHistoryBeforeDispatch(t *testing.T) {
	text := strings.Repeat("x", 100001) + "tail-proof"
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
	root := t.TempDir()
	var failures atomic.Int32
	tool := sessionToolUseJSON("tool_read", "read", "allow", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/events/stream"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case r.URL.Path == "/v1/sessions/sesn_one/events" && r.Method == http.MethodGet:
			if r.URL.Query().Get("page") == "" {
				fmt.Fprint(w, `{"data":[{"id":"sevt_mcp_result","type":"agent.mcp_tool_result","processed_at":null,"mcp_tool_use_id":"sevt_mcp","file_id":"file_full","content":[{"type":"text","text":"preview"}]}],"next_page":"two"}`)
			} else {
				fmt.Fprintf(w, `{"data":[%s],"next_page":null}`, tool)
			}
		case r.URL.Path == "/v1/files/file_full":
			fmt.Fprintf(w, `{"id":"file_full","type":"file","created_at":"2026-10-03T00:00:00Z","filename":"file_full.txt","mime_type":"text/plain","size_bytes":%d,"checksum_sha256":"%s"}`, len(text), sum)
		case r.URL.Path == "/v1/files/file_full/content":
			if failures.Add(1) == 1 {
				http.Error(w, "temporary outage", 503)
				return
			}
			fmt.Fprint(w, text)
		case r.Method == http.MethodPost:
			fmt.Fprint(w, `{"data":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	runner := NewSessionToolRunner(context.Background(), newSessionToolRunnerClient(t, server.URL), "sesn_one", SessionToolRunnerOptions{Workdir: root, MaxIdle: durationPointer(0), Tools: []SessionTool{sessionToolFunc{name: "read", run: func(_ context.Context, _ SessionToolCall) ([]ResultContentInput, error) {
		data, err := os.ReadFile(filepath.Join(root, sessionToolOutputDirectory, "file_full.txt"))
		if err != nil || string(data) != text {
			t.Errorf("tool ran without complete output: bytes=%d err=%v", len(data), err)
		}
		return textSessionToolResult("tail-proof"), nil
	}}}})
	defer runner.Close()
	runner.retryDelay = func(int) time.Duration { return time.Millisecond }
	if !runner.Next() || !runner.Current().Posted || runner.Current().IsError {
		t.Fatalf("dispatch failed: %v", runner.Err())
	}
	if failures.Load() != 2 {
		t.Fatalf("download attempts=%d", failures.Load())
	}
}

func TestToolOutputDownloadRejectsTamperingAndUnsafePaths(t *testing.T) {
	expected := "correct"
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(expected)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/content") {
			fmt.Fprint(w, "corrupt")
			return
		}
		fmt.Fprintf(w, `{"id":"file_full","type":"file","mime_type":"text/plain","size_bytes":7,"checksum_sha256":"%s"}`, sum)
	}))
	defer server.Close()
	root := t.TempDir()
	setup, err := newSessionToolOutputs(root)
	if err != nil {
		t.Fatal(err)
	}
	defer setup.root.Close()
	client := newSessionToolRunnerClient(t, server.URL)
	if err := setup.materialize(context.Background(), client, "file_full"); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	if _, err := os.Stat(filepath.Join(root, sessionToolOutputDirectory, "file_full.txt")); !os.IsNotExist(err) {
		t.Fatal("corrupt destination published")
	}
	if err := setup.materialize(context.Background(), client, "file_../escape"); err == nil {
		t.Fatal("unsafe ID accepted")
	}
	outside := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(outside, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, sessionToolOutputDirectory, "file_full.txt")); err != nil {
		t.Fatal(err)
	}
	if err := setup.materialize(context.Background(), client, "file_full"); err == nil {
		t.Fatal("symlink destination accepted")
	}
	data, _ := os.ReadFile(outside)
	if string(data) != "unchanged" {
		t.Fatal("outside file changed")
	}
	linked := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(linked, sessionToolOutputDirectory)); err != nil {
		t.Fatal(err)
	}
	if _, err := newSessionToolOutputs(linked); err == nil {
		t.Fatal("symlink directory accepted")
	}
}

func TestToolRunnerRetriesLiveMCPFileFailureBeforeLocalDispatch(t *testing.T) {
	text := "full-live-tail"
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
	root := t.TempDir()
	var attempts atomic.Int32
	mcp := `{"id":"sevt_live_mcp","type":"agent.mcp_tool_result","processed_at":null,"mcp_tool_use_id":"sevt_use","file_id":"file_live","content":[{"type":"text","text":"preview"}]}`
	tool := sessionToolUseJSON("tool_live_read", "read", "allow", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/events/stream"):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: agent.mcp_tool_result\ndata: %s\n\nevent: agent.tool_use\ndata: %s\n\n", mcp, tool)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, "/events") && r.Method == http.MethodGet:
			// First attach starts without history; reconnect replays the committed pair.
			if attempts.Load() == 0 {
				fmt.Fprint(w, `{"data":[],"next_page":null}`)
			} else {
				fmt.Fprintf(w, `{"data":[%s,%s],"next_page":null}`, mcp, tool)
			}
		case r.URL.Path == "/v1/files/file_live":
			fmt.Fprintf(w, `{"id":"file_live","type":"file","mime_type":"text/plain","size_bytes":%d,"checksum_sha256":"%s"}`, len(text), sum)
		case r.URL.Path == "/v1/files/file_live/content":
			if attempts.Add(1) == 1 {
				http.Error(w, "outage", 503)
				return
			}
			fmt.Fprint(w, text)
		case r.Method == http.MethodPost:
			fmt.Fprint(w, `{"data":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	runner := NewSessionToolRunner(context.Background(), newSessionToolRunnerClient(t, server.URL), "sesn_live", SessionToolRunnerOptions{Workdir: root, MaxIdle: durationPointer(0), Tools: []SessionTool{sessionToolFunc{name: "read", run: func(_ context.Context, _ SessionToolCall) ([]ResultContentInput, error) {
		data, err := os.ReadFile(filepath.Join(root, sessionToolOutputDirectory, "file_live.txt"))
		if err != nil || string(data) != text {
			t.Errorf("live tool ran before full output: %q %v", data, err)
		}
		return textSessionToolResult(text), nil
	}}}})
	defer runner.Close()
	runner.retryDelay = func(int) time.Duration { return time.Millisecond }
	if !runner.Next() || !runner.Current().Posted {
		t.Fatalf("live output did not recover: attempts=%d err=%v", attempts.Load(), runner.Err())
	}
	if attempts.Load() != 2 {
		t.Fatalf("download attempts=%d", attempts.Load())
	}
}
