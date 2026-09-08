package httpapi

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"testing"

	mango "github.com/yanpgwang/mango/sdk/go"
)

func TestSDK_FilesLifecycleAndBidirectionalPaging(t *testing.T) {
	service := newTestFileService()
	handler := NewServer(Deps{Files: service}, Config{
		RequireAuth: true,
	}).Handler()
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := mango.New(mango.Config{BaseURL: server.URL, APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	created := make([]mango.File, 0, 5)
	for index := 0; index < 5; index++ {
		file, err := client.Files.Upload(ctx, mango.FileUploadRequest{
			File: mango.Upload{
				Reader:   bytes.NewReader([]byte{byte('a' + index)}),
				Filename: "file-" + string(rune('a'+index)) + ".txt", ContentType: "text/plain",
			},
		})
		if err != nil {
			t.Fatalf("Upload %d: %v", index, err)
		}
		created = append(created, file)
	}

	pager := client.Files.ListAutoPaging(ctx, mango.ListFilesParams{
		Limit: mango.Some[int64](2),
	})
	seen := map[string]bool{}
	for pager.Next() {
		seen[pager.Value().ID] = true
	}
	if err := pager.Err(); err != nil {
		t.Fatalf("ListAutoPaging: %v", err)
	}
	if len(seen) != len(created) {
		t.Fatalf("forward auto-pager saw %d files, want %d", len(seen), len(created))
	}

	beforePager := client.Files.ListAutoPaging(ctx, mango.ListFilesParams{
		BeforeID: mango.Some(created[0].ID), Limit: mango.Some[int64](2),
	})
	beforeSeen := map[string]bool{}
	for beforePager.Next() {
		beforeSeen[beforePager.Value().ID] = true
	}
	if err := beforePager.Err(); err != nil {
		t.Fatalf("before ListAutoPaging: %v", err)
	}
	if len(beforeSeen) != len(created)-1 {
		t.Fatalf("backward auto-pager saw %d files, want %d", len(beforeSeen), len(created)-1)
	}

	metadata, err := client.Files.Get(ctx, created[2].ID)
	if err != nil || metadata.ID != created[2].ID {
		t.Fatalf("GetMetadata = %+v, %v", metadata, err)
	}
	output, err := client.Files.Upload(ctx, mango.FileUploadRequest{File: mango.Upload{Filename: "result.txt", ContentType: "text/plain", Reader: bytes.NewBufferString("result")}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Files.Download(ctx, output.ID)
	if err != nil {
		t.Fatalf("Download output: %v", err)
	}
	if got := response.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("download X-Content-Type-Options = %q", got)
	}
	if got := response.Header.Get("Content-Disposition"); got != `attachment; filename=result.txt` {
		t.Fatalf("download Content-Disposition = %q", got)
	}
	body, err := io.ReadAll(response)
	if closeErr := response.Close(); err == nil {
		err = closeErr
	}
	if err != nil || string(body) != "result" {
		t.Fatalf("download body = %q, %v", body, err)
	}

	deleted, err := client.Files.Delete(ctx, created[1].ID)
	if err != nil || deleted.ID != created[1].ID || deleted.Type != "file_deleted" {
		t.Fatalf("Delete = %+v, %v", deleted, err)
	}
	_, err = client.Files.Get(ctx, created[1].ID)
	assertAPIStatus(t, err, 404)
}

type namedFileReader struct {
	*bytes.Reader
	name     string
	mimeType string
}

func (r *namedFileReader) Name() string        { return r.name }
func (r *namedFileReader) ContentType() string { return r.mimeType }
