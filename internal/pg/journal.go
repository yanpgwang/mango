package pg

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

// The tool-execution journal preserves a prepared/started/completed/ambiguous
// side-effect boundary. A turn is identified by (session_id,
// trigger_event_id); each RunTurn Activity execution is an attempt. A Temporal
// retry creates a new attempt rather than erasing facts from a prior attempt.

// TurnAttempt is one durable execution attempt for a turn. It is internal
// bookkeeping and never serialized on the public API.
type TurnAttempt struct {
	ID             string
	SessionID      string
	TriggerEventID string
	AttemptNo      int
	State          domain.RunAttemptState
}

// BeginAttempt creates the next attempt for a turn. It refuses when an attempt is
// already active, or when a prior attempt already crossed the tool side-effect
// boundary (a started/completed/ambiguous step): such a turn must be recovered
// and classified, never freshly re-executed, so a side effect is not silently
// replayed. Call RecoverTurn first to classify leftovers.
func (s *Store) BeginAttempt(ctx context.Context, sessionID, triggerEventID string) (TurnAttempt, error) {
	var attempt TurnAttempt
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		if err := s.lockTurnExecutionOwner(
			ctx, tx, q, sessionID, triggerEventID,
		); err != nil {
			return err
		}
		if _, err := q.ActiveAttemptForTurn(ctx, pgstore.ActiveAttemptForTurnParams{
			SessionID: sessionID, TriggerEventID: triggerEventID,
		}); err == nil {
			return domain.Conflict("turn already has an active attempt")
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		prior, err := q.PriorToolExecutionForTurn(ctx, pgstore.PriorToolExecutionForTurnParams{
			SessionID: sessionID, TriggerEventID: triggerEventID,
		})
		if err != nil {
			return err
		}
		if prior {
			return domain.Conflict("turn has prior tool execution that requires recovery")
		}
		next, err := q.NextAttemptNo(ctx, pgstore.NextAttemptNoParams{
			SessionID: sessionID, TriggerEventID: triggerEventID,
		})
		if err != nil {
			return err
		}
		now := s.clock.Now().UTC()
		attempt = TurnAttempt{
			ID:             s.ids.NewID(domain.PrefixRunAttempt),
			SessionID:      sessionID,
			TriggerEventID: triggerEventID,
			AttemptNo:      int(next),
			State:          domain.RunAttemptActive,
		}
		return q.InsertTurnAttempt(ctx, pgstore.InsertTurnAttemptParams{
			ID:             attempt.ID,
			SessionID:      sessionID,
			TriggerEventID: triggerEventID,
			AttemptNo:      next,
			State:          string(domain.RunAttemptActive),
			CreatedAt:      tsUTC(now),
			UpdatedAt:      tsUTC(now),
		})
	})
	if err != nil {
		return TurnAttempt{}, err
	}
	return attempt, nil
}

// lockTurnExecutionOwner serializes attempt admission with Session and Thread
// lifecycle transitions. Once an owner is terminal, no delayed Activity may
// create an attempt that a completed termination transaction could not fence.
func (s *Store) lockTurnExecutionOwner(
	ctx context.Context,
	tx pgx.Tx,
	q *pgstore.Queries,
	sessionID string,
	triggerEventID string,
) error {
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
	if session.ArchivedAt != nil || session.Status == domain.StatusTerminated {
		return domain.Conflict("cannot execute a turn for a terminated Session")
	}

	var (
		threadStatus string
		archivedAt   *time.Time
	)
	err = tx.QueryRow(ctx, `
SELECT thread.status, thread.archived_at
FROM events AS event
JOIN session_threads AS thread
  ON thread.session_id = event.session_id
 AND thread.id = event.thread_id
WHERE event.session_id = $1 AND event.id = $2
FOR UPDATE OF thread`, sessionID, triggerEventID).Scan(
		&threadStatus, &archivedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NotFound("turn trigger event not found")
	}
	if err != nil {
		return err
	}
	if archivedAt != nil || domain.Status(threadStatus) == domain.StatusTerminated {
		return domain.Conflict("cannot execute a turn for a terminated Session Thread")
	}
	return nil
}

// EnsureAttempt returns the explicitly named active attempt for a
// Workflow-owned turn, creating it when none exists. Unlike BeginAttempt, this
// operation is intentionally idempotent: a Temporal Activity retry after the
// insert committed but before its result was acknowledged recovers the same id
// recorded in Workflow history.
//
// It still refuses a turn that has crossed a tool side-effect boundary in a
// non-active prior attempt. Only the live durable Workflow may reuse an active
// attempt; an abandoned turn is never silently restarted.
func (s *Store) EnsureAttempt(
	ctx context.Context,
	sessionID string,
	triggerEventID string,
	attemptID string,
) (TurnAttempt, error) {
	if attemptID == "" {
		return TurnAttempt{}, domain.Validation("turn attempt id is required")
	}
	var attempt TurnAttempt
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		if err := s.lockTurnExecutionOwner(
			ctx, tx, q, sessionID, triggerEventID,
		); err != nil {
			return err
		}
		activeID, err := q.ActiveAttemptForTurn(ctx, pgstore.ActiveAttemptForTurnParams{
			SessionID: sessionID, TriggerEventID: triggerEventID,
		})
		if err == nil {
			if activeID != attemptID {
				return domain.Conflict("turn already has a different active attempt")
			}
			row, err := q.GetTurnAttempt(ctx, activeID)
			if err != nil {
				return err
			}
			attempt = turnAttemptFromRow(row)
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		prior, err := q.PriorToolExecutionForTurn(ctx, pgstore.PriorToolExecutionForTurnParams{
			SessionID: sessionID, TriggerEventID: triggerEventID,
		})
		if err != nil {
			return err
		}
		if prior {
			return domain.Conflict("turn has prior tool execution without an active workflow attempt")
		}
		next, err := q.NextAttemptNo(ctx, pgstore.NextAttemptNoParams{
			SessionID: sessionID, TriggerEventID: triggerEventID,
		})
		if err != nil {
			return err
		}
		now := s.clock.Now().UTC()
		attempt = TurnAttempt{
			ID:             attemptID,
			SessionID:      sessionID,
			TriggerEventID: triggerEventID,
			AttemptNo:      int(next),
			State:          domain.RunAttemptActive,
		}
		return q.InsertTurnAttempt(ctx, pgstore.InsertTurnAttemptParams{
			ID:             attempt.ID,
			SessionID:      sessionID,
			TriggerEventID: triggerEventID,
			AttemptNo:      next,
			State:          string(domain.RunAttemptActive),
			CreatedAt:      tsUTC(now),
			UpdatedAt:      tsUTC(now),
		})
	})
	if err != nil {
		return TurnAttempt{}, err
	}
	return attempt, nil
}

// FinishAttempt closes an active attempt. A completed attempt requires every step
// to carry a durable result, and no terminal attempt may retain a started step:
// recovery must first classify such a step completed or ambiguous.
func (s *Store) FinishAttempt(ctx context.Context, attemptID string, state domain.RunAttemptState, attemptError *string) error {
	if err := validateAttemptFinish(state, attemptError); err != nil {
		return err
	}
	return s.withTx(ctx, func(q *pgstore.Queries) error {
		return s.finishAttemptLocked(ctx, q, attemptID, state, attemptError, "", "")
	})
}

func validateAttemptFinish(state domain.RunAttemptState, attemptError *string) error {
	switch state {
	case domain.RunAttemptCompleted:
		if attemptError != nil {
			return domain.Validation("completed attempt cannot carry an error")
		}
	case domain.RunAttemptFailed:
		if attemptError == nil || *attemptError == "" {
			return domain.Validation("failed attempt requires an error")
		}
	case domain.RunAttemptInterrupted:
		// An interrupt is not necessarily an error.
	default:
		return domain.Validation("invalid terminal attempt state")
	}
	return nil
}

// finishAttemptLocked closes an attempt using the caller's transaction. The
// parent row lock serializes finalization with StartToolStep.
func (s *Store) finishAttemptLocked(
	ctx context.Context,
	q *pgstore.Queries,
	attemptID string,
	state domain.RunAttemptState,
	attemptError *string,
	expectedSessionID string,
	expectedTriggerEventID string,
) error {
	// Lock the parent attempt before inspecting its steps. StartToolStep takes
	// the same lock before crossing the side-effect boundary, so completion
	// cannot race a prepared step into started after these checks.
	attempt, err := q.GetTurnAttempt(ctx, attemptID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NotFound("turn attempt not found")
	}
	if err != nil {
		return err
	}
	if expectedSessionID != "" &&
		(attempt.SessionID != expectedSessionID || attempt.TriggerEventID != expectedTriggerEventID) {
		return domain.Conflict("attempt does not belong to this turn")
	}
	if attempt.State != string(domain.RunAttemptActive) {
		return domain.Conflict("attempt is not active")
	}
	now := s.clock.Now().UTC()
	if state == domain.RunAttemptInterrupted {
		// An ordinary interrupt reaches this point after the Activity acknowledges
		// cancellation. Owner termination can also call it while committing the
		// terminal lifecycle. Locking the attempt serializes both fences with
		// StartToolStep: either Start already crossed the boundary and its
		// result-less step becomes ambiguous, or this transition wins and a stale
		// prepared step can never start.
		if _, err := q.MarkStartedStepsAmbiguousForAttempt(
			ctx,
			pgstore.MarkStartedStepsAmbiguousForAttemptParams{
				FinishedAt: tsUTC(now),
				UpdatedAt:  tsUTC(now),
				AttemptID:  attemptID,
			},
		); err != nil {
			return err
		}
	}
	started, err := q.CountStartedStepsForAttempt(ctx, attemptID)
	if err != nil {
		return err
	}
	if started > 0 {
		return domain.Conflict("attempt has unclassified started tool steps")
	}
	if state == domain.RunAttemptCompleted {
		incomplete, err := q.CountNonCompletedStepsForAttempt(ctx, attemptID)
		if err != nil {
			return err
		}
		if incomplete > 0 {
			return domain.Conflict("completed attempt has non-completed tool steps")
		}
	}
	affected, err := q.FinishTurnAttempt(ctx, pgstore.FinishTurnAttemptParams{
		State:      string(state),
		Error:      attemptError,
		UpdatedAt:  tsUTC(now),
		FinishedAt: tsUTC(now),
		ID:         attemptID,
	})
	if err != nil {
		return err
	}
	if affected != 1 {
		return domain.Conflict("attempt is not active")
	}
	return nil
}

// PrepareToolStep persists the model's tool request before an executor runs.
func (s *Store) PrepareToolStep(ctx context.Context, attemptID string, ordinal int, toolUseEventID, toolName string, input map[string]any) (string, error) {
	if ordinal < 0 {
		return "", domain.Validation("tool step ordinal must be non-negative")
	}
	if toolUseEventID == "" {
		return "", domain.Validation("tool step event id is required")
	}
	if toolName == "" {
		return "", domain.Validation("tool step name is required")
	}
	if input == nil {
		return "", domain.Validation("tool step input is required")
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	stepID := s.ids.NewID(domain.PrefixToolStep)
	err = s.withTx(ctx, func(q *pgstore.Queries) error {
		attempt, err := q.GetTurnAttempt(ctx, attemptID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("turn attempt not found")
		}
		if err != nil {
			return err
		}
		if attempt.State != string(domain.RunAttemptActive) {
			return domain.Conflict("tool step requires an active attempt")
		}
		conflict, err := q.ToolStepConflict(ctx, pgstore.ToolStepConflictParams{
			AttemptID: attemptID, Ordinal: int32(ordinal), ToolUseEventID: toolUseEventID,
		})
		if err != nil {
			return err
		}
		if conflict {
			return domain.Conflict("tool step ordinal or event id already exists")
		}
		now := s.clock.Now().UTC()
		return q.InsertToolStep(ctx, pgstore.InsertToolStepParams{
			ID:             stepID,
			AttemptID:      attemptID,
			Ordinal:        int32(ordinal),
			ToolUseEventID: toolUseEventID,
			ToolName:       toolName,
			Input:          inputJSON,
			CreatedAt:      tsUTC(now),
			UpdatedAt:      tsUTC(now),
		})
	})
	if err != nil {
		return "", err
	}
	return stepID, nil
}

// EnsureToolStep returns the stable tool step for one Workflow-owned model
// request, inserting it in prepared state when absent. The Workflow passes the
// explicit server-owned operation ids recorded in Temporal history, making the
// operation idempotent across Activity retries without namespace derivation.
func (s *Store) EnsureToolStep(
	ctx context.Context,
	attemptID string,
	stepID string,
	ordinal int,
	toolUseEventID string,
	toolName string,
	input map[string]any,
) (domain.ToolStep, error) {
	if ordinal < 0 {
		return domain.ToolStep{}, domain.Validation("tool step ordinal must be non-negative")
	}
	if stepID == "" {
		return domain.ToolStep{}, domain.Validation("tool step id is required")
	}
	if toolUseEventID == "" {
		return domain.ToolStep{}, domain.Validation("tool step event id is required")
	}
	if toolName == "" {
		return domain.ToolStep{}, domain.Validation("tool step name is required")
	}
	if input == nil {
		return domain.ToolStep{}, domain.Validation("tool step input is required")
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return domain.ToolStep{}, err
	}
	// Compare the same JSON representation PostgreSQL stores. Callers may supply
	// equivalent Go values with different concrete number types (for example int
	// on the first attempt and float64 after a JSON/Temporal round trip); that
	// must not turn an idempotent retry into a false conflict.
	var normalizedInput map[string]any
	if err := json.Unmarshal(inputJSON, &normalizedInput); err != nil {
		return domain.ToolStep{}, err
	}
	var step domain.ToolStep
	err = s.withTx(ctx, func(q *pgstore.Queries) error {
		attempt, err := q.GetTurnAttempt(ctx, attemptID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("turn attempt not found")
		}
		if err != nil {
			return err
		}
		if attempt.State != string(domain.RunAttemptActive) {
			return domain.Conflict("tool step requires an active attempt")
		}

		row, err := q.GetToolStep(ctx, stepID)
		if err == nil {
			existing, err := toolStepFromRow(row)
			if err != nil {
				return err
			}
			if existing.AttemptID != attemptID ||
				existing.Ordinal != ordinal ||
				existing.ToolUseEventID != toolUseEventID ||
				existing.ToolName != toolName ||
				!reflect.DeepEqual(existing.Input, normalizedInput) {
				return domain.Conflict("tool step id was reused with different input")
			}
			step = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		conflict, err := q.ToolStepConflict(ctx, pgstore.ToolStepConflictParams{
			AttemptID: attemptID, Ordinal: int32(ordinal), ToolUseEventID: toolUseEventID,
		})
		if err != nil {
			return err
		}
		if conflict {
			return domain.Conflict("tool step ordinal or event id already exists")
		}
		now := s.clock.Now().UTC()
		if err := q.InsertToolStep(ctx, pgstore.InsertToolStepParams{
			ID:             stepID,
			AttemptID:      attemptID,
			Ordinal:        int32(ordinal),
			ToolUseEventID: toolUseEventID,
			ToolName:       toolName,
			Input:          inputJSON,
			CreatedAt:      tsUTC(now),
			UpdatedAt:      tsUTC(now),
		}); err != nil {
			return err
		}
		step = domain.ToolStep{
			ID:             stepID,
			AttemptID:      attemptID,
			Ordinal:        ordinal,
			ToolUseEventID: toolUseEventID,
			ToolName:       toolName,
			Input:          normalizedInput,
			State:          domain.ToolStepPrepared,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		return nil
	})
	if err != nil {
		return domain.ToolStep{}, err
	}
	return step, nil
}

// StartToolStep advances prepared -> started: the executor may now change the
// external world.
func (s *Store) StartToolStep(ctx context.Context, stepID string) error {
	return s.withTx(ctx, func(q *pgstore.Queries) error {
		step, err := q.GetToolStep(ctx, stepID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("tool step not found")
		}
		if err != nil {
			return err
		}
		// Serialize the side-effect boundary with recovery and attempt
		// finalization. The SQL update repeats the active-state guard, but taking
		// this parent-row lock is what prevents a concurrent recovery from changing
		// the parent between the state check and prepared -> started.
		attempt, err := q.GetTurnAttempt(ctx, step.AttemptID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("turn attempt not found")
		}
		if err != nil {
			return err
		}
		if attempt.State != string(domain.RunAttemptActive) {
			return domain.Conflict("tool step requires an active attempt")
		}
		now := s.clock.Now().UTC()
		affected, err := q.StartToolStep(ctx, pgstore.StartToolStepParams{
			StartedAt: tsUTC(now), UpdatedAt: tsUTC(now), ID: stepID,
		})
		if err != nil {
			return err
		}
		if affected != 1 {
			return domain.Conflict("invalid tool step transition")
		}
		return nil
	})
}

// CompleteToolStep advances started -> completed with a durable result. Repeating
// the same completion is idempotent so a caller can retry a lost database
// acknowledgement without turning a known result into an ambiguous step.
func (s *Store) CompleteToolStep(ctx context.Context, stepID string, result domain.ToolStepResult) error {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return err
	}
	var normalizedResult domain.ToolStepResult
	if err := json.Unmarshal(resultJSON, &normalizedResult); err != nil {
		return err
	}
	return s.withTx(ctx, func(q *pgstore.Queries) error {
		now := s.clock.Now().UTC()
		affected, err := q.CompleteToolStep(ctx, pgstore.CompleteToolStepParams{
			Result: resultJSON, FinishedAt: tsUTC(now), UpdatedAt: tsUTC(now), ID: stepID,
		})
		if err != nil {
			return err
		}
		if affected == 1 {
			return nil
		}
		row, err := q.GetToolStep(ctx, stepID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("tool step not found")
		}
		if err != nil {
			return err
		}
		existing, err := toolStepFromRow(row)
		if err != nil {
			return err
		}
		if existing.State == domain.ToolStepCompleted &&
			existing.Result != nil &&
			reflect.DeepEqual(*existing.Result, normalizedResult) {
			return nil
		}
		return domain.Conflict("invalid tool step transition")
	})
}

// MarkToolStepAmbiguous records that an Activity retry found a step already
// started without a durable result. It is idempotent for an already-ambiguous
// step and never rewrites a completed result.
func (s *Store) MarkToolStepAmbiguous(ctx context.Context, stepID string) error {
	return s.withTx(ctx, func(q *pgstore.Queries) error {
		now := s.clock.Now().UTC()
		affected, err := q.MarkToolStepAmbiguous(ctx, pgstore.MarkToolStepAmbiguousParams{
			FinishedAt: tsUTC(now), UpdatedAt: tsUTC(now), ID: stepID,
		})
		if err != nil {
			return err
		}
		if affected == 1 {
			return nil
		}
		row, err := q.GetToolStep(ctx, stepID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("tool step not found")
		}
		if err != nil {
			return err
		}
		if row.State == string(domain.ToolStepAmbiguous) {
			return nil
		}
		return domain.Conflict("tool step cannot be marked ambiguous from state " + row.State)
	})
}

// RecoverTurn classifies leftovers from a crashed attempt: every started tool
// step for the turn is marked ambiguous (its side effect may have happened but no
// trustworthy result was recorded), and any still-active attempt is failed. It
// returns whether the turn now carries prior tool execution — in which case the
// turn must not be freshly re-run, only reported.
func (s *Store) RecoverTurn(ctx context.Context, sessionID, triggerEventID string) (hasPriorExecution bool, err error) {
	err = s.withTx(ctx, func(q *pgstore.Queries) error {
		// Lock the active parent attempt first. StartToolStep and FinishAttempt take
		// the same lock, establishing one ordering around the side-effect boundary:
		// either Start wins and recovery observes/classifies it, or recovery wins and
		// the stale Start is rejected.
		activeID, activeErr := q.ActiveAttemptForTurn(ctx, pgstore.ActiveAttemptForTurnParams{
			SessionID: sessionID, TriggerEventID: triggerEventID,
		})
		if activeErr != nil && !errors.Is(activeErr, pgx.ErrNoRows) {
			return activeErr
		}

		startedIDs, err := q.StartedStepsForTurn(ctx, pgstore.StartedStepsForTurnParams{
			SessionID: sessionID, TriggerEventID: triggerEventID,
		})
		if err != nil {
			return err
		}
		now := s.clock.Now().UTC()
		for _, id := range startedIDs {
			if _, err := q.MarkToolStepAmbiguous(ctx, pgstore.MarkToolStepAmbiguousParams{
				FinishedAt: tsUTC(now), UpdatedAt: tsUTC(now), ID: id,
			}); err != nil {
				return err
			}
		}
		// Fail the active attempt so a fresh BeginAttempt is not blocked by the
		// one-active guard once the turn is (correctly) refused.
		if activeErr == nil {
			msg := "recovered: attempt abandoned before completion"
			if _, err := q.FinishTurnAttempt(ctx, pgstore.FinishTurnAttemptParams{
				State: string(domain.RunAttemptFailed), Error: &msg,
				UpdatedAt: tsUTC(now), FinishedAt: tsUTC(now), ID: activeID,
			}); err != nil {
				return err
			}
		}
		prior, err := q.PriorToolExecutionForTurn(ctx, pgstore.PriorToolExecutionForTurnParams{
			SessionID: sessionID, TriggerEventID: triggerEventID,
		})
		if err != nil {
			return err
		}
		hasPriorExecution = prior
		return nil
	})
	return hasPriorExecution, err
}

// ToolStepStateByEventID returns the state of the tool step correlated to a
// tool_use event id. Used by tests to assert the ambiguous classification.
func (s *Store) ToolStepStateByEventID(ctx context.Context, toolUseEventID string) (domain.ToolStepState, bool, error) {
	var state string
	err := s.pool.QueryRow(ctx,
		`SELECT state FROM tool_steps WHERE tool_use_event_id = $1`, toolUseEventID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return domain.ToolStepState(state), true, nil
}
