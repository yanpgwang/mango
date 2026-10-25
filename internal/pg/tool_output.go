package pg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

// BeginToolOutputUpload assigns a fresh physical attempt to a durable File
// receipt. The reference and unknown-writer guard commit before any object I/O.
func (r *FileRepository) BeginToolOutputUpload(ctx context.Context, expected domain.File) (domain.File, error) {
	workspaceID, err := r.store.workspaceForWrite(ctx)
	if err != nil {
		return domain.File{}, err
	}
	var file domain.File
	err = r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		if err := lockToolOutputSession(ctx, tx, expected.SessionID, workspaceID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO files (id,created_at,updated_at,filename,mime_type,size_bytes,blob_key,checksum_sha256,state,workspace_id,session_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'uploading',$9,$10) ON CONFLICT (id) DO NOTHING`,
			expected.ID, expected.CreatedAt, expected.UpdatedAt, expected.Filename, expected.MimeType,
			expected.SizeBytes, expected.BlobKey, expected.ChecksumSHA256, workspaceID, expected.SessionID)
		if err != nil {
			return err
		}
		file, err = lockToolOutputFile(ctx, tx, expected.ID, workspaceID)
		if err != nil {
			return err
		}
		if !sameToolOutputReceipt(file, expected) {
			return domain.Conflict("tool output File identity differs from its durable receipt")
		}
		if file.State == domain.FileStateReady {
			return nil
		}
		if file.State != domain.FileStateUploading {
			return domain.Conflict("tool output File is no longer uploading")
		}
		file, err = scanFile(tx.QueryRow(ctx, `UPDATE files SET blob_key=$2,updated_at=clock_timestamp() WHERE id=$1
RETURNING id,created_at,updated_at,filename,mime_type,size_bytes,blob_key,checksum_sha256,state,COALESCE(session_id,'')`, expected.ID, expected.BlobKey))
		if err != nil {
			return err
		}
		// No upsert: an attempt key is single-use, including across restarts.
		if _, err := tx.Exec(ctx, `INSERT INTO blob_cleanup_intents (kind,blob_key,workspace_id) VALUES ('file',$1,$2)`, expected.BlobKey, workspaceID); err != nil {
			return err
		}
		return nil
	})
	return file, err
}

// CompleteToolOutputUpload publishes only the current attempt. A successful
// single Put settles this key, so publication can remove its guard atomically.
func (r *FileRepository) CompleteToolOutputUpload(ctx context.Context, expected domain.File) (domain.File, error) {
	workspaceID, err := r.store.workspaceForWrite(ctx)
	if err != nil {
		return domain.File{}, err
	}
	var file domain.File
	err = r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		// Match deletion's Session -> File lock order. A prepared deletion
		// fences both new writes and completion of a previously admitted write.
		if err := lockToolOutputSession(ctx, tx, expected.SessionID, workspaceID); err != nil {
			return err
		}
		file, err = lockToolOutputFile(ctx, tx, expected.ID, workspaceID)
		if err != nil {
			return err
		}
		if !sameToolOutputReceipt(file, expected) || file.BlobKey != expected.BlobKey ||
			(file.State != domain.FileStateUploading && file.State != domain.FileStateReady) {
			return errors.Join(app.ErrUploadLeaseLost, domain.Conflict("tool output write is no longer current"))
		}
		file, err = scanFile(tx.QueryRow(ctx, `UPDATE files SET state='ready',upload_expires_at=NULL,updated_at=clock_timestamp()
WHERE id=$1 RETURNING id,created_at,updated_at,filename,mime_type,size_bytes,blob_key,checksum_sha256,state,COALESCE(session_id,'')`, expected.ID))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM blob_cleanup_intents WHERE kind='file' AND blob_key=$1 AND workspace_id=$2`, expected.BlobKey, workspaceID)
		return err
	})
	return file, err
}

func lockToolOutputSession(ctx context.Context, tx pgx.Tx, id, workspaceID string) error {
	var locked string
	err := tx.QueryRow(ctx, `SELECT id FROM sessions WHERE id=$1 AND workspace_id=$2 AND deleting_at IS NULL FOR SHARE`, id, workspaceID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NotFound("tool output owning Session not found")
	}
	return err
}

func lockToolOutputFile(ctx context.Context, tx pgx.Tx, id, workspaceID string) (domain.File, error) {
	file, err := scanFile(tx.QueryRow(ctx, `SELECT id,created_at,updated_at,filename,mime_type,size_bytes,blob_key,checksum_sha256,state,COALESCE(session_id,'')
FROM files WHERE id=$1 AND workspace_id=$2 FOR UPDATE`, id, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.File{}, domain.NotFound("tool output File not found")
	}
	return file, err
}

func sameToolOutputReceipt(file, expected domain.File) bool {
	return file.SessionID == expected.SessionID && file.Filename == expected.Filename && file.MimeType == expected.MimeType &&
		file.SizeBytes == expected.SizeBytes && file.ChecksumSHA256 == expected.ChecksumSHA256
}
