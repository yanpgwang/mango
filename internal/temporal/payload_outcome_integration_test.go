package temporal

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/pg"
)

type payloadOutcomeClock struct{}

func (payloadOutcomeClock) Now() time.Time { return time.Now() }

type payloadOutcomeModel struct {
	mode    string
	working atomic.Int64
	grading atomic.Int64
}

func (m *payloadOutcomeModel) CreateMessage(_ context.Context, request model.Request) (model.Response, error) {
	if strings.HasPrefix(request.System, outcomeGraderSystem) {
		m.grading.Add(1)
		body, _ := json.Marshal(map[string]any{"result": "satisfied", "explanation": strings.Repeat("x", 700_000)})
		return model.Response{StopReason: "end_turn", Usage: domain.TokenUsage{InputTokens: 13, OutputTokens: 7, Speed: "fast"},
			Content: []domain.ContentBlock{{Type: "text", Text: string(body)}}}, nil
	}
	count := m.working.Add(1)
	text := strings.Repeat("x", 350_000)
	if m.mode == "model_response" {
		text = strings.Repeat("<", 400_000)
	}
	if m.mode == "input" {
		text = "prior reply"
		if count > 1 {
			text = strings.Repeat("x", 700_000)
		}
	}
	return model.Response{StopReason: "end_turn", Usage: domain.TokenUsage{InputTokens: 123, OutputTokens: 42},
		Content: []domain.ContentBlock{{Type: "text", Text: text}}}, nil
}

func (m *payloadOutcomeModel) CreateMessageStream(ctx context.Context, request model.Request, _ func(int, string)) (model.Response, error) {
	return m.CreateMessage(ctx, request)
}

// Exercise the actual Workflow, converter, Activities, completion transaction,
// and durable Outcome projection. Only inference is simulated. The interrupt
// case admits its interrupt immediately before completion's PG transaction,
// making its existing winner rule deterministic without timing a cancellation.
func TestPayloadLimitOutcomeDurableRealTemporal(t *testing.T) {
	database, address := os.Getenv("MANGO_TEST_DATABASE_URL"), os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	if database == "" || address == "" {
		t.Skip("set PostgreSQL and Temporal service-test addresses")
	}
	for _, mode := range []string{"prepare", "model_response", "input", "completion", "interrupt"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			ids := domain.NewRandomIDGen()
			schema := "payload_outcome_" + ids.NewID("")
			config, err := pgxpool.ParseConfig(database)
			require.NoError(t, err)
			config.ConnConfig.RuntimeParams["search_path"] = schema
			pool, err := pgxpool.NewWithConfig(ctx, config)
			require.NoError(t, err)
			defer pool.Close()
			_, err = pool.Exec(ctx, "CREATE SCHEMA "+schema)
			require.NoError(t, err, "explicit service invocation requires reachable PostgreSQL")
			defer func() {
				_, err := pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
				require.NoError(t, err)
			}()
			require.NoError(t, pg.Migrate(ctx, pool))
			store := pg.NewDefaultWorkspaceStore(pool, ids, payloadOutcomeClock{})
			source := storeSource{store: store}
			probe := &payloadOutcomeModel{mode: mode}
			acts := NewActivities(probe, source, source, ids)
			connection, err := client.Dial(client.Options{HostPort: address})
			require.NoError(t, err)
			defer connection.Close()
			queue := "payload-outcome-" + ids.NewID("")
			w := worker.New(connection, queue, worker.Options{})
			w.RegisterWorkflow(workflowTurnHarness)
			for name, fn := range map[string]any{
				ActivityPrepareTurn: acts.PrepareTurn, ActivityAdmitModelRequest: acts.AdmitModelRequest,
				ActivityStartModelRequest: acts.StartModelRequest, ActivityCallModel: acts.CallModel,
				ActivityAccountModelRequest: acts.AccountModelRequest, ActivityEvaluateOutcome: acts.EvaluateOutcome,
				ActivityAppendWorkflowEvents: acts.AppendWorkflowEvents,
			} {
				w.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
			}
			completionInputs := make(chan CompleteWorkflowTurnInput, 1)
			w.RegisterActivityWithOptions(func(ctx context.Context, input CompleteWorkflowTurnInput) (RunTurnResult, error) {
				if input.Status == domain.StatusTerminated {
					completionInputs <- input
					if mode == "interrupt" {
						_, err := store.AdmitEvents(ctx, input.SessionID, []domain.EventDraft{{Type: domain.EvUserInterrupt, Payload: map[string]any{}}})
						if err != nil {
							return RunTurnResult{}, err
						}
					}
				}
				return acts.CompleteWorkflowTurn(ctx, input)
			}, activity.RegisterOptions{Name: ActivityCompleteWorkflowTurn})
			require.NoError(t, w.Start())
			defer w.Stop()
			now := time.Now().UTC()
			session := domain.Session{ID: ids.NewID(domain.PrefixSession), Status: domain.StatusIdle,
				AgentID: "agent_payload", AgentVersion: 1, EnvironmentID: "env_payload", ListCostKnown: true,
				Metadata: map[string]any{}, CreatedAt: now, UpdatedAt: now,
				AgentSnapshot: domain.Agent{ID: "agent_payload", Version: 1, Model: domain.Model{ID: "claude-opus-4-8"}},
			}
			_, err = store.CreateSession(ctx, session, nil)
			require.NoError(t, err)
			runTurn := func(triggerID string) {
				t.Helper()
				run, err := connection.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: queue + "-" + triggerID, TaskQueue: queue},
					workflowTurnHarness, PrepareTurnInput{SessionID: session.ID, TriggerEventID: triggerID})
				require.NoError(t, err)
				defer func() {
					_ = connection.TerminateWorkflow(context.Background(), run.GetID(), run.GetRunID(), "test cleanup")
				}()
				require.NoError(t, run.Get(ctx, nil), "capacity failure must close, rather than retry an oversized payload")
			}
			if mode == "input" {
				prior, err := store.AdmitEvents(ctx, session.ID, []domain.EventDraft{{Type: domain.EvUserMessage,
					Payload: map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.Repeat("x", 400_000)}}}}})
				require.NoError(t, err)
				runTurn(prior.Events[0].ID)
			}
			description, rubric := strings.Repeat("x", 50_000), "correct"
			if mode == "prepare" {
				description, rubric = "report", strings.Repeat("<", 200_000)
			}
			admitted, err := store.AdmitEvents(ctx, session.ID, []domain.EventDraft{{Type: domain.EvUserDefineOutcome,
				Payload: map[string]any{"description": description, "rubric": map[string]any{"type": "text", "content": rubric}, "max_iterations": 1}}})
			require.NoError(t, err)
			trigger := admitted.Events[0]
			runTurn(trigger.ID)
			final, err := store.GetSession(ctx, session.ID)
			require.NoError(t, err)
			require.Len(t, final.Outcomes, 1)
			wantResult, wantStatus := "failed", domain.StatusTerminated
			if mode == "interrupt" {
				wantResult, wantStatus = "interrupted", domain.StatusIdle
			}
			require.Equal(t, wantResult, final.Outcomes[0].Result)
			require.NotNil(t, final.Outcomes[0].CompletedAt)
			require.Nil(t, final.ActiveOutcome())
			require.Equal(t, wantStatus, final.Status)
			events, err := store.EventsAfter(ctx, session.ID, 0, 100)
			require.NoError(t, err)
			var starts, ends []domain.Event
			for _, event := range events {
				if event.Type == domain.EvSpanOutcomeEvaluationStart {
					starts = append(starts, event)
				}
				if event.Type == domain.EvSpanOutcomeEvaluationEnd {
					ends = append(ends, event)
				}
				if mode == "interrupt" {
					require.NotContains(t, []string{domain.EvSessionError, domain.EvSessionStatusTerminated}, event.Type)
				}
			}
			require.Len(t, ends, 1)
			require.Equal(t, trigger.Payload["outcome_id"], ends[0].Payload["outcome_id"])
			if mode == "prepare" || mode == "model_response" {
				require.Empty(t, starts)
				require.Empty(t, ends[0].Payload["outcome_evaluation_start_id"])
			} else {
				require.Len(t, starts, 1)
				require.Equal(t, starts[0].ID, ends[0].Payload["outcome_evaluation_start_id"])
			}
			wantWorking, wantGrading := int64(1), int64(1)
			switch mode {
			case "prepare":
				wantWorking, wantGrading = 0, 0
			case "model_response":
				wantGrading = 0
			case "input":
				wantWorking, wantGrading = 2, 0
			}
			require.Equal(t, wantWorking, probe.working.Load())
			require.Equal(t, wantGrading, probe.grading.Load())
			require.Equal(t, wantWorking*123+wantGrading*13, final.Usage.InputTokens)
			require.Equal(t, wantWorking*42+wantGrading*7, final.Usage.OutputTokens)
			var finalInput CompleteWorkflowTurnInput
			select {
			case finalInput = <-completionInputs:
			case <-ctx.Done():
				t.Fatal("terminal completion was not delivered")
			}
			require.Contains(t, finalInput.Output[len(finalInput.Output)-2].Payload["error"].(map[string]any)["message"], "capacity")
			requireBoundedTransport(t, finalInput)
		})
	}
}
