package domain

import "time"

// FileState is internal lifecycle state. Only ready files are visible through
// the public API; uploading and deleting rows are durable reconciliation
// intents.
type FileState string

const (
	FileStateUploading FileState = "uploading"
	FileStateReady     FileState = "ready"
	FileStateDeleting  FileState = "deleting"
)

// File is the authoritative metadata projection for one object-store blob.
// BlobKey and ChecksumSHA256 are persistence/runtime fields and never cross
// the public wire.
type File struct {
	ID             string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Filename       string
	MimeType       string
	SizeBytes      int64
	BlobKey        string
	ChecksumSHA256 string
	State          FileState
}
