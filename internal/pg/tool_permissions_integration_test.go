package pg

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/permission"
)

func permissionReceipt(t *testing.T, store *Store, sessionID string) domain.ToolPermissionReceipt {
	t.Helper()
	ctx := context.Background()
	session := newSession(sessionID)
	session.AgentSnapshot = domain.Agent{ID: session.AgentID, Version: 1, Name: "test", Model: domain.Model{ID: "claude-opus-4-8"}}
	session.ListCostKnown = true
	admitted, err := store.CreateSession(ctx, session, []domain.EventDraft{{Type: domain.EvUserMessage, Payload: map[string]any{"content": "read the build instructions"}}})
	if err != nil {
		t.Fatal(err)
	}
	trigger := eventOfType(t, admitted.Events, domain.EvUserMessage)
	attempt, err := store.EnsureAttempt(ctx, sessionID, trigger.ID, "ratm_permission")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := permission.FingerprintInvocation("read", map[string]any{"path": "README.md"})
	if err != nil {
		t.Fatal(err)
	}
	return domain.ToolPermissionReceipt{
		ToolPermissionOwner: domain.ToolPermissionOwner{SessionID: sessionID, ThreadID: trigger.ThreadID, TriggerEventID: trigger.ID, AttemptID: attempt.ID},
		ToolUseEventID:      "sevt_permission", ToolName: "read", InvocationHash: hash, ContextHash: strings.Repeat("a", 64),
		Decision: domain.ToolPermissionDecision{Type: "allow"}, Model: session.AgentSnapshot.Model,
		Usage: domain.TokenUsage{InputTokens: 200, OutputTokens: 5}, StopReason: "end_turn", ResponseReceived: true,
	}
}

func requirePermissionConflict(t *testing.T, err error) {
	t.Helper()
	var target *domain.DomainError
	if !errors.As(err, &target) || target.Kind != domain.KindConflict {
		t.Fatalf("expected ownership conflict, got %v", err)
	}
}

func TestToolPermissionReceiptIsImmutableAndAtomicallyAccounted(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	receipt := permissionReceipt(t, store, "sesn_permission_receipt")
	got, err := store.RecordToolPermissionReceipt(ctx, receipt)
	if err != nil || got.Decision.Type != "allow" {
		t.Fatalf("record = %+v, %v", got, err)
	}
	// A committed response whose acknowledgement was lost is read on retry;
	// a different candidate judgment/usage must not replace or rebill it.
	retry := receipt
	retry.Decision = domain.ToolPermissionDecision{Type: "deny", ReasonCode: "high_risk"}
	retry.Usage.InputTokens = 900
	got, err = store.RecordToolPermissionReceipt(ctx, retry)
	if err != nil || got.Decision.Type != "allow" || got.Usage.InputTokens != 200 {
		t.Fatalf("winning receipt changed = %+v, %v", got, err)
	}
	session, err := store.GetSession(ctx, receipt.SessionID)
	if err != nil || session.Usage.InputTokens != 200 || session.Usage.OutputTokens != 5 {
		t.Fatalf("duplicate billing = %+v, %v", session.Usage, err)
	}
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM model_request_usage WHERE session_id=$1`, receipt.SessionID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("accounting rows = %d, %v", count, err)
	}
	lookup, err := store.GetToolPermissionReceipt(ctx, receipt)
	if err != nil || lookup == nil || lookup.Decision.Type != "allow" {
		t.Fatalf("lookup = %+v, %v", lookup, err)
	}
	for _, mutate := range []func(*domain.ToolPermissionReceipt){
		func(r *domain.ToolPermissionReceipt) { r.ContextHash = strings.Repeat("b", 64) },
		func(r *domain.ToolPermissionReceipt) { r.AttemptID = "ratm_other" },
		func(r *domain.ToolPermissionReceipt) { r.ToolName = "bash" },
		func(r *domain.ToolPermissionReceipt) { r.Model.ID = "other-model" },
	} {
		changed := receipt
		mutate(&changed)
		_, err := store.GetToolPermissionReceipt(ctx, changed)
		requirePermissionConflict(t, err)
		_, err = store.RecordToolPermissionReceipt(ctx, changed)
		requirePermissionConflict(t, err)
	}
}

func TestToolPermissionConcurrentFactsUseWinningUsage(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	receipt := permissionReceipt(t, store, "sesn_permission_concurrent")
	var group sync.WaitGroup
	errors := make(chan error, 2)
	for _, tokens := range []int64{200, 500} {
		candidate := receipt
		candidate.Usage.InputTokens = tokens
		group.Go(func() {
			_, err := store.RecordToolPermissionReceipt(ctx, candidate)
			errors <- err
		})
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	winner, err := store.GetToolPermissionReceipt(ctx, receipt)
	if err != nil || winner == nil {
		t.Fatalf("winner = %+v, %v", winner, err)
	}
	session, err := store.GetSession(ctx, receipt.SessionID)
	if err != nil || session.Usage.InputTokens != winner.Usage.InputTokens || (session.Usage.InputTokens != 200 && session.Usage.InputTokens != 500) {
		t.Fatalf("winner accounting = %+v, %v", session.Usage, err)
	}
}

func TestToolPermissionReceiptAndUsageRollBackTogether(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	receipt := permissionReceipt(t, store, "sesn_permission_atomic")
	if _, err := store.pool.Exec(ctx, `ALTER TABLE model_request_usage ADD CONSTRAINT reject_test_usage CHECK (false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordToolPermissionReceipt(ctx, receipt); err == nil {
		t.Fatal("accounting failure unexpectedly committed")
	}
	got, err := store.GetToolPermissionReceipt(ctx, receipt)
	if err != nil || got != nil {
		t.Fatalf("unaccounted receipt escaped rollback = %+v, %v", got, err)
	}
}

func TestToolPermissionLateFactsCannotAuthorizeExecution(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	receipt := permissionReceipt(t, store, "sesn_permission_late")
	if _, err := store.AdmitEvents(ctx, receipt.SessionID, []domain.EventDraft{{Type: domain.EvUserInterrupt, Payload: map[string]any{}}}); err != nil {
		t.Fatal(err)
	}
	_, err := store.AdmitToolPermissionEvaluation(ctx, receipt)
	requirePermissionConflict(t, err)
	if err := store.FinishAttempt(ctx, receipt.AttemptID, domain.RunAttemptInterrupted, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordToolPermissionReceipt(ctx, receipt); err != nil {
		t.Fatalf("late known usage was lost: %v", err)
	}
	_, err = store.AdmitToolPermissionEvaluation(ctx, receipt)
	requirePermissionConflict(t, err)
	session, err := store.GetSession(ctx, receipt.SessionID)
	if err != nil || session.Usage.InputTokens != 200 {
		t.Fatalf("late usage = %+v, %v", session.Usage, err)
	}
	var state string
	if err := store.pool.QueryRow(ctx, `SELECT state FROM turn_attempts WHERE id=$1`, receipt.AttemptID).Scan(&state); err != nil || state != "interrupted" {
		t.Fatalf("late accounting revived owner = %q, %v", state, err)
	}
}

func TestToolPermissionStartRequiresExactAllowedInvocationAndLiveOwner(t *testing.T) {
	for _, problem := range []string{"allowed", "wrong input", "denied", "interrupted"} {
		t.Run(problem, func(t *testing.T) {
			store := testStore(t)
			ctx := context.Background()
			receipt := permissionReceipt(t, store, "sesn_permission_start")
			if problem == "denied" {
				receipt.Decision = domain.ToolPermissionDecision{Type: "deny", ReasonCode: "high_risk"}
			}
			if _, err := store.RecordToolPermissionReceipt(ctx, receipt); err != nil {
				t.Fatal(err)
			}
			input := map[string]any{"path": "README.md"}
			if problem == "wrong input" {
				input["path"] = ".env"
			}
			step, err := store.EnsureToolStep(ctx, receipt.AttemptID, "tstep_permission", 0, receipt.ToolUseEventID, "read", input)
			if err != nil {
				t.Fatal(err)
			}
			if problem == "interrupted" {
				if _, err := store.AdmitEvents(ctx, receipt.SessionID, []domain.EventDraft{{Type: domain.EvUserInterrupt, Payload: map[string]any{}}}); err != nil {
					t.Fatal(err)
				}
			}
			err = store.StartToolStepWithPermission(ctx, step.ID, receipt.ToolUseEventID)
			if problem == "allowed" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				requirePermissionConflict(t, err)
				state, _, err := store.ToolStepStateByEventID(ctx, receipt.ToolUseEventID)
				if err != nil || state != domain.ToolStepPrepared {
					t.Fatalf("rejected invocation crossed execution boundary = %q, %v", state, err)
				}
			}
		})
	}
}

func TestToolPermissionRefusalKeepsUsageWithZeroResponsePrice(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	receipt := permissionReceipt(t, store, "sesn_permission_refusal")
	receipt.Model.ID, receipt.StopReason = "claude-fable-5", "refusal"
	receipt.Decision = domain.ToolPermissionDecision{Type: "ask", ReasonCode: "indeterminate"}
	if _, err := store.RecordToolPermissionReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	session, err := store.GetSession(ctx, receipt.SessionID)
	if err != nil || session.Usage.InputTokens != 200 || session.ModelListCostNanoUSD != 0 || !session.ListCostKnown {
		t.Fatalf("refusal facts = %+v, %v", session, err)
	}
}

func TestPermissionBudgetInterruptCannotReviveUnpublishedRound(t *testing.T) {
	for _, name := range []string{"single", "primary", "child"} {
		childTurn := name == "child"
		t.Run(name, func(t *testing.T) {
			store := testStore(t)
			ctx := context.Background()
			session := newSession("sesn_permission_budget")
			session.AgentSnapshot = domain.Agent{
				ID: session.AgentID, Version: 1, Name: "coordinator", Model: domain.Model{ID: "claude-opus-4-8"},
				Multiagent: &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{{Type: "agent", ID: "agent_child", Version: 1}}},
			}
			session.MultiagentRoster = []domain.Agent{{ID: "agent_child", Version: 1, Name: "child", Model: session.AgentSnapshot.Model}}
			if name == "single" {
				session.AgentSnapshot.Multiagent = nil
				session.MultiagentRoster = nil
			}
			session.ListCostKnown = true
			session.Budget = &domain.SessionBudget{MaxListCostCents: 1}
			admitted, err := store.CreateSession(ctx, session, []domain.EventDraft{{Type: domain.EvUserMessage, Payload: map[string]any{"content": "read build instructions"}}})
			if err != nil {
				t.Fatal(err)
			}
			trigger := eventOfType(t, admitted.Events, domain.EvUserMessage)
			threadID := trigger.ThreadID
			if childTurn {
				child, _, err := store.CreateChildSessionThread(ctx, session.ID, threadID, "child")
				if err != nil {
					t.Fatal(err)
				}
				threadID = child.ID
				childEvents, err := store.AppendThreadEvents(ctx, session.ID, threadID, []domain.EventDraft{{Type: domain.EvAgentThreadMessageReceived, Payload: map[string]any{"content": "read"}}})
				if err != nil {
					t.Fatal(err)
				}
				trigger = eventOfType(t, childEvents, domain.EvAgentThreadMessageReceived)
			}
			attempt, err := store.EnsureAttempt(ctx, session.ID, trigger.ID, "ratm_permission_budget")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.AccountModelRequest(ctx, session.ID, threadID, "sevt_working_response", session.AgentSnapshot.Model, domain.TokenUsage{InputTokens: 2000}, "end_turn"); err != nil {
				t.Fatal(err)
			}
			hash, _ := permission.FingerprintInvocation("read", map[string]any{"path": "README.md"})
			candidate := domain.ToolPermissionReceipt{
				ToolPermissionOwner: domain.ToolPermissionOwner{SessionID: session.ID, ThreadID: threadID, TriggerEventID: trigger.ID, AttemptID: attempt.ID},
				ToolUseEventID:      "sevt_paused_permission", ToolName: "read", Model: session.AgentSnapshot.Model, InvocationHash: hash, ContextHash: strings.Repeat("a", 64),
			}
			allowed, err := store.AdmitToolPermissionEvaluation(ctx, candidate)
			if err != nil || allowed {
				t.Fatalf("budget pause = %v, %v", allowed, err)
			}
			interruptPayload := map[string]any{}
			if childTurn {
				interruptPayload["session_thread_id"] = threadID
			}
			interrupted, err := store.AdmitEvents(ctx, session.ID, []domain.EventDraft{{Type: domain.EvUserInterrupt, Payload: interruptPayload}})
			if err != nil || len(interrupted.Events) == 0 {
				t.Fatalf("active unpublished turn interrupt was dropped: %+v, %v", interrupted.Events, err)
			}
			allowed, err = store.AdmitToolPermissionEvaluation(ctx, candidate)
			if allowed {
				t.Fatal("interrupted evaluator was admitted")
			}
			requirePermissionConflict(t, err)
			if childTurn {
				_, err = store.CompleteThreadWorkflowTurn(ctx, session.ID, threadID, trigger.ID, nil, domain.StatusIdle, attempt.ID, domain.RunAttemptInterrupted, nil, nil, nil, nil, nil, domain.TokenUsage{})
			} else {
				_, err = store.CompleteWorkflowTurn(ctx, session.ID, trigger.ID, nil, domain.StatusIdle, attempt.ID, domain.RunAttemptInterrupted, nil, nil, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			thread, err := store.GetSessionThread(ctx, session.ID, threadID)
			if err != nil || thread.BudgetPaused {
				t.Fatalf("interrupted round retains budget pause: %+v, %v", thread, err)
			}
			if _, err := store.UpdateSession(ctx, session.ID, domain.SessionUpdate{Budget: &domain.SessionBudgetUpdate{Budget: &domain.SessionBudget{MaxListCostCents: 2}}}); err != nil {
				t.Fatal(err)
			}
			thread, err = store.GetSessionThread(ctx, session.ID, threadID)
			if err != nil || thread.Status != domain.StatusIdle || thread.BudgetPaused {
				t.Fatalf("budget update revived interrupted round: %+v, %v", thread, err)
			}
			if _, err := store.AdmitToolPermissionEvaluation(ctx, candidate); err == nil {
				t.Fatal("old attempt was admitted after budget increase")
			}
		})
	}
}

func TestToolPermissionCachedReceiptBypassesSpentBudget(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	receipt := permissionReceipt(t, store, "sesn_permission_cached_budget")
	if _, err := store.RecordToolPermissionReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	// Establish a consumed ceiling in a controlled projection. No external
	// call is permitted here; only cache recovery may bypass budget admission.
	session, err := store.GetSession(ctx, receipt.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	session.Budget = &domain.SessionBudget{MaxListCostCents: 0}
	body, _ := json.Marshal(session)
	if _, err := store.pool.Exec(ctx, `UPDATE sessions SET body=$1 WHERE id=$2`, body, session.ID); err != nil {
		t.Fatal(err)
	}
	allowed, err := store.AdmitToolPermissionEvaluation(ctx, receipt)
	if err != nil || !allowed {
		t.Fatalf("already-accounted cache hit was budget blocked = %v, %v", allowed, err)
	}
	uncached := receipt
	uncached.ToolUseEventID = "sevt_new_permission"
	allowed, err = store.AdmitToolPermissionEvaluation(ctx, uncached)
	if err != nil || allowed {
		t.Fatalf("uncached request bypassed budget = %v, %v", allowed, err)
	}
}

func TestToolPermissionDeletionCascadesFacts(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	receipt := permissionReceipt(t, store, "sesn_permission_delete")
	if _, err := store.RecordToolPermissionReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdmitEvents(ctx, receipt.SessionID, []domain.EventDraft{{Type: domain.EvUserInterrupt, Payload: map[string]any{}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteWorkflowTurn(ctx, receipt.SessionID, receipt.TriggerEventID, nil, domain.StatusIdle, receipt.AttemptID, domain.RunAttemptInterrupted, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSession(ctx, receipt.SessionID); err != nil {
		t.Fatal(err)
	}
	var facts, usage int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM tool_permission_evaluations WHERE session_id=$1`, receipt.SessionID).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM model_request_usage WHERE session_id=$1`, receipt.SessionID).Scan(&usage); err != nil || facts != 0 || usage != 0 {
		t.Fatalf("deleted receipt facts=%d usage=%d err=%v", facts, usage, err)
	}
	if _, err := store.RecordToolPermissionReceipt(ctx, receipt); err == nil {
		t.Fatal("late response recreated deleted Session")
	}
}

func TestToolPermissionAttemptCannotBeRecreatedAfterCompletion(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	receipt := permissionReceipt(t, store, "sesn_permission_consumed")
	if _, err := store.CompleteWorkflowTurn(ctx, receipt.SessionID, receipt.TriggerEventID, nil, domain.StatusIdle, receipt.AttemptID, domain.RunAttemptCompleted, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	receipt.AttemptID = "ratm_delayed_permission"
	_, err := store.EnsureToolPermissionAttempt(ctx, receipt.ToolPermissionOwner)
	requirePermissionConflict(t, err)
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM turn_attempts WHERE session_id=$1`, receipt.SessionID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("delayed evaluator recreated a turn = %d, %v", count, err)
	}
}

func TestToolPermissionFallbackDoesNotInventUsage(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	receipt := permissionReceipt(t, store, "sesn_permission_no_response")
	receipt.ResponseReceived = false
	receipt.Usage = domain.TokenUsage{}
	receipt.StopReason = ""
	receipt.Model.ID = "unpriced-model"
	receipt.Decision = domain.ToolPermissionDecision{Type: "ask", ReasonCode: "indeterminate"}
	if _, err := store.RecordToolPermissionReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	session, err := store.GetSession(ctx, receipt.SessionID)
	if err != nil || !session.ListCostKnown || session.Usage.InputTokens != 0 {
		t.Fatalf("fallback invented unknown spend = %+v, %v", session, err)
	}
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM model_request_usage WHERE session_id=$1`, receipt.SessionID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("fallback invented a response = %d, %v", count, err)
	}
}

func TestToolPermissionRejectsConflictingAccountingIdentity(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	receipt := permissionReceipt(t, store, "sesn_permission_account_conflict")
	if err := store.AccountModelRequest(ctx, receipt.SessionID, receipt.ThreadID, receipt.ToolUseEventID, receipt.Model, domain.TokenUsage{InputTokens: 999}, "end_turn"); err != nil {
		t.Fatal(err)
	}
	_, err := store.RecordToolPermissionReceipt(ctx, receipt)
	requirePermissionConflict(t, err)
	got, err := store.GetToolPermissionReceipt(ctx, receipt)
	if err != nil || got != nil {
		t.Fatalf("receipt committed over conflicting accounting = %+v, %v", got, err)
	}
}

func TestToolPermissionLateUsagePreservesTerminalProjection(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	receipt := permissionReceipt(t, store, "sesn_permission_terminated")
	if _, err := store.CompleteWorkflowTurn(ctx, receipt.SessionID, receipt.TriggerEventID, nil, domain.StatusTerminated, receipt.AttemptID, domain.RunAttemptCompleted, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordToolPermissionReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	session, err := store.GetSession(ctx, receipt.SessionID)
	if err != nil || session.Status != domain.StatusTerminated || session.Usage.InputTokens != 200 {
		t.Fatalf("late billing revived terminal Session = %+v, %v", session, err)
	}
	thread, err := store.GetSessionThread(ctx, receipt.SessionID, receipt.ThreadID)
	if err != nil || thread.Status != domain.StatusTerminated || thread.Usage.InputTokens != 200 {
		t.Fatalf("late billing revived terminal Thread = %+v, %v", thread, err)
	}
	_, err = store.AdmitToolPermissionEvaluation(ctx, receipt)
	requirePermissionConflict(t, err)
}

func automaticPermissionDraft(receipt domain.ToolPermissionReceipt) domain.EventDraft {
	return domain.EventDraft{ID: receipt.ToolUseEventID, Type: domain.EvAgentToolUse, Payload: map[string]any{"name": receipt.ToolName, "input": map[string]any{"path": "README.md"}, "evaluated_permission": receipt.Decision.Type, "evaluation": domain.ToolPermissionEvaluation{Type: "auto", EvaluatedPermission: &receipt.Decision}, domain.InternalToolExecutionOwner: "self_hosted", domain.InternalPermissionToolName: receipt.ToolName}}
}
func TestToolPermissionPublicationRequiresExactCommittedJudgment(t *testing.T) {
	for _, scenario := range []string{"valid", "missing", "input", "outcome", "owner"} {
		t.Run(scenario, func(t *testing.T) {
			store := testStore(t)
			ctx := context.Background()
			receipt := permissionReceipt(t, store, "sesn_permission_publication")
			if scenario != "missing" {
				if _, err := store.RecordToolPermissionReceipt(ctx, receipt); err != nil {
					t.Fatal(err)
				}
			}
			draft := automaticPermissionDraft(receipt)
			switch scenario {
			case "input":
				draft.Payload["input"] = map[string]any{"path": "secrets.env"}
			case "outcome":
				draft.Payload["evaluated_permission"] = "ask"
			case "owner":
				draft.ID = "sevt_unassessed"
			}
			_, err := store.CompleteWorkflowTurn(ctx, receipt.SessionID, receipt.TriggerEventID, []domain.EventDraft{draft, requiresActionDraft([]string{draft.ID})}, domain.StatusIdle, receipt.AttemptID, domain.RunAttemptCompleted, nil, []string{draft.ID}, nil)
			if scenario == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				pending, err := store.UnresolvedPendingActions(ctx, receipt.SessionID)
				if err != nil || len(pending) != 1 || pending[0].Kind != domain.PendingToolResult {
					t.Fatalf("pending=%+v, %v", pending, err)
				}
			} else {
				requirePermissionConflict(t, err)
				pending, err := store.UnresolvedPendingActions(ctx, receipt.SessionID)
				if err != nil || len(pending) != 0 {
					t.Fatalf("unassessed Work published: %+v, %v", pending, err)
				}
			}
		})
	}
}
