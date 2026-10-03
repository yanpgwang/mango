package blob

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestS3PutMakesOneRemoteAttempt(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		if attempts.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "<Error><Code>SlowDown</Code><Message>uncertain first attempt</Message></Error>")
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := NewS3Store(ctx, S3Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "test", AccessKey: "test", SecretKey: "test", UsePathStyle: true, UploadTempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Put(ctx, "files/one-attempt", "text/plain", bytes.NewBufferString("bytes"), 100)
	if err == nil || attempts.Load() != 1 {
		t.Fatalf("uncertain write automatically retried: attempts=%d err=%v", attempts.Load(), err)
	}
}
