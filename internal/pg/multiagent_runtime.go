package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/agentruntime"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

const maxConcurrentSessionThreads = 25

// CoordinatorToolExecution is the result of one database-owned coordinator
// primitive. WakeThreadID is non-empty when the same transaction enqueued work
// for a persistent child Thread.
type CoordinatorToolExecution struct {
	Result       domain.ToolStepResult
	WakeThreadID string
}

// ExecuteCoordinatorToolStep completes a private coordinator tool step and all
// of its durable Thread/event effects in one PostgreSQL transaction. Unlike an
// external tool, this database-local operation can safely transition directly
// from prepared to completed: there is no state in which its side effect
// committed without its result.
func (s *Store) ExecuteCoordinatorToolStep(
	ctx context.Context,
	sessionID string,
	parentThreadID string,
	triggerEventID string,
	stepID string,
	toolName string,
	input map[string]any,
) (CoordinatorToolExecution, error) {
	var execution CoordinatorToolExecution
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

		step, err := lockCoordinatorToolStep(ctx, tx, stepID)
		if err != nil {
			return err
		}
		normalizedInput, err := normalizeCoordinatorInput(input)
		if err != nil {
			return err
		}
		if step.SessionID != sessionID || step.TriggerEventID != triggerEventID ||
			step.ToolName != toolName || !reflect.DeepEqual(step.Input, normalizedInput) {
			return domain.Conflict("coordinator tool step does not match this turn")
		}
		if step.State == domain.ToolStepCompleted {
			if step.Result == nil {
				return fmt.Errorf("pg: completed coordinator tool step %s has no result", stepID)
			}
			execution.Result = *step.Result
			execution.WakeThreadID = coordinatorWakeThreadID(*step.Result)
			return nil
		}
		if step.State != domain.ToolStepPrepared {
			return domain.Conflict("coordinator tool step is not replayable")
		}

		switch toolName {
		case agentruntime.ListAgentsToolName:
			if len(input) != 0 {
				execution.Result = coordinatorToolError("list_agents accepts no input fields")
			} else {
				execution.Result, err = s.listCoordinatorAgentsLocked(
					ctx, tx, session,
				)
			}
		case agentruntime.SendToAgentToolName:
			var parsed agentruntime.SendToAgentInput
			parsed, err = agentruntime.ParseSendToAgentInput(input)
			if err != nil {
				execution.Result = coordinatorToolError(err.Error())
				err = nil
				break
			}
			execution.Result, execution.WakeThreadID, err =
				s.executeSendToAgentLocked(
					ctx, tx, q, session, parentThreadID,
					triggerEventID, parsed,
				)
		default:
			return domain.Validation("unknown coordinator tool: " + toolName)
		}
		if err != nil {
			return err
		}
		resultJSON, err := json.Marshal(execution.Result)
		if err != nil {
			return err
		}
		now := s.clock.Now().UTC()
		command, err := tx.Exec(ctx, `
UPDATE tool_steps
SET state = 'completed', result = $2, finished_at = $3, updated_at = $3
WHERE id = $1 AND state = 'prepared'`, stepID, resultJSON, now)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return domain.Conflict("invalid coordinator tool step transition")
		}
		return nil
	})
	if err != nil {
		return CoordinatorToolExecution{}, err
	}
	if execution.WakeThreadID != "" {
		s.notifySession(ctx, sessionID)
	}
	return execution, nil
}

type lockedCoordinatorStep struct {
	SessionID      string
	TriggerEventID string
	ToolName       string
	Input          map[string]any
	State          domain.ToolStepState
	Result         *domain.ToolStepResult
}

func lockCoordinatorToolStep(
	ctx context.Context,
	tx pgx.Tx,
	stepID string,
) (lockedCoordinatorStep, error) {
	var (
		step         lockedCoordinatorStep
		inputJSON    []byte
		resultJSON   []byte
		attemptState string
	)
	err := tx.QueryRow(ctx, `
SELECT attempt.session_id, attempt.trigger_event_id, attempt.state,
       step.tool_name, step.input, step.state, step.result
FROM tool_steps AS step
JOIN turn_attempts AS attempt ON attempt.id = step.attempt_id
WHERE step.id = $1
FOR UPDATE OF attempt, step`, stepID).Scan(
		&step.SessionID, &step.TriggerEventID, &attemptState,
		&step.ToolName, &inputJSON, &step.State, &resultJSON,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedCoordinatorStep{}, domain.NotFound("tool step not found")
	}
	if err != nil {
		return lockedCoordinatorStep{}, err
	}
	if attemptState != string(domain.RunAttemptActive) {
		return lockedCoordinatorStep{}, domain.Conflict("coordinator tool requires an active attempt")
	}
	if err := json.Unmarshal(inputJSON, &step.Input); err != nil {
		return lockedCoordinatorStep{}, err
	}
	if len(resultJSON) > 0 {
		var result domain.ToolStepResult
		if err := json.Unmarshal(resultJSON, &result); err != nil {
			return lockedCoordinatorStep{}, err
		}
		step.Result = &result
	}
	return step, nil
}

func (s *Store) listCoordinatorAgentsLocked(
	ctx context.Context,
	tx pgx.Tx,
	session domain.Session,
) (domain.ToolStepResult, error) {
	if session.AgentSnapshot.Multiagent == nil {
		return coordinatorToolError("Session Agent is not a multiagent coordinator"), nil
	}
	type threadView struct {
		ID     string `json:"session_thread_id"`
		Status string `json:"status"`
	}
	type agentView struct {
		Name        string       `json:"name"`
		Description string       `json:"description,omitempty"`
		Model       string       `json:"model"`
		Threads     []threadView `json:"threads"`
	}
	views := make([]agentView, 0, len(session.MultiagentRoster))
	byName := make(map[string]int, len(session.MultiagentRoster))
	for _, member := range session.MultiagentRoster {
		description := ""
		if member.Description != nil {
			description = *member.Description
		}
		views = append(views, agentView{
			Name: member.Name, Description: description,
			Model: member.Model.ID, Threads: []threadView{},
		})
		byName[member.Name] = len(views) - 1
	}
	rows, err := tx.Query(ctx, `
SELECT body
FROM session_threads
WHERE session_id = $1 AND kind = 'child' AND archived_at IS NULL
ORDER BY created_at, id`, session.ID)
	if err != nil {
		return domain.ToolStepResult{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return domain.ToolStepResult{}, err
		}
		var thread domain.SessionThread
		if err := json.Unmarshal(body, &thread); err != nil {
			return domain.ToolStepResult{}, err
		}
		if index, ok := byName[thread.Agent.Name]; ok {
			views[index].Threads = append(views[index].Threads, threadView{
				ID: thread.ID, Status: string(thread.Status),
			})
		}
	}
	if err := rows.Err(); err != nil {
		return domain.ToolStepResult{}, err
	}
	return coordinatorToolJSON(map[string]any{"agents": views})
}

func (s *Store) executeSendToAgentLocked(
	ctx context.Context,
	tx pgx.Tx,
	q *pgstore.Queries,
	session domain.Session,
	parentThreadID string,
	triggerEventID string,
	input agentruntime.SendToAgentInput,
) (domain.ToolStepResult, string, error) {
	if session.ArchivedAt != nil || session.Status == domain.StatusTerminated {
		return domain.ToolStepResult{}, "", domain.Conflict("cannot delegate in a terminated Session")
	}
	if session.AgentSnapshot.Multiagent == nil || len(session.MultiagentRoster) == 0 {
		return coordinatorToolError("Session Agent is not a multiagent coordinator"), "", nil
	}
	var parentKind string
	if err := tx.QueryRow(ctx, `
SELECT kind FROM session_threads
WHERE session_id = $1 AND id = $2
FOR UPDATE`, session.ID, parentThreadID).Scan(&parentKind); errors.Is(err, pgx.ErrNoRows) {
		return domain.ToolStepResult{}, "", domain.NotFound("parent session thread not found")
	} else if err != nil {
		return domain.ToolStepResult{}, "", err
	}
	if parentKind != "primary" {
		return coordinatorToolError("child Threads cannot delegate to roster agents"), "", nil
	}
	trigger, err := q.GetEvent(ctx, pgstore.GetEventParams{
		SessionID: session.ID, ID: triggerEventID,
	})
	if err != nil {
		return domain.ToolStepResult{}, "", err
	}
	if trigger.ThreadID != parentThreadID {
		return domain.ToolStepResult{}, "", domain.Conflict("delegation trigger belongs to another Thread")
	}

	member, err := rosterMemberByName(session.MultiagentRoster, input.AgentName)
	if err != nil {
		return coordinatorToolError(err.Error()), "", nil
	}
	created := false
	var child domain.SessionThread
	if input.SessionThreadID == "" {
		var count int
		if err := tx.QueryRow(ctx, `
SELECT COUNT(*) FROM session_threads
WHERE session_id = $1 AND kind != 'advisor'
  AND archived_at IS NULL`, session.ID).Scan(&count); err != nil {
			return domain.ToolStepResult{}, "", err
		}
		if count >= maxConcurrentSessionThreads {
			return coordinatorToolError("Session has reached the 25 concurrent Thread limit"), "", nil
		}
		child = domain.NewChildSessionThread(
			s.ids.NewID(domain.PrefixSessionThread), session.ID,
			parentThreadID, member, s.clock.Now(),
		)
		created = true
	} else {
		child, err = loadSessionThreadForUpdate(
			ctx, tx, session.ID, input.SessionThreadID,
		)
		if err != nil {
			var domainErr *domain.DomainError
			if errors.As(err, &domainErr) && domainErr.Kind == domain.KindNotFound {
				return coordinatorToolError("session_thread_id does not name an existing child Thread"), "", nil
			}
			return domain.ToolStepResult{}, "", err
		}
		if child.ParentThreadID == nil || *child.ParentThreadID != parentThreadID ||
			child.Agent.Name != input.AgentName {
			return coordinatorToolError("session_thread_id does not belong to the named roster agent"), "", nil
		}
		if child.ArchivedAt != nil || child.Status == domain.StatusTerminated {
			return coordinatorToolError("cannot send to a terminated child Thread"), "", nil
		}
	}

	now := s.clock.Now().UTC()
	if created {
		if err := insertSessionThread(ctx, tx, child); err != nil {
			return domain.ToolStepResult{}, "", err
		}
	}
	maxSeq, err := q.MaxEventSeq(ctx, session.ID)
	if err != nil {
		return domain.ToolStepResult{}, "", err
	}
	turnID := triggerEventID
	if created {
		_, maxSeq, err = s.appendThreadDrafts(
			ctx, q, session.ID, parentThreadID,
			[]domain.EventDraft{{
				Type: domain.EvSessionThreadCreated,
				Payload: map[string]any{
					"agent_name":        input.AgentName,
					"session_thread_id": child.ID,
				},
			}}, maxSeq, &turnID,
		)
		if err != nil {
			return domain.ToolStepResult{}, "", err
		}
	}
	content := []any{map[string]any{"type": "text", "text": input.Message}}
	_, maxSeq, err = s.appendThreadDrafts(
		ctx, q, session.ID, parentThreadID,
		[]domain.EventDraft{{
			Type: domain.EvAgentThreadMessageSent,
			Payload: map[string]any{
				"to_session_thread_id": child.ID,
				"to_agent_name":        input.AgentName,
				"content":              content,
			},
		}}, maxSeq, &turnID,
	)
	if err != nil {
		return domain.ToolStepResult{}, "", err
	}
	_, maxSeq, err = s.appendThreadDrafts(
		ctx, q, session.ID, child.ID,
		[]domain.EventDraft{{
			Type: domain.EvAgentThreadMessageReceived,
			Payload: map[string]any{
				"from_session_thread_id":            parentThreadID,
				"from_agent_name":                   session.AgentSnapshot.Name,
				"content":                           content,
				domain.InternalOriginTriggerEventID: triggerEventID,
			},
		}}, maxSeq, nil,
	)
	if err != nil {
		return domain.ToolStepResult{}, "", err
	}
	if child.Status != domain.StatusRunning {
		child.TransitionStatus(domain.StatusRunning, now)
		status := domain.EventDraft{
			Type: domain.EvSessionThreadStatusRunning,
			Payload: map[string]any{
				"session_thread_id": child.ID,
				"agent_name":        input.AgentName,
			},
		}
		_, maxSeq, err = s.appendThreadDrafts(
			ctx, q, session.ID, child.ID,
			[]domain.EventDraft{status}, maxSeq, nil,
		)
		if err != nil {
			return domain.ToolStepResult{}, "", err
		}
		_, maxSeq, err = s.appendThreadDrafts(
			ctx, q, session.ID, parentThreadID,
			[]domain.EventDraft{status}, maxSeq, &turnID,
		)
		if err != nil {
			return domain.ToolStepResult{}, "", err
		}
		if err := putSessionThreadTx(ctx, tx, child); err != nil {
			return domain.ToolStepResult{}, "", err
		}
	}
	if session.Status != domain.StatusRunning {
		session.TransitionStatus(domain.StatusRunning, now)
		if err := putSessionOnlyTx(ctx, tx, session); err != nil {
			return domain.ToolStepResult{}, "", err
		}
	}
	if err := q.UpsertThreadOutbox(ctx, pgstore.UpsertThreadOutboxParams{
		SessionID: session.ID, ThreadID: child.ID,
		MaxEventSeq: maxSeq, EnqueuedAt: tsUTC(now),
	}); err != nil {
		return domain.ToolStepResult{}, "", err
	}
	result, err := coordinatorToolJSON(map[string]any{
		"status": "accepted", "agent_name": input.AgentName,
		"session_thread_id": child.ID, "created": created,
		"message": "The agent is running asynchronously; its report will arrive in a later turn.",
	})
	return result, child.ID, err
}

func rosterMemberByName(roster []domain.Agent, name string) (domain.Agent, error) {
	var found *domain.Agent
	for index := range roster {
		if roster[index].Name != name {
			continue
		}
		if found != nil {
			return domain.Agent{}, fmt.Errorf("agent name does not uniquely identify a roster member")
		}
		copy := roster[index]
		found = &copy
	}
	if found == nil {
		return domain.Agent{}, fmt.Errorf("agent name is not present in the Session roster")
	}
	return *found, nil
}

func coordinatorToolJSON(value any) (domain.ToolStepResult, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return domain.ToolStepResult{}, err
	}
	return domain.ToolStepResult{Content: []any{map[string]any{
		"type": "text", "text": string(encoded),
	}}}, nil
}

func coordinatorToolError(message string) domain.ToolStepResult {
	return domain.ToolStepResult{
		Content: []any{map[string]any{"type": "text", "text": message}},
		IsError: true,
	}
}

func coordinatorWakeThreadID(result domain.ToolStepResult) string {
	if result.IsError || len(result.Content) != 1 {
		return ""
	}
	block, _ := result.Content[0].(map[string]any)
	text, _ := block["text"].(string)
	var payload struct {
		ThreadID string `json:"session_thread_id"`
	}
	if json.Unmarshal([]byte(text), &payload) != nil {
		return ""
	}
	return payload.ThreadID
}

func normalizeCoordinatorInput(input map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var normalized map[string]any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

func loadSessionThreadForUpdate(
	ctx context.Context,
	tx pgx.Tx,
	sessionID string,
	threadID string,
) (domain.SessionThread, error) {
	var body []byte
	err := tx.QueryRow(ctx, `
SELECT body FROM session_threads
WHERE session_id = $1 AND id = $2
FOR UPDATE`, sessionID, threadID).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SessionThread{}, domain.NotFound("session thread not found")
	}
	if err != nil {
		return domain.SessionThread{}, err
	}
	var thread domain.SessionThread
	if err := json.Unmarshal(body, &thread); err != nil {
		return domain.SessionThread{}, err
	}
	return thread, nil
}

func insertSessionThread(ctx context.Context, tx pgx.Tx, thread domain.SessionThread) error {
	body, err := json.Marshal(thread)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO session_threads (
    id, session_id, parent_thread_id, kind, status, body,
    created_at, updated_at, archived_at
) VALUES ($1, $2, $3, 'child', $4, $5, $6, $7, NULL)`,
		thread.ID, thread.SessionID, thread.ParentThreadID,
		thread.Status, body, thread.CreatedAt, thread.UpdatedAt,
	)
	return err
}

func putSessionThreadTx(ctx context.Context, tx pgx.Tx, thread domain.SessionThread) error {
	body, err := json.Marshal(thread)
	if err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `
UPDATE session_threads
SET status = $3, body = $4, updated_at = $5, archived_at = $6
WHERE session_id = $1 AND id = $2`,
		thread.SessionID, thread.ID, thread.Status, body,
		thread.UpdatedAt, thread.ArchivedAt,
	)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return domain.NotFound("session thread not found")
	}
	return nil
}

func putSessionOnlyTx(ctx context.Context, tx pgx.Tx, session domain.Session) error {
	body, err := json.Marshal(session)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
UPDATE sessions
SET status = $2, body = $3, updated_at = $4
WHERE id = $1`, session.ID, session.Status, body, session.UpdatedAt)
	return err
}
