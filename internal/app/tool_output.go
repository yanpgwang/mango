package app

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/workspace"
)

const MaxToolOutputBytes = 32 << 20
const ToolOutputDirectory = ".mango-tool-results"

func ToolOutputPath(fileID string) string { return ToolOutputDirectory + "/" + fileID + ".txt" }

type toolOutputFileRepository interface {
	EnsureToolOutputUpload(context.Context, domain.File) (domain.File, error)
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
	expected := domain.File{ID: fileID, SessionID: sessionID, Filename: fileID + ".txt", MimeType: "text/plain", SizeBytes: info.SizeBytes, ChecksumSHA256: info.ChecksumSHA256, BlobKey: workspace.BlobKey(ctx, "files/"+fileID), State: domain.FileStateUploading, CreatedAt: now, UpdatedAt: now}
	file, err := repo.EnsureToolOutputUpload(ctx, expected)
	if err != nil {
		return err
	}
	if file.SessionID != expected.SessionID || file.Filename != expected.Filename || file.MimeType != expected.MimeType || file.BlobKey != expected.BlobKey || file.SizeBytes != expected.SizeBytes || file.ChecksumSHA256 != expected.ChecksumSHA256 {
		return domain.Conflict("tool output File identity differs from its durable receipt")
	}
	if file.State == domain.FileStateReady {
		return nil
	}
	if file.State != domain.FileStateUploading {
		return domain.Conflict("tool output File is no longer uploading")
	}
	stored, err := s.blobs.Put(ctx, file.BlobKey, file.MimeType, strings.NewReader(text), MaxToolOutputBytes)
	if err != nil {
		return err
	} // Keep the intent; the durable receipt can resume it.
	if stored != info {
		return errors.New("tool output blob failed integrity verification")
	}
	_, err = s.repo.CompleteUpload(ctx, fileID, stored)
	if err == nil {
		return nil
	}
	// A concurrent writer or lost acknowledgement may already have published it.
	ready, readErr := s.repo.Get(ctx, fileID)
	if readErr == nil && ready.State == domain.FileStateReady && ready.ChecksumSHA256 == info.ChecksumSHA256 && ready.SessionID == sessionID && ready.SizeBytes == info.SizeBytes {
		return nil
	}
	return err
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
