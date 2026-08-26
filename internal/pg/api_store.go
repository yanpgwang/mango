package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

// ListSessions applies the public session filters and bidirectional keyset
// pagination directly to the PostgreSQL projection.
func (s *Store) ListSessions(
	ctx context.Context,
	query app.ListPage,
) (app.SessionListPage, error) {
	workspaceID, scoped, err := s.workspaceForRead(ctx)
	if err != nil {
		return app.SessionListPage{}, err
	}
	if query.Limit <= 0 {
		query.Limit = 100
	}
	clauses, args := sessionListClauses(query)
	if scoped {
		args = append(args, workspaceID)
		clauses = append(clauses, fmt.Sprintf(`workspace_id = $%d`, len(args)))
	}
	pageClauses := append([]string(nil), clauses...)
	pageArgs := append([]any(nil), args...)

	displayOrder := "ASC"
	if query.Desc {
		displayOrder = "DESC"
	}
	fetchOrder := displayOrder
	if query.Boundary != nil {
		relationAfter := !query.Boundary.Backward
		if query.Boundary.Backward {
			fetchOrder = oppositeSQLOrder(displayOrder)
		}
		predicate, values := sessionBoundaryPredicate(
			relationAfter,
			query.Desc,
			query.Boundary.CreatedAt,
			query.Boundary.ID,
			len(pageArgs)+1,
		)
		pageClauses = append(pageClauses, predicate)
		pageArgs = append(pageArgs, values...)
	}

	statement := `SELECT body, created_at FROM sessions`
	if len(pageClauses) > 0 {
		statement += ` WHERE ` + strings.Join(pageClauses, ` AND `)
	}
	pageArgs = append(pageArgs, query.Limit)
	statement += fmt.Sprintf(
		` ORDER BY created_at %s, id %s LIMIT $%d`,
		fetchOrder,
		fetchOrder,
		len(pageArgs),
	)
	rows, err := s.pool.Query(ctx, statement, pageArgs...)
	if err != nil {
		return app.SessionListPage{}, err
	}
	sessions, err := scanSessionBodies(rows)
	rows.Close()
	if err != nil {
		return app.SessionListPage{}, err
	}
	if query.Boundary != nil && query.Boundary.Backward {
		reverseDomainSessions(sessions)
	}

	result := app.SessionListPage{Sessions: sessions}
	if len(sessions) == 0 {
		return result, nil
	}
	result.HasPrev, err = s.sessionExistsRelative(
		ctx, clauses, args, false, query.Desc, sessions[0],
	)
	if err != nil {
		return app.SessionListPage{}, err
	}
	result.HasNext, err = s.sessionExistsRelative(
		ctx, clauses, args, true, query.Desc, sessions[len(sessions)-1],
	)
	if err != nil {
		return app.SessionListPage{}, err
	}
	return result, nil
}

func sessionListClauses(query app.ListPage) ([]string, []any) {
	var clauses []string
	var args []any
	add := func(clause string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(clause, len(args)))
	}
	if !query.IncludeArchived {
		clauses = append(clauses, `archived_at IS NULL`)
	}
	if query.AgentID != "" {
		add(`agent_id = $%d`, query.AgentID)
	}
	if query.AgentVersion != nil {
		add(`agent_version = $%d`, *query.AgentVersion)
	}
	for _, bound := range []struct {
		value *time.Time
		op    string
	}{
		{query.CreatedAtGt, `>`},
		{query.CreatedAtGte, `>=`},
		{query.CreatedAtLt, `<`},
		{query.CreatedAtLte, `<=`},
	} {
		if bound.value != nil {
			add(`created_at `+bound.op+` $%d`, bound.value)
		}
	}
	if len(query.Statuses) > 0 {
		statuses := make([]string, len(query.Statuses))
		for i, status := range query.Statuses {
			statuses[i] = string(status)
		}
		add(`status = ANY($%d::text[])`, statuses)
	}
	if query.DeploymentID != nil {
		add(`deployment_id = $%d`, *query.DeploymentID)
	}
	if query.MemoryStoreID != nil {
		add(`EXISTS (
SELECT 1 FROM session_resources AS memory_resource
WHERE memory_resource.session_id = sessions.id
  AND memory_resource.resource_type = 'memory_store'
  AND memory_resource.memory_store_id = $%d
)`, *query.MemoryStoreID)
	}
	return clauses, args
}

func sessionBoundaryPredicate(
	relationAfter bool,
	desc bool,
	createdAt any,
	id string,
	firstPlaceholder int,
) (string, []any) {
	operator := ">"
	if !relationAfter {
		operator = "<"
	}
	if desc {
		if operator == ">" {
			operator = "<"
		} else {
			operator = ">"
		}
	}
	return fmt.Sprintf(
		`(created_at %s $%d OR (created_at = $%d AND id %s $%d))`,
		operator,
		firstPlaceholder,
		firstPlaceholder,
		operator,
		firstPlaceholder+1,
	), []any{createdAt, id}
}

func (s *Store) sessionExistsRelative(
	ctx context.Context,
	clauses []string,
	args []any,
	relationAfter bool,
	desc bool,
	session domain.Session,
) (bool, error) {
	predicate, values := sessionBoundaryPredicate(
		relationAfter, desc, session.CreatedAt, session.ID, len(args)+1,
	)
	allClauses := append(append([]string(nil), clauses...), predicate)
	allArgs := append(append([]any(nil), args...), values...)
	statement := `SELECT EXISTS(SELECT 1 FROM sessions WHERE ` +
		strings.Join(allClauses, ` AND `) + `)`
	var exists bool
	err := s.pool.QueryRow(ctx, statement, allArgs...).Scan(&exists)
	return exists, err
}

func scanSessionBodies(rows pgx.Rows) ([]domain.Session, error) {
	var sessions []domain.Session
	for rows.Next() {
		var body []byte
		var createdAt time.Time
		if err := rows.Scan(&body, &createdAt); err != nil {
			return nil, err
		}
		var session domain.Session
		if err := json.Unmarshal(body, &session); err != nil {
			return nil, fmt.Errorf("pg: decode session projection: %w", err)
		}
		// The relational timestamp is the pagination key and PostgreSQL's exact
		// microsecond value. Return that same value in the cursor-bearing object,
		// including for rows written before create-time normalization existed.
		session.CreatedAt = createdAt.UTC()
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func reverseDomainSessions(sessions []domain.Session) {
	for left, right := 0, len(sessions)-1; left < right; left, right = left+1, right-1 {
		sessions[left], sessions[right] = sessions[right], sessions[left]
	}
}

func oppositeSQLOrder(order string) string {
	if order == "ASC" {
		return "DESC"
	}
	return "ASC"
}

// Agent and Environment lists are forward-only and ordered newest-first by a
// stable (created_at, id) key. The public API does not expose an order
// parameter for either endpoint, so the order is an internal consistency
// choice rather than a new wire field.
const resourceListOrder = ` ORDER BY created_at DESC, id DESC`

func forwardResourceBoundary(
	args []any,
	boundary *app.ResourcePageBoundary,
) (string, []any) {
	args = append(args, boundary.CreatedAt, boundary.ID)
	createdAtPosition := len(args) - 1
	idPosition := len(args)
	return fmt.Sprintf(
		`(created_at < $%d OR (created_at = $%d AND id < $%d))`,
		createdAtPosition,
		createdAtPosition,
		idPosition,
	), args
}

// ListAgents pages over the latest version of each Agent. The underlying table
// is append-only by (id, version), so the DISTINCT ON subquery prevents older
// versions from leaking into the resource list.
func (s *Store) ListAgents(
	ctx context.Context,
	query app.AgentListQuery,
) (app.AgentListPage, error) {
	workspaceID, scoped, err := s.workspaceForRead(ctx)
	if err != nil {
		return app.AgentListPage{}, err
	}
	if query.Limit <= 0 {
		query.Limit = app.DefaultAgentListLimit
	}
	var clauses []string
	var args []any
	if scoped {
		args = append(args, workspaceID)
		clauses = append(clauses, fmt.Sprintf(`workspace_id = $%d`, len(args)))
	}
	if !query.IncludeArchived {
		clauses = append(clauses, `archived_at IS NULL`)
	}
	if query.CreatedAtGte != nil {
		args = append(args, *query.CreatedAtGte)
		clauses = append(clauses, fmt.Sprintf(`created_at >= $%d`, len(args)))
	}
	if query.CreatedAtLte != nil {
		args = append(args, *query.CreatedAtLte)
		clauses = append(clauses, fmt.Sprintf(`created_at <= $%d`, len(args)))
	}
	if query.After != nil {
		var boundary string
		boundary, args = forwardResourceBoundary(args, query.After)
		clauses = append(clauses, boundary)
	}

	statement := `SELECT id, body, created_at, archived_at FROM (
    SELECT DISTINCT ON (id) id, body, created_at, archived_at, workspace_id
    FROM agents
    ORDER BY id, version DESC
) AS latest`
	if len(clauses) > 0 {
		statement += ` WHERE ` + strings.Join(clauses, ` AND `)
	}
	args = append(args, query.Limit+1)
	statement += resourceListOrder + fmt.Sprintf(` LIMIT $%d`, len(args))

	rows, err := s.pool.Query(ctx, statement, args...)
	if err != nil {
		return app.AgentListPage{}, err
	}
	defer rows.Close()
	agents := make([]domain.Agent, 0, query.Limit+1)
	for rows.Next() {
		var (
			id         string
			body       []byte
			createdAt  time.Time
			archivedAt *time.Time
		)
		if err := rows.Scan(&id, &body, &createdAt, &archivedAt); err != nil {
			return app.AgentListPage{}, err
		}
		var agent domain.Agent
		if err := json.Unmarshal(body, &agent); err != nil {
			return app.AgentListPage{}, fmt.Errorf("pg: decode agent %s: %w", id, err)
		}
		agent.CreatedAt = createdAt.UTC()
		agent.ArchivedAt = utcTimePtr(archivedAt)
		agents = append(agents, agent)
	}
	if err := rows.Err(); err != nil {
		return app.AgentListPage{}, err
	}
	page := app.AgentListPage{Agents: agents}
	if len(agents) > query.Limit {
		page.Agents = agents[:query.Limit]
		page.HasNext = true
	}
	return page, nil
}

// ListEnvironments implements the documented include_archived, limit, and
// page surface. It intentionally has no created_at filters.
func (s *Store) ListEnvironments(
	ctx context.Context,
	query app.EnvironmentListQuery,
) (app.EnvironmentListPage, error) {
	workspaceID, scoped, err := s.workspaceForRead(ctx)
	if err != nil {
		return app.EnvironmentListPage{}, err
	}
	if query.Limit <= 0 {
		query.Limit = app.DefaultEnvironmentListLimit
	}
	var clauses []string
	var args []any
	if scoped {
		args = append(args, workspaceID)
		clauses = append(clauses, fmt.Sprintf(`workspace_id = $%d`, len(args)))
	}
	if !query.IncludeArchived {
		clauses = append(clauses, `archived_at IS NULL`)
	}
	if query.After != nil {
		var boundary string
		boundary, args = forwardResourceBoundary(args, query.After)
		clauses = append(clauses, boundary)
	}

	statement := `SELECT id, body, created_at, updated_at, archived_at FROM environments`
	if len(clauses) > 0 {
		statement += ` WHERE ` + strings.Join(clauses, ` AND `)
	}
	args = append(args, query.Limit+1)
	statement += resourceListOrder + fmt.Sprintf(` LIMIT $%d`, len(args))

	rows, err := s.pool.Query(ctx, statement, args...)
	if err != nil {
		return app.EnvironmentListPage{}, err
	}
	defer rows.Close()
	environments := make([]domain.Environment, 0, query.Limit+1)
	for rows.Next() {
		var (
			id         string
			body       []byte
			createdAt  time.Time
			updatedAt  time.Time
			archivedAt *time.Time
		)
		if err := rows.Scan(&id, &body, &createdAt, &updatedAt, &archivedAt); err != nil {
			return app.EnvironmentListPage{}, err
		}
		var environment domain.Environment
		if err := json.Unmarshal(body, &environment); err != nil {
			return app.EnvironmentListPage{}, fmt.Errorf(
				"pg: decode environment %s: %w", id, err,
			)
		}
		environment.CreatedAt = createdAt.UTC()
		environment.UpdatedAt = updatedAt.UTC()
		environment.ArchivedAt = utcTimePtr(archivedAt)
		environments = append(environments, environment)
	}
	if err := rows.Err(); err != nil {
		return app.EnvironmentListPage{}, err
	}
	page := app.EnvironmentListPage{Environments: environments}
	if len(environments) > query.Limit {
		page.Environments = environments[:query.Limit]
		page.HasNext = true
	}
	return page, nil
}

func utcTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}

// UpdateSession applies the public session update and keeps the projection and
// its session.updated event in one transaction under the per-session admission
// lock.
//
// A mid-session agent configuration change requires an idle session: the turn
// loop reads the resolved snapshot once when it prepares a turn, so replacing
// tools while a turn is in flight would leave that turn running a configuration
// no longer visible in the API. Holding the admission lock here also serializes
// the update against a concurrent admission that would start a turn.
func (s *Store) UpdateSession(
	ctx context.Context,
	sessionID string,
	update domain.SessionUpdate,
) (domain.Session, error) {
	var result domain.Session
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		row, err := q.LockSession(ctx, sessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("session not found")
		}
		if err != nil {
			return err
		}
		if row.DeletingAt.Valid {
			return domain.Conflict("session deletion is in progress")
		}
		session, err := sessionFromLockRow(row)
		if err != nil {
			return err
		}
		if update.TouchesAgent() && session.Status != domain.StatusIdle {
			return domain.Conflict(
				"session must be idle to update agent configuration; interrupt it first",
			)
		}
		if update.Budget != nil && update.Budget.Budget != nil &&
			session.Budget != nil && *update.Budget.Budget != *session.Budget {
			if !session.ListCostKnown {
				return domain.Validation(
					"budget cannot be changed while session list cost is unknown",
				)
			}
			maximum := update.Budget.Budget.MaxListCostCents * domain.NanoUSDPerCent
			if maximum <= session.ObservableListCostNanoUSD(s.clock.Now().UTC()) {
				return domain.Validation(
					"budget max_list_cost must be strictly greater than the session's consumed list cost",
				)
			}
		}
		next, change, err := session.ApplyUpdate(update)
		if err != nil {
			return err
		}
		if !change.Any() {
			result = session
			return nil
		}
		next.UpdatedAt = s.clock.Now().UTC()
		maxSeq, err := q.MaxEventSeq(ctx, sessionID)
		if err != nil {
			return err
		}
		if _, maxSeq, err = s.appendDrafts(ctx, q, sessionID, []domain.EventDraft{{
			Type:    domain.EvSessionUpdated,
			Payload: domain.SessionUpdatedPayload(next, change),
		}}, maxSeq, nil); err != nil {
			return err
		}
		if change.Budget {
			_, err = s.resumeBudgetPausedThreadsLocked(
				ctx, tx, q, &next, maxSeq,
			)
			if err != nil {
				return err
			}
		}
		if err := s.updateAPIProjection(ctx, q, next); err != nil {
			return err
		}
		if change.Agent {
			if !reflect.DeepEqual(
				session.AgentSnapshot.MCPServers,
				next.AgentSnapshot.MCPServers,
			) {
				threadID, err := q.GetPrimarySessionThreadID(ctx, sessionID)
				if err != nil {
					return err
				}
				if err := q.DeleteMCPDiscoverySnapshotsForThread(
					ctx,
					pgstore.DeleteMCPDiscoverySnapshotsForThreadParams{
						SessionID: sessionID,
						ThreadID:  threadID,
					},
				); err != nil {
					return err
				}
			}
			if err := s.putPrimarySessionThreadProjection(ctx, q, next); err != nil {
				return err
			}
		}
		result = next
		return nil
	})
	if err == nil {
		s.notifySession(ctx, sessionID)
	}
	return result, err
}

func (s *Store) ArchiveSession(ctx context.Context, sessionID string) (domain.Session, error) {
	var result domain.Session
	err := s.withTx(ctx, func(q *pgstore.Queries) error {
		row, err := q.LockSession(ctx, sessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("session not found")
		}
		if err != nil {
			return err
		}
		if row.DeletingAt.Valid {
			return domain.Conflict("session deletion is in progress")
		}
		session, err := sessionFromLockRow(row)
		if err != nil {
			return err
		}
		if session.Status == domain.StatusRunning {
			return domain.Conflict("cannot archive a running session; interrupt first")
		}
		if session.ArchivedAt == nil {
			now := s.clock.Now().UTC()
			session.ArchivedAt = &now
			session.UpdatedAt = now
			if err := s.updateAPIProjection(ctx, q, session); err != nil {
				return err
			}
			if err := s.putPrimarySessionThreadProjection(ctx, q, session); err != nil {
				return err
			}
			if err := s.enqueueWebhookEvent(
				ctx, q, row.WorkspaceID, domain.WebhookEventSessionArchived,
				sessionID, now, nil,
			); err != nil {
				return err
			}
		}
		result = session
		return nil
	})
	return result, err
}

// PrepareSessionDeletion closes admission before the control plane stops the
// Temporal Workflow and releases its sandbox. This prevents a concurrent
// user.message from turning the projection running during external cleanup.
func (s *Store) PrepareSessionDeletion(ctx context.Context, sessionID string) error {
	return s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		row, err := q.LockSession(ctx, sessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("session not found")
		}
		if err != nil {
			return err
		}
		session, err := sessionFromLockRow(row)
		if err != nil {
			return err
		}
		if session.Status == domain.StatusRunning {
			return domain.Conflict("cannot delete a running session; interrupt first")
		}
		now := s.clock.Now().UTC()
		maxSeq, err := q.MaxEventSeq(ctx, sessionID)
		if err != nil {
			return err
		}
		if err := enqueueChildThreadTerminationsLocked(
			ctx, tx, q, sessionID, maxSeq, now,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE files
SET state = 'deleting', updated_at = $2
WHERE state = 'ready' AND id IN (
    SELECT file_id FROM session_resources WHERE session_id = $1
)`, sessionID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE files
SET state = 'deleting', updated_at = $2
WHERE state = 'ready' AND scope_type = 'session' AND scope_id = $1
  AND output_path IS NOT NULL`, sessionID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE session_resources
SET state = 'deleting', updated_at = $2
WHERE session_id = $1 AND state = 'active'`, sessionID, now); err != nil {
			return err
		}
		if len(session.Resources) > 0 {
			session.Resources = []domain.SessionResource{}
			session.UpdatedAt = now
			if err := s.updateAPIProjection(ctx, q, session); err != nil {
				return err
			}
		}
		if row.DeletingAt.Valid {
			return nil
		}
		return q.MarkSessionDeleting(ctx, pgstore.MarkSessionDeletingParams{
			DeletingAt: tsUTC(now), ID: sessionID,
		})
	})
}

// FinalizeSessionDeletion physically deletes a previously fenced session.
func (s *Store) FinalizeSessionDeletion(ctx context.Context, sessionID string) error {
	deleted := false
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		row, err := q.LockSession(ctx, sessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			// A concurrent retry already completed the prepared deletion.
			return nil
		}
		if err != nil {
			return err
		}
		if !row.DeletingAt.Valid {
			return domain.Conflict("session deletion was not prepared")
		}
		var outputsRemain bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM files
    WHERE scope_type = 'session' AND scope_id = $1
      AND output_path IS NOT NULL AND workspace_id = $2
)`, sessionID, row.WorkspaceID).Scan(&outputsRemain); err != nil {
			return err
		}
		if outputsRemain {
			return domain.Conflict("session output cleanup is incomplete")
		}
		now := s.clock.Now().UTC()
		if err := s.enqueueWebhookEvent(
			ctx, q, row.WorkspaceID, domain.WebhookEventSessionDeleted,
			sessionID, now, nil,
		); err != nil {
			return err
		}
		affected, err := q.DeleteMarkedSession(ctx, sessionID)
		if err != nil {
			if isForeignKeyViolation(err) {
				return domain.Conflict("session sandbox or File Resource cleanup is incomplete")
			}
			return err
		}
		if affected != 1 {
			return domain.Conflict("session deletion was not prepared")
		}
		deleted = true
		return nil
	})
	if err != nil {
		return err
	}
	if deleted {
		s.notifySession(ctx, sessionID)
	}
	return nil
}

// SessionOutputFilesExist lets lifecycle reconciliation avoid requiring the
// object store for Sessions that never produced a deliverable.
func (s *Store) SessionOutputFilesExist(ctx context.Context, sessionID string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM files
    WHERE scope_type = 'session' AND scope_id = $1
      AND output_path IS NOT NULL
)`, sessionID).Scan(&exists)
	return exists, err
}

// ListDeletingSessionIDs returns fenced sessions in stable oldest-first order
// for worker-side lifecycle reconciliation.
func (s *Store) ListDeletingSessionIDs(
	ctx context.Context,
	limit int,
) ([]string, error) {
	if limit <= 0 {
		return []string{}, nil
	}
	return s.q.ListDeletingSessionIDs(ctx, int32(limit))
}

// DeleteSession is the storage-only convenience used by repository tests. The
// HTTP control plane uses the explicit prepare/terminate/finalize sequence.
func (s *Store) DeleteSession(ctx context.Context, sessionID string) error {
	if err := s.PrepareSessionDeletion(ctx, sessionID); err != nil {
		return err
	}
	return s.FinalizeSessionDeletion(ctx, sessionID)
}

func (s *Store) updateAPIProjection(
	ctx context.Context,
	q *pgstore.Queries,
	session domain.Session,
) error {
	body, err := json.Marshal(session)
	if err != nil {
		return err
	}
	return q.UpdateSessionProjection(ctx, pgstore.UpdateSessionProjectionParams{
		Status:     string(session.Status),
		Body:       body,
		UpdatedAt:  tsUTC(session.UpdatedAt),
		ArchivedAt: tsPtr(session.ArchivedAt),
		ID:         session.ID,
	})
}

// QueryEvents applies the public event filters to the durable ledger.
func (s *Store) QueryEvents(
	ctx context.Context,
	sessionID string,
	query app.EventQuery,
) ([]domain.Event, error) {
	if query.Limit <= 0 {
		query.Limit = 1000
	}
	clauses := []string{`session_id = $1`}
	args := []any{sessionID}
	add := func(clause string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(clause, len(args)))
	}
	if query.ThreadID == "" {
		clauses = append(clauses, `thread_id = (
            SELECT id FROM session_threads
            WHERE session_id = $1 AND kind = 'primary'
        )`)
	} else {
		add(`thread_id = $%d`, query.ThreadID)
	}
	if len(query.Types) > 0 {
		add(`type = ANY($%d::text[])`, query.Types)
	}
	for _, bound := range []struct {
		value *time.Time
		op    string
	}{
		{query.ProcessedAtGt, `>`},
		{query.ProcessedAtGte, `>=`},
		{query.ProcessedAtLt, `<`},
		{query.ProcessedAtLte, `<=`},
	} {
		if bound.value != nil {
			add(`processed_at `+bound.op+` $%d`, bound.value)
		}
	}
	if query.Boundary != nil {
		boundary := query.Boundary
		if query.Desc {
			if boundary.ProcessedAt == nil {
				add(`((processed_at IS NULL AND seq < $%d) OR processed_at IS NOT NULL)`, boundary.Sequence)
			} else {
				first := len(args) + 1
				clauses = append(clauses, fmt.Sprintf(
					`(processed_at < $%d OR (processed_at = $%d AND seq < $%d))`,
					first, first, first+1,
				))
				args = append(args, *boundary.ProcessedAt, boundary.Sequence)
			}
		} else if boundary.ProcessedAt == nil {
			add(`processed_at IS NULL AND seq > $%d`, boundary.Sequence)
		} else {
			first := len(args) + 1
			clauses = append(clauses, fmt.Sprintf(
				`(processed_at > $%d OR (processed_at = $%d AND seq > $%d) OR processed_at IS NULL)`,
				first, first, first+1,
			))
			args = append(args, *boundary.ProcessedAt, boundary.Sequence)
		}
	}
	order := "ASC"
	nulls := "NULLS LAST"
	if query.Desc {
		order = "DESC"
		nulls = "NULLS FIRST"
	}
	args = append(args, query.Limit)
	statement := `SELECT id, session_id, thread_id, seq, type, payload, turn_event_id, created_at, processed_at
FROM events
WHERE ` + strings.Join(clauses, ` AND `) +
		fmt.Sprintf(` ORDER BY processed_at %s %s, seq %s LIMIT $%d`, order, nulls, order, len(args))
	rows, err := s.pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.Event
	for rows.Next() {
		var row pgstore.Event
		if err := rows.Scan(
			&row.ID,
			&row.SessionID,
			&row.ThreadID,
			&row.Seq,
			&row.Type,
			&row.Payload,
			&row.TurnEventID,
			&row.CreatedAt,
			&row.ProcessedAt,
		); err != nil {
			return nil, err
		}
		event, err := eventFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, rows.Err()
}

func (s *Store) LatestEventSequence(ctx context.Context, sessionID string) (int64, error) {
	var sequence int64
	err := s.pool.QueryRow(
		ctx,
		`SELECT COALESCE(MAX(event.seq), 0)::bigint
FROM session_threads AS thread
LEFT JOIN events AS event
  ON event.session_id = thread.session_id AND event.thread_id = thread.id
WHERE thread.session_id = $1 AND thread.kind = 'primary'`,
		sessionID,
	).Scan(&sequence)
	return sequence, err
}

func (s *Store) LatestThreadEventSequence(
	ctx context.Context,
	sessionID string,
	threadID string,
) (int64, error) {
	var sequence int64
	err := s.pool.QueryRow(ctx, `
SELECT COALESCE(MAX(seq), 0)::bigint
FROM events
WHERE session_id = $1 AND thread_id = $2`, sessionID, threadID).Scan(&sequence)
	return sequence, err
}

// SessionExists reports whether the projection remains present. It lets the
// polling stream distinguish a quiet session from a concurrently deleted one.
func (s *Store) SessionExists(ctx context.Context, sessionID string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(
		ctx,
		`SELECT EXISTS(SELECT 1 FROM sessions WHERE id = $1)`,
		sessionID,
	).Scan(&exists)
	return exists, err
}
