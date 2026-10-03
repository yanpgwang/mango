package pg

import (
	"context"

	"github.com/yanpgwang/mango/internal/app"
)

type blobCleanupRepository struct {
	store *Store
	kind  string
}

func (r *blobCleanupRepository) RetainBlobCleanup(ctx context.Context, key string, writeFinished bool) (app.BlobCleanupIntent, error) {
	workspaceID, err := r.store.workspaceForWrite(ctx)
	if err != nil {
		return app.BlobCleanupIntent{}, err
	}
	intent := app.BlobCleanupIntent{BlobKey: key}
	err = r.store.pool.QueryRow(ctx, `INSERT INTO blob_cleanup_intents (kind,blob_key,workspace_id,writer_finished) VALUES ($1,$2,$3,$4)
ON CONFLICT (kind,blob_key) DO UPDATE SET revision=nextval('blob_cleanup_intents_revision_seq'), writer_finished=blob_cleanup_intents.writer_finished OR EXCLUDED.writer_finished
WHERE blob_cleanup_intents.workspace_id=EXCLUDED.workspace_id RETURNING revision`, r.kind, key, workspaceID, writeFinished).Scan(&intent.Revision)
	return intent, err
}

func (r *blobCleanupRepository) ListBlobCleanup(ctx context.Context) ([]app.BlobCleanupIntent, error) {
	workspaceID, _, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.store.pool.Query(ctx, `SELECT blob_key,revision FROM blob_cleanup_intents WHERE kind=$1 AND ($2='' OR workspace_id=$2) ORDER BY last_checked_at,blob_key LIMIT 100`, r.kind, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []app.BlobCleanupIntent
	for rows.Next() {
		var intent app.BlobCleanupIntent
		if err := rows.Scan(&intent.BlobKey, &intent.Revision); err != nil {
			return nil, err
		}
		result = append(result, intent)
	}
	return result, rows.Err()
}

func (r *blobCleanupRepository) RemoveBlobCleanup(ctx context.Context, intent app.BlobCleanupIntent) error {
	workspaceID, _, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return err
	}
	_, err = r.store.pool.Exec(ctx, `DELETE FROM blob_cleanup_intents WHERE kind=$1 AND blob_key=$2 AND revision=$3 AND writer_finished AND ($4='' OR workspace_id=$4)`, r.kind, intent.BlobKey, intent.Revision, workspaceID)
	return err
}

// Record an attempt before object I/O. Old unconfirmed guards and slow failures
// rotate behind unvisited keys rather than monopolizing each bounded scan.
func (r *blobCleanupRepository) TouchBlobCleanup(ctx context.Context, intent app.BlobCleanupIntent) error {
	workspaceID, _, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return err
	}
	_, err = r.store.pool.Exec(ctx, `UPDATE blob_cleanup_intents SET last_checked_at=clock_timestamp() WHERE kind=$1 AND blob_key=$2 AND revision=$3 AND ($4='' OR workspace_id=$4)`, r.kind, intent.BlobKey, intent.Revision, workspaceID)
	return err
}
