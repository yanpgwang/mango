package httpapi

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	if s.deps.Files == nil {
		writeError(w, domain.Unsupported("Files API is not configured"))
		return
	}
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, domain.Validation("invalid multipart body"))
		return
	}
	part, err := reader.NextPart()
	if err != nil {
		writeMultipartError(w, err)
		return
	}
	defer part.Close() //nolint:errcheck // request body cleanup

	disposition, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil || disposition != "form-data" || params["name"] != "file" || params["filename"] == "" {
		writeError(w, domain.Validation("multipart body must contain one file part named file"))
		return
	}
	contentType := part.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	body := &singleMultipartPartReader{part: part, multipart: reader}
	created, err := s.deps.Files.Upload(r.Context(), app.FileUploadInput{
		Filename: params["filename"], MimeType: contentType, Body: body,
	})
	if err != nil {
		if errors.Is(err, errInvalidMultipartBody) {
			writeMultipartError(w, err)
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fileToJSON(created))
}

var errInvalidMultipartBody = errors.New("multipart body must contain exactly one file part named file")

// singleMultipartPartReader validates the multipart tail before it reports EOF
// to the File service. That keeps an extra or malformed part inside the upload
// intent, so the service can remove both partial bytes and metadata instead of
// committing a File and attempting a best-effort rollback afterward.
type singleMultipartPartReader struct {
	part      *multipart.Part
	multipart *multipart.Reader
	checked   bool
}

func (r *singleMultipartPartReader) Read(p []byte) (int, error) {
	n, err := r.part.Read(p)
	if err == nil {
		return n, nil
	}
	if err != io.EOF {
		return n, fmt.Errorf("%w: %w", errInvalidMultipartBody, err)
	}
	if r.checked {
		return n, io.EOF
	}
	r.checked = true
	extra, nextErr := r.multipart.NextPart()
	if extra != nil {
		_ = extra.Close()
	}
	if nextErr == io.EOF {
		return n, io.EOF
	}
	if nextErr != nil {
		return n, fmt.Errorf("%w: %w", errInvalidMultipartBody, nextErr)
	}
	return n, errInvalidMultipartBody
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	if s.deps.Files == nil {
		writeError(w, domain.Unsupported("Files API is not configured"))
		return
	}
	file, err := s.deps.Files.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fileToJSON(file))
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	if s.deps.Files == nil {
		writeError(w, domain.Unsupported("Files API is not configured"))
		return
	}
	query, err := parseFileListQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	page, err := s.deps.Files.List(r.Context(), query)
	if err != nil {
		writeError(w, err)
		return
	}
	data := make([]map[string]any, 0, len(page.Files))
	for _, file := range page.Files {
		data = append(data, fileToJSON(file))
	}
	var firstID, lastID any
	if len(page.Files) > 0 {
		firstID = page.Files[0].ID
		lastID = page.Files[len(page.Files)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": data, "has_more": page.HasMore,
		"first_id": firstID, "last_id": lastID,
	})
}

func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request) {
	if s.deps.Files == nil {
		writeError(w, domain.Unsupported("Files API is not configured"))
		return
	}
	download, err := s.deps.Files.Download(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	defer download.Body.Close() //nolint:errcheck // response copy owns completion
	w.Header().Set("Content-Type", download.File.MimeType)
	w.Header().Set("Content-Length", strconv.FormatInt(download.File.SizeBytes, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
		"filename": download.File.Filename,
	}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, download.Body)
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	if s.deps.Files == nil {
		writeError(w, domain.Unsupported("Files API is not configured"))
		return
	}
	file, err := s.deps.Files.Delete(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": file.ID, "type": "file_deleted"})
}

func parseFileListQuery(r *http.Request) (app.FileListQuery, error) {
	values := r.URL.Query()
	query := app.FileListQuery{
		AfterID: values.Get("after_id"), BeforeID: values.Get("before_id"),
		Limit: app.DefaultFileListLimit,
	}
	if values.Has("limit") {
		limit, err := strconv.Atoi(values.Get("limit"))
		if err != nil || limit < 1 || limit > app.MaxFileListLimit {
			return app.FileListQuery{}, domain.Validation("limit must be between 1 and 1000")
		}
		query.Limit = limit
	}
	if query.AfterID != "" && query.BeforeID != "" {
		return app.FileListQuery{}, domain.Validation("after_id and before_id cannot be combined")
	}
	return query, nil
}

func fileToJSON(file domain.File) map[string]any {
	return map[string]any{
		"id": file.ID, "created_at": file.CreatedAt.Format(timeFmt),
		"filename": file.Filename, "mime_type": file.MimeType,
		"size_bytes": file.SizeBytes, "type": "file",
	}
}

func writeMultipartError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeError(w, domain.TooLarge("file request exceeds 500 MB limit"))
		return
	}
	writeError(w, domain.Validation("multipart body must contain exactly one file part named file"))
}
