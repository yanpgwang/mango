package pg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

func (r *FileRepository) RenewUpload(ctx context.Context, id string) error {
	workspaceID, _, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return err
	}
	return r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		if err := r.lockUpload(ctx, tx, id, workspaceID); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `UPDATE files SET upload_expires_at=clock_timestamp()+$3*interval '1 second'
WHERE id=$1 AND ($2='' OR workspace_id=$2) AND state='uploading' AND session_id IS NULL AND upload_expires_at>clock_timestamp()`, id, workspaceID, app.UploadLeaseDuration.Seconds())
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return app.ErrUploadLeaseLost
		}
		return nil
	})
}

func (r *FileRepository) ReleaseUpload(ctx context.Context, id, blobKey string, writeFinished bool) error {
	workspaceID, _, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return err
	}
	_, err = r.store.pool.Exec(ctx, `WITH released AS (
 UPDATE files SET upload_expires_at=clock_timestamp()
 WHERE id=$1 AND ($2='' OR workspace_id=$2) AND state='uploading' AND session_id IS NULL AND blob_key=$3
 RETURNING blob_key,workspace_id
 ), candidates AS (
 SELECT blob_key,workspace_id FROM released
 UNION SELECT blob_key,workspace_id FROM blob_cleanup_intents
 WHERE kind='file' AND blob_key=$3 AND ($2='' OR workspace_id=$2) AND $4 AND NOT writer_finished
 )
 INSERT INTO blob_cleanup_intents (kind,blob_key,workspace_id,writer_finished)
 SELECT 'file',blob_key,workspace_id,$4 FROM candidates
 ON CONFLICT (kind,blob_key) DO UPDATE SET revision=nextval('blob_cleanup_intents_revision_seq'),
 writer_finished=blob_cleanup_intents.writer_finished OR EXCLUDED.writer_finished
 WHERE blob_cleanup_intents.workspace_id=EXCLUDED.workspace_id`, id, workspaceID, blobKey, writeFinished)
	return err
}

func (r *FileRepository) ClaimIncomplete(ctx context.Context, id string) (domain.File, bool, error) {
	workspaceID, _, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return domain.File{}, false, err
	}
	file, err := scanFile(r.store.pool.QueryRow(ctx, `WITH claimed AS (UPDATE files SET state='deleting',
 upload_expires_at=CASE WHEN state='uploading' THEN COALESCE(upload_expires_at,clock_timestamp()) ELSE upload_expires_at END, updated_at=now()
WHERE id=$1 AND ($2='' OR workspace_id=$2)
  AND (state='deleting' OR (state='uploading' AND session_id IS NULL AND (upload_expires_at IS NULL OR upload_expires_at<=clock_timestamp())))
RETURNING id,created_at,updated_at,filename,mime_type,size_bytes,blob_key,checksum_sha256,state,session_id,workspace_id,upload_expires_at
), guarded AS (
 INSERT INTO blob_cleanup_intents (kind,blob_key,workspace_id)
 SELECT 'file',blob_key,workspace_id FROM claimed WHERE upload_expires_at IS NOT NULL
 ON CONFLICT (kind,blob_key) DO NOTHING
)
SELECT id,created_at,updated_at,filename,mime_type,size_bytes,blob_key,checksum_sha256,state,COALESCE(session_id,'') FROM claimed`, id, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.File{}, false, nil
	}
	return file, err == nil, err
}

func (r *SkillRepository) RenewUpload(ctx context.Context, id, version, blobKey string) error {
	workspaceID, _, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return err
	}
	return r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		if err := r.lockUpload(ctx, tx, id, version, blobKey, workspaceID); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `UPDATE skill_versions SET upload_expires_at=clock_timestamp()+$5*interval '1 second'
WHERE skill_id=$1 AND version=$2 AND state='uploading' AND upload_expires_at>clock_timestamp() AND blob_key=$4
 AND EXISTS (SELECT 1 FROM skills WHERE id=$1 AND ($3='' OR workspace_id=$3))`, id, version, workspaceID, blobKey, app.UploadLeaseDuration.Seconds())
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return app.ErrUploadLeaseLost
		}
		return nil
	})
}

func (r *SkillRepository) ReleaseUpload(ctx context.Context, id, version, blobKey string, writeFinished bool) error {
	workspaceID, _, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return err
	}
	_, err = r.store.pool.Exec(ctx, `WITH released AS (
 UPDATE skill_versions SET upload_expires_at=clock_timestamp()
 WHERE skill_id=$1 AND version=$2 AND state='uploading' AND blob_key=$4
 AND EXISTS (SELECT 1 FROM skills WHERE id=$1 AND ($3='' OR workspace_id=$3))
 RETURNING blob_key,(SELECT workspace_id FROM skills WHERE id=$1) AS workspace_id
 ), candidates AS (
 SELECT blob_key,workspace_id FROM released
 UNION SELECT blob_key,workspace_id FROM blob_cleanup_intents
 WHERE kind='skill' AND blob_key=$4 AND ($3='' OR workspace_id=$3) AND $5 AND NOT writer_finished
 )
 INSERT INTO blob_cleanup_intents (kind,blob_key,workspace_id,writer_finished)
 SELECT 'skill',blob_key,workspace_id,$5 FROM candidates
 ON CONFLICT (kind,blob_key) DO UPDATE SET revision=nextval('blob_cleanup_intents_revision_seq'),
 writer_finished=blob_cleanup_intents.writer_finished OR EXCLUDED.writer_finished
 WHERE blob_cleanup_intents.workspace_id=EXCLUDED.workspace_id`, id, version, workspaceID, blobKey, writeFinished)
	return err
}

func (r *SkillRepository) ClaimIncompleteVersion(ctx context.Context, id, version string) (domain.SkillVersion, bool, error) {
	workspaceID, _, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return domain.SkillVersion{}, false, err
	}
	item, err := scanSkillVersion(r.store.pool.QueryRow(ctx, `WITH claimed AS (UPDATE skill_versions SET state='deleting'
WHERE skill_id=$1 AND version=$2 AND (state='deleting' OR (state='uploading' AND upload_expires_at<=clock_timestamp()))
AND EXISTS (SELECT 1 FROM skills WHERE id=$1 AND ($3='' OR workspace_id=$3))
RETURNING skill_id,version,created_at,description,directory,name,blob_key,size_bytes,uncompressed_size_bytes,checksum_sha256,state,initial,upload_expires_at,
 (SELECT workspace_id FROM skills WHERE id=$1) AS workspace_id
), guarded AS (
 INSERT INTO blob_cleanup_intents (kind,blob_key,workspace_id)
 SELECT 'skill',blob_key,workspace_id FROM claimed WHERE upload_expires_at IS NOT NULL
 ON CONFLICT (kind,blob_key) DO NOTHING
)
SELECT skill_id,version,created_at,description,directory,name,blob_key,size_bytes,uncompressed_size_bytes,checksum_sha256,state,initial FROM claimed`, id, version, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SkillVersion{}, false, nil
	}
	return item, err == nil, err
}

// Time predicates must run after acquiring the row lock. An UPDATE predicate
// can be evaluated before a lock wait and is not rechecked if the blocker only
// locked (rather than updated) the tuple.
func (r *FileRepository) lockUpload(ctx context.Context, tx pgx.Tx, id, workspaceID string) error {
	var locked string
	err := tx.QueryRow(ctx, `SELECT id FROM files WHERE id=$1 AND ($2='' OR workspace_id=$2) FOR UPDATE`, id, workspaceID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.ErrUploadLeaseLost
	}
	return err
}

func (r *SkillRepository) lockUpload(ctx context.Context, tx pgx.Tx, id, version, blobKey, workspaceID string) error {
	var locked string
	err := tx.QueryRow(ctx, `SELECT version FROM skill_versions WHERE skill_id=$1 AND version=$2 AND blob_key=$3
 AND EXISTS (SELECT 1 FROM skills WHERE id=$1 AND ($4='' OR workspace_id=$4)) FOR UPDATE`, id, version, blobKey, workspaceID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.ErrUploadLeaseLost
	}
	return err
}
