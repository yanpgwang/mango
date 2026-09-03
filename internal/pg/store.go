package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
	"github.com/yanpgwang/mango/internal/workspace"
)

// Store is the primary PostgreSQL control-plane store. It owns the event-admission
// transaction, cursor reads for the SessionWorkflow, idempotent turn
// completion, and the coalescible orchestration outbox.
//
// PostgreSQL — not Temporal — is the source of truth for public events and the
// session projection.
type Store struct {
	pool     *pgxpool.Pool
	q        *pgstore.Queries
	ids      domain.IDGenerator
	clock    domain.Clock
	notifier EventNotifier
	// systemAccess is enabled only for the worker role and integration tests.
	// Public API stores fail closed when a repository call lacks HTTP scope.
	systemAccess bool
	// defaultWorkspace pins an explicitly single-tenant Store to the bootstrap
	// Workspace. Keeping this separate from systemAccess prevents a missing
	// worker scope from silently writing tenant data into wrkspc_default.
	defaultWorkspace bool
}

func NewStore(pool *pgxpool.Pool, ids domain.IDGenerator, clock domain.Clock) *Store {
	return &Store{pool: pool, q: pgstore.New(pool), ids: ids, clock: clock}
}

// NewSystemStore creates the explicitly unscoped persistence view required by
// reconcilers and Temporal workers. It must never back public HTTP handlers.
func NewSystemStore(pool *pgxpool.Pool, ids domain.IDGenerator, clock domain.Clock) *Store {
	store := NewStore(pool, ids, clock)
	store.systemAccess = true
	return store
}

// NewDefaultWorkspaceStore creates an explicitly single-tenant Store pinned to
// the bootstrap Workspace. It exists for embedders and compatibility tests;
// multi-tenant workers must use NewSystemStore and attach a Workspace scope
// before reading dependencies or creating tenant-owned resources.
func NewDefaultWorkspaceStore(
	pool *pgxpool.Pool,
	ids domain.IDGenerator,
	clock domain.Clock,
) *Store {
	store := NewStore(pool, ids, clock)
	store.defaultWorkspace = true
	return store
}

func (s *Store) workspaceForRead(ctx context.Context) (string, bool, error) {
	if scope, ok := workspace.FromContext(ctx); ok {
		return scope.ID, true, nil
	}
	if s.defaultWorkspace {
		return workspace.DefaultID, true, nil
	}
	if s.systemAccess {
		return "", false, nil
	}
	return "", false, workspace.ErrMissingScope
}

func (s *Store) workspaceForWrite(ctx context.Context) (string, error) {
	if scope, ok := workspace.FromContext(ctx); ok {
		return scope.ID, nil
	}
	if s.defaultWorkspace {
		return workspace.DefaultID, nil
	}
	return "", workspace.ErrMissingScope
}

// EventNotifier wakes live-event subscribers after a PostgreSQL commit. It is a
// latency optimization only: subscribers always reconcile from the durable
// event ledger and correctness never depends on notification delivery.
type EventNotifier interface {
	NotifySession(context.Context, string) error
}

// SetEventNotifier installs the process-local live notification publisher
// during startup, before the Store is shared with request handlers or workers.
func (s *Store) SetEventNotifier(notifier EventNotifier) {
	s.notifier = notifier
}

func (s *Store) notifySession(ctx context.Context, sessionID string) {
	if s.notifier == nil {
		return
	}
	if err := s.notifier.NotifySession(ctx, sessionID); err != nil {
		log.Printf(
			"pg: live event notification failed session_id=%s (ledger remains authoritative): %v",
			sessionID,
			err,
		)
	}
}

// Admission is the result of an event-admission transaction: the committed
// public events (including any synthetic session.status_running), the resulting
// session projection, the highest receipt sequence after the batch, and whether
// a coalescible orchestration wakeup was written.
type Admission struct {
	Session domain.Session
	Events  []domain.Event
	// SubmittedEvents contains exactly one committed event for every caller
	// draft, in request order. Events may additionally contain durable fan-out
	// copies and synthetic projection events.
	SubmittedEvents []domain.Event
	MaxSeq          int64
	Enqueued        bool
	PrimaryEnqueued bool
	WakeThreadIDs   []string
}

// withTx runs fn inside a transaction bound to a tx-scoped Queries. It commits
// on success and rolls back on any error or panic.
func (s *Store) withTx(ctx context.Context, fn func(q *pgstore.Queries) error) error {
	return s.withPGXTx(ctx, func(_ pgx.Tx, q *pgstore.Queries) error { return fn(q) })
}

func (s *Store) withPGXTx(
	ctx context.Context,
	fn func(pgx.Tx, *pgstore.Queries) error,
) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	if err := fn(tx, s.q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateSession inserts a session projection and, when initial events are
// present, admits them in one transaction: the events get receipt sequences, the
// session flips to running, and a coalescible orchestration wakeup is written.
func (s *Store) CreateSession(
	ctx context.Context,
	session domain.Session,
	drafts []domain.EventDraft,
) (Admission, error) {
	return s.createSession(ctx, session, drafts, false, nil, nil)
}

// CreateAPISession creates a public API session while holding share locks on
// its exact Agent version and Environment. FOR SHARE conflicts with both
// archival's non-key UPDATE and Environment deletion, so the dependency checks
// and session insert linearize with those lifecycle operations.
func (s *Store) CreateAPISession(
	ctx context.Context,
	session domain.Session,
	drafts []domain.EventDraft,
	resourceSets ...[]app.PreparedSessionResource,
) (Admission, error) {
	var resources []app.PreparedSessionResource
	if len(resourceSets) > 0 {
		resources = resourceSets[0]
	}
	return s.createSession(ctx, session, drafts, true, resources, nil)
}

// CreateDeploymentSession is the deployment admission boundary. The run row is
// inserted in the same transaction as the Session, its resources, initial
// events, and orchestration wakeup.
func (s *Store) CreateDeploymentSession(
	ctx context.Context,
	session domain.Session,
	drafts []domain.EventDraft,
	run domain.DeploymentRun,
	resourceSets ...[]app.PreparedSessionResource,
) (Admission, error) {
	var resources []app.PreparedSessionResource
	if len(resourceSets) > 0 {
		resources = resourceSets[0]
	}
	return s.createSession(ctx, session, drafts, true, resources, &run)
}

func (s *Store) createSession(
	ctx context.Context,
	session domain.Session,
	drafts []domain.EventDraft,
	checkDependencies bool,
	resources []app.PreparedSessionResource,
	deploymentRun *domain.DeploymentRun,
) (Admission, error) {
	workspaceID, err := s.workspaceForWrite(ctx)
	if err != nil {
		return Admission{}, err
	}
	if len(resources) > app.MaxSessionResources {
		return Admission{}, domain.Validation("resources must contain at most 500 entries")
	}
	session.WorkspaceID = workspaceID
	var resourceBytes int64
	for _, resource := range resources {
		if resource.Blob.SizeBytes > app.MaxSessionResourceBytes-resourceBytes {
			return Admission{}, domain.TooLarge(
				"Session File Resources exceed the 500 MB aggregate limit",
			)
		}
		resourceBytes += resource.Blob.SizeBytes
	}
	// PostgreSQL timestamptz has microsecond precision. Normalize the JSON
	// projection to the same value as the relational key so a list cursor never
	// compares a nanosecond boundary against a truncated database timestamp.
	session.CreatedAt = session.CreatedAt.UTC().Truncate(time.Microsecond)
	session.UpdatedAt = session.UpdatedAt.UTC().Truncate(time.Microsecond)
	if session.ArchivedAt != nil {
		archivedAt := session.ArchivedAt.UTC().Truncate(time.Microsecond)
		session.ArchivedAt = &archivedAt
	}
	if len(resources) > 0 {
		session.Resources = make([]domain.SessionResource, len(resources))
		for index := range resources {
			resources[index].Resource.CreatedAt = resources[index].Resource.CreatedAt.UTC().Truncate(time.Microsecond)
			resources[index].Resource.UpdatedAt = resources[index].Resource.UpdatedAt.UTC().Truncate(time.Microsecond)
			session.Resources[index] = resources[index].Resource
		}
	}
	body, err := json.Marshal(session)
	if err != nil {
		return Admission{}, err
	}
	var admission Admission
	err = s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		if checkDependencies {
			if _, err := q.LockActiveAgentVersion(ctx, pgstore.LockActiveAgentVersionParams{
				ID: session.AgentID, Version: int32(session.AgentVersion),
				WorkspaceID: workspaceID,
			}); errors.Is(err, pgx.ErrNoRows) {
				return domain.Validation("agent is missing or archived")
			} else if err != nil {
				return err
			}
			if err := lockActiveSessionRoster(ctx, q, workspaceID, session); err != nil {
				return err
			}
			if _, err := q.LockActiveEnvironment(ctx, pgstore.LockActiveEnvironmentParams{
				ID: session.EnvironmentID, WorkspaceID: workspaceID,
			}); errors.Is(err, pgx.ErrNoRows) {
				return domain.Validation("environment is missing or archived")
			} else if err != nil {
				return err
			}
			if err := lockActiveSessionVaults(ctx, tx, workspaceID, session.VaultIDs); err != nil {
				return err
			}
		}
		params := insertSessionParams(session, body)
		params.WorkspaceID = workspaceID
		if err := q.InsertSession(ctx, params); err != nil {
			return err
		}
		if err := s.insertPrimarySessionThread(ctx, tx, session); err != nil {
			return err
		}
		if err := s.enqueueWebhookEvent(
			ctx, q, workspaceID, domain.WebhookEventSessionStatusScheduled,
			session.ID, session.CreatedAt, nil,
		); err != nil {
			return err
		}
		if err := insertSessionSkillVersions(ctx, tx, session); err != nil {
			return err
		}
		if err := insertSessionVaults(ctx, tx, session); err != nil {
			return err
		}
		if err := insertPreparedSessionResources(
			ctx, tx, workspaceID, session.ID, resources,
		); err != nil {
			return err
		}
		admission = Admission{Session: session}
		if len(drafts) > 0 {
			var innerErr error
			admission, innerErr = s.admitLocked(ctx, tx, q, session, drafts)
			if innerErr != nil {
				return innerErr
			}
		}
		if deploymentRun != nil {
			return s.insertDeploymentRun(
				ctx, tx, q, workspaceID, *deploymentRun,
			)
		}
		return nil
	})
	if err != nil {
		return Admission{}, err
	}
	s.notifySession(ctx, session.ID)
	return admission, nil
}

func lockActiveSessionRoster(
	ctx context.Context,
	q *pgstore.Queries,
	workspaceID string,
	session domain.Session,
) error {
	type pin struct {
		id      string
		version int
	}
	pins := make([]pin, 0, len(session.MultiagentRoster))
	seen := map[pin]struct{}{{id: session.AgentID, version: session.AgentVersion}: {}}
	for _, member := range session.MultiagentRoster {
		candidate := pin{id: member.ID, version: member.Version}
		if _, duplicate := seen[candidate]; duplicate {
			continue
		}
		seen[candidate] = struct{}{}
		pins = append(pins, candidate)
	}
	sort.Slice(pins, func(i, j int) bool {
		if pins[i].id == pins[j].id {
			return pins[i].version < pins[j].version
		}
		return pins[i].id < pins[j].id
	})
	for _, candidate := range pins {
		if _, err := q.LockActiveAgentVersion(ctx, pgstore.LockActiveAgentVersionParams{
			ID: candidate.id, Version: int32(candidate.version),
			WorkspaceID: workspaceID,
		}); errors.Is(err, pgx.ErrNoRows) {
			return domain.Validation("multiagent roster member is missing or archived")
		} else if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) insertPrimarySessionThread(
	ctx context.Context,
	tx pgx.Tx,
	session domain.Session,
) error {
	thread := domain.NewPrimarySessionThread(
		s.ids.NewID(domain.PrefixSessionThread),
		session,
	)
	body, err := json.Marshal(thread)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO session_threads (
	id, session_id, parent_thread_id, kind, status, body,
	created_at, updated_at, archived_at
) VALUES ($1, $2, NULL, 'primary', $3, $4, $5, $6, $7)`,
		thread.ID,
		thread.SessionID,
		thread.Status,
		body,
		thread.CreatedAt,
		thread.UpdatedAt,
		thread.ArchivedAt,
	)
	return err
}

func (s *Store) insertDeploymentRun(
	ctx context.Context,
	tx pgx.Tx,
	q *pgstore.Queries,
	workspaceID string,
	run domain.DeploymentRun,
) error {
	run.CreatedAt = run.CreatedAt.UTC().Truncate(time.Microsecond)
	if run.ScheduledAt != nil {
		scheduled := run.ScheduledAt.UTC().Truncate(time.Microsecond)
		run.ScheduledAt = &scheduled
	}
	body, err := json.Marshal(run)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO deployment_runs (
    id, deployment_id, session_id, error_type, trigger_type,
    scheduled_at, body, created_at
) VALUES ($1, $2, $3, NULL, $4, $5, $6, $7)`,
		run.ID, run.DeploymentID, run.SessionID, run.TriggerType,
		run.ScheduledAt, body, run.CreatedAt,
	)
	if isUniqueViolation(err) {
		return domain.Conflict("deployment schedule occurrence already ran")
	}
	if err != nil || run.TriggerType != domain.DeploymentTriggerSchedule {
		return err
	}
	return s.enqueueWebhookEvent(
		ctx, q, workspaceID, domain.WebhookEventDeploymentRunSucceeded,
		run.ID, run.CreatedAt, nil,
	)
}

func lockActiveSessionVaults(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	vaultIDs []string,
) error {
	if len(vaultIDs) == 0 {
		return nil
	}
	ordered := append([]string(nil), vaultIDs...)
	sort.Strings(ordered)
	rows, err := tx.Query(ctx, `
SELECT id FROM vaults
WHERE id = ANY($1) AND workspace_id = $2 AND archived_at IS NULL
ORDER BY id
FOR SHARE`, ordered, workspaceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count != len(vaultIDs) {
		return domain.Validation("vault_ids references a missing or archived Vault")
	}
	return nil
}

func insertSessionVaults(ctx context.Context, tx pgx.Tx, session domain.Session) error {
	for position, vaultID := range session.VaultIDs {
		if _, err := tx.Exec(ctx, `
INSERT INTO session_vaults (session_id, position, vault_id)
VALUES ($1, $2, $3)`, session.ID, position, vaultID); err != nil {
			if isUniqueViolation(err) {
				return domain.Validation("vault_ids must not contain duplicates")
			}
			return err
		}
	}
	return nil
}

func insertSessionSkillVersions(ctx context.Context, tx pgx.Tx, session domain.Session) error {
	type executionScope struct {
		id      string
		version int
	}
	agents := make([]domain.Agent, 0, len(session.MultiagentRoster)+1)
	agents = append(agents, session.AgentSnapshot)
	seen := map[executionScope]struct{}{{
		id: session.AgentSnapshot.ID, version: session.AgentSnapshot.Version,
	}: {}}
	for _, member := range session.MultiagentRoster {
		scope := executionScope{id: member.ID, version: member.Version}
		if _, duplicate := seen[scope]; duplicate {
			continue
		}
		seen[scope] = struct{}{}
		agents = append(agents, member)
	}
	for _, agent := range agents {
		if err := insertSessionAgentSkillVersions(
			ctx, tx, session.WorkspaceID, session.ID, agent,
		); err != nil {
			return err
		}
	}
	return nil
}

func insertSessionAgentSkillVersions(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	sessionID string,
	agent domain.Agent,
) error {
	if len(agent.Skills) > app.MaxSessionSkills {
		return domain.Validation("skills must contain at most 500 entries")
	}
	for position, reference := range agent.Skills {
		if reference.Type != "custom" || reference.SkillID == "" ||
			reference.Version == "" || reference.Version == "latest" {
			return domain.Validation(
				"Session Agent Skill references must use concrete custom Versions",
			)
		}
		locked, err := lockReadySkillVersion(ctx, tx, workspaceID, reference)
		if err != nil {
			return err
		}
		if !locked {
			return domain.Validation("Session references a missing custom Skill Version")
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO session_skill_versions (
    session_id, agent_id, agent_version, position, skill_id, skill_version
) VALUES ($1, $2, $3, $4, $5, $6)`,
			sessionID, agent.ID, agent.Version, position,
			reference.SkillID, reference.Version,
		); err != nil {
			if isForeignKeyViolation(err) {
				return domain.Validation("Session references a missing custom Skill Version")
			}
			return err
		}
	}
	return nil
}

// lockReadySkillVersion is the common admission fence for Agent and Session
// pins. It conflicts with Version deletion's FOR UPDATE lock, so either the pin
// commits first and blocks deletion or deletion changes the Version state and
// the pin observes it as unavailable.
func lockReadySkillVersion(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	reference domain.SkillReference,
) (bool, error) {
	var locked int
	err := tx.QueryRow(ctx, `
SELECT 1
FROM skill_versions AS version
JOIN skills AS skill ON skill.id = version.skill_id AND skill.ready
WHERE version.skill_id = $1 AND version.version = $2
  AND skill.workspace_id = $3
  AND version.state = 'ready'
FOR SHARE OF version`, reference.SkillID, reference.Version, workspaceID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func insertSessionParams(session domain.Session, body []byte) pgstore.InsertSessionParams {
	return pgstore.InsertSessionParams{
		ID:            session.ID,
		Status:        string(session.Status),
		Body:          body,
		CreatedAt:     tsUTC(session.CreatedAt),
		UpdatedAt:     tsUTC(session.UpdatedAt),
		AgentID:       stringPtr(session.AgentID),
		AgentVersion:  int32Ptr(session.AgentVersion),
		EnvironmentID: stringPtr(session.EnvironmentID),
		DeploymentID:  session.DeploymentID,
		ArchivedAt:    tsPtr(session.ArchivedAt),
	}
}

// AdmitEvents is the PostgreSQL event-admission transaction for the Temporal
// path. It locks the session, validates and admits an ordered event batch,
// assigns durable per-session receipt sequences, appends the public events and
// projection changes, and writes a coalescible orchestration outbox wakeup — all
// atomically. The outbox row is a wakeup carrying the highest known sequence, not
// a run queue: a second admission before delivery coalesces into the same row.
func (s *Store) AdmitEvents(
	ctx context.Context,
	sessionID string,
	drafts []domain.EventDraft,
) (Admission, error) {
	if len(drafts) == 0 {
		return Admission{}, domain.Validation("no events to admit")
	}
	if requestScope, _ := workspace.FromContext(ctx); requestScope.Session != nil {
		for _, draft := range drafts {
			if !domain.IsSessionCredentialEvent(draft.Type) {
				return Admission{}, domain.Permission(
					"session credential may only send tool-result events",
				)
			}
		}
	}
	var admission Admission
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		row, err := q.LockSession(ctx, sessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("session not found")
		}
		if err != nil {
			return err
		}
		// Session is the root lifecycle lock. Take it before the Work lease
		// lock so admission uses the same order as deletion and cannot deadlock
		// against the environment_work cascade.
		if err := s.fenceSessionCredential(ctx, tx, sessionID); err != nil {
			return err
		}
		if row.DeletingAt.Valid {
			return domain.Conflict("session deletion is in progress")
		}
		session, err := sessionFromLockRow(row)
		if err != nil {
			return err
		}
		if session.ArchivedAt != nil {
			return domain.Conflict("cannot send events to an archived session")
		}
		if session.Status == domain.StatusTerminated {
			return domain.Conflict("cannot send events to a terminated session")
		}
		var innerErr error
		admission, innerErr = s.admitLocked(ctx, tx, q, session, drafts)
		return innerErr
	})
	if err != nil {
		return Admission{}, err
	}
	s.notifySession(ctx, sessionID)
	return admission, nil
}

// fenceSessionCredential makes a per-Work tool-result write linearizable with
// lease reclaim. Middleware establishes the narrow route scope; this database
// check proves the same token still owns a live, heartbeat-established claim in
// the transaction that appends the event.
func (s *Store) fenceSessionCredential(ctx context.Context, tx pgx.Tx, sessionID string) error {
	requestScope, _ := workspace.FromContext(ctx)
	if requestScope.Session == nil {
		return nil
	}
	claim := requestScope.Session
	if claim.SessionID != sessionID || len(claim.CredentialDigest) == 0 {
		return domain.Precondition("work lease is no longer owned")
	}
	var found bool
	err := tx.QueryRow(ctx, `
SELECT true
FROM environment_work
WHERE id = $1
  AND environment_id = $2
  AND session_id = $3
  AND sessions_token_hash = $4
  AND (
      (
          state = 'stopping' AND
          stop_requested_at >= $5::timestamptz - make_interval(secs => ttl_seconds)
      ) OR
      (
          state = 'active' AND
          latest_heartbeat_at >= $5::timestamptz - make_interval(secs => ttl_seconds)
      )
  )
FOR SHARE`, claim.WorkID, claim.EnvironmentID, sessionID, claim.CredentialDigest,
		s.clock.Now().UTC()).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Precondition("work lease is no longer owned")
	}
	if err != nil {
		return err
	}
	if !found {
		return domain.Precondition("work lease is no longer owned")
	}
	return nil
}

// admitLocked appends the batch under an already-taken session lock (or a
// freshly inserted session in CreateSession). It assigns receipt sequences,
// reopens the session to running when a client trigger arrives, and writes the
// coalescible wakeup.
func (s *Store) admitLocked(
	ctx context.Context,
	tx pgx.Tx,
	q *pgstore.Queries,
	session domain.Session,
	drafts []domain.EventDraft,
) (Admission, error) {
	var primaryThread *domain.SessionThread
	if session.AgentSnapshot.Multiagent != nil {
		body, err := q.GetPrimarySessionThreadProjection(ctx, session.ID)
		if err != nil {
			return Admission{}, err
		}
		var thread domain.SessionThread
		if err := json.Unmarshal(body, &thread); err != nil {
			return Admission{}, err
		}
		primaryThread = &thread
	}
	putRuntimeProjection := func() error {
		if primaryThread == nil {
			return s.putProjection(ctx, q, session)
		}
		body, err := json.Marshal(session)
		if err != nil {
			return err
		}
		if err := q.UpdateSessionStatus(ctx, pgstore.UpdateSessionStatusParams{
			Status: string(session.Status), Body: body,
			UpdatedAt: tsUTC(session.UpdatedAt), ID: session.ID,
		}); err != nil {
			return err
		}
		return putPrimaryThreadProjection(ctx, q, *primaryThread)
	}
	drafts = s.linkCompanionSystemMessage(drafts)
	var err error
	drafts, session, err = s.prepareOutcomeDrafts(session, drafts)
	if err != nil {
		return Admission{}, err
	}
	for _, d := range drafts {
		if !domain.IsClientSubmittable(d.Type) {
			return Admission{}, domain.Validation("event type is not client-submittable: " + d.Type)
		}
	}
	if session.BudgetReached(s.clock.Now().UTC()) {
		interruptOnly := len(drafts) > 0
		for _, draft := range drafts {
			switch draft.Type {
			case domain.EvUserMessage, domain.EvUserDefineOutcome:
				return Admission{}, domain.Conflict(
					"session budget has been reached; raise or remove the budget before submitting new work",
				)
			case domain.EvUserInterrupt:
			default:
				interruptOnly = false
			}
		}
		if interruptOnly && session.Status == domain.StatusIdle {
			pendingIDs, err := q.SessionPendingClientActionEventIDs(ctx, session.ID)
			if err != nil {
				return Admission{}, err
			}
			if len(pendingIDs) == 0 {
				// A budget-idle Session with no requires_action barrier has no
				// active provider/tool work to cancel. Accept the control request
				// as an eventless no-op. Pending actions remain interruptible.
				return Admission{Session: session}, nil
			}
		}
	}

	maxSeq, err := q.MaxEventSeq(ctx, session.ID)
	if err != nil {
		return Admission{}, err
	}
	primaryThreadID, err := q.GetPrimarySessionThreadID(ctx, session.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Admission{}, fmt.Errorf("pg: primary session thread is missing for %s", session.ID)
	}
	if err != nil {
		return Admission{}, err
	}
	routed, resolutionThreads, interruptTargets, err := s.routeClientDraftsLocked(
		ctx, tx, q, session.ID, primaryThreadID, drafts,
	)
	if err != nil {
		return Admission{}, err
	}
	events := make([]domain.Event, 0, len(routed))
	submittedEvents := make([]domain.Event, 0, len(drafts))
	submittedIDs := make(map[string]bool, len(drafts))
	for _, item := range routed {
		committed, nextSeq, appendErr := s.appendThreadDrafts(
			ctx, q, session.ID, item.ThreadID,
			[]domain.EventDraft{item.Draft}, maxSeq, nil,
		)
		if appendErr != nil {
			return Admission{}, appendErr
		}
		maxSeq = nextSeq
		events = append(events, committed...)
		if item.Submitted {
			for _, event := range committed {
				submittedIDs[event.ID] = true
			}
		}
	}

	// Claim matching client-action resolutions in the same transaction that
	// commits them. The row remains unresolved until the resume turn closes, so
	// ordinary queued messages cannot overtake an in-flight resolution.
	admittedResolution, err := s.claimPendingResolutionsLocked(
		ctx,
		q,
		session.ID,
		events,
	)
	if err != nil {
		return Admission{}, err
	}
	// Claiming consumes external approvals immediately. Return the same
	// processed_at receipt that history and live observers will read.
	for _, event := range events {
		if submittedIDs[event.ID] {
			submittedEvents = append(submittedEvents, event)
		}
	}

	hasMessage := false
	hasOutcome := false
	hasInterrupt := false
	for _, event := range events {
		switch event.Type {
		case domain.EvUserMessage:
			hasMessage = true
		case domain.EvUserDefineOutcome:
			hasOutcome = true
		case domain.EvUserInterrupt:
			hasInterrupt = true
		}
	}

	// Ordinary work may be admitted while a requires_action wait is open, but it
	// is not runnable yet: keep the session idle, emit no status_running, and
	// write no wakeup for work the Workflow must not consume. A partial resolution
	// is also only a durable claim: the official barrier reopens once every
	// blocking action has a result, not once per result. Interrupt is the control
	// exception: it always wakes the Workflow so an active Activity can be
	// canceled, but it never opens a pending-action gate or by itself projects an
	// idle session as running.
	gated, err := q.HasUnresolvedPendingActions(
		ctx,
		pgstore.HasUnresolvedPendingActionsParams{
			SessionID: session.ID, ThreadID: primaryThreadID,
		},
	)
	if err != nil {
		return Admission{}, err
	}
	if gated && len(events) >= 2 &&
		events[len(events)-1].Type == domain.EvSystemMessage &&
		events[len(events)-2].Type == domain.EvUserMessage {
		return Admission{}, domain.Validation(
			"system.message may accompany only a tool result while the session requires action",
		)
	}
	hasUnclaimed := false
	if gated {
		hasUnclaimed, err = q.HasUnclaimedPendingActions(
			ctx,
			pgstore.HasUnclaimedPendingActionsParams{
				SessionID: session.ID, ThreadID: primaryThreadID,
			},
		)
		if err != nil {
			return Admission{}, err
		}
	}
	_, primaryResolutionAdmitted := resolutionThreads[primaryThreadID]
	resolutionBarrierReady := admittedResolution && primaryResolutionAdmitted &&
		gated && !hasUnclaimed

	// A fully claimed child barrier wakes only that child. Its status transition,
	// child/primary lifecycle projections, and Thread outbox write share this
	// admission transaction with the client result.
	candidateChildThreads := make([]string, 0, len(resolutionThreads))
	for threadID := range resolutionThreads {
		if threadID != primaryThreadID {
			candidateChildThreads = append(candidateChildThreads, threadID)
		}
	}
	sort.Strings(candidateChildThreads)
	childWakeSet := make(map[string]struct{}, len(candidateChildThreads)+len(interruptTargets.Children))
	for _, threadID := range candidateChildThreads {
		childGated, err := q.HasUnresolvedPendingActions(
			ctx,
			pgstore.HasUnresolvedPendingActionsParams{
				SessionID: session.ID, ThreadID: threadID,
			},
		)
		if err != nil {
			return Admission{}, err
		}
		if !childGated {
			continue
		}
		childUnclaimed, err := q.HasUnclaimedPendingActions(
			ctx,
			pgstore.HasUnclaimedPendingActionsParams{
				SessionID: session.ID, ThreadID: threadID,
			},
		)
		if err != nil {
			return Admission{}, err
		}
		if childUnclaimed {
			continue
		}
		thread, err := loadSessionThreadForUpdate(ctx, tx, session.ID, threadID)
		if err != nil {
			return Admission{}, err
		}
		if thread.ParentThreadID == nil {
			return Admission{}, domain.Conflict(
				"child resolution cannot target the primary Thread",
			)
		}
		if thread.ArchivedAt != nil || thread.Status == domain.StatusTerminated {
			return Admission{}, domain.Conflict(
				"cannot resolve an action for a terminated child Thread",
			)
		}
		if thread.Status != domain.StatusRunning {
			thread.TransitionStatus(domain.StatusRunning, s.clock.Now().UTC())
			if err := putSessionThreadTx(ctx, tx, thread); err != nil {
				return Admission{}, err
			}
			lifecycle := domain.EventDraft{
				Type: domain.EvSessionThreadStatusRunning,
				Payload: map[string]any{
					"session_thread_id": thread.ID,
					"agent_name":        thread.Agent.Name,
				},
			}
			childEvents, nextSeq, err := s.appendThreadDrafts(
				ctx, q, session.ID, thread.ID,
				[]domain.EventDraft{lifecycle}, maxSeq, nil,
			)
			if err != nil {
				return Admission{}, err
			}
			maxSeq = nextSeq
			primaryEvents, nextSeq, err := s.appendThreadDrafts(
				ctx, q, session.ID, primaryThreadID,
				[]domain.EventDraft{lifecycle}, maxSeq, nil,
			)
			if err != nil {
				return Admission{}, err
			}
			maxSeq = nextSeq
			events = append(events, childEvents...)
			events = append(events, primaryEvents...)
		}
		childWakeSet[threadID] = struct{}{}
	}
	childHasRunnableWork := len(childWakeSet) > 0
	for threadID := range interruptTargets.Children {
		childWakeSet[threadID] = struct{}{}
	}
	childWakeThreads := make([]string, 0, len(childWakeSet))
	for threadID := range childWakeSet {
		childWakeThreads = append(childWakeThreads, threadID)
	}
	sort.Strings(childWakeThreads)

	primaryHasRunnableWork := hasMessage || hasOutcome || resolutionBarrierReady
	primaryWorkRunnable := primaryHasRunnableWork && (!gated || resolutionBarrierReady)
	hasRunnableWork := primaryWorkRunnable || childHasRunnableWork

	admission := Admission{
		Session: session, Events: events, SubmittedEvents: submittedEvents, MaxSeq: maxSeq,
	}
	if !hasRunnableWork && !hasInterrupt {
		if err := putRuntimeProjection(); err != nil {
			return Admission{}, err
		}
		return admission, nil
	}

	// Reopen an idle Session when runnable work arrives. A rescheduling Session
	// already owns an active turn: later input is queued behind it and must not
	// publish status_running before the retry actually resumes. An interrupt-only
	// admission is a control wakeup, not new work, so it deliberately emits no
	// synthetic running transition while idle.
	if primaryWorkRunnable {
		session.MarkActiveOutcomeRunning()
		if primaryThread != nil && primaryThread.Status != domain.StatusRunning {
			primaryThread.TransitionStatus(domain.StatusRunning, s.clock.Now().UTC())
		}
	}
	if hasRunnableWork && session.Status == domain.StatusIdle {
		session.TransitionStatus(domain.StatusRunning, s.clock.Now().UTC())
		statusEvents, newMax, err := s.appendDrafts(ctx, q, session.ID,
			[]domain.EventDraft{{Type: domain.EvSessionStatusRunning, Payload: map[string]any{}}},
			maxSeq, nil)
		if err != nil {
			return Admission{}, err
		}
		maxSeq = newMax
		events = append(events, statusEvents...)
		admission.Events = events
		admission.Session = session
		admission.MaxSeq = maxSeq
	}
	if err := putRuntimeProjection(); err != nil {
		return Admission{}, err
	}

	// The coalescible orchestration wakeup. One pending row per session; a burst
	// of admissions coalesces and raises max_event_seq to the newest receipt
	// sequence. This is the durable signal the relay delivers to Temporal.
	primaryEnqueued := primaryWorkRunnable || interruptTargets.Primary
	if primaryEnqueued {
		if err := q.UpsertOutbox(ctx, pgstore.UpsertOutboxParams{
			SessionID:   session.ID,
			MaxEventSeq: maxSeq,
			EnqueuedAt:  tsUTC(s.clock.Now().UTC()),
		}); err != nil {
			return Admission{}, err
		}
	}
	for _, threadID := range childWakeThreads {
		if err := q.UpsertThreadOutbox(ctx, pgstore.UpsertThreadOutboxParams{
			SessionID: session.ID, ThreadID: threadID,
			MaxEventSeq: maxSeq, EnqueuedAt: tsUTC(s.clock.Now().UTC()),
		}); err != nil {
			return Admission{}, err
		}
	}
	if hasRunnableWork && session.EnvironmentType == "self_hosted" {
		if err := q.EnqueueEnvironmentWork(ctx, pgstore.EnqueueEnvironmentWorkParams{
			ID:            s.ids.NewID(domain.PrefixEnvironmentWork),
			EnvironmentID: session.EnvironmentID,
			SessionID:     session.ID,
			ActivationSeq: maxSeq,
			CreatedAt:     tsUTC(s.clock.Now().UTC()),
		}); err != nil {
			return Admission{}, err
		}
	}
	admission.Session = session
	admission.Events = events
	admission.MaxSeq = maxSeq
	admission.PrimaryEnqueued = primaryEnqueued
	admission.WakeThreadIDs = childWakeThreads
	admission.Enqueued = primaryEnqueued || len(childWakeThreads) > 0
	return admission, nil
}

func (s *Store) prepareOutcomeDrafts(
	session domain.Session,
	drafts []domain.EventDraft,
) ([]domain.EventDraft, domain.Session, error) {
	prepared := append([]domain.EventDraft(nil), drafts...)
	for index, draft := range prepared {
		if draft.Type != domain.EvUserDefineOutcome {
			continue
		}
		payload := make(map[string]any, len(draft.Payload)+1)
		for key, value := range draft.Payload {
			payload[key] = value
		}
		outcomeID := s.ids.NewID(domain.PrefixOutcome)
		payload["outcome_id"] = outcomeID
		if _, configured := payload["max_iterations"]; !configured {
			payload["max_iterations"] = 3
		}
		description, _ := payload["description"].(string)
		if err := session.StartOutcome(domain.OutcomeSpec{
			OutcomeID: outcomeID, Description: description,
		}); err != nil {
			return nil, domain.Session{}, err
		}
		draft.Payload = payload
		prepared[index] = draft
	}
	return prepared, session, nil
}

func (s *Store) linkCompanionSystemMessage(
	drafts []domain.EventDraft,
) []domain.EventDraft {
	if len(drafts) < 2 || drafts[len(drafts)-1].Type != domain.EvSystemMessage {
		return drafts
	}
	linked := append([]domain.EventDraft(nil), drafts...)
	companion := linked[len(linked)-1]
	if companion.ID == "" {
		companion.ID = s.ids.NewID(domain.PrefixEvent)
	}
	linked[len(linked)-1] = companion
	preceding := linked[len(linked)-2]
	payload := make(map[string]any, len(preceding.Payload)+2)
	for key, value := range preceding.Payload {
		payload[key] = value
	}
	payload[domain.InternalCompanionSystemEventID] = companion.ID
	payload[domain.InternalCompanionSystemContent] = companion.Payload["content"]
	preceding.Payload = payload
	linked[len(linked)-2] = preceding
	return linked
}

// appendDrafts inserts a slice of drafts starting after startSeq, returning the
// committed events and the new max sequence. turnEventID, when non-nil, tags
// every appended event so a completed turn's output can be replayed
// idempotently by trigger id.
func (s *Store) appendDrafts(
	ctx context.Context,
	q *pgstore.Queries,
	sessionID string,
	drafts []domain.EventDraft,
	startSeq int64,
	turnEventID *string,
) ([]domain.Event, int64, error) {
	return s.appendDraftsAt(
		ctx, q, sessionID, drafts, startSeq, turnEventID, s.clock.Now().UTC(),
	)
}

// appendDraftsAt is the completion-boundary variant of appendDrafts. A turn's
// authoritative output and the input events it finishes share one processed_at
// instant, so the public processed-time ledger falls back to receipt sequence
// for causal ordering (input before output).
func (s *Store) appendDraftsAt(
	ctx context.Context,
	q *pgstore.Queries,
	sessionID string,
	drafts []domain.EventDraft,
	startSeq int64,
	turnEventID *string,
	eventTime time.Time,
) ([]domain.Event, int64, error) {
	threadID, err := q.GetPrimarySessionThreadID(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("pg: primary session thread is missing for %s", sessionID)
	}
	if err != nil {
		return nil, 0, err
	}
	return s.appendThreadDraftsAt(
		ctx, q, sessionID, threadID, drafts, startSeq, turnEventID, eventTime,
	)
}

// appendThreadDrafts writes into one Thread ledger while preserving the
// Session-wide sequence used for aggregate ordering and cross-post causality.
// The caller holds the Session admission lock.
func (s *Store) appendThreadDrafts(
	ctx context.Context,
	q *pgstore.Queries,
	sessionID string,
	threadID string,
	drafts []domain.EventDraft,
	startSeq int64,
	turnEventID *string,
) ([]domain.Event, int64, error) {
	return s.appendThreadDraftsAt(
		ctx, q, sessionID, threadID, drafts, startSeq, turnEventID,
		s.clock.Now().UTC(),
	)
}

func (s *Store) appendThreadDraftsAt(
	ctx context.Context,
	q *pgstore.Queries,
	sessionID string,
	threadID string,
	drafts []domain.EventDraft,
	startSeq int64,
	turnEventID *string,
	eventTime time.Time,
) ([]domain.Event, int64, error) {
	seq := startSeq
	out := make([]domain.Event, 0, len(drafts))
	now := eventTime.UTC()
	primaryThread := false
	var webhookWorkspaceID string
	if webhookCandidateDrafts(drafts) {
		primaryThreadID, err := q.GetPrimarySessionThreadID(ctx, sessionID)
		if err != nil {
			return nil, 0, err
		}
		primaryThread = primaryThreadID == threadID
		if primaryThread {
			webhookWorkspaceID, err = q.GetSessionWorkspaceID(ctx, sessionID)
			if err != nil {
				return nil, 0, err
			}
		}
	}
	for _, d := range drafts {
		seq++
		id := d.ID
		if id == "" {
			id = s.ids.NewID(domain.PrefixEvent)
		}
		payload := d.Payload
		if payload == nil {
			payload = map[string]any{}
		}
		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		var processedAt pgtype.Timestamptz
		var processedPtr *time.Time
		if domain.ProcessedOnReceipt(d.Type) {
			processedAt = tsUTC(now)
			t := now
			processedPtr = &t
		}
		if err := q.InsertEvent(ctx, pgstore.InsertEventParams{
			ID:          id,
			SessionID:   sessionID,
			ThreadID:    threadID,
			Seq:         seq,
			Type:        d.Type,
			Payload:     payloadJSON,
			TurnEventID: turnEventID,
			CreatedAt:   tsUTC(now),
			ProcessedAt: processedAt,
		}); err != nil {
			return nil, 0, err
		}
		if primaryThread {
			if err := s.enqueueWebhooksForSessionEvent(
				ctx, q, webhookWorkspaceID, sessionID, d.Type, payload, now,
			); err != nil {
				return nil, 0, err
			}
		}
		out = append(out, domain.Event{
			ID:          id,
			SessionID:   sessionID,
			ThreadID:    threadID,
			Sequence:    seq,
			Type:        d.Type,
			Payload:     payload,
			TurnEventID: turnEventID,
			CreatedAt:   now,
			ProcessedAt: processedPtr,
		})
	}
	return out, seq, nil
}

func (s *Store) putProjection(ctx context.Context, q *pgstore.Queries, session domain.Session) error {
	body, err := json.Marshal(session)
	if err != nil {
		return err
	}
	if err := q.UpdateSessionStatus(ctx, pgstore.UpdateSessionStatusParams{
		Status:    string(session.Status),
		Body:      body,
		UpdatedAt: tsUTC(session.UpdatedAt),
		ID:        session.ID,
	}); err != nil {
		return err
	}
	return s.putPrimarySessionThreadProjection(ctx, q, session)
}

// EventsAfter returns the session's public events with sequence strictly greater
// than cursor, in ascending receipt order, bounded by limit. This is the ordered
// consumption path the SessionWorkflow uses after its durable cursor.
func (s *Store) EventsAfter(ctx context.Context, sessionID string, cursor int64, limit int) ([]domain.Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.q.ListEventsAfter(ctx, pgstore.ListEventsAfterParams{
		SessionID: sessionID,
		AfterSeq:  cursor,
		RowLimit:  int32(limit),
	})
	if err != nil {
		return nil, err
	}
	return eventsFromRows(rows)
}

// FirstUnprocessedInterruptAfter returns the earliest interrupt that can still
// affect work triggered after the supplied receipt sequence. A nil event means
// no durable interrupt is currently pending.
func (s *Store) FirstUnprocessedInterruptAfter(
	ctx context.Context,
	sessionID string,
	afterSeq int64,
) (*domain.Event, error) {
	row, err := s.q.FirstUnprocessedInterruptAfter(
		ctx,
		pgstore.FirstUnprocessedInterruptAfterParams{
			SessionID: sessionID,
			AfterSeq:  afterSeq,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	event, err := eventFromRow(row)
	if err != nil {
		return nil, err
	}
	return &event, nil
}

// FirstUnprocessedThreadInterruptAfter returns the earliest independently
// processable interrupt in one child Thread ledger after the supplied trigger.
func (s *Store) FirstUnprocessedThreadInterruptAfter(
	ctx context.Context,
	sessionID string,
	threadID string,
	afterSeq int64,
) (*domain.Event, error) {
	row, err := s.q.FirstUnprocessedThreadInterruptAfter(
		ctx,
		pgstore.FirstUnprocessedThreadInterruptAfterParams{
			SessionID: sessionID,
			ThreadID:  threadID,
			AfterSeq:  afterSeq,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	event, err := eventFromRow(row)
	if err != nil {
		return nil, err
	}
	return &event, nil
}

// GetSession returns the current session projection.
func (s *Store) GetSession(ctx context.Context, id string) (domain.Session, error) {
	row, err := s.q.GetSession(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Session{}, domain.NotFound("session not found")
	}
	if err != nil {
		return domain.Session{}, err
	}
	return sessionFromGetRow(row)
}

// GetSessionForWorkspace is the public authorization boundary. Internal
// Temporal activities use GetSession after the HTTP/Deployment admission path
// has already bound the globally unique Session ID to a Workspace.
func (s *Store) GetSessionForWorkspace(
	ctx context.Context,
	id string,
) (domain.Session, error) {
	workspaceID, scoped, err := s.workspaceForRead(ctx)
	if err != nil {
		return domain.Session{}, err
	}
	if !scoped {
		return s.GetSession(ctx, id)
	}
	var body []byte
	if err := s.pool.QueryRow(ctx, `
SELECT body
FROM sessions
WHERE id = $1 AND workspace_id = $2`, id, workspaceID).Scan(&body); errors.Is(err, pgx.ErrNoRows) {
		return domain.Session{}, domain.NotFound("session not found")
	} else if err != nil {
		return domain.Session{}, err
	}
	var session domain.Session
	if err := json.Unmarshal(body, &session); err != nil {
		return domain.Session{}, fmt.Errorf("pg: decode session %s: %w", id, err)
	}
	session.WorkspaceID = workspaceID
	return session, nil
}

func (s *Store) AssertSessionWorkspace(ctx context.Context, id string) error {
	_, err := s.GetSessionForWorkspace(ctx, id)
	return err
}

// GetEvent returns a single public event by id.
func (s *Store) GetEvent(ctx context.Context, sessionID, id string) (domain.Event, error) {
	row, err := s.q.GetEvent(ctx, pgstore.GetEventParams{SessionID: sessionID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Event{}, domain.NotFound("event not found")
	}
	if err != nil {
		return domain.Event{}, err
	}
	return eventFromRow(row)
}

// HistoryThrough reconstructs the causal conversation history for the turn
// triggered by triggerEventID, to be projected into the model. Public receipt
// order is deliberately NOT what a turn replays, because a later user.message
// admitted before an earlier turn finished must not appear as a peer of the
// current trigger.
//
// The reconstruction walks prior *processed* model-driving triggers
// (user.message plus client-action results) in receipt order and, for each,
// appends that trigger followed by the exact output events it committed
// (identified by turn_event_id). It then appends the current trigger. This
// interleaves trigger / agent output in causal order, so a batch A,B projects as
// [A, agent(A), B] rather than collapsing A and B into two consecutive user
// turns, and later turns retain output produced by a resumed barrier.
//
// The result is bounded to the newest `limit` events, preserving causal order —
// an over-limit session carries its most recent context, not the oldest. A
// window that cuts a tool_use/tool_result pair is left to ProjectMessages'
// existing dangling/orphan repair.
func (s *Store) HistoryThrough(ctx context.Context, sessionID, triggerEventID string, limit int) ([]domain.Event, error) {
	trigger, err := s.GetEvent(ctx, sessionID, triggerEventID)
	if err != nil {
		return nil, err
	}

	priorRows, err := s.priorProcessedModelTriggers(
		ctx, sessionID, trigger.ThreadID, trigger.Sequence,
	)
	if err != nil {
		return nil, err
	}
	priors, err := eventsFromRows(priorRows)
	if err != nil {
		return nil, err
	}

	var ordered []domain.Event
	for _, prior := range priors {
		ordered = append(ordered, prior)
		id := prior.ID
		outRows, err := s.q.ListEventsByTurn(ctx, pgstore.ListEventsByTurnParams{
			SessionID:   sessionID,
			TurnEventID: &id,
		})
		if err != nil {
			return nil, err
		}
		outputs, err := eventsFromRows(outRows)
		if err != nil {
			return nil, err
		}
		ordered = append(ordered, outputs...)
	}
	ordered = append(ordered, trigger)

	// Bound to the newest `limit` events, keeping causal order.
	if limit > 0 && len(ordered) > limit {
		ordered = ordered[len(ordered)-limit:]
	}
	return ordered, nil
}

func (s *Store) priorProcessedModelTriggers(
	ctx context.Context,
	sessionID string,
	threadID string,
	beforeSeq int64,
) ([]pgstore.Event, error) {
	rows, err := s.pool.Query(ctx, `
SELECT event.*
FROM events AS event
WHERE event.session_id = $1
  AND event.thread_id = $2
  AND event.type IN (
      'user.message', 'user.custom_tool_result', 'user.tool_confirmation',
      'user.tool_result', 'agent.thread_message_received'
  )
  AND event.processed_at IS NOT NULL
	AND event.turn_event_id IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM pending_actions AS action
      WHERE action.session_id = event.session_id AND action.approval_event_id = event.id
  )
  AND event.seq < $3
ORDER BY event.seq`, sessionID, threadID, beforeSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEventRows(rows)
}

func scanEventRows(rows pgx.Rows) ([]pgstore.Event, error) {
	var events []pgstore.Event
	for rows.Next() {
		var event pgstore.Event
		if err := rows.Scan(
			&event.ID, &event.SessionID, &event.Seq, &event.Type,
			&event.Payload, &event.TurnEventID, &event.CreatedAt,
			&event.ProcessedAt, &event.ThreadID,
		); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// TurnCompletion reports the committed output of a turn and whether this call
// actually performed the commit (Applied) or replayed an already-processed turn.
type TurnCompletion struct {
	Session domain.Session
	Events  []domain.Event
	Applied bool
	Parked  bool
	// ThreadStatus is set by child completions so the independent Workflow
	// stops when its owner terminates even while the aggregate Session remains
	// idle or running because of another Thread.
	ThreadStatus domain.Status
}

// AppendWorkflowEvents commits a completed, non-terminal prefix of one active
// turn without processing its trigger or changing the Session projection. It
// is idempotent by explicit event ID so a retried Temporal Activity either
// observes the whole existing batch or appends the whole batch once.
func (s *Store) AppendWorkflowEvents(
	ctx context.Context,
	sessionID string,
	triggerEventID string,
	drafts []domain.EventDraft,
) error {
	if sessionID == "" || triggerEventID == "" || len(drafts) == 0 {
		return domain.Validation("session, trigger, and workflow events are required")
	}
	seen := make(map[string]struct{}, len(drafts))
	for _, draft := range drafts {
		if draft.ID == "" {
			return domain.Validation("workflow progress event id is required")
		}
		if _, duplicate := seen[draft.ID]; duplicate {
			return domain.Validation("duplicate workflow progress event id")
		}
		seen[draft.ID] = struct{}{}
		if domain.IsClientSubmittable(draft.Type) || isSessionProjectionEvent(draft.Type) {
			return domain.Validation("workflow progress must contain non-terminal server events")
		}
	}

	applied := false
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
		if session.Status != domain.StatusRunning {
			return domain.Conflict("workflow progress requires a running session")
		}
		trigger, err := q.GetEvent(ctx, pgstore.GetEventParams{
			SessionID: sessionID,
			ID:        triggerEventID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("trigger event not found")
		} else if err != nil {
			return err
		}

		existing := 0
		for _, draft := range drafts {
			event, err := q.GetEvent(ctx, pgstore.GetEventParams{
				SessionID: sessionID,
				ID:        draft.ID,
			})
			switch {
			case err == nil:
				if !workflowEventMatches(event, triggerEventID, draft) {
					return domain.Conflict("workflow progress event id already has different content")
				}
				existing++
			case errors.Is(err, pgx.ErrNoRows):
				// The batch is either wholly absent or wholly present because its
				// first application uses this same transaction and Session lock.
			default:
				return err
			}
		}
		if existing == len(drafts) {
			return nil
		}
		if existing != 0 {
			return domain.Conflict("workflow progress batch is only partially present")
		}

		maxSeq, err := q.MaxEventSeq(ctx, sessionID)
		if err != nil {
			return err
		}
		if _, _, err := s.appendThreadDrafts(
			ctx,
			q,
			sessionID,
			trigger.ThreadID,
			drafts,
			maxSeq,
			&triggerEventID,
		); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err != nil {
		return err
	}
	if applied {
		s.notifySession(ctx, sessionID)
	}
	return nil
}

// RecordWorkflowRetry atomically appends the documented retrying error and
// status_rescheduled event while moving the Session projection to
// rescheduling. Explicit event IDs make an Activity retry replay-safe.
func (s *Store) RecordWorkflowRetry(
	ctx context.Context,
	sessionID string,
	triggerEventID string,
	errorEventID string,
	statusEventID string,
	errorPayload map[string]any,
) error {
	if sessionID == "" || triggerEventID == "" || errorEventID == "" ||
		statusEventID == "" || errorEventID == statusEventID {
		return domain.Validation("session, trigger, and distinct retry event ids are required")
	}
	drafts := []domain.EventDraft{
		{ID: errorEventID, Type: domain.EvSessionError, Payload: map[string]any{"error": errorPayload}},
		{ID: statusEventID, Type: domain.EvSessionStatusRescheduling, Payload: map[string]any{}},
	}
	applied := false
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		session, err := s.lockWorkflowTrigger(ctx, q, sessionID, triggerEventID)
		if err != nil {
			return err
		}
		if session.AgentSnapshot.Multiagent != nil {
			err := s.recordPrimaryThreadWorkflowRetryLocked(
				ctx, tx, q, &session, triggerEventID,
				errorEventID, statusEventID, errorPayload,
			)
			applied = err == nil
			return err
		}
		existing, err := workflowDraftsExisting(ctx, q, sessionID, triggerEventID, drafts)
		if err != nil || existing {
			return err
		}
		if session.Status != domain.StatusRunning {
			return domain.Conflict("workflow retry requires a running session")
		}
		maxSeq, err := q.MaxEventSeq(ctx, sessionID)
		if err != nil {
			return err
		}
		if _, _, err := s.appendDrafts(ctx, q, sessionID, drafts, maxSeq, &triggerEventID); err != nil {
			return err
		}
		now := s.clock.Now().UTC()
		session.TransitionStatus(domain.StatusRescheduling, now)
		if err := s.putProjection(ctx, q, session); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err != nil {
		return err
	}
	if applied {
		s.notifySession(ctx, sessionID)
	}
	return nil
}

// ResumeWorkflowRetry publishes status_running together with the projection
// transition immediately before the Workflow schedules the next model call.
func (s *Store) ResumeWorkflowRetry(
	ctx context.Context,
	sessionID string,
	triggerEventID string,
	statusEventID string,
) error {
	if sessionID == "" || triggerEventID == "" || statusEventID == "" {
		return domain.Validation("session, trigger, and retry status event id are required")
	}
	draft := domain.EventDraft{
		ID: statusEventID, Type: domain.EvSessionStatusRunning, Payload: map[string]any{},
	}
	applied := false
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		session, err := s.lockWorkflowTrigger(ctx, q, sessionID, triggerEventID)
		if err != nil {
			return err
		}
		if session.AgentSnapshot.Multiagent != nil {
			err := s.resumePrimaryThreadWorkflowRetryLocked(
				ctx, tx, q, &session, triggerEventID, statusEventID,
			)
			applied = err == nil
			return err
		}
		existing, err := workflowDraftsExisting(
			ctx, q, sessionID, triggerEventID, []domain.EventDraft{draft},
		)
		if err != nil || existing {
			return err
		}
		if session.Status != domain.StatusRescheduling {
			return domain.Conflict("workflow retry resume requires a rescheduling session")
		}
		maxSeq, err := q.MaxEventSeq(ctx, sessionID)
		if err != nil {
			return err
		}
		if _, _, err := s.appendDrafts(
			ctx, q, sessionID, []domain.EventDraft{draft}, maxSeq, &triggerEventID,
		); err != nil {
			return err
		}
		now := s.clock.Now().UTC()
		session.TransitionStatus(domain.StatusRunning, now)
		if err := s.putProjection(ctx, q, session); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err != nil {
		return err
	}
	if applied {
		s.notifySession(ctx, sessionID)
	}
	return nil
}

// recordPrimaryThreadWorkflowRetryLocked keeps retry ownership on the
// coordinator Thread. A running child keeps the aggregate Session running even
// while the coordinator itself is rescheduling.
func (s *Store) recordPrimaryThreadWorkflowRetryLocked(
	ctx context.Context,
	tx pgx.Tx,
	q *pgstore.Queries,
	session *domain.Session,
	triggerEventID string,
	errorEventID string,
	statusEventID string,
	errorPayload map[string]any,
) error {
	errorDraft := domain.EventDraft{
		ID: errorEventID, Type: domain.EvSessionError,
		Payload: map[string]any{"error": errorPayload},
	}
	existing, err := workflowDraftsExisting(
		ctx, q, session.ID, triggerEventID, []domain.EventDraft{errorDraft},
	)
	if err != nil || existing {
		return err
	}
	primaryID, err := q.GetPrimarySessionThreadID(ctx, session.ID)
	if err != nil {
		return err
	}
	primary, err := loadSessionThreadForUpdate(ctx, tx, session.ID, primaryID)
	if err != nil {
		return err
	}
	trigger, err := q.GetEvent(ctx, pgstore.GetEventParams{
		SessionID: session.ID, ID: triggerEventID,
	})
	if err != nil {
		return err
	}
	if trigger.ThreadID != primary.ID {
		return domain.Conflict("coordinator retry trigger belongs to another Thread")
	}
	if primary.Status != domain.StatusRunning {
		return domain.Conflict("coordinator retry requires a running primary Thread")
	}

	now := s.clock.Now().UTC()
	primary.TransitionStatus(domain.StatusRescheduling, now)
	if err := putSessionThreadTx(ctx, tx, primary); err != nil {
		return err
	}
	aggregated, err := aggregateSessionThreadStatus(ctx, tx, session.ID)
	if err != nil {
		return err
	}
	drafts := []domain.EventDraft{errorDraft}
	if aggregated == domain.StatusRescheduling {
		drafts = append(drafts, domain.EventDraft{
			ID: statusEventID, Type: domain.EvSessionStatusRescheduling,
			Payload: map[string]any{},
		})
	}
	maxSeq, err := q.MaxEventSeq(ctx, session.ID)
	if err != nil {
		return err
	}
	if _, _, err := s.appendThreadDrafts(
		ctx, q, session.ID, primary.ID, drafts, maxSeq, &triggerEventID,
	); err != nil {
		return err
	}
	if session.Status != aggregated {
		session.TransitionStatus(aggregated, now)
		if err := putSessionOnlyTx(ctx, tx, *session); err != nil {
			return err
		}
	}
	return nil
}

// resumePrimaryThreadWorkflowRetryLocked returns only the coordinator Thread
// to running. The Session emits status_running only when its aggregate
// projection actually changes.
func (s *Store) resumePrimaryThreadWorkflowRetryLocked(
	ctx context.Context,
	tx pgx.Tx,
	q *pgstore.Queries,
	session *domain.Session,
	triggerEventID string,
	statusEventID string,
) error {
	primaryID, err := q.GetPrimarySessionThreadID(ctx, session.ID)
	if err != nil {
		return err
	}
	primary, err := loadSessionThreadForUpdate(ctx, tx, session.ID, primaryID)
	if err != nil {
		return err
	}
	trigger, err := q.GetEvent(ctx, pgstore.GetEventParams{
		SessionID: session.ID, ID: triggerEventID,
	})
	if err != nil {
		return err
	}
	if trigger.ThreadID != primary.ID {
		return domain.Conflict("coordinator retry trigger belongs to another Thread")
	}
	// The primary projection is the durable idempotency marker when another
	// running child means no aggregate Session status event is necessary.
	if primary.Status == domain.StatusRunning {
		return nil
	}
	if primary.Status != domain.StatusRescheduling {
		return domain.Conflict("coordinator retry resume requires a rescheduling primary Thread")
	}

	now := s.clock.Now().UTC()
	primary.TransitionStatus(domain.StatusRunning, now)
	if err := putSessionThreadTx(ctx, tx, primary); err != nil {
		return err
	}
	aggregated, err := aggregateSessionThreadStatus(ctx, tx, session.ID)
	if err != nil {
		return err
	}
	if session.Status == aggregated {
		return nil
	}
	maxSeq, err := q.MaxEventSeq(ctx, session.ID)
	if err != nil {
		return err
	}
	if _, _, err := s.appendThreadDrafts(
		ctx, q, session.ID, primary.ID,
		[]domain.EventDraft{{
			ID: statusEventID, Type: domain.EvSessionStatusRunning,
			Payload: map[string]any{},
		}}, maxSeq, &triggerEventID,
	); err != nil {
		return err
	}
	session.TransitionStatus(aggregated, now)
	return putSessionOnlyTx(ctx, tx, *session)
}

func (s *Store) lockWorkflowTrigger(
	ctx context.Context,
	q *pgstore.Queries,
	sessionID string,
	triggerEventID string,
) (domain.Session, error) {
	row, err := q.LockSession(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Session{}, domain.NotFound("session not found")
	}
	if err != nil {
		return domain.Session{}, err
	}
	if row.DeletingAt.Valid {
		return domain.Session{}, domain.Conflict("session deletion is in progress")
	}
	session, err := sessionFromLockRow(row)
	if err != nil {
		return domain.Session{}, err
	}
	trigger, err := q.GetEvent(ctx, pgstore.GetEventParams{SessionID: sessionID, ID: triggerEventID})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Session{}, domain.NotFound("trigger event not found")
	}
	if err != nil {
		return domain.Session{}, err
	}
	if trigger.ProcessedAt.Valid {
		return domain.Session{}, domain.Conflict("workflow trigger is already processed")
	}
	return session, nil
}

// workflowDraftsExisting validates an idempotent retry batch. It returns true
// only when every event already exists with exactly the expected turn and
// payload; a partial or conflicting batch is rejected.
func workflowDraftsExisting(
	ctx context.Context,
	q *pgstore.Queries,
	sessionID string,
	triggerEventID string,
	drafts []domain.EventDraft,
) (bool, error) {
	existing := 0
	for _, draft := range drafts {
		event, err := q.GetEvent(ctx, pgstore.GetEventParams{SessionID: sessionID, ID: draft.ID})
		switch {
		case err == nil:
			if !workflowEventMatches(event, triggerEventID, draft) {
				return false, domain.Conflict("workflow retry event id already has different content")
			}
			existing++
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return false, err
		}
	}
	if existing != 0 && existing != len(drafts) {
		return false, domain.Conflict("workflow retry event batch is only partially present")
	}
	return existing == len(drafts), nil
}

func isSessionProjectionEvent(eventType string) bool {
	switch eventType {
	case domain.EvSessionStatusIdle,
		domain.EvSessionStatusRunning,
		domain.EvSessionStatusRescheduling,
		domain.EvSessionStatusTerminated,
		domain.EvSessionUpdated,
		domain.EvSessionUsage,
		domain.EvSessionDeleted:
		return true
	default:
		return false
	}
}

func workflowEventMatches(
	event pgstore.Event,
	triggerEventID string,
	draft domain.EventDraft,
) bool {
	if event.Type != draft.Type || event.TurnEventID == nil ||
		*event.TurnEventID != triggerEventID {
		return false
	}
	var existing map[string]any
	if err := json.Unmarshal(event.Payload, &existing); err != nil {
		return false
	}
	expected := draft.Payload
	if expected == nil {
		expected = map[string]any{}
	}
	existingJSON, err := json.Marshal(existing)
	if err != nil {
		return false
	}
	expectedJSON, err := json.Marshal(expected)
	return err == nil && string(existingJSON) == string(expectedJSON)
}

// CompleteTurn atomically commits the authoritative output of one turn: it
// appends the runtime's output events (tagged with the trigger id), marks the
// trigger event processed, and updates the session projection to status. It is
// idempotent — the required property for a Temporal Activity, which may run more
// than once: a retry that finds the trigger already processed replays the exact
// events the first commit wrote instead of appending a second copy.
func (s *Store) CompleteTurn(
	ctx context.Context,
	sessionID string,
	triggerEventID string,
	outputDrafts []domain.EventDraft,
	status domain.Status,
) (TurnCompletion, error) {
	return s.completeTurn(ctx, sessionID, triggerEventID, outputDrafts, status, "", "", nil, nil, nil, nil, nil, domain.TokenUsage{})
}

// CompleteWorkflowTurn atomically finalizes a Workflow-owned tool attempt (when
// present), commits the public turn output, and persists any client-action wait
// rows. Keeping those mutations in one PostgreSQL transaction closes the crash
// windows between "attempt completed", "requires_action published", "wait
// durable", and "trigger processed"; a retried Activity either applies the
// whole transition or observes the already-processed trigger.
func (s *Store) CompleteWorkflowTurn(
	ctx context.Context,
	sessionID string,
	triggerEventID string,
	outputDrafts []domain.EventDraft,
	status domain.Status,
	attemptID string,
	attemptState domain.RunAttemptState,
	attemptError *string,
	pendingActionEventIDs []string,
	resolutionEventIDs []string,
) (TurnCompletion, error) {
	return s.CompleteWorkflowTurnWithUsage(
		ctx,
		sessionID,
		triggerEventID,
		outputDrafts,
		status,
		attemptID,
		attemptState,
		attemptError,
		pendingActionEventIDs,
		resolutionEventIDs,
		domain.TokenUsage{},
	)
}

// CompleteWorkflowTurnWithUsage extends CompleteWorkflowTurn with the token
// usage observed while producing the turn. The usage is committed in the same
// transaction as the public events, so Activity retries cannot double-count it.
func (s *Store) CompleteWorkflowTurnWithUsage(
	ctx context.Context,
	sessionID string,
	triggerEventID string,
	outputDrafts []domain.EventDraft,
	status domain.Status,
	attemptID string,
	attemptState domain.RunAttemptState,
	attemptError *string,
	pendingActionEventIDs []string,
	resolutionEventIDs []string,
	usage domain.TokenUsage,
) (TurnCompletion, error) {
	if attemptID == "" {
		if attemptState != "" || attemptError != nil {
			return TurnCompletion{}, domain.Validation("attempt state requires an attempt id")
		}
	} else if err := validateAttemptFinish(attemptState, attemptError); err != nil {
		return TurnCompletion{}, err
	}
	return s.completeTurn(
		ctx,
		sessionID,
		triggerEventID,
		outputDrafts,
		status,
		attemptID,
		attemptState,
		attemptError,
		pendingActionEventIDs,
		resolutionEventIDs,
		nil,
		nil,
		usage,
	)
}

// CompleteWorkflowTurnWithTranscript extends CompleteWorkflowTurn with the
// provider-private model-continuation delta and public/provider tool-id
// mappings. All records commit atomically with the public events.
func (s *Store) CompleteWorkflowTurnWithTranscript(
	ctx context.Context,
	sessionID string,
	triggerEventID string,
	outputDrafts []domain.EventDraft,
	status domain.Status,
	attemptID string,
	attemptState domain.RunAttemptState,
	attemptError *string,
	pendingActionEventIDs []string,
	resolutionEventIDs []string,
	transcriptDelta []domain.Message,
	toolUseMappings []domain.ProviderToolUseMapping,
) (TurnCompletion, error) {
	return s.CompleteWorkflowTurnWithTranscriptAndUsage(
		ctx,
		sessionID,
		triggerEventID,
		outputDrafts,
		status,
		attemptID,
		attemptState,
		attemptError,
		pendingActionEventIDs,
		resolutionEventIDs,
		transcriptDelta,
		toolUseMappings,
		domain.TokenUsage{},
	)
}

// CompleteWorkflowTurnWithTranscriptAndUsage atomically commits the public
// output, private provider transcript, tool-id mappings, and token usage.
func (s *Store) CompleteWorkflowTurnWithTranscriptAndUsage(
	ctx context.Context,
	sessionID string,
	triggerEventID string,
	outputDrafts []domain.EventDraft,
	status domain.Status,
	attemptID string,
	attemptState domain.RunAttemptState,
	attemptError *string,
	pendingActionEventIDs []string,
	resolutionEventIDs []string,
	transcriptDelta []domain.Message,
	toolUseMappings []domain.ProviderToolUseMapping,
	usage domain.TokenUsage,
) (TurnCompletion, error) {
	if attemptID == "" {
		if attemptState != "" || attemptError != nil {
			return TurnCompletion{}, domain.Validation("attempt state requires an attempt id")
		}
	} else if err := validateAttemptFinish(attemptState, attemptError); err != nil {
		return TurnCompletion{}, err
	}
	return s.completeTurn(
		ctx,
		sessionID,
		triggerEventID,
		outputDrafts,
		status,
		attemptID,
		attemptState,
		attemptError,
		pendingActionEventIDs,
		resolutionEventIDs,
		transcriptDelta,
		toolUseMappings,
		usage,
	)
}

func (s *Store) completeTurn(
	ctx context.Context,
	sessionID string,
	triggerEventID string,
	outputDrafts []domain.EventDraft,
	status domain.Status,
	attemptID string,
	attemptState domain.RunAttemptState,
	attemptError *string,
	pendingActionEventIDs []string,
	resolutionEventIDs []string,
	transcriptDelta []domain.Message,
	toolUseMappings []domain.ProviderToolUseMapping,
	usage domain.TokenUsage,
) (TurnCompletion, error) {
	var result TurnCompletion
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
		var primaryThread domain.SessionThread
		independentPrimary := session.AgentSnapshot.Multiagent != nil
		if independentPrimary {
			body, err := q.GetPrimarySessionThreadProjection(ctx, sessionID)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(body, &primaryThread); err != nil {
				return err
			}
		}

		trigger, err := q.GetEvent(ctx, pgstore.GetEventParams{SessionID: sessionID, ID: triggerEventID})
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("trigger event not found")
		}
		if err != nil {
			return err
		}
		var triggerPayload map[string]any
		if err := json.Unmarshal(trigger.Payload, &triggerPayload); err != nil {
			return err
		}
		// Idempotent replay: a trigger already stamped processed normally means
		// this turn's completion committed. There are two receipt-processed
		// exceptions whose actual turn still has to run:
		//   - a claimed pending-action resolution, disambiguated by its unresolved
		//     pending row; and
		//   - the define_outcome event that owns the Session's active outcome.
		// Once that outcome becomes terminal ActiveOutcome returns nil, so a retry
		// correctly replays the already-committed turn instead of applying it twice.
		if trigger.ProcessedAt.Valid {
			pendingResume, err := q.IsUnresolvedPendingResolution(
				ctx,
				pgstore.IsUnresolvedPendingResolutionParams{
					SessionID:        sessionID,
					ThreadID:         trigger.ThreadID,
					ResolvingEventID: &triggerEventID,
				},
			)
			if err != nil {
				return err
			}
			if !pendingResume && !activeReceiptProcessedOutcome(session, trigger.Type, triggerPayload) {
				priorRows, err := q.ListEventsByTurn(ctx, pgstore.ListEventsByTurnParams{
					SessionID:   sessionID,
					TurnEventID: &triggerEventID,
				})
				if err != nil {
					return err
				}
				prior, err := eventsFromRows(priorRows)
				if err != nil {
					return err
				}
				result = TurnCompletion{
					Session: session,
					Events:  prior,
					Applied: false,
					Parked:  turnEventsParked(prior),
				}
				return nil
			}
		}

		// Admission and completion both hold this session row lock. Therefore the
		// earliest unprocessed interrupt after the trigger is the exact
		// finish-vs-interrupt linearization result: if it is visible now,
		// interrupt admission committed first; if not, this completion commits
		// first and any later interrupt is an idle control event.
		var interrupt *domain.Event
		if trigger.Type != domain.EvUserInterrupt {
			row, err := q.FirstUnprocessedInterruptAfter(
				ctx,
				pgstore.FirstUnprocessedInterruptAfterParams{
					SessionID: sessionID,
					AfterSeq:  trigger.Seq,
				},
			)
			switch {
			case err == nil:
				event, err := eventFromRow(row)
				if err != nil {
					return err
				}
				interrupt = &event
			case errors.Is(err, pgx.ErrNoRows):
				// Normal completion won.
			default:
				return err
			}
		}
		if interrupt != nil {
			// Keep already-produced authoritative agent/tool drafts, but terminal
			// ownership moves to the interrupt. It contributes the single public
			// idle/end_turn and never publishes requires_action, session.error, or
			// terminated. A named attempt is fenced as interrupted below.
			var interruptedOutcomeIteration int
			var interruptedOutcomeStartID string
			outputDrafts, interruptedOutcomeIteration, interruptedOutcomeStartID =
				interruptedTurnDrafts(outputDrafts)
			transcriptDelta = closeInterruptedProviderTranscript(
				transcriptDelta,
			)
			toolUseMappings = retainCommittedProviderMappings(
				toolUseMappings,
				outputDrafts,
			)
			status = domain.StatusIdle
			pendingActionEventIDs = nil
			if attemptID != "" {
				attemptState = domain.RunAttemptInterrupted
				attemptError = nil
			}
			// Outcome admission updates the Session projection before its queued
			// evaluation turn starts. An interrupt that belongs to some earlier
			// ordinary turn must not terminate that unrelated active outcome.
			if activeReceiptProcessedOutcome(session, trigger.Type, triggerPayload) {
				outcome := session.ActiveOutcome()
				iteration := max(outcome.Iteration, interruptedOutcomeIteration)
				outputDrafts = append(outputDrafts,
					domain.EventDraft{
						Type: domain.EvSpanOutcomeEvaluationEnd,
						Payload: map[string]any{
							// Correlate an interrupted evaluation when its start was
							// published; otherwise the official contract uses an empty ID.
							"outcome_evaluation_start_id": interruptedOutcomeStartID,
							"outcome_id":                  outcome.OutcomeID,
							"result":                      "interrupted",
							"explanation":                 "Outcome work was interrupted by the user.",
							"iteration":                   iteration,
							"usage": map[string]any{
								"cache_creation_input_tokens": 0,
								"cache_read_input_tokens":     0,
								"input_tokens":                0,
								"output_tokens":               0,
								"speed":                       nil,
							},
						},
					},
				)
			}
			outputDrafts = append(outputDrafts, domain.EventDraft{
				Type: domain.EvSessionStatusIdle,
				Payload: map[string]any{
					"stop_reason": map[string]any{"type": "end_turn"},
				},
			})
		}

		if err := validatePendingCompletion(
			status,
			outputDrafts,
			pendingActionEventIDs,
		); err != nil {
			return err
		}
		if attemptID != "" {
			if err := s.finishAttemptLocked(
				ctx,
				q,
				attemptID,
				attemptState,
				attemptError,
				sessionID,
				triggerEventID,
			); err != nil {
				return err
			}
		}

		// Resolutions keep their pending rows unresolved while the resume turn is
		// in flight. Clear the complete barrier only inside this completion
		// transaction; if it rolls back, ordinary queued work remains gated.
		resolvedPending, err := s.resolvePendingBarrierLocked(
			ctx,
			q,
			sessionID,
			trigger.ThreadID,
			triggerEventID,
			resolutionEventIDs,
		)
		if err != nil {
			return err
		}
		hasUnresolved, err := q.HasUnresolvedPendingActions(
			ctx,
			pgstore.HasUnresolvedPendingActionsParams{
				SessionID: sessionID, ThreadID: trigger.ThreadID,
			},
		)
		if err != nil {
			return err
		}
		gatedAfterCompletion := hasUnresolved || len(pendingActionEventIDs) > 0

		maxSeq, err := q.MaxEventSeq(ctx, sessionID)
		if err != nil {
			return err
		}

		// Intermediate-idle suppression: when this is an ordinary end_turn
		// completion (status idle) but more user.message triggers are still
		// unprocessed — e.g. a batch A,B where A finishes while B is queued — the
		// session must NOT flip to idle between turns. Doing so would emit a
		// spurious public session.status_idle and momentarily lie about the session
		// being done. Keep it running and drop the terminal idle draft; only the
		// last turn's completion (no remaining unprocessed user.message) idles the
		// session. A terminated status is never softened, and a non-idle status
		// (e.g. still running by the caller's choice) is left as-is.
		effectiveStatus := status
		drafts := outputDrafts
		retriesExhausted := turnRetriesExhausted(outputDrafts)
		var remaining int32
		if interrupt != nil {
			remaining, err = q.CountUnprocessedUserMessages(
				ctx,
				pgstore.CountUnprocessedUserMessagesParams{
					SessionID: sessionID,
					ExcludeID: triggerEventID,
				},
			)
			if err != nil {
				return err
			}
			if remaining > 0 && !gatedAfterCompletion {
				// The interrupted turn must visibly hand control back with idle,
				// even when a redirect is already queued. Reopen immediately after
				// that boundary so the projection stays truthful while the next
				// message runs.
				effectiveStatus = domain.StatusRunning
				drafts = append(drafts, domain.EventDraft{
					Type:    domain.EvSessionStatusRunning,
					Payload: map[string]any{},
				})
			}
		} else if status == domain.StatusIdle && !gatedAfterCompletion && !retriesExhausted {
			remaining, err = q.CountUnprocessedUserMessages(ctx, pgstore.CountUnprocessedUserMessagesParams{
				SessionID: sessionID,
				ExcludeID: triggerEventID,
			})
			if err != nil {
				return err
			}
			if remaining > 0 {
				effectiveStatus = domain.StatusRunning
				drafts = withoutTerminalIdle(outputDrafts)
			}
		}

		ownerStatus := effectiveStatus
		if independentPrimary && effectiveStatus == domain.StatusIdle {
			activeChildren, err := q.HasActiveChildThreads(ctx, sessionID)
			if err != nil {
				return err
			}
			if activeChildren {
				effectiveStatus = domain.StatusRunning
				drafts = withoutTerminalIdle(drafts)
			}
		}
		completionTime := s.clock.Now().UTC()
		if effectiveStatus == domain.StatusIdle {
			sessionPendingIDs, err := q.SessionPendingClientActionEventIDs(
				ctx, sessionID,
			)
			if err != nil {
				return err
			}
			sessionPendingIDs = appendUniqueStrings(
				sessionPendingIDs, pendingActionEventIDs...,
			)
			if len(sessionPendingIDs) > 0 {
				drafts = rewriteTerminalSessionIdleStopReason(
					drafts,
					map[string]any{
						"type":      "requires_action",
						"event_ids": sessionPendingIDs,
					},
				)
			} else if session.BudgetReached(completionTime) {
				drafts = rewriteTerminalSessionIdleStopReason(
					drafts,
					map[string]any{"type": "budget_reached"},
				)
				drafts = insertUsageBeforeTerminalIdle(
					drafts,
					domain.EventDraft{
						Type:    domain.EvSessionUsage,
						Payload: session.UsageEventPayload(completionTime),
					},
				)
			}
		}

		events, finalMaxSeq, err := s.appendDraftsAt(
			ctx,
			q,
			sessionID,
			drafts,
			maxSeq,
			&triggerEventID,
			completionTime,
		)
		if err != nil {
			return err
		}
		if independentPrimary && effectiveStatus == domain.StatusTerminated {
			terminatedEvents, next, err := s.terminateChildSessionThreadsLocked(
				ctx, tx, q, sessionID, primaryThread.ID,
				triggerEventID, finalMaxSeq, completionTime,
			)
			if err != nil {
				return err
			}
			events = append(events, terminatedEvents...)
			finalMaxSeq = next
		}
		allowedActions := make(map[string]domain.Event, len(events))
		for _, event := range events {
			allowedActions[event.ID] = event
		}
		if err := s.insertPendingActionsLocked(
			ctx,
			q,
			sessionID,
			trigger.ThreadID,
			pendingActionEventIDs,
			allowedActions,
			nil,
		); err != nil {
			return err
		}
		now := completionTime
		if transcriptDelta != nil {
			representedEventIDs := resolutionEventIDs
			if len(representedEventIDs) == 0 {
				representedEventIDs = []string{triggerEventID}
			}
			representedJSON, err := json.Marshal(representedEventIDs)
			if err != nil {
				return err
			}
			messagesJSON, err := json.Marshal(transcriptDelta)
			if err != nil {
				return err
			}
			mappingsJSON, err := json.Marshal(toolUseMappings)
			if err != nil {
				return err
			}
			if err := q.InsertProviderTranscriptTurn(
				ctx,
				pgstore.InsertProviderTranscriptTurnParams{
					SessionID:           sessionID,
					TriggerEventID:      triggerEventID,
					CommittedThroughSeq: finalMaxSeq,
					RepresentedEventIds: representedJSON,
					Messages:            messagesJSON,
					ToolUseMappings:     mappingsJSON,
					CreatedAt:           tsUTC(now),
				},
			); err != nil {
				return err
			}
		}
		extraProcessedIDs := []string(nil)
		if interrupt != nil {
			extraProcessedIDs = append(extraProcessedIDs, interrupt.ID)
		}
		processedIDs, err := turnProcessedEventIDs(
			ctx, q, sessionID, triggerEventID, triggerPayload,
			resolutionEventIDs, extraProcessedIDs...,
		)
		if err != nil {
			return err
		}
		for _, eventID := range processedIDs {
			if err := q.MarkEventProcessed(ctx, pgstore.MarkEventProcessedParams{
				ProcessedAt: tsUTC(now),
				SessionID:   sessionID,
				ID:          eventID,
			}); err != nil {
				return err
			}
		}
		if retriesExhausted {
			if err := q.FlushQueuedUserMessages(
				ctx,
				pgstore.FlushQueuedUserMessagesParams{
					SessionID:   sessionID,
					ExcludeID:   triggerEventID,
					ProcessedAt: tsUTC(now),
				},
			); err != nil {
				return err
			}
		}
		session.Usage.Add(usage)
		applyOutcomeResults(&session, drafts, now)
		session.TransitionStatus(effectiveStatus, now)
		if independentPrimary {
			primaryThread.Usage.Add(usage)
			primaryThread.TransitionStatus(ownerStatus, now)
			if err := putPrimaryThreadProjection(ctx, q, primaryThread); err != nil {
				return err
			}
			body, err := json.Marshal(session)
			if err != nil {
				return err
			}
			if err := q.UpdateSessionStatus(ctx, pgstore.UpdateSessionStatusParams{
				Status: string(session.Status), Body: body,
				UpdatedAt: tsUTC(session.UpdatedAt), ID: session.ID,
			}); err != nil {
				return err
			}
		} else if err := s.putProjection(ctx, q, session); err != nil {
			return err
		}
		// Messages admitted while the barrier was open intentionally wrote no
		// wakeup. When this transaction clears the last row and exposes queued
		// ordinary work, enqueue a fresh durable wakeup in the same commit. Without
		// it a message racing after the current Workflow drain could leave a
		// truthful running projection with no future signal.
		if resolvedPending &&
			!gatedAfterCompletion &&
			effectiveStatus == domain.StatusRunning &&
			remaining > 0 {
			if err := q.UpsertOutbox(ctx, pgstore.UpsertOutboxParams{
				SessionID:   sessionID,
				MaxEventSeq: finalMaxSeq,
				EnqueuedAt:  tsUTC(now),
			}); err != nil {
				return err
			}
		}
		result = TurnCompletion{
			Session: session,
			Events:  events,
			Applied: true,
			Parked:  len(pendingActionEventIDs) > 0,
		}
		return nil
	})
	if err != nil {
		return TurnCompletion{}, err
	}
	s.notifySession(ctx, sessionID)
	return result, nil
}

func activeReceiptProcessedOutcome(
	session domain.Session,
	triggerType string,
	triggerPayload map[string]any,
) bool {
	if triggerType != domain.EvUserDefineOutcome {
		return false
	}
	outcomeID, _ := triggerPayload["outcome_id"].(string)
	active := session.ActiveOutcome()
	return outcomeID != "" && active != nil && active.OutcomeID == outcomeID
}

func applyOutcomeResults(
	session *domain.Session,
	drafts []domain.EventDraft,
	now time.Time,
) {
	for _, draft := range drafts {
		if draft.Type != domain.EvSpanOutcomeEvaluationEnd {
			continue
		}
		outcomeID, _ := draft.Payload["outcome_id"].(string)
		result, _ := draft.Payload["result"].(string)
		explanation, _ := draft.Payload["explanation"].(string)
		iteration := intFromAny(draft.Payload["iteration"])
		session.ApplyOutcomeResult(outcomeID, result, explanation, iteration, now)
	}
}

func intFromAny(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int32:
		return int(typed)
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}

// withoutTerminalIdle returns drafts with any session.status_idle draft removed,
// keeping every other draft in order. Used when an intermediate turn must not
// publish an idle event because later user.message work is still queued.
func withoutTerminalIdle(drafts []domain.EventDraft) []domain.EventDraft {
	out := make([]domain.EventDraft, 0, len(drafts))
	for _, d := range drafts {
		if d.Type == domain.EvSessionStatusIdle {
			continue
		}
		out = append(out, d)
	}
	return out
}

func rewriteTerminalSessionIdleStopReason(
	drafts []domain.EventDraft,
	stopReason map[string]any,
) []domain.EventDraft {
	out := append([]domain.EventDraft(nil), drafts...)
	for index := len(out) - 1; index >= 0; index-- {
		if out[index].Type != domain.EvSessionStatusIdle {
			continue
		}
		payload := cloneEventPayload(out[index].Payload)
		payload["stop_reason"] = stopReason
		out[index].Payload = payload
		break
	}
	return out
}

func insertUsageBeforeTerminalIdle(
	drafts []domain.EventDraft,
	usage domain.EventDraft,
) []domain.EventDraft {
	out := make([]domain.EventDraft, 0, len(drafts)+1)
	inserted := false
	for _, draft := range drafts {
		if !inserted && draft.Type == domain.EvSessionStatusIdle {
			out = append(out, usage)
			inserted = true
		}
		out = append(out, draft)
	}
	if !inserted {
		out = append(out, usage)
	}
	return out
}

func appendUniqueStrings(values []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(additions))
	out := make([]string, 0, len(values)+len(additions))
	for _, value := range append(append([]string(nil), values...), additions...) {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// interruptedTurnDrafts preserves work that definitely completed before an
// interrupt while removing every competing terminal projection. The interrupt
// completion transaction appends the one authoritative idle/end_turn after
// this filtered prefix.
func interruptedTurnDrafts(drafts []domain.EventDraft) ([]domain.EventDraft, int, string) {
	completedToolUses := make(map[string]struct{})
	completedOutcomeStarts := make(map[string]struct{})
	interruptedOutcomeIteration := 0
	interruptedOutcomeStartID := ""
	for _, draft := range drafts {
		if startID, _ := draft.Payload[domain.InternalOutcomeEvaluationStart].(string); startID != "" {
			interruptedOutcomeStartID = startID
			interruptedOutcomeIteration = max(
				interruptedOutcomeIteration,
				intFromAny(draft.Payload[domain.InternalOutcomeIteration]),
			)
		}
		switch draft.Type {
		case domain.EvAgentToolResult, domain.EvAgentMcpToolResult:
			if toolUseID, _ := domain.AgentToolResultReference(
				draft.Type,
				draft.Payload,
			); toolUseID != "" {
				completedToolUses[toolUseID] = struct{}{}
			}
		case domain.EvSpanOutcomeEvaluationEnd:
			result, _ := draft.Payload["result"].(string)
			if result != "needs_revision" {
				continue
			}
			if startID, _ := draft.Payload["outcome_evaluation_start_id"].(string); startID != "" {
				completedOutcomeStarts[startID] = struct{}{}
			}
			interruptedOutcomeIteration = max(
				interruptedOutcomeIteration,
				intFromAny(draft.Payload["iteration"])+1,
			)
		}
	}

	out := make([]domain.EventDraft, 0, len(drafts))
	for _, draft := range drafts {
		switch draft.Type {
		case domain.EvSessionError,
			domain.EvSessionStatusIdle,
			domain.EvSessionStatusRescheduling,
			domain.EvSessionStatusTerminated,
			domain.EvSpanOutcomeEvaluationOngoing:
			continue
		case domain.EvSpanOutcomeEvaluationStart:
			if _, completed := completedOutcomeStarts[draft.ID]; !completed {
				continue
			}
		case domain.EvSpanOutcomeEvaluationEnd:
			if result, _ := draft.Payload["result"].(string); result != "needs_revision" {
				continue
			}
		case domain.EvAgentToolUse, domain.EvAgentCustomToolUse,
			domain.EvAgentMcpToolUse:
			// An interrupted turn cannot publish a new client-action wait. Keep a
			// tool-use only when its result durably completed in the same output;
			// otherwise the public ledger would contain an orphan call that cannot
			// be resumed and must not be executed.
			if _, completed := completedToolUses[draft.ID]; !completed {
				continue
			}
		}
		out = append(out, draft)
	}
	return out, interruptedOutcomeIteration, interruptedOutcomeStartID
}

func turnEventsParked(events []domain.Event) bool {
	for _, event := range events {
		if event.Type != domain.EvSessionStatusIdle &&
			event.Type != domain.EvSessionThreadStatusIdle {
			continue
		}
		stopReason, _ := event.Payload["stop_reason"].(map[string]any)
		if stopReason["type"] == "requires_action" {
			return true
		}
	}
	return false
}

func turnRetriesExhausted(drafts []domain.EventDraft) bool {
	for _, draft := range drafts {
		if draft.Type != domain.EvSessionStatusIdle {
			continue
		}
		stopReason, _ := draft.Payload["stop_reason"].(map[string]any)
		if stopReason["type"] == "retries_exhausted" {
			return true
		}
	}
	return false
}
