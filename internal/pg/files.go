package pg

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

var _ app.FileRepository = (*FileRepository)(nil)

type FileRepository struct {
	store *Store
}

const insertFileStatement = `
INSERT INTO files (
    id, created_at, updated_at, filename, mime_type, size_bytes,
    downloadable, scope_id, scope_type, blob_key, checksum_sha256, state,
    workspace_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`

func NewFileRepository(store *Store) *FileRepository {
	return &FileRepository{store: store}
}

func (r *FileRepository) BeginUpload(ctx context.Context, file domain.File) error {
	workspaceID, err := r.store.workspaceForWrite(ctx)
	if err != nil {
		return err
	}
	file.CreatedAt = file.CreatedAt.UTC().Truncate(time.Microsecond)
	file.UpdatedAt = file.UpdatedAt.UTC().Truncate(time.Microsecond)
	var scopeID, scopeType *string
	if file.Scope != nil {
		scopeID, scopeType = &file.Scope.ID, &file.Scope.Type
	}
	_, err = r.store.pool.Exec(ctx, insertFileStatement,
		file.ID, file.CreatedAt, file.UpdatedAt, file.Filename, file.MimeType,
		file.SizeBytes, file.Downloadable, scopeID, scopeType, file.BlobKey,
		file.ChecksumSHA256, string(file.State),
		workspaceID,
	)
	if isUniqueViolation(err) {
		return domain.Conflict("file id already exists")
	}
	return err
}

func (r *FileRepository) CompleteUpload(
	ctx context.Context,
	id string,
	info app.BlobInfo,
) (domain.File, error) {
	workspaceID, _, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return domain.File{}, err
	}
	row := r.store.pool.QueryRow(ctx, `
UPDATE files
SET size_bytes = $2,
    checksum_sha256 = $3,
    state = 'ready',
    updated_at = now()
WHERE id = $1 AND ($4 = '' OR workspace_id = $4) AND state = 'uploading'
RETURNING id, created_at, updated_at, filename, mime_type, size_bytes,
          downloadable, scope_id, scope_type, blob_key, checksum_sha256, state`,
		id, info.SizeBytes, info.ChecksumSHA256, workspaceID,
	)
	file, err := scanFile(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.File{}, domain.Conflict("file upload is no longer pending")
	}
	return file, err
}

func (r *FileRepository) Get(ctx context.Context, id string) (domain.File, error) {
	workspaceID, _, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return domain.File{}, err
	}
	row := r.store.pool.QueryRow(ctx, `
SELECT id, created_at, updated_at, filename, mime_type, size_bytes,
       downloadable, scope_id, scope_type, blob_key, checksum_sha256, state
FROM files
WHERE id = $1 AND ($2 = '' OR workspace_id = $2) AND state = 'ready'`, id, workspaceID)
	file, err := scanFile(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.File{}, domain.NotFound("file not found")
	}
	return file, err
}

func (r *FileRepository) List(
	ctx context.Context,
	query app.FileListQuery,
) (app.FileListPage, error) {
	workspaceID, scoped, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return app.FileListPage{}, err
	}
	args := make([]any, 0, 5)
	where := []string{"state = 'ready'"}
	if scoped {
		args = append(args, workspaceID)
		where = append(where, fmt.Sprintf("workspace_id = $%d", len(args)))
	}
	if query.ScopeID != "" {
		args = append(args, query.ScopeID)
		where = append(where, fmt.Sprintf("scope_id = $%d", len(args)))
	}

	before := query.BeforeID != ""
	cursorID := query.AfterID
	operator := "<"
	order := "created_at DESC, id DESC"
	if before {
		cursorID = query.BeforeID
		operator = ">"
		order = "created_at ASC, id ASC"
	}
	if cursorID != "" {
		boundary, err := r.Get(ctx, cursorID)
		if err != nil {
			var domainErr *domain.DomainError
			if errors.As(err, &domainErr) && domainErr.Kind == domain.KindNotFound {
				return app.FileListPage{}, domain.Validation("file cursor not found")
			}
			return app.FileListPage{}, err
		}
		args = append(args, boundary.CreatedAt, boundary.ID)
		where = append(where, fmt.Sprintf(
			"(created_at, id) %s ($%d, $%d)", operator, len(args)-1, len(args),
		))
	}
	args = append(args, query.Limit+1)
	statement := fmt.Sprintf(`
SELECT id, created_at, updated_at, filename, mime_type, size_bytes,
       downloadable, scope_id, scope_type, blob_key, checksum_sha256, state
FROM files
WHERE %s
ORDER BY %s
LIMIT $%d`, strings.Join(where, " AND "), order, len(args))
	rows, err := r.store.pool.Query(ctx, statement, args...)
	if err != nil {
		return app.FileListPage{}, err
	}
	defer rows.Close()
	files := make([]domain.File, 0, query.Limit+1)
	for rows.Next() {
		file, scanErr := scanFile(rows)
		if scanErr != nil {
			return app.FileListPage{}, scanErr
		}
		files = append(files, file)
	}
	if err := rows.Err(); err != nil {
		return app.FileListPage{}, err
	}
	hasMore := len(files) > query.Limit
	if hasMore {
		files = files[:query.Limit]
	}
	if before {
		slices.Reverse(files)
	}
	return app.FileListPage{Files: files, HasMore: hasMore}, nil
}

func (r *FileRepository) BeginDelete(ctx context.Context, id string) (domain.File, error) {
	workspaceID, _, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return domain.File{}, err
	}
	row := r.store.pool.QueryRow(ctx, `
UPDATE files AS target
SET state = 'deleting', updated_at = now()
WHERE target.id = $1 AND ($2 = '' OR target.workspace_id = $2) AND target.state = 'ready'
RETURNING id, created_at, updated_at, filename, mime_type, size_bytes,
          downloadable, scope_id, scope_type, blob_key, checksum_sha256, state`, id, workspaceID)
	file, err := scanFile(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.File{}, domain.NotFound("file not found")
	}
	return file, err
}

func (r *FileRepository) RemoveIncomplete(ctx context.Context, id string) error {
	workspaceID, _, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return err
	}
	_, err = r.store.pool.Exec(ctx, `
DELETE FROM files
WHERE id = $1 AND ($2 = '' OR workspace_id = $2) AND state <> 'ready'`, id, workspaceID)
	return err
}

func (r *FileRepository) ListIncomplete(ctx context.Context) ([]domain.File, error) {
	rows, err := r.store.pool.Query(ctx, `
SELECT id, created_at, updated_at, filename, mime_type, size_bytes,
       downloadable, scope_id, scope_type, blob_key, checksum_sha256, state
FROM files
WHERE state <> 'ready'
ORDER BY updated_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := make([]domain.File, 0)
	for rows.Next() {
		file, scanErr := scanFile(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

type fileScanner interface {
	Scan(...any) error
}

func scanFile(row fileScanner) (domain.File, error) {
	var file domain.File
	var scopeID, scopeType *string
	err := row.Scan(
		&file.ID, &file.CreatedAt, &file.UpdatedAt, &file.Filename,
		&file.MimeType, &file.SizeBytes, &file.Downloadable,
		&scopeID, &scopeType, &file.BlobKey, &file.ChecksumSHA256,
		&file.State,
	)
	if err != nil {
		return domain.File{}, err
	}
	file.CreatedAt = file.CreatedAt.UTC()
	file.UpdatedAt = file.UpdatedAt.UTC()
	if scopeID != nil && scopeType != nil {
		file.Scope = &domain.FileScope{ID: *scopeID, Type: *scopeType}
	}
	return file, nil
}
