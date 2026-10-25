package pg

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
	"github.com/yanpgwang/mango/internal/workspace"
)

type idleStopFixture struct {
	store   *Store
	repo    *EnvironmentWorkRepository
	clock   *environmentWorkTestClock
	session domain.Session
	work    domain.EnvironmentWork
	workCtx context.Context
	idleID  string
}

func newIdleStopFixture(t *testing.T) *idleStopFixture {
	t.Helper()
	ctx := context.Background()
	store := testStore(t)
	clock := &environmentWorkTestClock{now: time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)}
	store.clock = clock
	session := newSession("sesn_idle_stop")
	session.EnvironmentID, session.EnvironmentType = "env_idle_stop", "self_hosted"
	require.NoError(t, NewEnvironmentRepository(store).Put(ctx, domain.Environment{
		ID: session.EnvironmentID, Name: "idle-stop", ConfigType: "self_hosted",
		Config: map[string]any{"type": "self_hosted"}, CreatedAt: clock.Now(), UpdatedAt: clock.Now(),
	}))
	admitted, err := store.CreateSession(ctx, session, []domain.EventDraft{{
		Type: domain.EvUserMessage, Payload: map[string]any{"content": "first turn"},
	}})
	require.NoError(t, err)
	repo := NewEnvironmentWorkRepository(store)
	work, err := repo.PollWork(ctx, session.EnvironmentID, app.EnvironmentWorkPollInput{})
	require.NoError(t, err)
	require.NotNil(t, work)
	_, err = repo.AckWork(ctx, session.EnvironmentID, work.ID)
	require.NoError(t, err)
	_, err = repo.HeartbeatWork(ctx, session.EnvironmentID, work.ID, nil, nil)
	require.NoError(t, err)
	workspaceID, claim, err := store.AuthenticateSessionToken(ctx, decodeSessionsToken(t, work.Secret))
	require.NoError(t, err)
	completed, err := store.CompleteWorkflowTurn(ctx, session.ID, admitted.SubmittedEvents[0].ID,
		[]domain.EventDraft{{Type: domain.EvSessionStatusIdle, Payload: map[string]any{"stop_reason": map[string]any{"type": "end_turn"}}}}, domain.StatusIdle, "", "", nil, nil, nil)
	require.NoError(t, err)
	boundary, err := store.GetEvent(ctx, session.ID, completed.Events[0].ID)
	require.NoError(t, err)
	require.NotNil(t, boundary.ProcessedAt, "idle guard requires the actual Workflow completion's committed output")
	return &idleStopFixture{store: store, repo: repo, clock: clock, session: session,
		work: *work, workCtx: workspace.WithSessionScope(ctx, workspaceID, claim), idleID: completed.Events[0].ID}
}

func (f *idleStopFixture) pendingTool(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	admitted, err := f.store.AdmitEvents(ctx, f.session.ID, []domain.EventDraft{{
		Type: domain.EvUserMessage, Payload: map[string]any{"content": "second turn"},
	}})
	require.NoError(t, err)
	triggerID := admitted.SubmittedEvents[0].ID
	actionID := "sevt_new_pending_tool"
	_, err = f.store.CompleteWorkflowTurn(ctx, f.session.ID, triggerID, []domain.EventDraft{
		{ID: actionID, Type: domain.EvAgentToolUse, Payload: map[string]any{
			"name": "bash", "input": map[string]any{"command": "echo recovered"}, "evaluated_permission": "allow",
			domain.InternalToolExecutionOwner: "self_hosted",
		}}, requiresActionDraft([]string{actionID}),
	}, domain.StatusIdle, "", "", nil, []string{actionID}, nil)
	require.NoError(t, err)
	trigger, err := f.store.GetEvent(ctx, f.session.ID, triggerID)
	require.NoError(t, err)
	require.NotNil(t, trigger.ProcessedAt, "proof requires already-processed new input")
	pending, err := f.store.UnresolvedPendingActions(ctx, f.session.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, domain.PendingToolResult, pending[0].Kind)
	return actionID
}

func TestConditionalIdleStopRejectsProcessedToolAndAllowsReclaim(t *testing.T) {
	f := newIdleStopFixture(t)
	f.pendingTool(t)
	before, err := f.repo.GetWork(context.Background(), f.work.EnvironmentID, f.work.ID)
	require.NoError(t, err)
	err = f.repo.StopWork(f.workCtx, f.work.EnvironmentID, f.work.ID, true, &f.idleID)
	requireDomainKind(t, err, domain.KindPrecondition)
	after, err := f.repo.GetWork(context.Background(), f.work.EnvironmentID, f.work.ID)
	require.NoError(t, err)
	require.Equal(t, before, after, "412 must neither stop nor renew the Work")
	f.clock.Advance(31 * time.Second)
	next, err := f.repo.PollWork(context.Background(), f.work.EnvironmentID, app.EnvironmentWorkPollInput{})
	require.NoError(t, err)
	require.NotNil(t, next, "pending worker tool must retain a reclaimable activation")
	require.Equal(t, f.work.ID, next.ID)
	require.NotEqual(t, f.work.Secret, next.Secret)
	_, err = f.repo.AckWork(context.Background(), next.EnvironmentID, next.ID)
	require.NoError(t, err)
	_, err = f.repo.HeartbeatWork(context.Background(), next.EnvironmentID, next.ID, nil, nil)
	require.NoError(t, err)
	err = f.repo.StopWork(f.workCtx, f.work.EnvironmentID, f.work.ID, true, &f.idleID)
	requireDomainKind(t, err, domain.KindPrecondition)
	workspaceID, claim, err := f.store.AuthenticateSessionToken(context.Background(), decodeSessionsToken(t, next.Secret))
	require.NoError(t, err)
	recoveredCtx := workspace.WithSessionScope(context.Background(), workspaceID, claim)
	_, err = f.store.AdmitEvents(recoveredCtx, f.session.ID, []domain.EventDraft{{
		Type: domain.EvUserToolResult, Payload: map[string]any{
			"tool_use_id": "sevt_new_pending_tool", "content": []any{map[string]any{"type": "text", "text": "recovered"}},
		},
	}})
	require.NoError(t, err, "reclaimed worker must be able to deliver the committed action's result")
}

func TestConditionalIdleStopAcceptsExactCurrentBoundary(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "graceful", true: "forced"}[force], func(t *testing.T) {
			f := newIdleStopFixture(t)
			require.NoError(t, f.repo.StopWork(f.workCtx, f.work.EnvironmentID, f.work.ID, force, &f.idleID))
			work, err := f.repo.GetWork(context.Background(), f.work.EnvironmentID, f.work.ID)
			require.NoError(t, err)
			want := domain.EnvironmentWorkStopping
			if force {
				want = domain.EnvironmentWorkStopped
			}
			require.Equal(t, want, work.State)
		})
	}
}

func TestConditionalIdleStopRejectsInactiveOrChangedSession(t *testing.T) {
	for _, state := range []string{"terminated", "archived", "deleting", "new_event", "wrong_id"} {
		t.Run(state, func(t *testing.T) {
			f := newIdleStopFixture(t)
			ctx := context.Background()
			expected := f.idleID
			switch state {
			case "terminated":
				_, err := f.store.pool.Exec(ctx, `UPDATE sessions SET status='terminated',body=jsonb_set(body,'{Status}','"terminated"') WHERE id=$1`, f.session.ID)
				require.NoError(t, err)
			case "archived":
				_, err := f.store.ArchiveSession(ctx, f.session.ID)
				require.NoError(t, err)
			case "deleting":
				_, err := f.store.pool.Exec(ctx, `UPDATE sessions SET deleting_at=now() WHERE id=$1`, f.session.ID)
				require.NoError(t, err)
			case "new_event":
				_, err := f.store.AdmitEvents(ctx, f.session.ID, []domain.EventDraft{{Type: domain.EvUserMessage, Payload: map[string]any{"content": "new activity"}}})
				require.NoError(t, err)
			case "wrong_id":
				expected = "sevt_missing"
			}
			requireDomainKind(t, f.repo.StopWork(f.workCtx, f.work.EnvironmentID, f.work.ID, true, &expected), domain.KindPrecondition)
			work, err := f.repo.GetWork(ctx, f.work.EnvironmentID, f.work.ID)
			require.NoError(t, err)
			require.Equal(t, domain.EnvironmentWorkActive, work.State)
		})
	}
}

// Both orders use real row-lock waits rather than scheduling sleeps. A new
// admission either invalidates retirement or creates the successor after it.
func TestConditionalIdleStopSerializesWithAdmission(t *testing.T) {
	t.Run("admission wins", func(t *testing.T) {
		f := newIdleStopFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tx, err := f.store.pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		q := pgstore.New(tx)
		row, err := q.LockSession(ctx, f.session.ID)
		require.NoError(t, err)
		session, err := sessionFromLockRow(row)
		require.NoError(t, err)
		_, err = f.store.admitLocked(ctx, tx, q, session, []domain.EventDraft{{Type: domain.EvUserMessage, Payload: map[string]any{"content": "new input"}}})
		require.NoError(t, err)
		stopped := make(chan error, 1)
		go func() { stopped <- f.repo.StopWork(f.workCtx, f.work.EnvironmentID, f.work.ID, true, &f.idleID) }()
		waitForCredentialSQLLock(t, ctx, f.store, "sessions", stopped)
		require.NoError(t, tx.Commit(ctx))
		requireDomainKind(t, <-stopped, domain.KindPrecondition)
		work, err := f.repo.GetWork(ctx, f.work.EnvironmentID, f.work.ID)
		require.NoError(t, err)
		require.Equal(t, domain.EnvironmentWorkActive, work.State)
	})
	t.Run("retirement wins", func(t *testing.T) {
		f := newIdleStopFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		blocker, err := f.store.pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = blocker.Rollback(ctx) }()
		_, err = blocker.Exec(ctx, "SELECT id FROM environment_work WHERE id=$1 FOR UPDATE", f.work.ID)
		require.NoError(t, err)
		stopped := make(chan error, 1)
		go func() { stopped <- f.repo.StopWork(f.workCtx, f.work.EnvironmentID, f.work.ID, true, &f.idleID) }()
		waitForCredentialSQLLock(t, ctx, f.store, "environment_work", stopped)
		admitted := make(chan error, 1)
		go func() {
			_, err := f.store.AdmitEvents(ctx, f.session.ID, []domain.EventDraft{{Type: domain.EvUserMessage, Payload: map[string]any{"content": "after retirement"}}})
			admitted <- err
		}()
		waitForCredentialSQLLock(t, ctx, f.store, "sessions", admitted)
		require.NoError(t, blocker.Commit(ctx))
		require.NoError(t, <-stopped)
		require.NoError(t, <-admitted)
		next, err := f.repo.PollWork(ctx, f.work.EnvironmentID, app.EnvironmentWorkPollInput{})
		require.NoError(t, err)
		require.NotNil(t, next, "post-retirement input must have a successor activation")
		require.NotEqual(t, f.work.ID, next.ID)
	})
}

func TestConditionalIdleStopRejectsNonPrimaryOrUncommittedBoundary(t *testing.T) {
	for _, state := range []string{"other_session", "child", "unprocessed", "active_attempt", "requires_action"} {
		t.Run(state, func(t *testing.T) {
			f := newIdleStopFixture(t)
			ctx := context.Background()
			switch state {
			case "other_session":
				other := newSession("sesn_other_idle")
				_, err := f.store.CreateSession(ctx, other, nil)
				require.NoError(t, err)
				_, err = f.store.pool.Exec(ctx, "UPDATE events SET session_id=$2,thread_id=(SELECT id FROM session_threads WHERE session_id=$2 AND kind='primary') WHERE id=$1", f.idleID, other.ID)
				require.NoError(t, err)
			case "child":
				// Change the otherwise exact event's Thread identity; no newer seq can
				// account for rejection, so this tests the primary boundary requirement.
				_, err := f.store.pool.Exec(ctx, "UPDATE session_threads SET kind='child',parent_thread_id=id WHERE session_id=$1", f.session.ID)
				require.NoError(t, err)
			case "unprocessed":
				_, err := f.store.pool.Exec(ctx, "UPDATE events SET processed_at=NULL WHERE id=$1", f.idleID)
				require.NoError(t, err)
			case "active_attempt":
				_, err := f.store.pool.Exec(ctx, `INSERT INTO turn_attempts (id,session_id,trigger_event_id,attempt_no,state,created_at,updated_at) VALUES ('attempt_idle_stop',$1,$2,1,'active',now(),now())`, f.session.ID, f.idleID)
				require.NoError(t, err)
			case "requires_action":
				_, err := f.store.pool.Exec(ctx, `UPDATE events SET payload='{"stop_reason":{"type":"requires_action","event_ids":[]}}'::jsonb WHERE id=$1`, f.idleID)
				require.NoError(t, err)
			}
			requireDomainKind(t, f.repo.StopWork(f.workCtx, f.work.EnvironmentID, f.work.ID, true, &f.idleID), domain.KindPrecondition)
		})
	}
}

func TestConditionalIdleStopRejectsExpiredLeaseAndHealthcheck(t *testing.T) {
	t.Run("expired lease", func(t *testing.T) {
		f := newIdleStopFixture(t)
		f.clock.Advance(31 * time.Second)
		requireDomainKind(t, f.repo.StopWork(f.workCtx, f.work.EnvironmentID, f.work.ID, true, &f.idleID), domain.KindPrecondition)
		work, err := f.repo.GetWork(context.Background(), f.work.EnvironmentID, f.work.ID)
		require.NoError(t, err)
		require.Equal(t, domain.EnvironmentWorkActive, work.State)
	})
	t.Run("healthcheck", func(t *testing.T) {
		f := newIdleStopFixture(t)
		ctx := context.Background()
		check, err := f.repo.CreateHealthcheck(ctx, f.work.EnvironmentID)
		require.NoError(t, err)
		requireDomainKind(t, f.repo.StopWork(ctx, check.EnvironmentID, check.ID, true, &f.idleID), domain.KindValidation)
		require.NoError(t, f.repo.StopWork(ctx, check.EnvironmentID, check.ID, true, nil), "operator healthcheck Stop remains unconditional")
	})
}

func TestConditionalIdleStopRejectsForeignScopeBeforeSessionLock(t *testing.T) {
	f := newIdleStopFixture(t)
	ctx := context.Background()
	// Holding the target Session lock makes any premature lookup observable.
	blocker, err := f.store.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = blocker.Rollback(ctx) }()
	_, err = pgstore.New(blocker).LockSession(ctx, f.session.ID)
	require.NoError(t, err)
	scope, _ := workspace.FromContext(f.workCtx)
	for _, mismatch := range []string{"work", "session", "digest"} {
		t.Run(mismatch, func(t *testing.T) {
			claim := *scope.Session
			switch mismatch {
			case "work":
				claim.WorkID = "other_work"
			case "session":
				claim.SessionID = "other_session"
			case "digest":
				claim.CredentialDigest = nil
			}
			bounded, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
			defer cancel()
			scoped := workspace.WithSessionScope(bounded, scope.ID, claim)
			err := f.repo.StopWork(scoped, f.work.EnvironmentID, f.work.ID, true, &f.idleID)
			requireDomainKind(t, err, domain.KindPrecondition)
			require.NoError(t, bounded.Err(), "foreign scope must fail before waiting on the Session")
		})
	}
}
