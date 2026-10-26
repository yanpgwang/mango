package pg

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

// These internal storage operations have no HTTP/model-tool admission path and
// are not registered as a program executor. They establish ownership/receipt
// transactions before the separate pump, barrier and engine gates are enabled.
type WorkflowRunRequest struct {
	Source         string          `json:"source"`
	RuntimeProfile string          `json:"runtime_profile"`
	Input          json.RawMessage `json:"input"`
	Deadline       time.Time       `json:"deadline"`
	MaxInvocations int             `json:"max_invocations"`
	MaxResultBytes int             `json:"max_result_bytes"`
}

type WorkflowRun struct {
	ID, SessionID string
}

type WorkflowInvocationCommand struct {
	AgentName string `json:"agent_name"`
	Prompt    string `json:"prompt"`
}

// A business attempt owns one Thread. The existing turn_attempts journal still
// describes individual model/tool turns, not a retry of this business task.
// This first kernel admits attempt 1 only; business retries are not implemented.
type WorkflowInvocationAttempt struct {
	ID, InvocationID, ThreadID, RootTriggerID string
	AttemptNo                                 int
	Receipt                                   *WorkflowInvocationReceipt
}

type WorkflowInvocationReceipt struct {
	AdapterVersion int    `json:"adapter_version"`
	Type           string `json:"type"`
	Text           string `json:"text,omitempty"`
	ErrorCode      string `json:"error_code,omitempty"`
}

type WorkflowRunWakeup struct {
	SessionID, RunID string
	Version          int64
	LeaseToken       string
	DeliveryAttempt  int
}

func (s *Store) AdmitWorkflowRun(ctx context.Context, sessionID, key string, request WorkflowRunRequest) (WorkflowRun, error) {
	if key == "" || len(key) > 256 || !utf8.ValidString(key) || request.Source == "" || len(request.Source) > 65536 ||
		!utf8.ValidString(request.Source) || request.RuntimeProfile == "" || len(request.RuntimeProfile) > 256 ||
		!utf8.ValidString(request.RuntimeProfile) || len(request.Input) > 65536 || !json.Valid(request.Input) ||
		request.MaxInvocations < 1 || request.MaxInvocations > 25 || request.MaxResultBytes < 1 || request.MaxResultBytes > 65536 {
		return WorkflowRun{}, domain.Validation("invalid internal Workflow admission")
	}
	request.Deadline = request.Deadline.UTC().Truncate(time.Microsecond)
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return WorkflowRun{}, err
	}
	var result WorkflowRun
	err = s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		session, err := s.lockWorkflowSession(ctx, q, sessionID)
		if err != nil {
			return err
		}
		var sameRequest bool
		err = tx.QueryRow(ctx, `SELECT id,request=$3::jsonb FROM workflow_runs WHERE session_id=$1 AND admission_key=$2`, sessionID, key, requestJSON).Scan(&result.ID, &sameRequest)
		if err == nil {
			if !sameRequest {
				return domain.Conflict("Workflow admission key has different input")
			}
			result.SessionID = sessionID
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if session.ArchivedAt != nil || session.Status == domain.StatusTerminated || !request.Deadline.After(s.clock.Now()) {
			return domain.Conflict("Workflow admission owner or deadline is no longer eligible")
		}
		if err := rejectActiveOutcomeGrading(ctx, tx, sessionID); err != nil {
			return err
		}
		// Freeze the already-admitted Session roster; no saved Agent or live
		// credential value is copied or resolved by this operation.
		if len(session.MultiagentRoster) == 0 {
			return domain.Validation("Workflow admission requires a predefined Session roster")
		}
		roster, err := json.Marshal(session.MultiagentRoster)
		if err != nil {
			return err
		}
		result = WorkflowRun{ID: s.ids.NewID("wrun_"), SessionID: sessionID}
		_, err = tx.Exec(ctx, `INSERT INTO workflow_runs(id,session_id,admission_key,request,roster,deadline,max_invocations,max_result_bytes,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, result.ID, sessionID, key, requestJSON, roster, request.Deadline, request.MaxInvocations, request.MaxResultBytes, s.clock.Now())
		if err != nil {
			return err
		}
		return s.enqueueWorkflowRunLocked(ctx, tx, sessionID, result.ID, 1)
	})
	return result, err
}

// AdmitWorkflowInvocation atomically creates the invocation, its first business
// attempt, the actual child Thread/input and its ordinary execution outbox.
// Run-local ordinal is the command identity; neither prompt nor Agent name is.
func (s *Store) AdmitWorkflowInvocation(ctx context.Context, sessionID, runID string, ordinal int, command WorkflowInvocationCommand) (WorkflowInvocationAttempt, error) {
	if ordinal < 0 || command.AgentName == "" || command.Prompt == "" || len(command.Prompt) > 65536 || !utf8.ValidString(command.Prompt) {
		return WorkflowInvocationAttempt{}, domain.Validation("invalid Workflow invocation")
	}
	commandJSON, err := json.Marshal(command)
	if err != nil {
		return WorkflowInvocationAttempt{}, err
	}
	var result WorkflowInvocationAttempt
	err = s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		session, err := s.lockWorkflowSession(ctx, q, sessionID)
		if err != nil {
			return err
		}
		var epoch int64
		var coordinator string
		var pauses []string
		if err := tx.QueryRow(ctx, `SELECT evaluator_epoch,pause_reasons,coordinator_execution_id FROM workflow_runs WHERE session_id=$1 AND id=$2 FOR UPDATE`, sessionID, runID).Scan(&epoch, &pauses, &coordinator); err != nil {
			return err
		}
		if epoch != 0 || coordinator != "" {
			return domain.Conflict("Workflow admission requires its fenced frontier")
		}
		if len(pauses) != 0 || session.BudgetReached(s.clock.Now()) {
			return domain.Conflict("Workflow admission is control-paused")
		}
		var state string
		var deadline time.Time
		var rosterJSON []byte
		var limit int
		if err := tx.QueryRow(ctx, `SELECT state,deadline,roster,max_invocations FROM workflow_runs WHERE session_id=$1 AND id=$2 FOR UPDATE`, sessionID, runID).Scan(&state, &deadline, &rosterJSON, &limit); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("Workflow Run not found")
			}
			return err
		}
		var sameCommand bool
		err = tx.QueryRow(ctx, `SELECT attempt.id,invocation.id,attempt.thread_id,attempt.root_trigger_id,attempt.attempt_no,invocation.command=$3::jsonb
FROM workflow_invocations invocation JOIN workflow_invocation_attempts attempt ON attempt.invocation_id=invocation.id AND attempt.attempt_no=1
WHERE invocation.run_id=$1 AND invocation.ordinal=$2`, runID, ordinal, commandJSON).Scan(&result.ID, &result.InvocationID, &result.ThreadID, &result.RootTriggerID, &result.AttemptNo, &sameCommand)
		if err == nil {
			if !sameCommand {
				return domain.Conflict("Workflow invocation identity has different input")
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if state != "open" || !deadline.After(s.clock.Now()) || session.ArchivedAt != nil || session.Status == domain.StatusTerminated {
			return domain.Conflict("Workflow invocation owner or deadline is no longer eligible")
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM workflow_invocations WHERE run_id=$1`, runID).Scan(&count); err != nil {
			return err
		}
		if ordinal != count || count >= limit {
			return domain.Conflict("Workflow invocation frontier or expansion limit exceeded")
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM session_threads WHERE session_id=$1 AND kind<>'advisor' AND archived_at IS NULL`, sessionID).Scan(&count); err != nil {
			return err
		}
		if count >= maxConcurrentSessionThreads {
			return domain.Conflict("Session concurrent Thread limit exceeded")
		}
		var roster []domain.Agent
		if err := json.Unmarshal(rosterJSON, &roster); err != nil {
			return err
		}
		agent, err := rosterMemberByName(roster, command.AgentName)
		if err != nil {
			return domain.Validation("Workflow Agent is not in the admitted roster")
		}
		result, err = s.createWorkflowInvocationLocked(ctx, tx, q, session, runID, ordinal, command, agent)
		if err != nil {
			return err
		}
		return s.advanceWorkflowRunLocked(ctx, tx, sessionID, runID)
	})
	return result, err
}

// createWorkflowInvocationLocked shares the actual Thread/input/outbox mutation.
// Caller owns Session and Run locks and has admitted this exact command.
func (s *Store) createWorkflowInvocationLocked(ctx context.Context, tx pgx.Tx, q *pgstore.Queries, session domain.Session, runID string, ordinal int, command WorkflowInvocationCommand, agent domain.Agent) (WorkflowInvocationAttempt, error) {
	sessionID := session.ID
	commandJSON, err := json.Marshal(command)
	if err != nil {
		return WorkflowInvocationAttempt{}, err
	}
	primaryID, err := q.GetPrimarySessionThreadID(ctx, sessionID)
	if err != nil {
		return WorkflowInvocationAttempt{}, err
	}
	now := s.clock.Now().UTC()
	child := domain.NewChildSessionThread(s.ids.NewID(domain.PrefixSessionThread), sessionID, primaryID, agent, now)
	child.TransitionStatus(domain.StatusRunning, now)
	if err := insertSessionThread(ctx, tx, child); err != nil {
		return WorkflowInvocationAttempt{}, err
	}
	maxSeq, err := q.MaxEventSeq(ctx, sessionID)
	if err != nil {
		return WorkflowInvocationAttempt{}, err
	}
	// This is a host-admitted child input, not a fabricated coordinator tool
	// call or primary sent-message. The private ownership row is authoritative.
	inputs, maxSeq, err := s.appendThreadDraftsAt(ctx, q, sessionID, child.ID, []domain.EventDraft{{
		Type:    domain.EvAgentThreadMessageReceived,
		Payload: map[string]any{"content": []any{map[string]any{"type": "text", "text": command.Prompt}}},
	}}, maxSeq, nil, now)
	if err != nil {
		return WorkflowInvocationAttempt{}, err
	}
	result := WorkflowInvocationAttempt{ID: s.ids.NewID("wiat_"), InvocationID: s.ids.NewID("winv_"), ThreadID: child.ID, RootTriggerID: inputs[0].ID, AttemptNo: 1}
	snapshot, err := json.Marshal(child.Agent)
	if err != nil {
		return WorkflowInvocationAttempt{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO workflow_invocations(id,session_id,run_id,ordinal,command,agent_snapshot) VALUES($1,$2,$3,$4,$5,$6)`, result.InvocationID, sessionID, runID, ordinal, commandJSON, snapshot)
	if err != nil {
		return WorkflowInvocationAttempt{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO workflow_invocation_attempts(id,session_id,run_id,invocation_id,attempt_no,thread_id,root_trigger_id) VALUES($1,$2,$3,$4,1,$5,$6)`, result.ID, sessionID, runID, result.InvocationID, child.ID, result.RootTriggerID)
	if err != nil {
		return WorkflowInvocationAttempt{}, err
	}
	if err := q.UpsertThreadOutbox(ctx, pgstore.UpsertThreadOutboxParams{SessionID: sessionID, ThreadID: child.ID, MaxEventSeq: maxSeq, EnqueuedAt: tsUTC(now)}); err != nil {
		return WorkflowInvocationAttempt{}, err
	}
	if session.EnvironmentType == "self_hosted" {
		if err := q.EnqueueEnvironmentWork(ctx, pgstore.EnqueueEnvironmentWorkParams{
			ID: s.ids.NewID(domain.PrefixEnvironmentWork), EnvironmentID: session.EnvironmentID,
			SessionID: &sessionID, ActivationSeq: &maxSeq, CreatedAt: tsUTC(now),
		}); err != nil {
			return WorkflowInvocationAttempt{}, err
		}
	}
	session.TransitionStatus(domain.StatusRunning, now)
	if err := putSessionOnlyTx(ctx, tx, session); err != nil {
		return WorkflowInvocationAttempt{}, err
	}
	return result, nil
}

type workflowCompletionOwner struct {
	runID, attemptID, threadID, state, runState string
	deadline                                    time.Time
	maxResultBytes                              int
}

// Caller already holds the Session lock. Acquire Run/attempt ownership before
// the Thread lock, matching invocation admission's Session-first ordering.
func lockWorkflowCompletionOwner(ctx context.Context, tx pgx.Tx, sessionID, threadID string) (*workflowCompletionOwner, error) {
	var runID string
	err := tx.QueryRow(ctx, `SELECT run_id FROM workflow_invocation_attempts WHERE session_id=$1 AND thread_id=$2`, sessionID, threadID).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	owner := workflowCompletionOwner{threadID: threadID}
	err = tx.QueryRow(ctx, `SELECT id,state,deadline,max_result_bytes FROM workflow_runs WHERE session_id=$1 AND id=$2 FOR UPDATE`, sessionID, runID).Scan(&owner.runID, &owner.runState, &owner.deadline, &owner.maxResultBytes)
	if err != nil {
		return nil, err
	}
	err = tx.QueryRow(ctx, `SELECT id,state FROM workflow_invocation_attempts WHERE session_id=$1 AND run_id=$2 AND thread_id=$3 FOR UPDATE`, sessionID, runID, threadID).Scan(&owner.attemptID, &owner.state)
	return &owner, err
}

func (s *Store) settleWorkflowCompletionLocked(ctx context.Context, tx pgx.Tx, owner *workflowCompletionOwner, sessionID, triggerID, turnAttemptID string, candidate domain.ThreadReturnCandidate, drafts []domain.EventDraft, events []domain.Event, interrupted, terminated bool) error {
	receipt := WorkflowInvocationReceipt{AdapterVersion: 1, Type: "failure"}
	switch {
	case owner.runState != "open":
		receipt.ErrorCode = "run_closed"
	case !owner.deadline.After(s.clock.Now()):
		receipt.ErrorCode = "deadline_exceeded"
	case interrupted:
		receipt.ErrorCode = "interrupted"
	case terminated:
		receipt.ErrorCode = "thread_failed"
	default:
		receipt = workflowTextReceipt(drafts, owner.maxResultBytes, candidate)
	}
	var terminalID, messageID string
	for _, event := range events {
		if event.ThreadID == owner.threadID && event.TurnEventID != nil && *event.TurnEventID == triggerID {
			terminalID = event.ID
			if event.Type == domain.EvAgentMessage && event.ID == candidate.MessageEventID {
				messageID = event.ID
			}
		}
	}
	if messageID != "" {
		terminalID = messageID
	}
	if terminalID == "" {
		return domain.Conflict("Workflow completion has no owned terminal event")
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	var turnID *string
	if turnAttemptID != "" {
		turnID = &turnAttemptID
	}
	command, err := tx.Exec(ctx, `UPDATE workflow_invocation_attempts SET state='settled',receipt=$2,terminal_trigger_id=$3,terminal_event_id=$4,turn_attempt_id=$5 WHERE id=$1 AND state='active'`, owner.attemptID, encoded, triggerID, terminalID, turnID)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return domain.Conflict("Workflow attempt already settled")
	}
	return s.advanceWorkflowRunLocked(ctx, tx, sessionID, owner.runID)
}

func workflowTextReceipt(drafts []domain.EventDraft, limit int, candidate domain.ThreadReturnCandidate) WorkflowInvocationReceipt {
	failed := func(code string) WorkflowInvocationReceipt {
		return WorkflowInvocationReceipt{AdapterVersion: 1, Type: "failure", ErrorCode: code}
	}
	if candidate.StopReason != "end_turn" {
		return failed("nonterminal_output")
	}
	if !candidate.TextOnly {
		return failed("unsupported_output")
	}
	if candidate.MessageEventID == "" {
		return failed("missing_output")
	}
	for i := len(drafts) - 1; i >= 0; i-- {
		if drafts[i].Type != domain.EvAgentMessage || drafts[i].ID != candidate.MessageEventID {
			continue
		}
		blocks, ok := drafts[i].Payload["content"].([]any)
		if !ok || len(blocks) == 0 {
			return failed("missing_output")
		}
		var text strings.Builder
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if !ok || block["type"] != "text" {
				return failed("unsupported_output")
			}
			value, ok := block["text"].(string)
			if !ok || !utf8.ValidString(value) {
				return failed("invalid_text")
			}
			if text.Len()+len(value) > limit {
				return failed("output_too_large")
			}
			text.WriteString(value)
		}
		if text.Len() == 0 {
			return failed("missing_output")
		}
		return WorkflowInvocationReceipt{AdapterVersion: 1, Type: "text", Text: text.String()}
	}
	return failed("missing_output")
}

func (s *Store) advanceWorkflowRunLocked(ctx context.Context, tx pgx.Tx, sessionID, runID string) error {
	var version int64
	if err := tx.QueryRow(ctx, `UPDATE workflow_runs SET version=version+1 WHERE session_id=$1 AND id=$2 RETURNING version`, sessionID, runID).Scan(&version); err != nil {
		return err
	}
	return s.enqueueWorkflowRunLocked(ctx, tx, sessionID, runID, version)
}

func (s *Store) enqueueWorkflowRunLocked(ctx context.Context, tx pgx.Tx, sessionID, runID string, version int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO workflow_run_outbox(session_id,run_id,version,enqueued_at,available_at) VALUES($1,$2,$3,$4,statement_timestamp())
ON CONFLICT(run_id) DO UPDATE SET version=EXCLUDED.version,
 delivery_attempts=0,available_at=statement_timestamp(),lease_token=''
 WHERE EXCLUDED.version>workflow_run_outbox.version`, sessionID, runID, version, s.clock.Now())
	return err
}

func (s *Store) lockWorkflowSession(ctx context.Context, q *pgstore.Queries, sessionID string) (domain.Session, error) {
	row, err := q.LockSession(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Session{}, domain.NotFound("Session not found")
	}
	if err != nil {
		return domain.Session{}, err
	}
	id, scoped, err := s.workspaceForRead(ctx)
	if err != nil {
		return domain.Session{}, err
	}
	if scoped && row.WorkspaceID != id {
		return domain.Session{}, domain.NotFound("Session not found")
	}
	if row.DeletingAt.Valid {
		return domain.Session{}, domain.Conflict("Session deletion is in progress")
	}
	return sessionFromLockRow(row)
}

func workflowCompletionWait(drafts []domain.EventDraft) bool {
	for i := len(drafts) - 1; i >= 0; i-- {
		if drafts[i].Type != domain.EvSessionStatusIdle && drafts[i].Type != domain.EvSessionThreadStatusIdle {
			continue
		}
		reason, _ := drafts[i].Payload["stop_reason"].(map[string]any)
		return reason["type"] == "requires_action" || reason["type"] == "budget_reached"
	}
	return false
}

// WorkflowRunWakeups lists the existing Run outbox. The ordinary Session relay
// ignores it; the internal coordinator gate uses a separate opt-in relay.
// Application startup does not enable that coordinator or relay.
func (s *Store) WorkflowRunWakeups(ctx context.Context) ([]WorkflowRunWakeup, error) {
	id, _, err := s.workspaceForRead(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT wake.session_id,wake.run_id,wake.version FROM workflow_run_outbox wake
JOIN sessions session ON session.id=wake.session_id WHERE ($1::text='' OR session.workspace_id=$1)
ORDER BY wake.enqueued_at,wake.run_id LIMIT 100`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkflowRunWakeup{}
	for rows.Next() {
		var wake WorkflowRunWakeup
		if err := rows.Scan(&wake.SessionID, &wake.RunID, &wake.Version); err != nil {
			return nil, err
		}
		out = append(out, wake)
	}
	return out, rows.Err()
}

func (s *Store) DeleteWorkflowRunWakeupIfUnchanged(ctx context.Context, wake WorkflowRunWakeup) (bool, error) {
	deleted := false
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		if _, err := s.lockWorkflowSession(ctx, q, wake.SessionID); err != nil {
			return err
		}
		command, err := tx.Exec(ctx, `DELETE FROM workflow_run_outbox WHERE session_id=$1 AND run_id=$2 AND version=$3 AND lease_token=$4 AND ($4<>'' OR delivery_attempts=0)`, wake.SessionID, wake.RunID, wake.Version, wake.LeaseToken)
		deleted = command.RowsAffected() == 1
		return err
	})
	return deleted, err
}

func (s *Store) GetWorkflowInvocationAttempt(ctx context.Context, sessionID, attemptID string) (WorkflowInvocationAttempt, error) {
	var result WorkflowInvocationAttempt
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		if _, err := s.lockWorkflowSession(ctx, q, sessionID); err != nil {
			return err
		}
		var receipt []byte
		err := tx.QueryRow(ctx, `SELECT id,invocation_id,thread_id,root_trigger_id,attempt_no,receipt FROM workflow_invocation_attempts WHERE session_id=$1 AND id=$2`, sessionID, attemptID).Scan(&result.ID, &result.InvocationID, &result.ThreadID, &result.RootTriggerID, &result.AttemptNo, &receipt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("Workflow attempt not found")
		}
		if err != nil {
			return err
		}
		if receipt != nil {
			return json.Unmarshal(receipt, &result.Receipt)
		}
		return nil
	})
	return result, err
}
