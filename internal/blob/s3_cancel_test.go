package blob

import (
	"context"
	"errors"
	"github.com/yanpgwang/mango/internal/app"
	"io"
	"testing"
	"time"
)

type blockedUploadBody struct {
	*io.PipeReader
	entered chan struct{}
}

func (b *blockedUploadBody) Read(p []byte) (int, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	return b.PipeReader.Read(p)
}

func TestS3PutCancellationUnblocksClosableUploadSource(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	renewalFailure := errors.New("database renewal unavailable")
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	body := &blockedUploadBody{PipeReader: reader, entered: make(chan struct{}, 1)}
	store := &S3Store{uploadTempDir: t.TempDir()}
	finished := make(chan error, 1)
	go func() { _, err := store.Put(ctx, "files/canceled", "text/plain", body, 100); finished <- err }()
	<-body.entered
	cancel(renewalFailure)
	select {
	case err := <-finished:
		if !errors.Is(err, renewalFailure) {
			t.Fatalf("upload lost cancellation cause: %v", err)
		}
		if !errors.Is(err, app.ErrBlobNotWritten) {
			t.Fatalf("canceled spool did not identify pre-write failure: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("context cancellation left source Read blocked")
	}
}
