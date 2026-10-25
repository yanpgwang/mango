package mango

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The server sees real HTTP writes. The transport can discard a completed
// response, or fail before sending, without telling the Memory helper which
// happened. The fixture therefore exercises reconciliation rather than retries.
type memoryResponseLoss struct {
	base          http.RoundTripper
	nextWrite     string
	nextListError bool
	writeRequests int
	afterCommit   func()
	replyStatus   int
}

func (f *memoryResponseLoss) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodGet && r.URL.Query().Get("view") == "basic" && f.nextListError {
		f.nextListError = false
		return memoryFaultResponse(r, http.StatusServiceUnavailable), nil
	}
	if r.Method != http.MethodPost {
		return f.base.RoundTrip(r)
	}
	f.writeRequests++
	fault := f.nextWrite
	f.nextWrite = ""
	switch fault {
	case "before_send":
		if r.Body != nil {
			if err := r.Body.Close(); err != nil {
				return nil, err
			}
		}
		return nil, errors.New("test: upload response unavailable")
	case "rejected":
		if r.Body != nil {
			if err := r.Body.Close(); err != nil {
				return nil, err
			}
		}
		return memoryFaultResponse(r, http.StatusBadRequest), nil
	}
	response, err := f.base.RoundTrip(r)
	if err != nil || fault != "after_commit" {
		return response, err
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		_ = response.Body.Close()
		return nil, err
	}
	if err := response.Body.Close(); err != nil {
		return nil, err
	}
	if f.afterCommit != nil {
		f.afterCommit()
	}
	if f.replyStatus != 0 {
		return memoryFaultResponse(r, f.replyStatus), nil
	}
	return nil, errors.New("test: upload response unavailable")
}

func memoryFaultResponse(r *http.Request, status int) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Request: r,
		Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"api_error","message":"test fault"}}`))}
}

type memoryRecoveryFixture struct {
	remote *memorySyncServer
	loss   *memoryResponseLoss
	stores *SessionMemoryStores
	path   string
	now    time.Time
}

func newMemoryRecoveryFixture(t *testing.T, existing bool) *memoryRecoveryFixture {
	t.Helper()
	initial := map[string]string{}
	if existing {
		initial["/notes.txt"] = "A"
	}
	remote, server := newMemorySyncServer(t, initial)
	t.Cleanup(server.Close)
	loss := &memoryResponseLoss{base: http.DefaultTransport}
	client, err := New(Config{BaseURL: server.URL, APIKey: "session-token", HTTPClient: &http.Client{Transport: loss}})
	if err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(t.TempDir(), "memory")
	stores, err := NewSessionMemoryStores(client, SessionMemoryStoresOptions{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.Download(context.Background(), memorySession(mount, "read_write")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stores.Dispose(); err != nil {
			t.Error(err)
		}
	})
	f := &memoryRecoveryFixture{remote: remote, loss: loss, stores: stores,
		path: filepath.Join(mount, "notes.txt"), now: time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)}
	stores.now = func() time.Time { return f.now }
	stores.lastSync = f.now
	return f
}

func (f *memoryRecoveryFixture) write(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(f.path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *memoryRecoveryFixture) sync() {
	f.now = f.now.Add(DefaultMemorySyncInterval)
	f.stores.SyncIfDue(context.Background())
}

func (f *memoryRecoveryFixture) assertContents(t *testing.T, local, remote string, writes int) {
	t.Helper()
	data, err := os.ReadFile(f.path)
	if err != nil || string(data) != local {
		t.Fatalf("local = %q, %v; want %q", data, err, local)
	}
	if got, exists := f.remote.content("/notes.txt"); !exists || got != remote {
		t.Fatalf("remote = %q, exists %v; want %q", got, exists, remote)
	}
	if f.loss.writeRequests != writes {
		t.Fatalf("write requests = %d; want %d", f.loss.writeRequests, writes)
	}
}

func TestSessionMemoryLostResponsePreservesNewerLocalEdit(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "create"
		if existing {
			name = "update"
		}
		t.Run(name, func(t *testing.T) {
			f := newMemoryRecoveryFixture(t, existing)
			f.write(t, "B")
			f.loss.nextWrite = "after_commit"
			// A background process may edit after the request bytes were read.
			f.loss.afterCommit = func() { f.write(t, "C") }
			f.sync()
			f.assertContents(t, "C", "B", 1)
			f.sync()
			f.assertContents(t, "C", "C", 2)
		})
	}
}

func TestSessionMemoryLostResponseCancellationFlush(t *testing.T) {
	for _, fault := range []string{"after_commit", "before_send"} {
		for _, latest := range []string{"A", "C"} {
			t.Run(fault+"/"+latest, func(t *testing.T) {
				f := newMemoryRecoveryFixture(t, true)
				f.write(t, "B")
				f.loss.nextWrite = fault
				f.sync()
				f.write(t, latest)
				f.stores.FlushWrites(context.Background())
				writes := 2
				if fault == "before_send" && latest == "A" {
					writes = 1
				}
				f.assertContents(t, latest, latest, writes)
			})
		}
	}
}

func TestSessionMemoryAmbiguousHTTPReplyPreservesLaterEdit(t *testing.T) {
	for _, status := range []int{http.StatusRequestTimeout, http.StatusServiceUnavailable} {
		for _, latest := range []string{"A", "C"} {
			t.Run(http.StatusText(status)+"/"+latest, func(t *testing.T) {
				f := newMemoryRecoveryFixture(t, true)
				f.write(t, "B")
				f.loss.nextWrite = "after_commit"
				f.loss.replyStatus = status
				f.sync()
				f.assertContents(t, "B", "B", 1)
				f.write(t, latest)
				f.stores.FlushWrites(context.Background())
				f.assertContents(t, latest, latest, 2)
			})
		}
	}
}

func TestSessionMemoryLostResponseStillHonorsRemoteConflict(t *testing.T) {
	f := newMemoryRecoveryFixture(t, true)
	f.write(t, "B")
	f.loss.nextWrite = "after_commit"
	f.sync()
	f.write(t, "C")
	f.remote.replace("/notes.txt", "other writer")
	f.sync()
	f.assertContents(t, "other writer", "other writer", 1)
}

func TestSessionMemoryLostResponseUnchangedContentNeedsNoWrite(t *testing.T) {
	f := newMemoryRecoveryFixture(t, true)
	f.write(t, "B")
	f.loss.nextWrite = "after_commit"
	f.sync()
	f.sync()
	f.assertContents(t, "B", "B", 1)
}

func TestSessionMemoryLostResponseSettlesOnOneSuccessfulListing(t *testing.T) {
	f := newMemoryRecoveryFixture(t, true)
	f.write(t, "B")
	f.loss.nextWrite = "before_send"
	f.sync()
	f.write(t, "A")
	f.sync() // Fresh A proves the attempted B did not remain the current state.
	f.assertContents(t, "A", "A", 1)
	f.remote.replace("/notes.txt", "B")
	f.write(t, "C")
	f.sync()
	f.assertContents(t, "B", "B", 1)
}

func TestSessionMemoryLostResponseSurvivesUnavailableListing(t *testing.T) {
	f := newMemoryRecoveryFixture(t, true)
	f.write(t, "B")
	f.loss.nextWrite = "after_commit"
	f.sync()
	f.write(t, "C")
	f.loss.nextListError = true
	f.sync()
	f.assertContents(t, "C", "B", 1)
	f.sync()
	f.assertContents(t, "C", "C", 2)
}

func TestSessionMemoryRejectedWriteDoesNotClaimLaterRemoteContent(t *testing.T) {
	f := newMemoryRecoveryFixture(t, true)
	f.write(t, "B")
	f.loss.nextWrite = "rejected"
	f.sync()
	f.remote.replace("/notes.txt", "B")
	f.write(t, "C")
	f.sync()
	f.assertContents(t, "B", "B", 1)
}

func TestSessionMemoryLostResponsePreservesCorroboratedDeletion(t *testing.T) {
	f := newMemoryRecoveryFixture(t, true)
	f.write(t, "B")
	f.loss.nextWrite = "after_commit"
	f.sync()
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	f.sync()
	if _, err := os.Stat(f.path); !os.IsNotExist(err) {
		t.Fatalf("deleted local file was restored: %v", err)
	}
	if _, exists := f.remote.content("/notes.txt"); !exists {
		t.Fatal("remote deletion occurred before corroboration")
	}
	f.now = f.now.Add(MemoryDeleteConfirmDelay)
	f.sync()
	if _, exists := f.remote.content("/notes.txt"); exists {
		t.Fatal("corroborated deletion was not propagated")
	}
	if f.loss.writeRequests != 1 {
		t.Fatalf("unexpected repeated upload: %d requests", f.loss.writeRequests)
	}
}

func TestSessionMemoryLostCreateRebuildsEmptiedStore(t *testing.T) {
	f := newMemoryRecoveryFixture(t, true)
	extra := filepath.Join(filepath.Dir(f.path), "extra.txt")
	if err := os.WriteFile(extra, []byte("B"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.loss.nextWrite = "after_commit"
	f.sync()
	if remote, exists := f.remote.content("/extra.txt"); !exists || remote != "B" {
		t.Fatalf("new Memory did not commit before the lost reply: %q, %v", remote, exists)
	}
	for _, path := range []string{f.path, extra} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	f.sync()
	f.assertContents(t, "A", "A", 1)
	if data, err := os.ReadFile(extra); err != nil || string(data) != "B" {
		t.Fatalf("emptied Store did not restore committed create: %q, %v", data, err)
	}
}
