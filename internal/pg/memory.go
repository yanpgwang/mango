package pg

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
	"github.com/yanpgwang/mango/internal/workspace"
)

var _ app.MemoryRepository = (*MemoryRepository)(nil)

type MemoryRepository struct{ store *Store }

func NewMemoryRepository(store *Store) *MemoryRepository {
	return &MemoryRepository{store: store}
}

// fenceSessionMemoryWrite makes a worker-side Memory mutation linearizable
// with Work reclaim. Route authorization narrows the token to an attached,
// read-write Store; this in-transaction check proves the same token still owns
// the live Session lease at the instant the Store is changed.
func (r *MemoryRepository) fenceSessionMemoryWrite(
	ctx context.Context,
	tx pgx.Tx,
	storeID string,
) error {
	requestScope, _ := workspace.FromContext(ctx)
	if requestScope.Session == nil {
		return nil
	}
	access, attached := requestScope.Session.Memories[storeID]
	if !attached || access != domain.MemoryAccessReadWrite {
		return domain.Precondition("memory store is not writable by this Session")
	}
	return r.store.fenceSessionCredential(ctx, tx, requestScope.Session.SessionID)
}

func (r *MemoryRepository) CreateStore(ctx context.Context, item domain.MemoryStore) (domain.MemoryStore, error) {
	workspaceID, err := r.store.workspaceForWrite(ctx)
	if err != nil {
		return domain.MemoryStore{}, err
	}
	normalizeMemoryStoreTimes(&item)
	metadata, err := json.Marshal(item.Metadata)
	if err != nil {
		return domain.MemoryStore{}, err
	}
	created, err := scanMemoryStore(r.store.pool.QueryRow(ctx, `
INSERT INTO memory_stores (id, workspace_id, name, description, metadata, created_at, updated_at, archived_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, NULL)
RETURNING id, name, description, metadata, created_at, updated_at, archived_at`,
		item.ID, workspaceID, item.Name, item.Description, metadata, item.CreatedAt, item.UpdatedAt,
	))
	if isUniqueViolation(err) {
		return domain.MemoryStore{}, domain.Conflict("memory store already exists")
	}
	return created, err
}

func (r *MemoryRepository) GetStore(ctx context.Context, id string) (domain.MemoryStore, error) {
	workspaceID, scoped, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return domain.MemoryStore{}, err
	}
	query := `
SELECT id, name, description, metadata, created_at, updated_at, archived_at
FROM memory_stores WHERE id = $1`
	args := []any{id}
	if scoped {
		query += ` AND workspace_id = $2`
		args = append(args, workspaceID)
	}
	item, err := scanMemoryStore(r.store.pool.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.MemoryStore{}, domain.NotFound("memory store not found")
	}
	return item, err
}

func (r *MemoryRepository) UpdateStore(
	ctx context.Context,
	id string,
	patch app.MemoryStoreUpdateInput,
	clock domain.Clock,
) (domain.MemoryStore, error) {
	workspaceID, scoped, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return domain.MemoryStore{}, err
	}
	var updated domain.MemoryStore
	err = r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		query := `
SELECT id, name, description, metadata, created_at, updated_at, archived_at
FROM memory_stores WHERE id = $1`
		args := []any{id}
		if scoped {
			query += ` AND workspace_id = $2`
			args = append(args, workspaceID)
		}
		current, err := scanMemoryStore(tx.QueryRow(ctx, query+` FOR UPDATE`, args...))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("memory store not found")
		}
		if err != nil {
			return err
		}
		if current.ArchivedAt != nil {
			return domain.Validation("archived memory stores are read-only")
		}
		name, description := current.Name, current.Description
		metadata := clonePGStringMap(current.Metadata)
		if patch.Name != nil {
			name = *patch.Name
		}
		if patch.Description != nil {
			description = *patch.Description
		}
		for key, value := range patch.Metadata {
			if value == nil {
				delete(metadata, key)
			} else {
				metadata[key] = *value
			}
		}
		if len(metadata) > 16 {
			return domain.Validation("metadata must contain at most 16 entries")
		}
		if name == current.Name && description == current.Description && equalStringMap(metadata, current.Metadata) {
			updated = current
			return nil
		}
		metadataJSON, err := json.Marshal(metadata)
		if err != nil {
			return err
		}
		updatedAt := clock.Now().UTC().Truncate(time.Microsecond)
		updateQuery := `
UPDATE memory_stores
		SET name = $2, description = $3, metadata = $4, updated_at = $5
		WHERE id = $1`
		updateArgs := []any{id, name, description, metadataJSON, updatedAt}
		if scoped {
			updateQuery += ` AND workspace_id = $6`
			updateArgs = append(updateArgs, workspaceID)
		}
		updated, err = scanMemoryStore(tx.QueryRow(ctx, updateQuery+`
RETURNING id, name, description, metadata, created_at, updated_at, archived_at`, updateArgs...))
		return err
	})
	return updated, err
}

func (r *MemoryRepository) ListStores(ctx context.Context, query app.MemoryStoreListQuery) (app.MemoryStoreListPage, error) {
	args := make([]any, 0, 7)
	where := []string{"true"}
	workspaceID, scoped, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return app.MemoryStoreListPage{}, err
	}
	if scoped {
		args = append(args, workspaceID)
		where = append(where, fmt.Sprintf("workspace_id = $%d", len(args)))
	}
	if !query.IncludeArchived {
		where = append(where, "archived_at IS NULL")
	}
	if query.CreatedAtGte != nil {
		args = append(args, query.CreatedAtGte.UTC())
		where = append(where, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	if query.CreatedAtLte != nil {
		args = append(args, query.CreatedAtLte.UTC())
		where = append(where, fmt.Sprintf("created_at <= $%d", len(args)))
	}
	if query.After != nil {
		args = append(args, query.After.CreatedAt.UTC(), query.After.ID)
		where = append(where, fmt.Sprintf("(created_at, id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, query.Limit+1)
	rows, err := r.store.pool.Query(ctx, fmt.Sprintf(`
SELECT id, name, description, metadata, created_at, updated_at, archived_at
FROM memory_stores
WHERE %s
ORDER BY created_at DESC, id DESC
LIMIT $%d`, strings.Join(where, " AND "), len(args)), args...)
	if err != nil {
		return app.MemoryStoreListPage{}, err
	}
	defer rows.Close()
	items := make([]domain.MemoryStore, 0, query.Limit+1)
	for rows.Next() {
		item, scanErr := scanMemoryStore(rows)
		if scanErr != nil {
			return app.MemoryStoreListPage{}, scanErr
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return app.MemoryStoreListPage{}, err
	}
	page := app.MemoryStoreListPage{Stores: items}
	if len(items) > query.Limit {
		page.Stores = items[:query.Limit]
		page.HasNext = true
	}
	return page, nil
}

func (r *MemoryRepository) ArchiveStore(ctx context.Context, id string, clock domain.Clock) (domain.MemoryStore, error) {
	workspaceID, scoped, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return domain.MemoryStore{}, err
	}
	now := clock.Now().UTC().Truncate(time.Microsecond)
	query := `
UPDATE memory_stores
SET archived_at = COALESCE(archived_at, $2),
    updated_at = CASE WHEN archived_at IS NULL THEN $2 ELSE updated_at END
WHERE id = $1`
	args := []any{id, now}
	if scoped {
		query += ` AND workspace_id = $3`
		args = append(args, workspaceID)
	}
	item, err := scanMemoryStore(r.store.pool.QueryRow(ctx, query+`
RETURNING id, name, description, metadata, created_at, updated_at, archived_at`, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.MemoryStore{}, domain.NotFound("memory store not found")
	}
	return item, err
}

func (r *MemoryRepository) DeleteStore(ctx context.Context, id string) error {
	workspaceID, scoped, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return err
	}
	query := `DELETE FROM memory_stores WHERE id = $1`
	args := []any{id}
	if scoped {
		query += ` AND workspace_id = $2`
		args = append(args, workspaceID)
	}
	tag, err := r.store.pool.Exec(ctx, query, args...)
	if isForeignKeyViolation(err) {
		return domain.Validation("memory store is attached to a session")
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.NotFound("memory store not found")
	}
	return nil
}

func (r *MemoryRepository) CreateMemory(ctx context.Context, item domain.Memory, version domain.MemoryVersion) (domain.Memory, error) {
	if _, err := r.GetStore(ctx, item.MemoryStoreID); err != nil {
		return domain.Memory{}, err
	}
	normalizeMemoryTimes(&item, &version)
	var created domain.Memory
	err := r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		if err := r.fenceSessionMemoryWrite(ctx, tx, item.MemoryStoreID); err != nil {
			return err
		}
		var archivedAt *time.Time
		if err := tx.QueryRow(ctx, `SELECT archived_at FROM memory_stores WHERE id = $1 FOR UPDATE`, item.MemoryStoreID).Scan(&archivedAt); errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("memory store not found")
		} else if err != nil {
			return err
		}
		if archivedAt != nil {
			return domain.Validation("archived memory stores are read-only")
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM memories WHERE memory_store_id = $1`, item.MemoryStoreID).Scan(&count); err != nil {
			return err
		}
		if count >= app.MaxMemoriesPerStore {
			return domain.Validation("memory store already contains 2000 memories")
		}
		if err := insertMemoryVersion(ctx, tx, version); err != nil {
			return err
		}
		var err error
		created, err = scanMemory(tx.QueryRow(ctx, `
INSERT INTO memories (
    id, memory_store_id, memory_version_id, path, content,
    content_size_bytes, content_sha256, created_at, updated_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
RETURNING id, memory_store_id, memory_version_id, path, content,
          content_size_bytes, content_sha256, created_at, updated_at`,
			item.ID, item.MemoryStoreID, item.MemoryVersionID, item.Path, item.Content,
			item.ContentSize, item.ContentSHA256, item.CreatedAt, item.UpdatedAt,
		))
		return err
	})
	if isUniqueViolation(err) {
		return domain.Memory{}, domain.Conflict("a memory already exists at this path")
	}
	return created, err
}

func (r *MemoryRepository) GetMemory(ctx context.Context, storeID, memoryID string) (domain.Memory, error) {
	if _, err := r.GetStore(ctx, storeID); err != nil {
		return domain.Memory{}, err
	}
	item, err := scanMemory(r.store.pool.QueryRow(ctx, `
SELECT id, memory_store_id, memory_version_id, path, content,
       content_size_bytes, content_sha256, created_at, updated_at
FROM memories WHERE memory_store_id = $1 AND id = $2`, storeID, memoryID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Memory{}, domain.NotFound("memory not found")
	}
	return item, err
}

func (r *MemoryRepository) ListMemoryHeads(ctx context.Context, storeID, prefix string) ([]domain.Memory, error) {
	if _, err := r.GetStore(ctx, storeID); err != nil {
		return nil, err
	}
	rows, err := r.store.pool.Query(ctx, `
SELECT id, memory_store_id, memory_version_id, path, content,
       content_size_bytes, content_sha256, created_at, updated_at
FROM memories
WHERE memory_store_id = $1 AND ($2 = '' OR left(path, length($2)) = $2)
ORDER BY path ASC`, storeID, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.Memory, 0)
	for rows.Next() {
		item, scanErr := scanMemory(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *MemoryRepository) UpdateMemory(
	ctx context.Context,
	storeID, memoryID string,
	patch app.MemoryUpdateInput,
	versionID string,
	clock domain.Clock,
) (domain.Memory, error) {
	if _, err := r.GetStore(ctx, storeID); err != nil {
		return domain.Memory{}, err
	}
	var updated domain.Memory
	err := r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		if err := r.fenceSessionMemoryWrite(ctx, tx, storeID); err != nil {
			return err
		}
		var storeArchivedAt *time.Time
		if err := tx.QueryRow(ctx, `
SELECT archived_at FROM memory_stores WHERE id = $1 FOR UPDATE`, storeID).Scan(&storeArchivedAt); errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("memory store not found")
		} else if err != nil {
			return err
		}
		if storeArchivedAt != nil {
			return domain.Validation("archived memory stores are read-only")
		}
		var archivedAt *time.Time
		current, err := scanMemoryWithArchive(tx.QueryRow(ctx, `
SELECT m.id, m.memory_store_id, m.memory_version_id, m.path, m.content,
       m.content_size_bytes, m.content_sha256, m.created_at, m.updated_at,
       s.archived_at
FROM memories AS m
JOIN memory_stores AS s ON s.id = m.memory_store_id
WHERE m.memory_store_id = $1 AND m.id = $2
FOR UPDATE OF m`, storeID, memoryID), &archivedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("memory not found")
		}
		if err != nil {
			return err
		}
		path, content := current.Path, current.Content
		if patch.Path != nil {
			path = *patch.Path
		}
		if patch.Content != nil {
			content = *patch.Content
		}
		if path == current.Path && content == current.Content {
			updated = current
			return nil
		}
		_ = archivedAt
		if patch.ExpectedContentSHA != nil && *patch.ExpectedContentSHA != current.ContentSHA256 {
			return domain.MemoryPrecondition("memory content_sha256 precondition failed")
		}
		now := clock.Now().UTC().Truncate(time.Microsecond)
		sha := appMemoryContentSHA(content)
		size := int64(len([]byte(content)))
		version := domain.MemoryVersion{
			ID: versionID, MemoryStoreID: storeID, MemoryID: memoryID,
			Operation: "modified", Path: &path, Content: &content,
			ContentSize: &size, ContentSHA256: &sha, CreatedAt: now,
			CreatedBy: patch.Actor,
		}
		if err := insertMemoryVersion(ctx, tx, version); err != nil {
			return err
		}
		updated, err = scanMemory(tx.QueryRow(ctx, `
UPDATE memories
SET memory_version_id = $3, path = $4, content = $5,
    content_size_bytes = $6, content_sha256 = $7, updated_at = $8
WHERE memory_store_id = $1 AND id = $2
RETURNING id, memory_store_id, memory_version_id, path, content,
          content_size_bytes, content_sha256, created_at, updated_at`,
			storeID, memoryID, versionID, path, content, size, sha, now,
		))
		return err
	})
	if isUniqueViolation(err) {
		return domain.Memory{}, domain.Conflict("a memory already exists at this path")
	}
	return updated, err
}

func (r *MemoryRepository) DeleteMemory(
	ctx context.Context,
	storeID, memoryID string,
	expected *string,
	actor domain.MemoryActor,
	versionID string,
	clock domain.Clock,
) (domain.Memory, error) {
	if _, err := r.GetStore(ctx, storeID); err != nil {
		return domain.Memory{}, err
	}
	var deleted domain.Memory
	err := r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		if err := r.fenceSessionMemoryWrite(ctx, tx, storeID); err != nil {
			return err
		}
		var storeArchivedAt *time.Time
		if err := tx.QueryRow(ctx, `
SELECT archived_at FROM memory_stores WHERE id = $1 FOR UPDATE`, storeID).Scan(&storeArchivedAt); errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("memory store not found")
		} else if err != nil {
			return err
		}
		if storeArchivedAt != nil {
			return domain.Validation("archived memory stores are read-only")
		}
		var archivedAt *time.Time
		current, err := scanMemoryWithArchive(tx.QueryRow(ctx, `
SELECT m.id, m.memory_store_id, m.memory_version_id, m.path, m.content,
       m.content_size_bytes, m.content_sha256, m.created_at, m.updated_at,
       s.archived_at
FROM memories AS m
JOIN memory_stores AS s ON s.id = m.memory_store_id
WHERE m.memory_store_id = $1 AND m.id = $2
FOR UPDATE OF m`, storeID, memoryID), &archivedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("memory not found")
		}
		if err != nil {
			return err
		}
		_ = archivedAt
		if expected != nil && *expected != current.ContentSHA256 {
			return domain.MemoryPrecondition("memory content_sha256 precondition failed")
		}
		now := clock.Now().UTC().Truncate(time.Microsecond)
		path := current.Path
		if err := insertMemoryVersion(ctx, tx, domain.MemoryVersion{
			ID: versionID, MemoryStoreID: storeID, MemoryID: memoryID,
			Operation: "deleted", Path: &path, CreatedAt: now, CreatedBy: actor,
		}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM memories WHERE memory_store_id = $1 AND id = $2`, storeID, memoryID); err != nil {
			return err
		}
		deleted = current
		return nil
	})
	return deleted, err
}

// SyncMemoryStore applies a complete sandbox diff in one Store-serialized
// transaction. Unchanged paths accept newer remote heads; locally changed
// paths require the exact baseline SHA so concurrent Sessions cannot silently
// overwrite one another.
func (r *MemoryRepository) SyncMemoryStore(
	ctx context.Context,
	storeID string,
	baseline []app.MemoryStoreSyncBaseline,
	current []app.MemoryStoreSyncContent,
	actor domain.MemoryActor,
	ids domain.IDGenerator,
	clock domain.Clock,
) ([]domain.Memory, error) {
	if _, err := r.GetStore(ctx, storeID); err != nil {
		return nil, err
	}
	var result []domain.Memory
	err := r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		var archivedAt *time.Time
		if err := tx.QueryRow(ctx, `
SELECT archived_at FROM memory_stores WHERE id = $1 FOR UPDATE`, storeID).Scan(&archivedAt); errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("memory store not found")
		} else if err != nil {
			return err
		}

		rows, err := tx.Query(ctx, `
SELECT id, memory_store_id, memory_version_id, path, content,
       content_size_bytes, content_sha256, created_at, updated_at
FROM memories
WHERE memory_store_id = $1
ORDER BY path
FOR UPDATE`, storeID)
		if err != nil {
			return err
		}
		heads := make([]domain.Memory, 0)
		for rows.Next() {
			item, scanErr := scanMemory(rows)
			if scanErr != nil {
				rows.Close()
				return scanErr
			}
			heads = append(heads, item)
		}
		rowsErr := rows.Err()
		rows.Close()
		if rowsErr != nil {
			return rowsErr
		}
		dbByID := make(map[string]domain.Memory, len(heads))
		dbByPath := make(map[string]domain.Memory, len(heads))
		for _, item := range heads {
			dbByID[item.ID] = item
			dbByPath[item.Path] = item
		}
		baselineByPath := make(map[string]app.MemoryStoreSyncBaseline, len(baseline))
		for _, item := range baseline {
			baselineByPath[item.Path] = item
		}
		currentByPath := make(map[string]app.MemoryStoreSyncContent, len(current))
		currentSHA := make(map[string]string, len(current))
		for _, item := range current {
			currentByPath[item.Path] = item
			currentSHA[item.Path] = appMemoryContentSHA(item.Content)
		}

		type updateOperation struct {
			head    domain.Memory
			content app.MemoryStoreSyncContent
			sha     string
		}
		deletes := make([]domain.Memory, 0)
		updates := make([]updateOperation, 0)
		creates := make([]app.MemoryStoreSyncContent, 0)
		for _, base := range baseline {
			local, present := currentByPath[base.Path]
			if present && currentSHA[base.Path] == base.ContentSHA256 {
				continue
			}
			db, exists := dbByID[base.MemoryID]
			if !present {
				if !exists {
					continue
				}
				if db.Path != base.Path || db.ContentSHA256 != base.ContentSHA256 {
					return domain.MemoryPrecondition(
						"memory changed in another Session before local deletion",
					)
				}
				deletes = append(deletes, db)
				continue
			}
			desiredSHA := currentSHA[base.Path]
			if exists && db.Path == base.Path && db.ContentSHA256 == desiredSHA {
				continue
			}
			if !exists || db.Path != base.Path || db.ContentSHA256 != base.ContentSHA256 {
				return domain.MemoryPrecondition(
					"memory changed in another Session before local update",
				)
			}
			updates = append(updates, updateOperation{head: db, content: local, sha: desiredSHA})
		}
		for _, local := range current {
			if _, existed := baselineByPath[local.Path]; existed {
				continue
			}
			if db, exists := dbByPath[local.Path]; exists {
				if db.ContentSHA256 == currentSHA[local.Path] {
					continue
				}
				return domain.MemoryPrecondition(
					"memory path was created in another Session",
				)
			}
			creates = append(creates, local)
		}
		if len(heads)-len(deletes)+len(creates) > app.MaxMemoriesPerStore {
			return domain.TooLarge("memory store cannot contain more than 2000 memories")
		}
		if archivedAt != nil && (len(deletes) > 0 || len(updates) > 0 || len(creates) > 0) {
			return domain.Validation("archived memory stores are read-only")
		}
		now := clock.Now().UTC().Truncate(time.Microsecond)
		for _, operation := range updates {
			content := operation.content.Content
			size := int64(len([]byte(content)))
			versionID := ids.NewID(domain.PrefixMemoryVersion)
			path := operation.head.Path
			if err := insertMemoryVersion(ctx, tx, domain.MemoryVersion{
				ID: versionID, MemoryStoreID: storeID, MemoryID: operation.head.ID,
				Operation: "modified", Path: &path, Content: &content,
				ContentSize: &size, ContentSHA256: &operation.sha,
				CreatedAt: now, CreatedBy: actor,
			}); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
UPDATE memories
SET memory_version_id = $3, content = $4, content_size_bytes = $5,
    content_sha256 = $6, updated_at = $7
WHERE memory_store_id = $1 AND id = $2`,
				storeID, operation.head.ID, versionID, content, size,
				operation.sha, now,
			); err != nil {
				return err
			}
		}
		for _, head := range deletes {
			versionID := ids.NewID(domain.PrefixMemoryVersion)
			path := head.Path
			if err := insertMemoryVersion(ctx, tx, domain.MemoryVersion{
				ID: versionID, MemoryStoreID: storeID, MemoryID: head.ID,
				Operation: "deleted", Path: &path, CreatedAt: now, CreatedBy: actor,
			}); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
DELETE FROM memories WHERE memory_store_id = $1 AND id = $2`, storeID, head.ID); err != nil {
				return err
			}
		}
		for _, local := range creates {
			memoryID := ids.NewID(domain.PrefixMemory)
			versionID := ids.NewID(domain.PrefixMemoryVersion)
			sha := currentSHA[local.Path]
			size := int64(len([]byte(local.Content)))
			path, content := local.Path, local.Content
			if err := insertMemoryVersion(ctx, tx, domain.MemoryVersion{
				ID: versionID, MemoryStoreID: storeID, MemoryID: memoryID,
				Operation: "created", Path: &path, Content: &content,
				ContentSize: &size, ContentSHA256: &sha,
				CreatedAt: now, CreatedBy: actor,
			}); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO memories (
    id, memory_store_id, memory_version_id, path, content,
    content_size_bytes, content_sha256, created_at, updated_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8)`,
				memoryID, storeID, versionID, path, content, size, sha, now,
			); err != nil {
				if isUniqueViolation(err) {
					return domain.MemoryPrecondition("memory path was created concurrently")
				}
				return err
			}
		}

		resultRows, err := tx.Query(ctx, `
SELECT id, memory_store_id, memory_version_id, path, content,
       content_size_bytes, content_sha256, created_at, updated_at
FROM memories WHERE memory_store_id = $1 ORDER BY path`, storeID)
		if err != nil {
			return err
		}
		defer resultRows.Close()
		result = make([]domain.Memory, 0, len(heads)-len(deletes)+len(creates))
		for resultRows.Next() {
			item, scanErr := scanMemory(resultRows)
			if scanErr != nil {
				return scanErr
			}
			result = append(result, item)
		}
		return resultRows.Err()
	})
	return result, err
}

func (r *MemoryRepository) GetMemoryVersion(ctx context.Context, storeID, versionID string) (domain.MemoryVersion, error) {
	if _, err := r.GetStore(ctx, storeID); err != nil {
		return domain.MemoryVersion{}, err
	}
	item, err := scanMemoryVersion(r.store.pool.QueryRow(ctx, memoryVersionSelect+`
WHERE memory_store_id = $1 AND id = $2`, storeID, versionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.MemoryVersion{}, domain.NotFound("memory version not found")
	}
	return item, err
}

func (r *MemoryRepository) ListMemoryVersions(ctx context.Context, storeID string, query app.MemoryVersionListQuery) (app.MemoryVersionListPage, error) {
	if _, err := r.GetStore(ctx, storeID); err != nil {
		return app.MemoryVersionListPage{}, err
	}
	args := []any{storeID}
	where := []string{"memory_store_id = $1"}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if query.APIKeyID != "" {
		add("created_by_type = 'api_actor' AND created_by_id = $%d", query.APIKeyID)
	}
	if query.SessionID != "" {
		add("created_by_type = 'session_actor' AND created_by_id = $%d", query.SessionID)
	}
	if query.MemoryID != "" {
		add("memory_id = $%d", query.MemoryID)
	}
	if query.Operation != "" {
		add("operation = $%d", query.Operation)
	}
	if query.CreatedAtGte != nil {
		add("created_at >= $%d", query.CreatedAtGte.UTC())
	}
	if query.CreatedAtLte != nil {
		add("created_at <= $%d", query.CreatedAtLte.UTC())
	}
	if query.After != nil {
		args = append(args, query.After.CreatedAt.UTC(), query.After.ID)
		where = append(where, fmt.Sprintf("(created_at, id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, query.Limit+1)
	rows, err := r.store.pool.Query(ctx, memoryVersionSelect+fmt.Sprintf(`
WHERE %s
ORDER BY created_at DESC, id DESC
LIMIT $%d`, strings.Join(where, " AND "), len(args)), args...)
	if err != nil {
		return app.MemoryVersionListPage{}, err
	}
	defer rows.Close()
	items := make([]domain.MemoryVersion, 0, query.Limit+1)
	for rows.Next() {
		item, scanErr := scanMemoryVersion(rows)
		if scanErr != nil {
			return app.MemoryVersionListPage{}, scanErr
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return app.MemoryVersionListPage{}, err
	}
	page := app.MemoryVersionListPage{Versions: items}
	if len(items) > query.Limit {
		page.Versions = items[:query.Limit]
		page.HasNext = true
	}
	return page, nil
}

func (r *MemoryRepository) RedactMemoryVersion(
	ctx context.Context,
	storeID, versionID string,
	actor domain.MemoryActor,
	clock domain.Clock,
) (domain.MemoryVersion, error) {
	if _, err := r.GetStore(ctx, storeID); err != nil {
		return domain.MemoryVersion{}, err
	}
	var redacted domain.MemoryVersion
	err := r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		current, err := scanMemoryVersion(tx.QueryRow(ctx, memoryVersionSelect+`
WHERE memory_store_id = $1 AND id = $2 FOR UPDATE`, storeID, versionID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("memory version not found")
		}
		if err != nil {
			return err
		}
		if current.RedactedAt != nil {
			redacted = current
			return nil
		}
		var isHead bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(
    SELECT 1 FROM memories WHERE memory_store_id = $1 AND memory_version_id = $2
)`, storeID, versionID).Scan(&isHead); err != nil {
			return err
		}
		if isHead {
			return domain.Validation("the current memory version cannot be redacted")
		}
		now := clock.Now().UTC().Truncate(time.Microsecond)
		redacted, err = scanMemoryVersion(tx.QueryRow(ctx, `
UPDATE memory_versions
SET path = NULL, content = NULL, content_size_bytes = NULL,
    content_sha256 = NULL, redacted_at = $3,
    redacted_by_type = $4, redacted_by_id = $5
WHERE memory_store_id = $1 AND id = $2
RETURNING id, memory_store_id, memory_id, operation, path, content,
          content_size_bytes, content_sha256, created_at, created_by_type,
          created_by_id, redacted_at, redacted_by_type, redacted_by_id`,
			storeID, versionID, now, actor.Type, actor.ID,
		))
		return err
	})
	return redacted, err
}

const memoryVersionSelect = `
SELECT id, memory_store_id, memory_id, operation, path, content,
       content_size_bytes, content_sha256, created_at, created_by_type,
       created_by_id, redacted_at, redacted_by_type, redacted_by_id
FROM memory_versions
`

type memoryScanner interface{ Scan(...any) error }

func scanMemoryStore(row memoryScanner) (domain.MemoryStore, error) {
	var item domain.MemoryStore
	var metadata []byte
	if err := row.Scan(&item.ID, &item.Name, &item.Description, &metadata,
		&item.CreatedAt, &item.UpdatedAt, &item.ArchivedAt); err != nil {
		return domain.MemoryStore{}, err
	}
	if err := json.Unmarshal(metadata, &item.Metadata); err != nil {
		return domain.MemoryStore{}, err
	}
	if item.Metadata == nil {
		item.Metadata = map[string]string{}
	}
	item.CreatedAt = item.CreatedAt.UTC()
	item.UpdatedAt = item.UpdatedAt.UTC()
	if item.ArchivedAt != nil {
		value := item.ArchivedAt.UTC()
		item.ArchivedAt = &value
	}
	return item, nil
}

func scanMemory(row memoryScanner) (domain.Memory, error) {
	var item domain.Memory
	if err := row.Scan(
		&item.ID, &item.MemoryStoreID, &item.MemoryVersionID, &item.Path,
		&item.Content, &item.ContentSize, &item.ContentSHA256,
		&item.CreatedAt, &item.UpdatedAt,
	); err != nil {
		return domain.Memory{}, err
	}
	item.CreatedAt = item.CreatedAt.UTC()
	item.UpdatedAt = item.UpdatedAt.UTC()
	return item, nil
}

func scanMemoryWithArchive(row memoryScanner, archivedAt **time.Time) (domain.Memory, error) {
	var item domain.Memory
	if err := row.Scan(
		&item.ID, &item.MemoryStoreID, &item.MemoryVersionID, &item.Path,
		&item.Content, &item.ContentSize, &item.ContentSHA256,
		&item.CreatedAt, &item.UpdatedAt, archivedAt,
	); err != nil {
		return domain.Memory{}, err
	}
	item.CreatedAt = item.CreatedAt.UTC()
	item.UpdatedAt = item.UpdatedAt.UTC()
	return item, nil
}

func scanMemoryVersion(row memoryScanner) (domain.MemoryVersion, error) {
	var item domain.MemoryVersion
	var redactedType, redactedID *string
	if err := row.Scan(
		&item.ID, &item.MemoryStoreID, &item.MemoryID, &item.Operation,
		&item.Path, &item.Content, &item.ContentSize, &item.ContentSHA256,
		&item.CreatedAt, &item.CreatedBy.Type, &item.CreatedBy.ID,
		&item.RedactedAt, &redactedType, &redactedID,
	); err != nil {
		return domain.MemoryVersion{}, err
	}
	item.CreatedAt = item.CreatedAt.UTC()
	if item.RedactedAt != nil {
		value := item.RedactedAt.UTC()
		item.RedactedAt = &value
	}
	if redactedType != nil && redactedID != nil {
		item.RedactedBy = &domain.MemoryActor{Type: *redactedType, ID: *redactedID}
	}
	return item, nil
}

func insertMemoryVersion(ctx context.Context, tx pgx.Tx, item domain.MemoryVersion) error {
	_, err := tx.Exec(ctx, `
INSERT INTO memory_versions (
    id, memory_store_id, memory_id, operation, path, content,
    content_size_bytes, content_sha256, created_at, created_by_type,
    created_by_id, redacted_at, redacted_by_type, redacted_by_id
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULL,NULL,NULL)`,
		item.ID, item.MemoryStoreID, item.MemoryID, item.Operation, item.Path,
		item.Content, item.ContentSize, item.ContentSHA256, item.CreatedAt,
		item.CreatedBy.Type, item.CreatedBy.ID,
	)
	return err
}

func normalizeMemoryStoreTimes(item *domain.MemoryStore) {
	item.CreatedAt = item.CreatedAt.UTC().Truncate(time.Microsecond)
	item.UpdatedAt = item.UpdatedAt.UTC().Truncate(time.Microsecond)
}

func normalizeMemoryTimes(item *domain.Memory, version *domain.MemoryVersion) {
	item.CreatedAt = item.CreatedAt.UTC().Truncate(time.Microsecond)
	item.UpdatedAt = item.UpdatedAt.UTC().Truncate(time.Microsecond)
	version.CreatedAt = version.CreatedAt.UTC().Truncate(time.Microsecond)
}

func clonePGStringMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func equalStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

// Keep hashing local to persistence so API and filesystem write-back use the
// exact same byte semantics without exporting a service implementation detail.
func appMemoryContentSHA(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", sum)
}
