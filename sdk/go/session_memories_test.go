package mango

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type memorySyncServer struct {
	t      *testing.T
	mu     sync.Mutex
	nextID int
	byID   map[string]Memory
}

func newMemorySyncServer(t *testing.T, initial map[string]string) (*memorySyncServer, *httptest.Server) {
	t.Helper()
	fixture := &memorySyncServer{t: t, byID: map[string]Memory{}}
	for path, content := range initial {
		fixture.create(path, content)
	}
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	return fixture, server
}

func (s *memorySyncServer) create(path, content string) Memory {
	s.nextID++
	id := "mem_" + string(rune('a'+s.nextID-1))
	value := content
	item := Memory{
		ID: id, Type: "memory", MemoryStoreID: "store_test", MemoryVersionID: "ver_" + id,
		Path: MemoryPath(path), Content: &value, ContentSizeBytes: int64(len(content)),
		ContentSHA256: MemoryContentSHA256(memorySHA([]byte(content))),
		CreatedAt:     "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}
	s.byID[id] = item
	return item
}

func (s *memorySyncServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer session-token" {
		http.Error(w, "missing scoped token", http.StatusUnauthorized)
		return
	}
	prefix := "/v1/memory_stores/store_test/memories"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == prefix:
		full := r.URL.Query().Get("view") == "full"
		data := make([]Memory, 0, len(s.byID))
		for _, item := range s.byID {
			copy := item
			if !full {
				copy.Content = nil
			}
			data = append(data, copy)
		}
		writeMemoryJSON(s.t, w, map[string]any{"data": data, "has_more": false, "next_page": nil})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, prefix+"/"):
		id := strings.TrimPrefix(r.URL.Path, prefix+"/")
		item, ok := s.byID[id]
		if !ok {
			http.Error(w, `{"type":"error","error":{"type":"not_found_error","message":"missing"}}`, http.StatusNotFound)
			return
		}
		writeMemoryJSON(s.t, w, item)
	case r.Method == http.MethodPost && r.URL.Path == prefix:
		var body MemoryCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.t.Errorf("decode create: %v", err)
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		for _, item := range s.byID {
			if item.Path == body.Path {
				http.Error(w, `{"type":"error","error":{"type":"conflict_error","message":"exists"}}`, http.StatusConflict)
				return
			}
		}
		writeMemoryJSON(s.t, w, s.create(string(body.Path), body.Content))
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, prefix+"/"):
		id := strings.TrimPrefix(r.URL.Path, prefix+"/")
		current, ok := s.byID[id]
		if !ok {
			http.Error(w, `{"type":"error","error":{"type":"not_found_error","message":"missing"}}`, http.StatusNotFound)
			return
		}
		var body MemoryUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.t.Errorf("decode update: %v", err)
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		if precondition, present := body.Precondition.Get(); present && precondition.ContentSHA256 != current.ContentSHA256 {
			http.Error(w, `{"type":"error","error":{"type":"memory_precondition_error","message":"changed"}}`, http.StatusConflict)
			return
		}
		if content, present := body.Content.Get(); present {
			current.Content = &content
			current.ContentSizeBytes = int64(len(content))
			current.ContentSHA256 = MemoryContentSHA256(memorySHA([]byte(content)))
		}
		s.byID[id] = current
		writeMemoryJSON(s.t, w, current)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, prefix+"/"):
		id := strings.TrimPrefix(r.URL.Path, prefix+"/")
		current, ok := s.byID[id]
		if !ok {
			http.Error(w, `{"type":"error","error":{"type":"not_found_error","message":"missing"}}`, http.StatusNotFound)
			return
		}
		if expected := r.URL.Query().Get("expected_content_sha256"); expected != "" && expected != string(current.ContentSHA256) {
			http.Error(w, `{"type":"error","error":{"type":"memory_precondition_error","message":"changed"}}`, http.StatusConflict)
			return
		}
		delete(s.byID, id)
		writeMemoryJSON(s.t, w, map[string]any{"id": id, "type": "memory_deleted"})
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusNotFound)
	}
}

func writeMemoryJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func (s *memorySyncServer) content(path string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.byID {
		if string(item.Path) == path && item.Content != nil {
			return *item.Content, true
		}
	}
	return "", false
}

func (s *memorySyncServer) replace(path, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, item := range s.byID {
		if string(item.Path) != path {
			continue
		}
		value := content
		item.Content = &value
		item.ContentSizeBytes = int64(len(content))
		item.ContentSHA256 = MemoryContentSHA256(memorySHA([]byte(content)))
		s.byID[id] = item
		return
	}
	s.create(path, content)
}

func (s *memorySyncServer) remove(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, item := range s.byID {
		if string(item.Path) == path {
			delete(s.byID, id)
		}
	}
}

func memorySession(mount string, access string) Session {
	return Session{Resources: []SessionResource{{MemoryStoreSessionResource: &MemoryStoreSessionResource{
		Type: "memory_store", MemoryStoreID: "store_test", Name: "Test",
		Description: "", MountPath: mount, Access: access,
	}}}}
}

func TestSessionMemoryStoresDownloadsSyncsAndDisposes(t *testing.T) {
	fixture, server := newMemorySyncServer(t, map[string]string{"/notes/a.txt": "one"})
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, APIKey: "session-token"})
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	mount := filepath.Join(parent, "store")
	stores, err := NewSessionMemoryStores(client, SessionMemoryStoresOptions{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.Download(context.Background(), memorySession(mount, "read_write")); err != nil {
		t.Fatal(err)
	}
	if got := stores.Roots(); len(got) != 1 || got[0] != mount {
		t.Fatalf("roots = %v", got)
	}
	if data, err := os.ReadFile(filepath.Join(mount, "notes", "a.txt")); err != nil || string(data) != "one" {
		t.Fatalf("downloaded file = %q, %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(mount, "notes", "a.txt"), []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "new.txt"), []byte("created"), 0o600); err != nil {
		t.Fatal(err)
	}
	stores.Finish(context.Background())
	if content, ok := fixture.content("/notes/a.txt"); !ok || content != "local" {
		t.Fatalf("updated remote = %q, %v", content, ok)
	}
	if content, ok := fixture.content("/new.txt"); !ok || content != "created" {
		t.Fatalf("created remote = %q, %v", content, ok)
	}
	if err := stores.Dispose(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(mount); !os.IsNotExist(err) {
		t.Fatalf("mount still exists: %v", err)
	}
}

func TestSessionMemoryStoresStoreWinsConflictAndFinalDelete(t *testing.T) {
	fixture, server := newMemorySyncServer(t, map[string]string{"/facts.txt": "base", "/delete.txt": "gone"})
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, APIKey: "session-token"})
	mount := filepath.Join(t.TempDir(), "store")
	stores, err := NewSessionMemoryStores(client, SessionMemoryStoresOptions{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.Download(context.Background(), memorySession(mount, "read_write")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "facts.txt"), []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(mount, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	fixture.replace("/facts.txt", "remote")
	stores.Finish(context.Background())
	if data, err := os.ReadFile(filepath.Join(mount, "facts.txt")); err != nil || string(data) != "remote" {
		t.Fatalf("conflict winner = %q, %v", data, err)
	}
	if _, ok := fixture.content("/delete.txt"); ok {
		t.Fatal("final sync did not propagate the corroborated local deletion")
	}
	if err := stores.Dispose(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionMemoryStoresReadOnlyNeverUploads(t *testing.T) {
	fixture, server := newMemorySyncServer(t, map[string]string{"/facts.txt": "fixed"})
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, APIKey: "session-token"})
	mount := filepath.Join(t.TempDir(), "store")
	stores, err := NewSessionMemoryStores(client, SessionMemoryStoresOptions{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.Download(context.Background(), memorySession(mount, "read_only")); err != nil {
		t.Fatal(err)
	}
	if roots := stores.ReadOnlyRoots(); len(roots) != 1 || roots[0] != mount {
		t.Fatalf("read-only roots = %v", roots)
	}
	if err := os.WriteFile(filepath.Join(mount, "facts.txt"), []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	stores.Finish(context.Background())
	if content, _ := fixture.content("/facts.txt"); content != "fixed" {
		t.Fatalf("read-only Store content = %q", content)
	}
	if err := stores.Dispose(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionMemoryStoresReadOnlyPullsRecreatedRemoteWithOldChecksum(t *testing.T) {
	fixture, server := newMemorySyncServer(t, map[string]string{"/facts.txt": "fixed"})
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, APIKey: "session-token"})
	mount := filepath.Join(t.TempDir(), "store")
	stores, err := NewSessionMemoryStores(client, SessionMemoryStoresOptions{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.Download(context.Background(), memorySession(mount, "read_only")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "facts.txt"), []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	stores.now = func() time.Time { return now }
	fixture.remove("/facts.txt")
	stores.lastSync = now.Add(-DefaultMemorySyncInterval)
	stores.SyncIfDue(context.Background())
	fixture.replace("/facts.txt", "fixed")
	now = now.Add(DefaultMemorySyncInterval)
	stores.SyncIfDue(context.Background())
	if data, err := os.ReadFile(filepath.Join(mount, "facts.txt")); err != nil || string(data) != "fixed" {
		t.Fatalf("recreated remote did not replace read-only local edit: %q, %v", data, err)
	}
	if err := stores.Dispose(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionMemoryStoresCorroboratesPeriodicDeletion(t *testing.T) {
	fixture, server := newMemorySyncServer(t, map[string]string{"/delete.txt": "gone"})
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, APIKey: "session-token"})
	mount := filepath.Join(t.TempDir(), "store")
	stores, err := NewSessionMemoryStores(client, SessionMemoryStoresOptions{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.Download(context.Background(), memorySession(mount, "read_write")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(mount, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	stores.now = func() time.Time { return now }
	stores.lastSync = now.Add(-DefaultMemorySyncInterval)
	stores.SyncIfDue(context.Background())
	if _, ok := fixture.content("/delete.txt"); !ok {
		t.Fatal("first periodic observation deleted Memory without corroboration")
	}
	now = now.Add(MemoryDeleteConfirmDelay + DefaultMemorySyncInterval)
	stores.SyncIfDue(context.Background())
	if _, ok := fixture.content("/delete.txt"); ok {
		t.Fatal("corroborated periodic deletion was not uploaded")
	}
	if err := stores.Dispose(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionMemoryStoresDoesNotRemoveDistrustedDirectory(t *testing.T) {
	_, server := newMemorySyncServer(t, map[string]string{"/notes.txt": "memory"})
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, APIKey: "session-token"})
	mount := filepath.Join(t.TempDir(), "store")
	stores, err := NewSessionMemoryStores(client, SessionMemoryStoresOptions{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.Download(context.Background(), memorySession(mount, "read_write")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(mount, MemoryMarkerPath)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "operator.txt"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stores.Dispose(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(mount, "operator.txt"))
	if err != nil || string(data) != "preserve" {
		t.Fatalf("distrusted directory was changed: %q, %v", data, err)
	}
}

func TestSessionMemoryStoresSkipsOversizedFileWithoutBlockingStore(t *testing.T) {
	fixture, server := newMemorySyncServer(t, map[string]string{"/good.txt": "initial"})
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, APIKey: "session-token"})
	mount := filepath.Join(t.TempDir(), "store")
	stores, err := NewSessionMemoryStores(client, SessionMemoryStoresOptions{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.Download(context.Background(), memorySession(mount, "read_write")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "good.txt"), []byte("updated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "too-large.txt"), bytes.Repeat([]byte("x"), int(maxSessionMemoryBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	stores.Finish(context.Background())
	if content, _ := fixture.content("/good.txt"); content != "updated" {
		t.Fatalf("valid file was blocked by oversized neighbor: %q", content)
	}
	if _, exists := fixture.content("/too-large.txt"); exists {
		t.Fatal("oversized file was uploaded")
	}
	if err := stores.Dispose(); err != nil {
		t.Fatal(err)
	}
}
