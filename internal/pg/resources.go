package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

var (
	_ app.AgentRepository       = (*AgentRepository)(nil)
	_ app.EnvironmentRepository = (*EnvironmentRepository)(nil)
)

// AgentRepository stores append-only Agent configuration versions in
// PostgreSQL. Lifecycle state is projected from the relational archived_at
// column so archiving a resource applies consistently to every historical
// version without rewriting its immutable JSON configuration.
type AgentRepository struct {
	store *Store
}

func NewAgentRepository(store *Store) *AgentRepository {
	return &AgentRepository{store: store}
}

func (r *AgentRepository) PutVersion(ctx context.Context, agent domain.Agent) error {
	workspaceID, err := r.store.workspaceForWrite(ctx)
	if err != nil {
		return err
	}
	params, err := agentInsertParams(workspaceID, agent)
	if err != nil {
		return err
	}
	return r.store.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		if err := q.InsertAgentVersion(ctx, params); err != nil {
			return err
		}
		return replaceAgentSkillVersions(ctx, tx, workspaceID, agent)
	})
}

func (r *AgentRepository) UpdateVersion(
	ctx context.Context,
	id string,
	mutate func(domain.Agent) (domain.Agent, bool, error),
) (domain.Agent, error) {
	workspaceID, err := r.store.workspaceForWrite(ctx)
	if err != nil {
		return domain.Agent{}, err
	}
	var result domain.Agent
	err = r.store.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		row, err := q.LockLatestAgent(ctx, pgstore.LockLatestAgentParams{
			ID: id, WorkspaceID: workspaceID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("agent not found")
		}
		if err != nil {
			return err
		}
		current, err := agentFromRow(row)
		if err != nil {
			return err
		}
		next, changed, err := mutate(current)
		if err != nil {
			return err
		}
		if !changed {
			result = current
			return nil
		}

		next.ID = current.ID
		next.Version = current.Version + 1
		next.CreatedAt = current.CreatedAt
		next.ArchivedAt = nil
		params, err := agentInsertParams(workspaceID, next)
		if err != nil {
			return err
		}
		if err := q.InsertAgentVersion(ctx, params); err != nil {
			if isUniqueViolation(err) {
				return domain.Conflict("agent version changed during update")
			}
			return err
		}
		if err := replaceAgentSkillVersions(ctx, tx, workspaceID, next); err != nil {
			return err
		}
		result = next
		return nil
	})
	return result, err
}

func (r *AgentRepository) Archive(
	ctx context.Context,
	id string,
	archivedAt time.Time,
) (domain.Agent, error) {
	workspaceID, err := r.store.workspaceForWrite(ctx)
	if err != nil {
		return domain.Agent{}, err
	}
	var result domain.Agent
	err = r.store.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		// Serialize archival with version creation before updating every Agent
		// row. Without this fence, an UpdateVersion blocked inside ArchiveAgent's
		// statement snapshot can commit a new active version and pin; the archive
		// statement then misses that version while its later pin cleanup sees and
		// deletes the new pin.
		if _, err := q.LockLatestAgent(ctx, pgstore.LockLatestAgentParams{
			ID: id, WorkspaceID: workspaceID,
		}); errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("agent not found")
		} else if err != nil {
			return err
		}
		affected, err := q.ArchiveAgent(ctx, pgstore.ArchiveAgentParams{
			ArchivedAt:  tsUTC(archivedAt),
			ID:          id,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			return err
		}
		if affected == 0 {
			return domain.NotFound("agent not found")
		}
		if _, err := tx.Exec(ctx, `
DELETE FROM agent_skill_versions AS pin
USING agents AS agent
WHERE pin.agent_id = agent.id
  AND pin.agent_version = agent.version
  AND pin.agent_id = $1
  AND agent.workspace_id = $2`, id, workspaceID); err != nil {
			return err
		}
		row, err := q.GetLatestAgent(ctx, pgstore.GetLatestAgentParams{
			ID: id, WorkspaceID: workspaceID,
		})
		if err != nil {
			return err
		}
		result, err = agentFromRow(row)
		return err
	})
	return result, err
}

func replaceAgentSkillVersions(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	agent domain.Agent,
) error {
	if _, err := tx.Exec(ctx, `DELETE FROM agent_skill_versions WHERE agent_id = $1`, agent.ID); err != nil {
		return err
	}
	for position, reference := range agent.Skills {
		if reference.Type != "custom" || reference.SkillID == "" ||
			reference.Version == "" || reference.Version == "latest" {
			return domain.Validation("Agent requires resolved custom Skill references")
		}
		locked, err := lockReadySkillVersion(ctx, tx, workspaceID, reference)
		if err != nil {
			return err
		}
		if !locked {
			return domain.Validation("Agent references a missing custom Skill Version")
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO agent_skill_versions (
    agent_id, agent_version, position, skill_id, skill_version
) VALUES ($1, $2, $3, $4, $5)`,
			agent.ID, agent.Version, position, reference.SkillID, reference.Version,
		); err != nil {
			if isForeignKeyViolation(err) {
				return domain.Validation("Agent references a missing custom Skill Version")
			}
			return err
		}
	}
	return nil
}

func (r *AgentRepository) Latest(ctx context.Context, id string) (domain.Agent, error) {
	workspaceID, err := r.store.workspaceForWrite(ctx)
	if err != nil {
		return domain.Agent{}, err
	}
	row, err := r.store.q.GetLatestAgent(ctx, pgstore.GetLatestAgentParams{
		ID: id, WorkspaceID: workspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Agent{}, domain.NotFound("agent not found")
	}
	if err != nil {
		return domain.Agent{}, err
	}
	return agentFromRow(row)
}

func (r *AgentRepository) GetVersion(
	ctx context.Context,
	id string,
	version int,
) (domain.Agent, error) {
	workspaceID, err := r.store.workspaceForWrite(ctx)
	if err != nil {
		return domain.Agent{}, err
	}
	row, err := r.store.q.GetAgentVersion(ctx, pgstore.GetAgentVersionParams{
		ID: id, Version: int32(version), WorkspaceID: workspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Agent{}, domain.NotFound("agent version not found")
	}
	if err != nil {
		return domain.Agent{}, err
	}
	return agentFromRow(row)
}

func (r *AgentRepository) Versions(
	ctx context.Context,
	id string,
	query app.AgentVersionListQuery,
) (app.AgentVersionListPage, error) {
	workspaceID, err := r.store.workspaceForWrite(ctx)
	if err != nil {
		return app.AgentVersionListPage{}, err
	}
	if query.Limit <= 0 {
		query.Limit = app.DefaultAgentListLimit
	}
	rows, err := r.store.q.ListAgentVersions(ctx, pgstore.ListAgentVersionsParams{
		ID: id, WorkspaceID: workspaceID,
		AfterVersion: int32(query.AfterVersion), RowLimit: int32(query.Limit + 1),
	})
	if err != nil {
		return app.AgentVersionListPage{}, err
	}
	versions, err := agentsFromRows(rows)
	if err != nil {
		return app.AgentVersionListPage{}, err
	}
	page := app.AgentVersionListPage{Versions: versions}
	if len(versions) > query.Limit {
		page.Versions = versions[:query.Limit]
		page.HasNext = true
	}
	return page, nil
}

func (r *AgentRepository) ListLatest(
	ctx context.Context,
	query app.AgentListQuery,
) (app.AgentListPage, error) {
	return r.store.ListAgents(ctx, query)
}

func agentInsertParams(
	workspaceID string,
	agent domain.Agent,
) (pgstore.InsertAgentVersionParams, error) {
	body, err := json.Marshal(agent)
	if err != nil {
		return pgstore.InsertAgentVersionParams{}, err
	}
	return pgstore.InsertAgentVersionParams{
		ID:          agent.ID,
		Version:     int32(agent.Version),
		Name:        agent.Name,
		Body:        body,
		CreatedAt:   tsUTC(agent.CreatedAt),
		UpdatedAt:   tsUTC(agent.UpdatedAt),
		ArchivedAt:  tsPtr(agent.ArchivedAt),
		WorkspaceID: workspaceID,
	}, nil
}

func agentFromRow(row pgstore.Agent) (domain.Agent, error) {
	var agent domain.Agent
	if err := json.Unmarshal(row.Body, &agent); err != nil {
		return domain.Agent{}, fmt.Errorf("pg: decode agent %s version %d: %w", row.ID, row.Version, err)
	}
	agent.ArchivedAt = timePtr(row.ArchivedAt)
	return agent, nil
}

func agentsFromRows(rows []pgstore.Agent) ([]domain.Agent, error) {
	out := make([]domain.Agent, 0, len(rows))
	for _, row := range rows {
		agent, err := agentFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, agent)
	}
	return out, nil
}

// EnvironmentRepository stores Environment resources and makes
// delete-if-unused one PostgreSQL statement so a concurrent Session creation
// cannot commit an orphaned reference.
type EnvironmentRepository struct {
	store *Store
}

func NewEnvironmentRepository(store *Store) *EnvironmentRepository {
	return &EnvironmentRepository{store: store}
}

func (r *EnvironmentRepository) Put(ctx context.Context, environment domain.Environment) error {
	workspaceID, err := r.store.workspaceForWrite(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(environment)
	if err != nil {
		return err
	}
	affected, err := r.store.q.UpsertEnvironment(ctx, pgstore.UpsertEnvironmentParams{
		ID:          environment.ID,
		Name:        environment.Name,
		ConfigType:  environment.ConfigType,
		Body:        body,
		CreatedAt:   tsUTC(environment.CreatedAt),
		UpdatedAt:   tsUTC(environment.UpdatedAt),
		ArchivedAt:  tsPtr(environment.ArchivedAt),
		WorkspaceID: workspaceID,
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return domain.Conflict("environment already exists")
	}
	return nil
}

// Update replaces the mutable Environment projection only while the resource
// remains active. The archived_at predicate prevents a stale read followed by
// an update from reviving an Environment that was archived concurrently.
func (r *EnvironmentRepository) Update(
	ctx context.Context,
	environment domain.Environment,
) (domain.Environment, error) {
	workspaceID, err := r.store.workspaceForWrite(ctx)
	if err != nil {
		return domain.Environment{}, err
	}
	body, err := json.Marshal(environment)
	if err != nil {
		return domain.Environment{}, err
	}
	row := r.store.pool.QueryRow(ctx, `
UPDATE environments
SET name = $2, config_type = $3, body = $4, updated_at = $5
WHERE id = $1 AND workspace_id = $6 AND archived_at IS NULL
RETURNING id, name, config_type, body, created_at, updated_at, archived_at`,
		environment.ID,
		environment.Name,
		environment.ConfigType,
		body,
		tsUTC(environment.UpdatedAt),
		workspaceID,
	)
	var stored pgstore.Environment
	if err := row.Scan(
		&stored.ID,
		&stored.Name,
		&stored.ConfigType,
		&stored.Body,
		&stored.CreatedAt,
		&stored.UpdatedAt,
		&stored.ArchivedAt,
	); errors.Is(err, pgx.ErrNoRows) {
		exists, existsErr := r.store.q.EnvironmentExists(ctx, pgstore.EnvironmentExistsParams{
			ID: environment.ID, WorkspaceID: workspaceID,
		})
		if existsErr != nil {
			return domain.Environment{}, existsErr
		}
		if !exists {
			return domain.Environment{}, domain.NotFound("environment not found")
		}
		return domain.Environment{}, domain.Validation("archived environment is read-only")
	} else if err != nil {
		return domain.Environment{}, err
	}
	return environmentFromRow(stored)
}

// Archive is idempotent and updates lifecycle columns without rewriting the
// JSON resource body. It therefore serializes safely with Update instead of
// allowing a stale read/Put pair to discard a concurrent field change.
func (r *EnvironmentRepository) Archive(
	ctx context.Context,
	id string,
	archivedAt time.Time,
) (domain.Environment, error) {
	workspaceID, err := r.store.workspaceForWrite(ctx)
	if err != nil {
		return domain.Environment{}, err
	}
	row := r.store.pool.QueryRow(ctx, `
UPDATE environments
SET archived_at = COALESCE(archived_at, $2),
    updated_at = CASE WHEN archived_at IS NULL THEN $2 ELSE updated_at END
WHERE id = $1 AND workspace_id = $3
RETURNING id, name, config_type, body, created_at, updated_at, archived_at`,
		id,
		tsUTC(archivedAt),
		workspaceID,
	)
	var stored pgstore.Environment
	if err := row.Scan(
		&stored.ID,
		&stored.Name,
		&stored.ConfigType,
		&stored.Body,
		&stored.CreatedAt,
		&stored.UpdatedAt,
		&stored.ArchivedAt,
	); errors.Is(err, pgx.ErrNoRows) {
		return domain.Environment{}, domain.NotFound("environment not found")
	} else if err != nil {
		return domain.Environment{}, err
	}
	return environmentFromRow(stored)
}

func (r *EnvironmentRepository) Get(
	ctx context.Context,
	id string,
) (domain.Environment, error) {
	workspaceID, err := r.store.workspaceForWrite(ctx)
	if err != nil {
		return domain.Environment{}, err
	}
	row, err := r.store.q.GetEnvironment(ctx, pgstore.GetEnvironmentParams{
		ID: id, WorkspaceID: workspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Environment{}, domain.NotFound("environment not found")
	}
	if err != nil {
		return domain.Environment{}, err
	}
	return environmentFromRow(row)
}

func (r *EnvironmentRepository) List(
	ctx context.Context,
	query app.EnvironmentListQuery,
) (app.EnvironmentListPage, error) {
	return r.store.ListEnvironments(ctx, query)
}

func (r *EnvironmentRepository) DeleteIfUnreferenced(ctx context.Context, id string) error {
	workspaceID, err := r.store.workspaceForWrite(ctx)
	if err != nil {
		return err
	}
	return r.store.withTx(ctx, func(q *pgstore.Queries) error {
		affected, err := q.DeleteEnvironmentIfUnreferenced(ctx, pgstore.DeleteEnvironmentIfUnreferencedParams{
			ID: id, WorkspaceID: workspaceID,
		})
		if err != nil {
			return err
		}
		if affected == 1 {
			return nil
		}
		exists, err := q.EnvironmentExists(ctx, pgstore.EnvironmentExistsParams{
			ID: id, WorkspaceID: workspaceID,
		})
		if err != nil {
			return err
		}
		if !exists {
			return domain.NotFound("environment not found")
		}
		return domain.Conflict("environment is referenced by a session or deployment")
	})
}

func environmentFromRow(row pgstore.Environment) (domain.Environment, error) {
	var environment domain.Environment
	if err := json.Unmarshal(row.Body, &environment); err != nil {
		return domain.Environment{}, fmt.Errorf("pg: decode environment %s: %w", row.ID, err)
	}
	environment.ID = row.ID
	environment.Name = row.Name
	environment.ConfigType = row.ConfigType
	environment.CreatedAt = row.CreatedAt.Time.UTC()
	environment.UpdatedAt = row.UpdatedAt.Time.UTC()
	environment.ArchivedAt = timePtr(row.ArchivedAt)
	return environment, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
