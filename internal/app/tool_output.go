package app

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/workspace"
)

const MaxToolOutputBytes = 32 << 20
const ToolOutputDirectory = ".mango-tool-results"

func ToolOutputPath(fileID string) string { return ToolOutputDirectory + "/" + fileID + ".txt" }

type toolOutputFileRepository interface {
	BeginToolOutputUpload(context.Context, domain.File) (domain.File, error)
	CompleteToolOutputUpload(context.Context, domain.File) (domain.File, error)
}

// StoreToolOutput publishes a journaled MCP receipt as a regular File. The ID
// is allocated before receipt persistence; only storage is retried. Session
// association pins bytes until Session deletion and constrains Work-token reads.
func (s *FileService) StoreToolOutput(ctx context.Context, sessionID, fileID, text string) error {
	if sessionID == "" || !validToolOutputID(fileID) || !utf8.ValidString(text) || len(text) > MaxToolOutputBytes {
		return domain.Validation("invalid textual tool output")
	}
	repo, ok := s.repo.(toolOutputFileRepository)
	if !ok {
		return errors.New("tool output File repository is unavailable")
	}
	info := ComputeBlobInfo([]byte(text))
	now := s.clock.Now().UTC()
	// The public identity comes from the durable receipt. Physical write keys
	// must never be reused, even when a process restarts with a fresh ID source.
	key := workspace.BlobKey(ctx, "files/"+fileID+"/"+domain.NewRandomIDGen().NewID("upload_"))
	expected := domain.File{ID: fileID, SessionID: sessionID, Filename: fileID + ".txt", MimeType: "text/plain", SizeBytes: info.SizeBytes, ChecksumSHA256: info.ChecksumSHA256, BlobKey: key, State: domain.FileStateUploading, CreatedAt: now, UpdatedAt: now}
	file, err := repo.BeginToolOutputUpload(ctx, expected)
	if err != nil {
		return err
	}
	if !sameToolOutputFile(file, expected) {
		return domain.Conflict("tool output File identity differs from its durable receipt")
	}
	if file.State == domain.FileStateReady {
		return nil
	}
	if file.State != domain.FileStateUploading || file.BlobKey != expected.BlobKey {
		return domain.Conflict("tool output File is no longer uploading")
	}
	stored, err := s.blobs.Put(ctx, file.BlobKey, file.MimeType, strings.NewReader(text), MaxToolOutputBytes)
	if err != nil {
		if errors.Is(err, ErrBlobNotWritten) || errors.Is(err, ErrBlobTooLarge) {
			s.acknowledgeToolOutputWrite(ctx, file.BlobKey)
		}
		return err
	} // Unknown writers keep their pre-I/O guard; the receipt can resume storage.
	if stored != info {
		s.acknowledgeToolOutputWrite(ctx, file.BlobKey)
		return errors.New("tool output blob failed integrity verification")
	}
	_, err = repo.CompleteToolOutputUpload(ctx, file)
	if err == nil {
		return nil
	}
	// Put definitely finished, but a completion error may be a lost commit
	// acknowledgement. Never delete here: uploading/ready references exclude
	// this key from reconciliation. A superseded key can be collected safely.
	s.acknowledgeToolOutputWrite(ctx, file.BlobKey)
	// A concurrent writer or lost acknowledgement may already have published it.
	ready, readErr := s.repo.Get(ctx, fileID)
	if readErr == nil && ready.State == domain.FileStateReady && sameToolOutputFile(ready, expected) {
		return nil
	}
	return err
}

func sameToolOutputFile(file, expected domain.File) bool {
	return file.ID == expected.ID && file.SessionID == expected.SessionID && file.Filename == expected.Filename && file.MimeType == expected.MimeType &&
		file.SizeBytes == expected.SizeBytes && file.ChecksumSHA256 == expected.ChecksumSHA256
}

func (s *FileService) acknowledgeToolOutputWrite(ctx context.Context, key string) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	// Failure keeps the existing unknown guard. It must not justify deleting
	// bytes or removing File metadata after an uncertain database response.
	_, _ = s.repo.RetainBlobCleanup(cleanup, key, true)
}

func validToolOutputID(id string) bool {
	if !strings.HasPrefix(id, domain.PrefixFile) || len(id) <= len(domain.PrefixFile) || len(id) > 128 {
		return false
	}
	for _, c := range strings.TrimPrefix(id, domain.PrefixFile) {
		valid := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
		if !valid {
			return false
		}
	}
	return true
}
