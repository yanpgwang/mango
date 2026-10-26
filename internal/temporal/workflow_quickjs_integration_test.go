package temporal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/pg"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

const quickJSParallelSource = `// PRIVATE_REAL_QUICKJS_SOURCE_SENTINEL
const order = [];
const a = agent(args.prefix + ":" + args.topic, {agent:"reviewer"}).then(text => {
  order.push("A"); return JSON.parse(text);
});
const b = agent("B:" + args.topic, {agent:"reviewer"}).then(text => {
  order.push("B"); return text;
});
const [facts, note] = await Promise.all([a,b]);
return {total:facts.count + args.offset, note, order, tag:args.tag};`
const quickJSSequentialSource = `// PRIVATE_REAL_QUICKJS_SOURCE_SENTINEL
const facts = JSON.parse(await agent(args.prefix + ":" + args.topic, {agent:"reviewer"}));
const note = await agent("B:" + facts.count, {agent:"reviewer"});
return {total:facts.count + args.offset, note};`

const quickJSFailureSource = `// PRIVATE_REAL_QUICKJS_SOURCE_SENTINEL
let caught = "";
try { await agent(args.prefix + ":" + args.topic, {agent:"reviewer"}); }
catch (error) { caught = error.code; }
const note = await agent("B:" + caught, {agent:"reviewer"});
return {caught, note};`

type quickJSVerticalFixture struct {
	ctx                   context.Context
	pool                  *pgxpool.Pool
	store                 *pg.Store
	client                client.Client
	ids                   domain.IDGenerator
	queue, image, profile string
}

func newQuickJSVerticalFixture(t *testing.T, image string) *quickJSVerticalFixture {
	t.Helper()
	require.NotEmpty(t, os.Getenv("MANGO_TEST_DATABASE_URL"), "explicit disposable database endpoint required")
	require.NotEmpty(t, os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT"), "explicit disposable Temporal endpoint required")
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	t.Cleanup(cancel)
	ids := domain.NewRandomIDGen()
	schema := "quickjs_vertical_" + ids.NewID("")
	cfg, err := pgxpool.ParseConfig(os.Getenv("MANGO_TEST_DATABASE_URL"))
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		require.NoError(t, err)
	})
	require.NoError(t, pg.Migrate(ctx, pool))
	c, err := client.Dial(client.Options{HostPort: os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")})
	require.NoError(t, err)
	t.Cleanup(c.Close)
	return &quickJSVerticalFixture{ctx: ctx, pool: pool, store: pg.NewDefaultWorkspaceStore(pool, ids, payloadOutcomeClock{}), client: c, ids: ids, queue: "quickjs-vertical-" + ids.NewID(""), image: image, profile: "test-only/quickjs-study/v1/" + image}
}
func (f *quickJSVerticalFixture) admit(t *testing.T, source string) pg.WorkflowRun {
	return f.admitRequest(t, pg.WorkflowRunRequest{Source: source, RuntimeProfile: f.profile, Input: json.RawMessage(`{"prefix":"A","topic":"workflow","offset":5,"tag":{"study":true}}`), Deadline: time.Now().UTC().Add(time.Hour), MaxInvocations: 2, MaxResultBytes: 1024})
}

func (f *quickJSVerticalFixture) admitRequest(t *testing.T, request pg.WorkflowRunRequest) pg.WorkflowRun {
	return f.admitWithPrimary(t, request, "must-not-run")
}
func (f *quickJSVerticalFixture) admitWithPrimary(t *testing.T, request pg.WorkflowRunRequest, primaryModel string) pg.WorkflowRun {
	t.Helper()
	now := time.Now().UTC()
	session := domain.Session{ID: f.ids.NewID(domain.PrefixSession), AgentID: "primary", AgentVersion: 1, EnvironmentID: "env_quickjs_proof", Status: domain.StatusIdle, Metadata: map[string]any{}, CreatedAt: now, UpdatedAt: now, AgentSnapshot: domain.Agent{ID: "primary", Version: 1, Model: domain.Model{ID: primaryModel}}, MultiagentRoster: []domain.Agent{{ID: "reviewer", Name: "reviewer", Version: 1, Model: domain.Model{ID: "quickjs-oracle"}}}}
	if primaryModel != "must-not-run" {
		session.AgentSnapshot.Multiagent = &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{{Type: "agent", ID: "reviewer", Version: 1}}}
	}
	_, err := f.store.CreateSession(f.ctx, session, nil)
	require.NoError(t, err)
	run, err := f.store.AdmitWorkflowRun(f.ctx, session.ID, "frozen-js", request)
	require.NoError(t, err)
	t.Cleanup(func() {
		threads, err := f.store.ListChildSessionThreadIDs(context.Background(), session.ID)
		require.NoError(t, err)
		ids := []string{run.SessionID}
		rows, err := f.pool.Query(context.Background(), `SELECT id FROM workflow_runs WHERE session_id=$1`, run.SessionID)
		require.NoError(t, err)
		for rows.Next() {
			var rid string
			require.NoError(t, rows.Scan(&rid))
			ids = append(ids, workflowRunWorkflowID(rid))
		}
		require.NoError(t, rows.Err())
		rows.Close()
		for _, thread := range threads {
			ids = append(ids, sessionThreadWorkflowID(thread))
		}
		for _, id := range ids {
			err := f.client.TerminateWorkflow(context.Background(), id, "", "QuickJS fixture cleanup")
			var missing *serviceerror.NotFound
			if !errors.As(err, &missing) {
				require.NoError(t, err)
			}
		}
	})
	return run
}

type quickJSOracleModel struct {
	mu           sync.Mutex
	prompts      []string
	secondPrompt string
	failA        bool
	releaseA     chan struct{}
	release      sync.Once
}

func (m *quickJSOracleModel) releaseFirst() { m.release.Do(func() { close(m.releaseA) }) }
func (m *quickJSOracleModel) CreateMessage(ctx context.Context, in model.Request) (model.Response, error) {
	if in.Model != "quickjs-oracle" {
		return model.Response{}, errors.New("unexpected primary inference")
	}
	prompt := ""
	for _, message := range in.Messages {
		for _, block := range message.Content {
			for _, candidate := range []string{"A:workflow", m.secondPrompt} {
				if strings.Contains(block.Text, candidate) {
					prompt = candidate
				}
			}
		}
	}
	if prompt == "" {
		return model.Response{}, errors.New("JS source did not drive the independently expected Thread input")
	}
	m.mu.Lock()
	for _, seen := range m.prompts {
		if seen == prompt {
			m.mu.Unlock()
			return model.Response{}, errors.New("duplicate model inference")
		}
	}
	m.prompts = append(m.prompts, prompt)
	m.mu.Unlock()
	text := "beta-text"
	if prompt == "A:workflow" {
		select {
		case <-m.releaseA:
		case <-ctx.Done():
			return model.Response{}, ctx.Err()
		}
		text = `{"count":7,"type":"evidence"}`
		if m.failA {
			text = strings.Repeat("oversized", 256)
		}
	}
	return model.Response{StopReason: "end_turn", Content: []domain.ContentBlock{{Type: "text", Text: text}}, Usage: domain.TokenUsage{InputTokens: 7, OutputTokens: 4}}, nil
}
func (m *quickJSOracleModel) CreateMessageStream(ctx context.Context, in model.Request, _ func(int, string)) (model.Response, error) {
	return m.CreateMessage(ctx, in)
}

type quickJSACKLossStore struct {
	*pg.Store
	notice chan pg.WorkflowPumpRequest
	once   sync.Once
	mu     sync.Mutex
	lost   bool
}

func (s *quickJSACKLossStore) CommitWorkflowPump(ctx context.Context, sid, rid string, in pg.WorkflowPumpRequest) (pg.WorkflowPumpDecision, error) {
	out, err := s.Store.CommitWorkflowPump(ctx, sid, rid, in)
	if err == nil && in.ExpectedCursor == 2 && in.Command.Kind == "deliver" && in.Command.InvocationCursor == 1 {
		s.mu.Lock()
		s.lost = true
		s.mu.Unlock()
		s.once.Do(func() { s.notice <- in })
		return pg.WorkflowPumpDecision{}, errors.New("injected postcommit SQL ACK loss")
	}
	return out, err
}
func (s *quickJSACKLossStore) wasLost() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.lost }

func TestQuickJSRunVerticalRealServices(t *testing.T) {
	if os.Getenv("MANGO_TEST_WORKFLOW_QUICKJS") != "1" {
		t.Skip("explicit real QuickJS/PG/Temporal proof only")
	}
	require.NotEmpty(t, os.Getenv("MANGO_TEST_DATABASE_URL"))
	require.NotEmpty(t, os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT"))
	image := os.Getenv("MANGO_TEST_QUICKJS_IMAGE")
	if image == "" {
		image = "mango-workflow-quickjs-vertical:study"
	}
	encoded, err := quickJSDocker(t.Context(), "image", "inspect", "--format", "{{.Id}} {{.Os}}/{{.Architecture}}", image)
	require.NoError(t, err, "build the pinned study candidate first")
	identity := strings.Fields(string(encoded))
	require.Len(t, identity, 2)
	image = identity[0]
	t.Logf("candidate=%s platform=%s", image, identity[1])
	evidence := map[string]any{"image": image, "platform": identity[1], "production_engine_selected": false}
	for _, mode := range []string{"parallel-recovery", "sequential-interleaving", "timeout", "memory", "effect-then-timeout", "owned-failure-fallback", "replay-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			f := newQuickJSVerticalFixture(t, image)
			source := quickJSParallelSource
			switch mode {
			case "sequential-interleaving":
				source = quickJSSequentialSource
			case "replay-mismatch":
				source = quickJSSequentialSource
			case "owned-failure-fallback":
				source = quickJSFailureSource
			case "timeout":
				source = `try{while(true){}}catch(_){await agent("MUST_NOT_ESCAPE",{agent:"reviewer"});}return "escaped";`
			case "effect-then-timeout":
				source = `agent("MUST_NOT_ESCAPE",{agent:"reviewer"});while(true){}`
			case "memory":
				source = `let data=[];try{while(true){data.push(new Array(1000000).fill(1));}}catch(_){data=null;await agent("MUST_NOT_ESCAPE",{agent:"reviewer"});}return "escaped";`
			}
			oracle := &quickJSOracleModel{secondPrompt: "B:workflow", releaseA: make(chan struct{})}
			t.Cleanup(oracle.releaseFirst)
			if mode != "parallel-recovery" {
				oracle.releaseFirst()
			}
			if mode == "sequential-interleaving" {
				oracle.secondPrompt = "B:7"
			}
			if mode == "owned-failure-fallback" {
				oracle.failA = true
				oracle.secondPrompt = "B:output_too_large"
			}
			sourceStore := storeSource{store: f.store}
			threadActs := NewActivities(oracle, sourceStore, sourceStore, f.ids)
			evaluator := &quickJSTestEvaluator{image: image, profile: f.profile, killEnabled: mode == "parallel-recovery", killAtCursor: 1}
			if mode == "replay-mismatch" {
				evaluator.replaySourceFault = `return await agent("MUST_NOT_REPLACE_PREFIX",{agent:"reviewer"});`
			}
			loss := &quickJSACKLossStore{Store: f.store, notice: make(chan pg.WorkflowPumpRequest, 1)}
			createWorker := func(before bool, e *quickJSTestEvaluator) worker.Worker {
				w := NewWorkerOnTaskQueue(f.client, threadActs, f.queue)
				pumpStore := WorkflowRunStore(f.store)
				if before && mode == "parallel-recovery" {
					pumpStore = loss
				}
				acts := &WorkflowRunActivities{Store: pumpStore, Evaluator: e}
				RegisterWorkflowRunCoordinator(w, acts)
				w.RegisterWorkflowWithOptions(func(ctx workflow.Context, in WorkflowRunWorkflowInput) error {
					return workflowRunWorkflow(ctx, in, 2, 200*time.Millisecond)
				}, workflow.RegisterOptions{Name: WorkflowRunWorkflowType, DisableAlreadyRegisteredCheck: true})
				if before && mode == "parallel-recovery" {
					w.RegisterActivityWithOptions(func(ctx context.Context, in WorkflowRunPumpInput) (WorkflowRunPumpResult, error) {
						out, err := acts.Pump(ctx, in)
						if err == nil && in.Cursor == 2 && loss.wasLost() {
							return out, errors.New("injected committed Activity ACK loss until replacement")
						}
						return out, err
					}, activity.RegisterOptions{Name: ActivityWorkflowRunPump, DisableAlreadyRegisteredCheck: true})
				}
				return w
			}
			w := createWorker(true, evaluator)
			require.NoError(t, w.Start())
			t.Cleanup(func() { w.Stop() })
			run := f.admit(t, source)
			signaler := NewSignalerOnTaskQueue(f.client, f.queue)
			relayCtx, stopRelays := context.WithCancel(f.ctx)
			var relays sync.WaitGroup
			for _, runRelay := range []bool{true, false} {
				relays.Go(func() {
					if runRelay {
						_ = NewWorkflowRunRelay(f.store, signaler, RelayConfig{PollInterval: 100 * time.Millisecond}).Run(relayCtx)
					} else {
						_ = NewRelay(f.store, signaler, RelayConfig{PollInterval: 100 * time.Millisecond}).Run(relayCtx)
					}
				})
			}
			t.Cleanup(func() { stopRelays(); relays.Wait() })
			started := time.Now()
			var old pg.WorkflowPumpRequest
			adapters := []*quickJSTestEvaluator{evaluator}
			if mode == "parallel-recovery" {
				old = threadRecoveryReceive(t, f.ctx, loss.notice)
				require.Equal(t, int64(1), old.Command.InvocationCursor)
				oracle.releaseFirst()
				require.Eventually(t, func() bool {
					var settled int
					return f.pool.QueryRow(f.ctx, `SELECT count(*) FROM workflow_invocation_attempts WHERE run_id=$1 AND state='settled'`, run.ID).Scan(&settled) == nil && settled == 2
				}, 15*time.Second, 20*time.Millisecond)
				w.Stop()
				worker.PurgeStickyWorkflowCache()
				replacement := &quickJSTestEvaluator{image: image, profile: f.profile}
				adapters = append(adapters, replacement)
				w = createWorker(false, replacement)
				require.NoError(t, w.Start())
			}
			if mode == "timeout" || mode == "memory" || mode == "effect-then-timeout" || mode == "replay-mismatch" {
				require.Eventually(t, func() bool {
					state, err := f.client.DescribeWorkflowExecution(f.ctx, workflowRunWorkflowID(run.ID), "")
					return err == nil && state.WorkflowExecutionInfo.Status == enumspb.WORKFLOW_EXECUTION_STATUS_FAILED
				}, 15*time.Second, 30*time.Millisecond)
				require.Less(t, time.Since(started), 12*time.Second)
				var effects, returns int
				require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM workflow_invocations WHERE run_id=$1),(SELECT count(*) FROM workflow_pump_decisions WHERE run_id=$1)`, run.ID).Scan(&effects, &returns))
				if mode == "replay-mismatch" {
					require.Equal(t, 1, effects)
					require.Equal(t, 1, returns, "changed replay prefix cannot commit a new decision")
					var frozen []byte
					require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT request->>'source' FROM workflow_runs WHERE id=$1`, run.ID).Scan(&frozen))
					require.Equal(t, source, string(frozen), "fault injection never changes the frozen PG program")
				} else {
					require.Zero(t, effects)
					require.Zero(t, returns, "hard resource failure cannot become catchable fallback")
				}
				evaluator.mu.Lock()
				exits := append([]quickJSExit(nil), evaluator.exits...)
				evaluator.mu.Unlock()
				require.NotEmpty(t, exits)
				for _, exit := range exits {
					require.True(t, exit.Observed)
					if mode != "replay-mismatch" {
						require.NotZero(t, exit.Code)
					}
				}
				evidence[mode] = exits
				return
			}
			var result []byte
			require.Eventually(t, func() bool {
				return f.pool.QueryRow(f.ctx, `SELECT decision->'value' FROM workflow_pump_decisions WHERE run_id=$1 AND decision->>'type'='return'`, run.ID).Scan(&result) == nil
			}, 30*time.Second, 30*time.Millisecond)
			expected := `{"total":12,"note":"beta-text","order":["B","A"],"tag":{"study":true}}`
			if mode == "sequential-interleaving" {
				expected = `{"total":12,"note":"beta-text"}`
			}
			if mode == "owned-failure-fallback" {
				expected = `{"caught":"output_too_large","note":"beta-text"}`
			}
			require.JSONEq(t, expected, string(result))
			rows, err := f.pool.Query(f.ctx, `SELECT command,decision FROM workflow_pump_decisions WHERE run_id=$1 ORDER BY expected_cursor`, run.ID)
			require.NoError(t, err)
			var commands []pg.WorkflowPumpCommand
			var decisions []pg.WorkflowPumpDecision
			for rows.Next() {
				var c, d []byte
				require.NoError(t, rows.Scan(&c, &d))
				var command pg.WorkflowPumpCommand
				var decision pg.WorkflowPumpDecision
				require.NoError(t, json.Unmarshal(c, &command))
				require.NoError(t, json.Unmarshal(d, &decision))
				commands = append(commands, command)
				decisions = append(decisions, decision)
			}
			require.NoError(t, rows.Err())
			rows.Close()
			require.Len(t, commands, 5)
			if mode == "owned-failure-fallback" {
				require.Equal(t, "failure", decisions[1].Delivery.Receipt.Type)
				require.Equal(t, "output_too_large", decisions[1].Delivery.Receipt.ErrorCode)
			}
			if mode == "parallel-recovery" {
				require.Equal(t, []string{"invoke", "invoke", "deliver", "deliver", "return"}, []string{commands[0].Kind, commands[1].Kind, commands[2].Kind, commands[3].Kind, commands[4].Kind})
				require.EqualValues(t, 1, commands[2].InvocationCursor)
				require.Zero(t, commands[3].InvocationCursor)
				require.True(t, evaluator.killed.Load())
			} else {
				require.Equal(t, []string{"invoke", "deliver", "invoke", "deliver", "return"}, []string{commands[0].Kind, commands[1].Kind, commands[2].Kind, commands[3].Kind, commands[4].Kind})
				require.Zero(t, commands[1].InvocationCursor)
				require.EqualValues(t, 2, commands[3].InvocationCursor)
			}
			var count int
			var state, chain, owner string
			var epoch int64
			require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT count(*) FROM workflow_invocations WHERE run_id=$1`, run.ID).Scan(&count))
			require.Equal(t, 2, count)
			require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT state,coordinator_chain_id,coordinator_execution_id,evaluator_epoch FROM workflow_runs WHERE id=$1`, run.ID).Scan(&state, &chain, &owner, &epoch))
			require.Equal(t, "open", state, "candidate return is not terminal Run lifecycle")
			if mode == "parallel-recovery" {
				_, err = f.store.CommitWorkflowPump(f.ctx, run.SessionID, run.ID, pg.WorkflowPumpRequest{RequestID: "stale-js", Epoch: old.Epoch, ExpectedCursor: 3, CoordinatorExecutionID: owner, Command: pg.WorkflowPumpCommand{Kind: "invoke", AgentName: "reviewer", Prompt: "MUST_NOT_RUN"}})
				require.ErrorContains(t, err, "stale")
			}
			session, err := f.store.GetSession(f.ctx, run.SessionID)
			require.NoError(t, err)
			require.EqualValues(t, 14, session.Usage.InputTokens)
			require.EqualValues(t, 8, session.Usage.OutputTokens)
			oracle.mu.Lock()
			prompts := append([]string(nil), oracle.prompts...)
			oracle.mu.Unlock()
			require.ElementsMatch(t, []string{"A:workflow", oracle.secondPrompt}, prompts)
			histories := 0
			for current := chain; current != ""; {
				events := coordinatorHistory(t, f.ctx, f.client, workflowRunWorkflowID(run.ID), current)
				coordinatorAssertSmallHistory(t, events)
				next := ""
				for _, event := range events {
					encoded, err := json.Marshal(event)
					require.NoError(t, err)
					require.NotContains(t, string(encoded), "PRIVATE_REAL_QUICKJS_SOURCE_SENTINEL")
					if continued := event.GetWorkflowExecutionContinuedAsNewEventAttributes(); continued != nil {
						next = continued.NewExecutionRunId
					}
				}
				current = next
				histories++
			}
			require.GreaterOrEqual(t, histories, 2)
			var exits []quickJSExit
			for _, adapter := range adapters {
				adapter.mu.Lock()
				exits = append(exits, adapter.exits...)
				adapter.mu.Unlock()
			}
			for _, exit := range exits {
				require.True(t, exit.Observed)
			}
			digest := sha256.Sum256([]byte(source))
			var value any
			require.NoError(t, json.Unmarshal(result, &value))
			evidence[mode] = map[string]any{"result": value, "commands": commands, "decisions": decisions, "model_prompts": prompts, "usage": session.Usage, "temporal_histories": histories, "source_sha256": hex.EncodeToString(digest[:]), "owned_evaluator_exits": exits, "run_state": state}
		})
	}
	if path := os.Getenv("MANGO_TEST_QUICKJS_REPORT"); path != "" {
		encoded, err := json.MarshalIndent(evidence, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, append(encoded, '\n'), 0600))
	}
}
