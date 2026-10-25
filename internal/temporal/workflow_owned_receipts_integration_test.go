package temporal

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/pg"
)

// No evaluator is executed. Trusted test admission uses the new storage kernel;
// actual existing child Workflows, Activities and Thread/outbox transactions do
// all execution. Only inference and a post-commit completion ACK are simulated.
func TestWorkflowOwnedReceiptsUseExistingThreadRunner(t *testing.T) {
	database, address := os.Getenv("MANGO_TEST_DATABASE_URL"), os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	if database == "" || address == "" {
		t.Skip("set PostgreSQL and Temporal service-test addresses")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	ids := domain.NewRandomIDGen()
	schema := "workflow_owned_" + ids.NewID("")
	config, err := pgxpool.ParseConfig(database)
	require.NoError(t, err)
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)
	defer pool.Close()
	_, err = pool.Exec(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	defer func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		require.NoError(t, err)
	}()
	require.NoError(t, pg.Migrate(ctx, pool))
	store := pg.NewDefaultWorkspaceStore(pool, ids, payloadOutcomeClock{})
	connection, err := client.Dial(client.Options{HostPort: address})
	require.NoError(t, err)
	defer connection.Close()
	queue := "workflow-owned-" + ids.NewID("")
	probe := &workflowOwnedTextModel{}
	source := storeSource{store: store}
	acts := NewActivities(probe, source, source, ids)
	w := NewWorkerOnTaskQueue(connection, acts, queue)
	var lost atomic.Bool
	var completionCalls atomic.Int64
	w.RegisterActivityWithOptions(func(actx context.Context, input CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completionCalls.Add(1)
		out, err := acts.CompleteWorkflowTurn(actx, input)
		if err == nil && lost.CompareAndSwap(false, true) {
			return RunTurnResult{}, errors.New("injected owned-receipt acknowledgement loss")
		}
		return out, err
	}, activity.RegisterOptions{Name: ActivityCompleteWorkflowTurn, DisableAlreadyRegisteredCheck: true})
	require.NoError(t, w.Start())
	defer w.Stop()
	now := time.Now().UTC()
	session := domain.Session{ID: ids.NewID(domain.PrefixSession), AgentID: "agent_primary", AgentVersion: 1,
		EnvironmentID: "env_receipt", Status: domain.StatusIdle, Metadata: map[string]any{}, CreatedAt: now, UpdatedAt: now,
		AgentSnapshot:    domain.Agent{ID: "agent_primary", Version: 1, Model: domain.Model{ID: "must-not-run"}},
		MultiagentRoster: []domain.Agent{{ID: "agent_review", Version: 1, Name: "reviewer", Model: domain.Model{ID: "owned-text"}}},
	}
	_, err = store.CreateSession(ctx, session, nil)
	require.NoError(t, err)
	run, err := store.AdmitWorkflowRun(ctx, session.ID, "test-admission", pg.WorkflowRunRequest{
		Source: "unexecuted fixture", RuntimeProfile: "test-only/no-evaluator", Input: []byte(`{}`), Deadline: now.Add(time.Minute), MaxInvocations: 2, MaxResultBytes: 1024,
	})
	require.NoError(t, err)
	attempts := make([]pg.WorkflowInvocationAttempt, 2)
	for ordinal := range attempts {
		attempts[ordinal], err = store.AdmitWorkflowInvocation(ctx, session.ID, run.ID, ordinal, pg.WorkflowInvocationCommand{AgentName: "reviewer", Prompt: "Review this test input."})
		require.NoError(t, err)
	}
	defer func() {
		for _, attempt := range attempts {
			err := connection.TerminateWorkflow(context.Background(), sessionThreadWorkflowID(attempt.ThreadID), "", "owned receipt test cleanup")
			var missing *serviceerror.NotFound
			if !errors.As(err, &missing) {
				require.NoError(t, err)
			}
		}
	}()
	relay := NewRelay(store, NewSignalerOnTaskQueue(connection, queue), RelayConfig{})
	require.Eventually(t, func() bool {
		if _, err := relay.RunOnce(ctx); err != nil {
			return false
		}
		for _, attempt := range attempts {
			stored, err := store.GetWorkflowInvocationAttempt(ctx, session.ID, attempt.ID)
			if err != nil || stored.Receipt == nil {
				return false
			}
		}
		// A receipt existing does not prove its lost completion ACK was replayed.
		for _, attempt := range attempts {
			state, err := connection.DescribeWorkflowExecution(ctx, sessionThreadWorkflowID(attempt.ThreadID), "")
			if err != nil || len(state.PendingActivities) != 0 {
				return false
			}
		}
		return completionCalls.Load() >= 3
	}, 25*time.Second, 50*time.Millisecond)
	require.EqualValues(t, 2, probe.calls.Load(), "ACK loss must not repeat inference")
	for _, attempt := range attempts {
		stored, err := store.GetWorkflowInvocationAttempt(ctx, session.ID, attempt.ID)
		require.NoError(t, err)
		require.Equal(t, &pg.WorkflowInvocationReceipt{AdapterVersion: 1, Type: "text", Text: "owned terminal answer"}, stored.Receipt)
		var eventID string
		require.NoError(t, pool.QueryRow(ctx, "SELECT terminal_event_id FROM workflow_invocation_attempts WHERE id=$1", attempt.ID).Scan(&eventID))
		event, err := store.GetEvent(ctx, session.ID, eventID)
		require.NoError(t, err)
		require.Equal(t, attempt.ThreadID, event.ThreadID)
		require.Equal(t, domain.EvAgentMessage, event.Type)
		thread, err := store.GetSessionThread(ctx, session.ID, attempt.ThreadID)
		require.NoError(t, err)
		require.EqualValues(t, 5, thread.Usage.InputTokens)
	}
	wakes, err := store.WorkflowRunWakeups(ctx)
	require.NoError(t, err)
	require.Len(t, wakes, 1, "ordinary relay must leave the unsupported Run pump queue untouched")
	require.EqualValues(t, 5, wakes[0].Version)
	_, primaryWake, err := store.PendingWakeup(ctx, session.ID)
	require.NoError(t, err)
	require.False(t, primaryWake)
	primaryEvents, err := store.EventsAfter(ctx, session.ID, 0, 100)
	require.NoError(t, err)
	for _, event := range primaryEvents {
		require.NotEqual(t, domain.EvAgentThreadMessageReceived, event.Type)
	}
}

type workflowOwnedTextModel struct{ calls atomic.Int64 }

func (m *workflowOwnedTextModel) CreateMessage(_ context.Context, request model.Request) (model.Response, error) {
	if request.Model != "owned-text" {
		return model.Response{}, errors.New("primary must not receive an invocation result as an ordinary report")
	}
	foundPrompt := false
	for _, message := range request.Messages {
		for _, block := range message.Content {
			foundPrompt = foundPrompt || strings.Contains(block.Text, "Review this test input.")
		}
	}
	if !foundPrompt {
		return model.Response{}, errors.New("owned Thread context omitted its admitted prompt")
	}
	m.calls.Add(1)
	return model.Response{StopReason: "end_turn", Content: []domain.ContentBlock{{Type: "text", Text: "owned terminal answer"}}, Usage: domain.TokenUsage{InputTokens: 5, OutputTokens: 3}}, nil
}

func (m *workflowOwnedTextModel) CreateMessageStream(ctx context.Context, request model.Request, _ func(int, string)) (model.Response, error) {
	return m.CreateMessage(ctx, request)
}
