package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

var _ app.SkillRepository = (*SkillRepository)(nil)

type SkillRepository struct {
	store *Store
	*blobCleanupRepository
}

func NewSkillRepository(store *Store) *SkillRepository {
	return &SkillRepository{store: store, blobCleanupRepository: &blobCleanupRepository{store: store, kind: "skill"}}
}

func (r *SkillRepository) BeginSkill(
	ctx context.Context,
	skill domain.Skill,
	version domain.SkillVersion,
) error {
	workspaceID, err := r.store.workspaceForWrite(ctx)
	if err != nil {
		return err
	}
	normalizeSkillTimes(&skill, &version)
	err = r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		// Every creator of the same normalized title takes the same transaction
		// lock. Two derived titles may coexist, but an explicit title must not
		// collide with any custom Skill and an existing explicit title remains
		// exclusive when a later derived title is created.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(lower($1), 0))`,
			skill.DisplayTitle); err != nil {
			return err
		}
		var titleConflict bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM skills
    WHERE lower(display_title) = lower($1)
      AND workspace_id = $3
      AND (display_title_explicit OR $2)
)`, skill.DisplayTitle, skill.TitleExplicit, workspaceID).Scan(&titleConflict); err != nil {
			return err
		}
		if titleConflict {
			return domain.Validation("display_title is already used by another custom Skill")
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO skills (
    id, created_at, updated_at, display_title, latest_version, source,
    display_title_explicit, ready, workspace_id
) VALUES ($1, $2, $3, $4, NULL, $5, $6, false, $7)`,
			skill.ID, skill.CreatedAt, skill.UpdatedAt, skill.DisplayTitle,
			skill.Source, skill.TitleExplicit, workspaceID,
		); err != nil {
			return err
		}
		return insertSkillVersion(ctx, tx, version)
	})
	if isUniqueViolation(err) {
		if skill.TitleExplicit {
			return domain.Validation("display_title is already used by another custom Skill")
		}
		return domain.Conflict("skill or Skill Version already exists")
	}
	return err
}

func (r *SkillRepository) BeginVersion(ctx context.Context, version domain.SkillVersion) error {
	workspaceID, _, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return err
	}
	version.CreatedAt = version.CreatedAt.UTC().Truncate(time.Microsecond)
	tag, err := r.store.pool.Exec(ctx, `
INSERT INTO skill_versions (
    skill_id, version, created_at, description, directory, name, blob_key,
    size_bytes, uncompressed_size_bytes, checksum_sha256, state, initial, upload_expires_at
)
SELECT $1, $2, $3, $4, $5, $6, $7, 0, $8, '', $9, $10, clock_timestamp() + $12 * interval '1 second'
FROM skills
WHERE id = $1 AND ($11 = '' OR workspace_id = $11) AND ready`,
		version.SkillID, version.Version, version.CreatedAt, version.Description,
		version.Directory, version.Name, version.BlobKey, version.UncompressedSizeBytes,
		string(version.State), version.Initial, workspaceID, app.UploadLeaseDuration.Seconds(),
	)
	if isUniqueViolation(err) {
		return domain.Conflict("Skill Version already exists")
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.NotFound("skill not found")
	}
	return nil
}

func (r *SkillRepository) CompleteVersion(
	ctx context.Context,
	skillID string,
	version string,
	blobKey string,
	info app.BlobInfo,
) (domain.Skill, domain.SkillVersion, error) {
	workspaceID, _, accessErr := r.store.workspaceForRead(ctx)
	if accessErr != nil {
		return domain.Skill{}, domain.SkillVersion{}, accessErr
	}
	var skill domain.Skill
	var item domain.SkillVersion
	err := r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		if err := r.lockUpload(ctx, tx, skillID, version, blobKey, workspaceID); err != nil {
			if errors.Is(err, app.ErrUploadLeaseLost) {
				return errors.Join(err, domain.Conflict("Skill Version upload is no longer pending"))
			}
			return err
		}
		row := tx.QueryRow(ctx, `
UPDATE skill_versions
SET size_bytes = $3, checksum_sha256 = $4, state = 'ready', upload_expires_at = NULL
WHERE skill_id = $1 AND version = $2 AND state = 'uploading' AND upload_expires_at > clock_timestamp() AND blob_key = $6
  AND EXISTS (
      SELECT 1 FROM skills
      WHERE id = $1 AND ($5 = '' OR workspace_id = $5)
  )
RETURNING skill_id, version, created_at, description, directory, name, blob_key,
          size_bytes, uncompressed_size_bytes, checksum_sha256, state, initial`,
			skillID, version, info.SizeBytes, info.ChecksumSHA256, workspaceID, blobKey,
		)
		var err error
		item, err = scanSkillVersion(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.Join(app.ErrUploadLeaseLost, domain.Conflict("Skill Version upload is no longer pending"))
		}
		if err != nil {
			return err
		}
		skill, err = scanSkill(tx.QueryRow(ctx, `
UPDATE skills AS skill
SET latest_version = newest.version,
    updated_at = newest.created_at,
    ready = true
FROM (
    SELECT candidate.version, candidate.created_at
    FROM skill_versions AS candidate
    WHERE candidate.skill_id = $1 AND candidate.state = 'ready'
    ORDER BY candidate.created_at DESC, candidate.version DESC
    LIMIT 1
) AS newest
WHERE skill.id = $1 AND ($2 = '' OR skill.workspace_id = $2)
RETURNING skill.id, skill.created_at, skill.updated_at, skill.display_title,
          skill.latest_version, skill.source, skill.display_title_explicit,
          skill.ready`, skillID, workspaceID))
		return err
	})
	return skill, item, err
}

func (r *SkillRepository) GetSkill(ctx context.Context, id string) (domain.Skill, error) {
	workspaceID, _, accessErr := r.store.workspaceForRead(ctx)
	if accessErr != nil {
		return domain.Skill{}, accessErr
	}
	skill, err := scanSkill(r.store.pool.QueryRow(ctx, `
SELECT id, created_at, updated_at, display_title, latest_version, source,
       display_title_explicit, ready
FROM skills
WHERE id = $1 AND ($2 = '' OR workspace_id = $2) AND ready`, id, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Skill{}, domain.NotFound("skill not found")
	}
	return skill, err
}

func (r *SkillRepository) ListSkills(
	ctx context.Context,
	query app.SkillListQuery,
) (app.SkillListPage, error) {
	workspaceID, scoped, accessErr := r.store.workspaceForRead(ctx)
	if accessErr != nil {
		return app.SkillListPage{}, accessErr
	}
	args := make([]any, 0, 4)
	where := []string{"ready"}
	if scoped {
		args = append(args, workspaceID)
		where = append(where, fmt.Sprintf("workspace_id = $%d", len(args)))
	}
	if query.After != nil {
		args = append(args, query.After.CreatedAt, query.After.ID)
		where = append(where, fmt.Sprintf("(created_at, id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, query.Limit+1)
	rows, err := r.store.pool.Query(ctx, fmt.Sprintf(`
SELECT id, created_at, updated_at, display_title, latest_version, source,
       display_title_explicit, ready
FROM skills
WHERE %s
ORDER BY created_at DESC, id DESC
LIMIT $%d`, strings.Join(where, " AND "), len(args)), args...)
	if err != nil {
		return app.SkillListPage{}, err
	}
	defer rows.Close()
	items := make([]domain.Skill, 0, query.Limit+1)
	for rows.Next() {
		item, scanErr := scanSkill(rows)
		if scanErr != nil {
			return app.SkillListPage{}, scanErr
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return app.SkillListPage{}, err
	}
	page := app.SkillListPage{Skills: items}
	if len(items) > query.Limit {
		page.Skills = items[:query.Limit]
		page.HasNext = true
	}
	return page, nil
}

func (r *SkillRepository) GetVersion(
	ctx context.Context,
	skillID string,
	version string,
) (domain.SkillVersion, error) {
	workspaceID, _, accessErr := r.store.workspaceForRead(ctx)
	if accessErr != nil {
		return domain.SkillVersion{}, accessErr
	}
	item, err := scanSkillVersion(r.store.pool.QueryRow(ctx, `
SELECT v.skill_id, v.version, v.created_at, v.description, v.directory, v.name,
       v.blob_key, v.size_bytes, v.uncompressed_size_bytes,
       v.checksum_sha256, v.state, v.initial
FROM skill_versions AS v
JOIN skills AS s ON s.id = v.skill_id AND s.ready
WHERE v.skill_id = $1 AND v.version = $2
  AND ($3 = '' OR s.workspace_id = $3)
  AND v.state = 'ready'`, skillID, version, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SkillVersion{}, domain.NotFound("Skill Version not found")
	}
	return item, err
}

func (r *SkillRepository) ListVersions(
	ctx context.Context,
	skillID string,
	query app.SkillVersionListQuery,
) (app.SkillVersionListPage, error) {
	if _, err := r.GetSkill(ctx, skillID); err != nil {
		return app.SkillVersionListPage{}, err
	}
	args := []any{skillID}
	where := []string{"skill_id = $1", "state = 'ready'"}
	if query.After != nil {
		args = append(args, query.After.CreatedAt, query.After.ID)
		where = append(where, fmt.Sprintf("(created_at, version) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, query.Limit+1)
	rows, err := r.store.pool.Query(ctx, fmt.Sprintf(`
SELECT skill_id, version, created_at, description, directory, name, blob_key,
       size_bytes, uncompressed_size_bytes, checksum_sha256, state, initial
FROM skill_versions
WHERE %s
ORDER BY created_at DESC, version DESC
LIMIT $%d`, strings.Join(where, " AND "), len(args)), args...)
	if err != nil {
		return app.SkillVersionListPage{}, err
	}
	defer rows.Close()
	items := make([]domain.SkillVersion, 0, query.Limit+1)
	for rows.Next() {
		item, scanErr := scanSkillVersion(rows)
		if scanErr != nil {
			return app.SkillVersionListPage{}, scanErr
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return app.SkillVersionListPage{}, err
	}
	page := app.SkillVersionListPage{Versions: items}
	if len(items) > query.Limit {
		page.Versions = items[:query.Limit]
		page.HasNext = true
	}
	return page, nil
}

func (r *SkillRepository) BeginDeleteVersion(
	ctx context.Context,
	skillID string,
	version string,
) (domain.SkillVersion, error) {
	workspaceID, _, accessErr := r.store.workspaceForRead(ctx)
	if accessErr != nil {
		return domain.SkillVersion{}, accessErr
	}
	var item domain.SkillVersion
	err := r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		var err error
		item, err = scanSkillVersion(tx.QueryRow(ctx, `
SELECT target.skill_id, target.version, target.created_at, target.description,
       target.directory, target.name, target.blob_key, target.size_bytes,
       target.uncompressed_size_bytes, target.checksum_sha256,
       target.state, target.initial
FROM skill_versions AS target
JOIN skills AS skill ON skill.id = target.skill_id AND skill.ready
WHERE target.skill_id = $1 AND target.version = $2
  AND ($3 = '' OR skill.workspace_id = $3)
  AND target.state = 'ready'
FOR UPDATE OF target`, skillID, version, workspaceID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("Skill Version not found")
		}
		if err != nil {
			return err
		}
		var referenced bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM agent_skill_versions
    WHERE skill_id = $1 AND skill_version = $2
    UNION ALL
    SELECT 1 FROM session_skill_versions
    WHERE skill_id = $1 AND skill_version = $2
)`, skillID, version).Scan(&referenced); err != nil {
			return err
		}
		if referenced {
			return domain.Validation("Skill Version is in use by an Agent or Session")
		}
		if _, err := tx.Exec(ctx, `
UPDATE skill_versions
SET state = 'deleting'
WHERE skill_id = $1 AND version = $2 AND state = 'ready'`, skillID, version); err != nil {
			return err
		}
		item.State = domain.SkillVersionDeleting
		_, err = tx.Exec(ctx, `
UPDATE skills
SET latest_version = (
        SELECT candidate.version
        FROM skill_versions AS candidate
        WHERE candidate.skill_id = $1 AND candidate.state = 'ready'
        ORDER BY candidate.created_at DESC, candidate.version DESC
        LIMIT 1
    ),
    updated_at = now()
WHERE id = $1`, skillID)
		return err
	})
	return item, err
}

func (r *SkillRepository) RemoveIncompleteVersion(
	ctx context.Context,
	skillID string,
	version string,
	blobKey string,
) error {
	workspaceID, _, accessErr := r.store.workspaceForRead(ctx)
	if accessErr != nil {
		return accessErr
	}
	_, err := r.store.pool.Exec(ctx, `
DELETE FROM skill_versions AS version
USING skills AS skill
WHERE version.skill_id = skill.id
  AND version.skill_id = $1
  AND version.version = $2
  AND ($3 = '' OR skill.workspace_id = $3)
  AND version.state <> 'ready' AND version.blob_key = $4`, skillID, version, workspaceID, blobKey)
	return err
}

func (r *SkillRepository) ListIncompleteVersions(ctx context.Context) ([]domain.SkillVersion, error) {
	rows, err := r.store.pool.Query(ctx, `
SELECT skill_id, version, created_at, description, directory, name, blob_key,
       size_bytes, uncompressed_size_bytes, checksum_sha256, state, initial
FROM skill_versions
WHERE state <> 'ready'
ORDER BY created_at, skill_id, version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.SkillVersion, 0)
	for rows.Next() {
		item, scanErr := scanSkillVersion(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// SessionSkillsForRuntime preserves the single-Agent runtime entry point by
// selecting the Session primary Thread's resolved execution scope.
func (s *Store) SessionSkillsForRuntime(
	ctx context.Context,
	sessionID string,
) ([]domain.SkillVersion, error) {
	threadID, err := s.q.GetPrimarySessionThreadID(ctx, sessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.NotFound("session not found")
		}
		return nil, err
	}
	runtime, err := s.SessionThreadSkillRuntime(ctx, sessionID, threadID)
	return runtime.Versions, err
}

// SessionThreadSkillRuntime returns the immutable Version metadata and runtime
// root selected by a Thread's resolved Agent. The relational pins are the
// authority; no mutable Agent resource lookup belongs on this path.
func (s *Store) SessionThreadSkillRuntime(
	ctx context.Context,
	sessionID string,
	threadID string,
) (domain.SkillRuntime, error) {
	thread, err := s.GetSessionThread(ctx, sessionID, threadID)
	if err != nil {
		return domain.SkillRuntime{}, err
	}
	session, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return domain.SkillRuntime{}, err
	}
	versions, err := s.sessionAgentSkillsForRuntime(
		ctx, sessionID, thread.Agent.ID, thread.Agent.Version,
	)
	if err != nil {
		return domain.SkillRuntime{}, err
	}
	return domain.SkillRuntime{
		Root: domain.SessionAgentSkillRoot(
			session.AgentID, session.AgentVersion, thread.Agent,
		),
		Versions: versions,
	}, nil
}

func (s *Store) sessionAgentSkillsForRuntime(
	ctx context.Context,
	sessionID string,
	agentID string,
	agentVersion int,
) ([]domain.SkillVersion, error) {
	rows, err := s.pool.Query(ctx, `
SELECT version.skill_id, version.version, version.created_at,
       version.description, version.directory, version.name, version.blob_key,
       version.size_bytes, version.uncompressed_size_bytes,
       version.checksum_sha256, version.state, version.initial
FROM session_skill_versions AS pin
JOIN skill_versions AS version
  ON version.skill_id = pin.skill_id
 AND version.version = pin.skill_version
 AND version.state = 'ready'
JOIN skills AS skill ON skill.id = version.skill_id AND skill.ready
WHERE pin.session_id = $1
  AND pin.agent_id = $2
  AND pin.agent_version = $3
ORDER BY pin.position`, sessionID, agentID, agentVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.SkillVersion, 0)
	for rows.Next() {
		item, scanErr := scanSkillVersion(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *SkillRepository) DeleteEmptySkill(ctx context.Context, id string) error {
	workspaceID, _, accessErr := r.store.workspaceForRead(ctx)
	if accessErr != nil {
		return accessErr
	}
	_, err := r.store.pool.Exec(ctx, `
DELETE FROM skills AS skill
WHERE skill.id = $1 AND ($2 = '' OR skill.workspace_id = $2) AND NOT skill.ready
  AND NOT EXISTS (SELECT 1 FROM skill_versions WHERE skill_id = skill.id)`, id, workspaceID)
	return err
}

func (r *SkillRepository) DeleteSkill(ctx context.Context, id string) (domain.Skill, error) {
	workspaceID, _, accessErr := r.store.workspaceForRead(ctx)
	if accessErr != nil {
		return domain.Skill{}, accessErr
	}
	skill, err := scanSkill(r.store.pool.QueryRow(ctx, `
DELETE FROM skills AS skill
WHERE skill.id = $1 AND ($2 = '' OR skill.workspace_id = $2) AND skill.ready
  AND NOT EXISTS (SELECT 1 FROM skill_versions WHERE skill_id = skill.id)
RETURNING id, created_at, updated_at, display_title, latest_version, source,
          display_title_explicit, ready`, id, workspaceID))
	if isForeignKeyViolation(err) {
		return domain.Skill{}, domain.Validation("delete all Skill Versions before deleting the Skill")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return skill, err
	}
	var exists bool
	if queryErr := r.store.pool.QueryRow(ctx, `
SELECT EXISTS(
    SELECT 1 FROM skills
    WHERE id = $1 AND ($2 = '' OR workspace_id = $2) AND ready
)`, id, workspaceID).Scan(&exists); queryErr != nil {
		return domain.Skill{}, queryErr
	}
	if exists {
		return domain.Skill{}, domain.Validation("delete all Skill Versions before deleting the Skill")
	}
	return domain.Skill{}, domain.NotFound("skill not found")
}

func insertSkillVersion(ctx context.Context, tx pgx.Tx, item domain.SkillVersion) error {
	_, err := tx.Exec(ctx, `
INSERT INTO skill_versions (
    skill_id, version, created_at, description, directory, name, blob_key,
    size_bytes, uncompressed_size_bytes, checksum_sha256, state, initial, upload_expires_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, clock_timestamp() + $13 * interval '1 second')`,
		item.SkillID, item.Version, item.CreatedAt, item.Description, item.Directory,
		item.Name, item.BlobKey, item.SizeBytes, item.UncompressedSizeBytes,
		item.ChecksumSHA256,
		string(item.State), item.Initial, app.UploadLeaseDuration.Seconds(),
	)
	return err
}

type skillScanner interface {
	Scan(...any) error
}

func scanSkill(row skillScanner) (domain.Skill, error) {
	var skill domain.Skill
	var latest *string
	err := row.Scan(
		&skill.ID, &skill.CreatedAt, &skill.UpdatedAt, &skill.DisplayTitle, &latest,
		&skill.Source, &skill.TitleExplicit, &skill.Ready,
	)
	if err != nil {
		return domain.Skill{}, err
	}
	if latest != nil {
		skill.LatestVersion = *latest
	}
	skill.CreatedAt = skill.CreatedAt.UTC()
	skill.UpdatedAt = skill.UpdatedAt.UTC()
	return skill, nil
}

func scanSkillVersion(row skillScanner) (domain.SkillVersion, error) {
	var item domain.SkillVersion
	err := row.Scan(
		&item.SkillID, &item.Version, &item.CreatedAt, &item.Description,
		&item.Directory, &item.Name, &item.BlobKey, &item.SizeBytes,
		&item.UncompressedSizeBytes, &item.ChecksumSHA256, &item.State, &item.Initial,
	)
	if err != nil {
		return domain.SkillVersion{}, err
	}
	item.ID = item.Version
	item.CreatedAt = item.CreatedAt.UTC()
	return item, nil
}

func normalizeSkillTimes(skill *domain.Skill, version *domain.SkillVersion) {
	skill.CreatedAt = skill.CreatedAt.UTC().Truncate(time.Microsecond)
	skill.UpdatedAt = skill.UpdatedAt.UTC().Truncate(time.Microsecond)
	version.CreatedAt = version.CreatedAt.UTC().Truncate(time.Microsecond)
}
