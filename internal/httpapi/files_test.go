package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

func TestMultipartUploadSourceCloseInterruptsPartRead(t *testing.T) {
	source, writer := io.Pipe()
	defer func() { _ = source.Close() }()
	defer func() { _ = writer.Close() }()
	multipartWriter := multipart.NewWriter(writer)
	go func() { _, _ = multipartWriter.CreateFormFile("file", "blocked.txt") }()
	reader := multipart.NewReader(source, multipartWriter.Boundary())
	part, err := reader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	body := &singleMultipartPartReader{source: source, part: part, multipart: reader}
	finished := make(chan error, 1)
	go func() { _, err := body.Read(make([]byte, 1)); finished <- err }()
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("closed HTTP source returned success")
		}
	case <-time.After(time.Second):
		t.Fatal("closing upload reader did not interrupt multipart Read")
	}
}

type cancelHTTPUploadService struct {
	FileService
	started  chan struct{}
	cancel   chan struct{}
	finished chan error
}

func (s *cancelHTTPUploadService) Upload(_ context.Context, input app.FileUploadInput) (domain.File, error) {
	go func() { <-s.cancel; _ = input.Body.(io.Closer).Close() }()
	close(s.started)
	_, err := io.ReadAll(input.Body)
	s.finished <- err
	return domain.File{}, err
}

func TestFilesHTTP_CancelInterruptsRealHTTP1Upload(t *testing.T) {
	service := &cancelHTTPUploadService{started: make(chan struct{}), cancel: make(chan struct{}), finished: make(chan error, 1)}
	server := httptest.NewServer(NewServer(Deps{Files: service}, Config{}).Handler())
	defer server.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	prefix := "--stall\r\nContent-Disposition: form-data; name=\"file\"; filename=\"blocked.txt\"\r\nContent-Type: text/plain\r\n\r\n"
	request := "POST /v1/files HTTP/1.1\r\nHost: localhost\r\nContent-Type: multipart/form-data; boundary=stall\r\nContent-Length: 1048576\r\n\r\n" + prefix
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	select {
	case <-service.started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start upload")
	}
	close(service.cancel)
	select {
	case err := <-service.finished:
		if err == nil {
			t.Fatal("canceled upload returned success")
		}
	case <-time.After(time.Second):
		t.Fatal("actual HTTP Body.Close left multipart Read blocked")
	}
}

func TestFilesHTTP_UploadShapeAndMultipartValidation(t *testing.T) {
	service := newTestFileService()
	handler := NewServer(Deps{Files: service}, Config{}).Handler()

	body, contentType := multipartUpload(t, "report.txt", "text/plain", []byte("hello"), false)
	req := httptest.NewRequest(http.MethodPost, "/v1/files", body)
	req.Header.Set("content-type", contentType)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d: %s", rec.Code, rec.Body.String())
	}
	assertJSONFields(t, rec.Body.Bytes(), map[string]any{
		"type": "file", "filename": "report.txt", "mime_type": "text/plain",
		"size_bytes": float64(5),
	})
	var uploaded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	if len(uploaded) != 7 {
		t.Fatalf("File metadata contains unexpected fields: %s", rec.Body.Bytes())
	}
	assertRawObjectHasFields(t, rec.Body.String(), "id", "type", "created_at", "filename", "mime_type", "size_bytes")
	fileID, ok := uploaded["id"].(string)
	if !ok || fileID == "" {
		t.Fatal("File metadata omitted id")
	}
	download := httptest.NewRecorder()
	handler.ServeHTTP(download, httptest.NewRequest(http.MethodGet, "/v1/files/"+fileID+"/content", nil))
	if download.Code != http.StatusOK || download.Body.String() != "hello" {
		t.Fatalf("uploaded bytes could not be downloaded: %d %s", download.Code, download.Body.String())
	}

	body, contentType = multipartUpload(t, "extra.txt", "text/plain", []byte("x"), true)
	req = httptest.NewRequest(http.MethodPost, "/v1/files", body)
	req.Header.Set("content-type", contentType)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("extra part = %d: %s", rec.Code, rec.Body.String())
	}
	if len(service.files) != 1 {
		t.Fatalf("invalid multipart committed file: %+v", service.files)
	}

	malformed := strings.NewReader("--broken\r\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"broken.txt\"\r\n" +
		"Content-Type: text/plain\r\n\r\npartial")
	req = httptest.NewRequest(http.MethodPost, "/v1/files", malformed)
	req.Header.Set("content-type", "multipart/form-data; boundary=broken")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed multipart = %d: %s", rec.Code, rec.Body.String())
	}
	if len(service.files) != 1 {
		t.Fatalf("malformed multipart committed file: %+v", service.files)
	}

	body, contentType = multipartUpload(t, "../secret", "text/plain", []byte("x"), false)
	req = httptest.NewRequest(http.MethodPost, "/v1/files", body)
	req.Header.Set("content-type", contentType)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsafe filename = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFilesHTTP_BearerMultipartAndLimit(t *testing.T) {
	service := newTestFileService()
	handler := NewServer(Deps{Files: service}, Config{
		RequireAuth: true,
	}).Handler()
	body, contentType := multipartUpload(t, "strict.txt", "text/plain", []byte("ok"), false)

	req := httptest.NewRequest(http.MethodPost, "/v1/files", body)
	req.Header.Set("content-type", contentType)
	req.Header.Set("x-api-key", "sk-test")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("x-api-key-only upload = %d: %s", rec.Code, rec.Body.String())
	}

	body, contentType = multipartUpload(t, "strict.txt", "text/plain", []byte("ok"), false)
	req = httptest.NewRequest(http.MethodPost, "/v1/files", body)
	req.Header.Set("content-type", contentType)
	req.Header.Set("authorization", "Bearer sk-test")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer upload = %d: %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/files", bytes.NewReader(nil))
	req.ContentLength = maxFileRequestBytes + 1
	rec = httptest.NewRecorder()
	NewServer(Deps{Files: service}, Config{}).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize request = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFilesHTTP_DisabledWithoutObjectStore(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/files", nil)
	rec := httptest.NewRecorder()
	NewServer(Deps{}, Config{}).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("disabled Files = %d: %s", rec.Code, rec.Body.String())
	}
}

func multipartUpload(
	t *testing.T,
	filename string,
	contentType string,
	data []byte,
	extra bool,
) (*bytes.Buffer, string) {
	t.Helper()
	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	header := make(map[string][]string)
	header["Content-Disposition"] = []string{`form-data; name="file"; filename="` + filename + `"`}
	header["Content-Type"] = []string{contentType}
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if extra {
		if err := writer.WriteField("unexpected", "value"); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body, writer.FormDataContentType()
}

func assertJSONFields(t *testing.T, body []byte, expected map[string]any) {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatalf("decode response: %v: %s", err, body)
	}
	for key, want := range expected {
		if got := object[key]; !reflect.DeepEqual(got, want) {
			t.Errorf("field %s = %#v, want %#v", key, got, want)
		}
	}
}

type testFileService struct {
	mu       sync.Mutex
	next     int
	files    map[string]domain.File
	contents map[string][]byte
}

func newTestFileService() *testFileService {
	return &testFileService{files: map[string]domain.File{}, contents: map[string][]byte{}}
}

func (s *testFileService) Upload(_ context.Context, input app.FileUploadInput) (domain.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.ContainsAny(input.Filename, `/\\<>:"|?*`) {
		return domain.File{}, domain.Validation("filename contains a forbidden character")
	}
	data, err := io.ReadAll(input.Body)
	if err != nil {
		return domain.File{}, err
	}
	s.next++
	id := "file_" + strconv.Itoa(s.next)
	file := domain.File{
		ID: id, CreatedAt: time.Date(2026, 8, 4, 0, 0, s.next, 0, time.UTC),
		UpdatedAt: time.Date(2026, 8, 4, 0, 0, s.next, 0, time.UTC),
		Filename:  input.Filename, MimeType: input.MimeType, SizeBytes: int64(len(data)),
		BlobKey: "files/" + id, State: domain.FileStateReady, ChecksumSHA256: app.ComputeBlobInfo(data).ChecksumSHA256,
	}
	s.files[id], s.contents[id] = file, data
	return file, nil
}

func (s *testFileService) Get(_ context.Context, id string) (domain.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	file, present := s.files[id]
	if !present {
		return domain.File{}, domain.NotFound("file not found")
	}
	return file, nil
}

func (s *testFileService) List(_ context.Context, query app.FileListQuery) (app.FileListPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files := make([]domain.File, 0, len(s.files))
	for _, file := range s.files {
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].CreatedAt.After(files[j].CreatedAt)
	})
	start := 0
	if query.AfterID != "" {
		start = fileIndex(files, query.AfterID) + 1
		if start == 0 {
			return app.FileListPage{}, domain.Validation("file cursor not found")
		}
		end := min(start+query.Limit, len(files))
		return app.FileListPage{Files: files[start:end], HasMore: end < len(files)}, nil
	}
	if query.BeforeID != "" {
		end := fileIndex(files, query.BeforeID)
		if end < 0 {
			return app.FileListPage{}, domain.Validation("file cursor not found")
		}
		start = max(0, end-query.Limit)
		return app.FileListPage{Files: files[start:end], HasMore: start > 0}, nil
	}
	end := min(query.Limit, len(files))
	return app.FileListPage{Files: files[:end], HasMore: end < len(files)}, nil
}

func fileIndex(files []domain.File, id string) int {
	for index, file := range files {
		if file.ID == id {
			return index
		}
	}
	return -1
}

func (s *testFileService) Download(_ context.Context, id string) (app.FileDownload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	file, present := s.files[id]
	if !present {
		return app.FileDownload{}, domain.NotFound("file not found")
	}
	return app.FileDownload{File: file, Body: io.NopCloser(bytes.NewReader(s.contents[id]))}, nil
}

func (s *testFileService) Delete(_ context.Context, id string) (domain.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	file, present := s.files[id]
	if !present {
		return domain.File{}, domain.NotFound("file not found")
	}
	delete(s.files, id)
	delete(s.contents, id)
	return file, nil
}
