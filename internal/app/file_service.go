package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/workspace"
)

const (
	DefaultFileListLimit        = 20
	MaxFileListLimit            = 1000
	MaxFileBytes          int64 = 500 << 20
	fileCleanupTimeout          = 10 * time.Second
	maxOutcomeRubricBytes int64 = domain.MaxOutcomeRubricCharacters * utf8.UTFMax
	maxFileMessageBytes   int64 = domain.MaxFileMessageCharacters * utf8.UTFMax
)

// ErrBlobTooLarge lets a streaming blob implementation stop after one byte
// beyond MaxFileBytes without buffering the complete upload.
var ErrBlobTooLarge = errors.New("blob exceeds maximum size")

type FileUploadInput struct {
	Filename string
	MimeType string
	Body     io.Reader
}

type FileListQuery struct {
	AfterID  string
	BeforeID string
	ScopeID  string
	Limit    int
}

type FileListPage struct {
	Files   []domain.File
	HasMore bool
}

type BlobInfo struct {
	SizeBytes      int64
	ChecksumSHA256 string
}

type FileDownload struct {
	File domain.File
	Body io.ReadCloser
}

// FileRepository owns workspace-scoped metadata and lifecycle intents.
// BeginUpload must commit before blob I/O; CompleteUpload is the only
// transition that makes a file visible. Get returns only ready Files, and
// BeginDelete hides a file before object deletion begins.
type FileRepository interface {
	BeginUpload(context.Context, domain.File) error
	CompleteUpload(context.Context, string, BlobInfo) (domain.File, error)
	Get(context.Context, string) (domain.File, error)
	List(context.Context, FileListQuery) (FileListPage, error)
	BeginDelete(context.Context, string) (domain.File, error)
	RemoveIncomplete(context.Context, string) error
	ListIncomplete(context.Context) ([]domain.File, error)
}

// BlobStore is intentionally small so S3-compatible storage can back Files
// and immutable Skill archives without leaking provider types into the
// application or HTTP layers.
type BlobStore interface {
	Put(context.Context, string, string, io.Reader, int64) (BlobInfo, error)
	Open(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
}

// FileBlobStore preserves the original Files application boundary name for
// callers while the underlying store is shared with other blob-backed
// resources.
type FileBlobStore = BlobStore

type FileService struct {
	repo  FileRepository
	blobs FileBlobStore
	ids   domain.IDGenerator
	clock domain.Clock
}

func NewFileService(
	repo FileRepository,
	blobs FileBlobStore,
	ids domain.IDGenerator,
	clock domain.Clock,
) *FileService {
	return &FileService{repo: repo, blobs: blobs, ids: ids, clock: clock}
}

func (s *FileService) Upload(ctx context.Context, input FileUploadInput) (domain.File, error) {
	filename, mimeType, err := validateFileUpload(input)
	if err != nil {
		return domain.File{}, err
	}
	now := s.clock.Now().UTC()
	id := s.ids.NewID(domain.PrefixFile)
	file := domain.File{
		ID: id, CreatedAt: now, UpdatedAt: now,
		Filename: filename, MimeType: mimeType,
		BlobKey: workspace.BlobKey(ctx, "files/"+id), State: domain.FileStateUploading,
	}
	if err := s.repo.BeginUpload(ctx, file); err != nil {
		return domain.File{}, err
	}

	info, err := s.blobs.Put(ctx, file.BlobKey, mimeType, input.Body, MaxFileBytes)
	if err != nil {
		s.cleanupIncomplete(ctx, file)
		if errors.Is(err, ErrBlobTooLarge) {
			return domain.File{}, domain.TooLarge("file exceeds 500 MB limit")
		}
		return domain.File{}, err
	}
	completed, err := s.repo.CompleteUpload(ctx, id, info)
	if err != nil {
		// The database update may have committed even when the client observes a
		// connection error. Deleting the blob here could leave a visible ready
		// row with no bytes. Preserve both sides: a committed row remains valid,
		// while an uncommitted uploading intent is removed by reconciliation.
		return domain.File{}, err
	}
	return completed, nil
}

func (s *FileService) Get(ctx context.Context, id string) (domain.File, error) {
	return s.publicFile(ctx, id)
}

func (s *FileService) publicFile(ctx context.Context, id string) (domain.File, error) {
	file, err := s.repo.Get(ctx, id)
	if err != nil {
		return domain.File{}, err
	}
	return file, nil
}

func (s *FileService) List(ctx context.Context, query FileListQuery) (FileListPage, error) {
	if query.Limit == 0 {
		query.Limit = DefaultFileListLimit
	}
	if query.Limit < 1 || query.Limit > MaxFileListLimit {
		return FileListPage{}, domain.Validation("limit must be between 1 and 1000")
	}
	if query.AfterID != "" && query.BeforeID != "" {
		return FileListPage{}, domain.Validation("after_id and before_id cannot be combined")
	}
	return s.repo.List(ctx, query)
}

func (s *FileService) Download(ctx context.Context, id string) (FileDownload, error) {
	file, err := s.publicFile(ctx, id)
	if err != nil {
		return FileDownload{}, err
	}
	if !file.Downloadable {
		return FileDownload{}, domain.Validation("file is not downloadable")
	}
	body, err := s.blobs.Open(ctx, file.BlobKey)
	if err != nil {
		return FileDownload{}, err
	}
	return FileDownload{File: file, Body: body}, nil
}

// ReadOutcomeRubric returns validated text from a reusable top-level File.
// Unlike the public content endpoint, this internal admission path may read
// non-downloadable client uploads. Reads remain bounded to the largest valid
// UTF-8 encoding of the documented character limit.
func (s *FileService) ReadOutcomeRubric(ctx context.Context, id string) (string, error) {
	file, err := s.publicFile(ctx, id)
	if err != nil {
		return "", err
	}
	content, err := s.readTopLevelUTF8(
		ctx, file, "file outcome rubric", "file outcome rubric content",
		domain.MaxOutcomeRubricCharacters, maxOutcomeRubricBytes,
	)
	if errors.Is(err, errFileContentInvalidUTF8) {
		return "", domain.Validation("file outcome rubric must contain valid UTF-8")
	}
	return content, err
}

// ReadMessageFile returns a bounded, integrity-checked snapshot for a
// user.message document source. Generic application/octet-stream is accepted
// so official SDK uploads and source files without a registered media type can
// still be projected after their bytes pass UTF-8 validation.
func (s *FileService) ReadMessageFile(
	ctx context.Context,
	id string,
) (domain.FileMessageContent, error) {
	file, err := s.publicFile(ctx, id)
	if err != nil {
		return domain.FileMessageContent{}, err
	}
	if strings.EqualFold(filepath.Ext(file.Filename), ".pdf") ||
		!isTextMessageMediaType(file.MimeType) {
		return domain.FileMessageContent{}, domain.Unsupported(
			"file message content supports UTF-8 text files only",
		)
	}
	content, err := s.readTopLevelUTF8(
		ctx, file, "file message content", "file message content",
		domain.MaxFileMessageCharacters, maxFileMessageBytes,
	)
	if errors.Is(err, errFileContentInvalidUTF8) {
		return domain.FileMessageContent{}, domain.Unsupported(
			"file message content supports UTF-8 text files only",
		)
	}
	if err != nil {
		return domain.FileMessageContent{}, err
	}
	if strings.ContainsRune(content, '\x00') {
		return domain.FileMessageContent{}, domain.Unsupported(
			"file message content supports UTF-8 text files only",
		)
	}
	return domain.FileMessageContent{
		FileID: file.ID, Filename: file.Filename, MimeType: file.MimeType,
		Content: content,
	}, nil
}

var errFileContentInvalidUTF8 = errors.New("file content is not valid UTF-8")

func (s *FileService) readTopLevelUTF8(
	ctx context.Context,
	file domain.File,
	label string,
	limitLabel string,
	maxCharacters int,
	maxBytes int64,
) (string, error) {
	if file.Scope != nil {
		return "", domain.Validation(label + " must reference a top-level File")
	}
	if file.SizeBytes == 0 {
		return "", domain.Validation(label + " must not be empty")
	}
	if file.SizeBytes < 0 {
		return "", errors.New(label + " has invalid stored size")
	}
	if file.SizeBytes > maxBytes {
		return "", domain.Validation(
			fmt.Sprintf("%s must contain at most %d characters", limitLabel, maxCharacters),
		)
	}
	body, err := s.blobs.Open(ctx, file.BlobKey)
	if err != nil {
		return "", err
	}
	defer body.Close() //nolint:errcheck // the bounded read reports content errors

	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > maxBytes {
		return "", domain.Validation(
			fmt.Sprintf("%s must contain at most %d characters", limitLabel, maxCharacters),
		)
	}
	info := ComputeBlobInfo(data)
	if info.SizeBytes != file.SizeBytes ||
		!strings.EqualFold(info.ChecksumSHA256, file.ChecksumSHA256) {
		return "", errors.New(label + " blob failed integrity verification")
	}
	if len(data) == 0 {
		return "", domain.Validation(label + " must not be empty")
	}
	if !utf8.Valid(data) {
		return "", errFileContentInvalidUTF8
	}
	if utf8.RuneCount(data) > maxCharacters {
		return "", domain.Validation(
			fmt.Sprintf("%s must contain at most %d characters", limitLabel, maxCharacters),
		)
	}
	return string(data), nil
}

func isTextMessageMediaType(mediaType string) bool {
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	if mediaType == "application/octet-stream" || strings.HasSuffix(mediaType, "+json") ||
		strings.HasSuffix(mediaType, "+xml") {
		return true
	}
	switch mediaType {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml",
		"application/toml", "application/x-toml", "application/javascript",
		"application/x-javascript", "application/ecmascript", "application/sql",
		"application/graphql", "application/x-sh":
		return true
	default:
		return false
	}
}

func (s *FileService) Delete(ctx context.Context, id string) (domain.File, error) {
	file, err := s.repo.BeginDelete(ctx, id)
	if err != nil {
		return domain.File{}, err
	}
	if err := s.blobs.Delete(ctx, file.BlobKey); err != nil {
		return domain.File{}, err
	}
	if err := s.repo.RemoveIncomplete(ctx, id); err != nil {
		return domain.File{}, err
	}
	return file, nil
}

// Reconcile removes objects and rows left by an API-process crash during an
// upload or delete. It runs before the HTTP server starts accepting requests.
func (s *FileService) Reconcile(ctx context.Context) error {
	files, err := s.repo.ListIncomplete(ctx)
	if err != nil {
		return err
	}
	for _, file := range files {
		if err := s.blobs.Delete(ctx, file.BlobKey); err != nil {
			return err
		}
		if err := s.repo.RemoveIncomplete(ctx, file.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *FileService) cleanupIncomplete(ctx context.Context, file domain.File) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fileCleanupTimeout)
	defer cancel()
	_ = s.blobs.Delete(cleanupCtx, file.BlobKey)
	_ = s.repo.RemoveIncomplete(cleanupCtx, file.ID)
}

func validateFileUpload(input FileUploadInput) (string, string, error) {
	name := input.Filename
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 255 {
		return "", "", domain.Validation("filename must contain between 1 and 255 characters")
	}
	for _, r := range name {
		if unicode.IsControl(r) || strings.ContainsRune("<>:\"|?*/\\", r) {
			return "", "", domain.Validation("filename contains a forbidden character")
		}
	}
	mediaType, _, err := mime.ParseMediaType(input.MimeType)
	if err != nil || mediaType == "" {
		return "", "", domain.Validation("file content type is invalid")
	}
	if input.Body == nil {
		return "", "", domain.Validation("file is required")
	}
	return name, mediaType, nil
}

// ComputeBlobInfo is shared by blob implementations and test doubles.
func ComputeBlobInfo(body []byte) BlobInfo {
	sum := sha256.Sum256(body)
	return BlobInfo{SizeBytes: int64(len(body)), ChecksumSHA256: hex.EncodeToString(sum[:])}
}
