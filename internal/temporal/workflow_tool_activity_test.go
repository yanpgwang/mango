package temporal

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/pg"
	"github.com/yanpgwang/mango/internal/sandbox"
	"github.com/yanpgwang/mango/internal/sandbox/sandboxtest"
)

var toolSchemaSeq atomic.Int64

func newToolTestStore(t *testing.T) *pg.Store {
	t.Helper()
	url := os.Getenv("MANGO_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MANGO_TEST_DATABASE_URL not set; skipping PostgreSQL tool-path test")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	schema := "tool_test_" + itoaTest(toolSchemaSeq.Add(1))
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
		pool.Close()
		t.Skipf("cannot create schema (db unreachable?): %v", err)
	}
	if err := pg.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		pool.Close()
	})
	return pg.NewDefaultWorkspaceStore(pool, domain.NewRandomIDGen(), toolClock{})
}

type toolClock struct{}

func (toolClock) Now() time.Time { return time.Now().UTC() }

func toolSession(t *testing.T, store *pg.Store, sessionID string) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	sess := domain.Session{
		ID:            sessionID,
		AgentID:       "agent_1",
		AgentVersion:  1,
		EnvironmentID: "env_1",
		Status:        domain.StatusIdle,
		Metadata:      map[string]any{},
		AgentSnapshot: domain.Agent{
			ID:      "agent_1",
			Version: 1,
			Model:   domain.Model{ID: "fake"},
			Tools:   []any{map[string]any{"type": domain.BuiltinToolsetType}},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err := store.CreateSession(ctx, sess, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	adm, err := store.AdmitEvents(ctx, sessionID, []domain.EventDraft{{
		Type: domain.EvUserMessage,
		Payload: map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "use a tool"}},
		},
	}})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	return adm.Events[0].ID
}

func assertHasType(t *testing.T, events []domain.Event, typ string) {
	t.Helper()
	for _, event := range events {
		if event.Type == typ {
			return
		}
	}
	t.Fatalf("expected an event of type %s; not found", typ)
}

type countingSandboxLease struct {
	mu    sync.Mutex
	count int
}

type advisorProbeClient struct {
	mu       sync.Mutex
	calls    int
	request  model.Request
	response model.Response
	err      error
}

func (c *advisorProbeClient) CreateMessage(
	_ context.Context,
	request model.Request,
) (model.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.request = request
	return c.response, c.err
}

func (c *advisorProbeClient) CreateMessageStream(
	ctx context.Context,
	request model.Request,
	_ func(int, string),
) (model.Response, error) {
	return c.CreateMessage(ctx, request)
}

type advisorActivitySource struct {
	*fakeSource
	journal      *memoryMCPJournal
	mu           sync.Mutex
	calls        int
	stepID       string
	result       domain.ToolStepResult
	consultation domain.AdvisorConsultation
}

func (s *advisorActivitySource) CompleteAdvisorToolStep(
	ctx context.Context,
	_ string,
	_ string,
	_ string,
	stepID string,
	result domain.ToolStepResult,
	consultation domain.AdvisorConsultation,
) error {
	if err := s.journal.CompleteToolStep(ctx, stepID, result); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.stepID = stepID
	s.result = result
	s.consultation = consultation
	return nil
}

func advisorExecuteInput() ExecuteToolInput {
	return ExecuteToolInput{
		SessionID: "sesn_advisor", ThreadID: "sthr_primary",
		TriggerEventID: "sevt_trigger", AttemptID: "ratm_advisor",
		Ordinal: 0, ToolUseEventID: "sevt_tool_advisor",
		ToolStepID: "tstep_advisor", ToolName: "advisor",
		ToolKind: TurnToolAdvisor, Input: map[string]any{},
		AdvisorRequest: model.Request{
			Model: "reviewer-model", System: "review independently", MaxTokens: 2048,
			Messages: []domain.Message{{
				Role:    domain.RoleUser,
				Content: []domain.ContentBlock{{Type: "text", Text: "quoted context"}},
			}},
		},
		AdvisorConsultation: domain.AdvisorConsultation{
			ThreadID: "sthr_advisor", UsageRequestID: "sevt_advisor_usage",
			LifecycleIDs: []string{
				"sevt_a", "sevt_b", "sevt_c", "sevt_d", "sevt_e",
				"sevt_f", "sevt_g", "sevt_h", "sevt_i",
			},
			Model: "reviewer-model",
		},
	}
}

func TestExecuteTool_AdvisorInferenceIsDurableAndNotRepeated(t *testing.T) {
	journal := &memoryMCPJournal{}
	source := &advisorActivitySource{
		fakeSource: newFakeSource(nil),
		journal:    journal,
	}
	client := &advisorProbeClient{response: model.Response{
		Content:    []domain.ContentBlock{{Type: "text", Text: "check the shutdown race"}},
		StopReason: "end_turn",
		Usage:      domain.TokenUsage{InputTokens: 120, OutputTokens: 15},
	}}
	activities := NewActivities(client, source, journal, nil, &testIDGen{})
	in := advisorExecuteInput()

	first, err := activities.ExecuteTool(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := activities.ExecuteTool(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Result.IsError || second.Result.IsError ||
		first.Result.Content[0].(map[string]any)["text"] != "check the shutdown race" {
		t.Fatalf("advisor results = %+v / %+v", first.Result, second.Result)
	}
	if client.calls != 1 || source.calls != 1 {
		t.Fatalf("advisor calls model=%d persistence=%d, want 1/1", client.calls, source.calls)
	}
	if len(client.request.Tools) != 0 || client.request.Model != "reviewer-model" {
		t.Fatalf("advisor request = %+v", client.request)
	}
	if !source.consultation.AdviceDelivered || !source.consultation.UsageKnown ||
		source.consultation.Usage.InputTokens != 120 ||
		source.consultation.PublicContent[0].(map[string]any)["text"] != "check the shutdown race" {
		t.Fatalf("consultation = %+v", source.consultation)
	}
}

func TestExecuteTool_AdvisorRejectsOversizedContextBeforeInference(t *testing.T) {
	journal := &memoryMCPJournal{}
	source := &advisorActivitySource{
		fakeSource: newFakeSource(nil),
		journal:    journal,
	}
	client := &advisorProbeClient{}
	activities := NewActivities(client, source, journal, nil, &testIDGen{})
	in := advisorExecuteInput()
	in.AdvisorRequest.Messages[0].Content[0].Text = strings.Repeat("x", 700_000)

	result, err := activities.ExecuteTool(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Result.IsError || client.calls != 0 || source.calls != 1 {
		t.Fatalf("oversized advisor result=%+v model=%d persistence=%d",
			result.Result, client.calls, source.calls)
	}
	if source.consultation.StopReason != "context_limit" ||
		source.consultation.UsageKnown {
		t.Fatalf("oversized consultation = %+v", source.consultation)
	}
}

func TestExecuteTool_StartedAdvisorRecoversWithoutRepeatingInference(t *testing.T) {
	in := advisorExecuteInput()
	journal := &memoryMCPJournal{step: domain.ToolStep{
		ID: in.ToolStepID, AttemptID: in.AttemptID, Ordinal: in.Ordinal,
		ToolUseEventID: in.ToolUseEventID, ToolName: in.ToolName,
		Input: in.Input, State: domain.ToolStepStarted,
	}}
	source := &advisorActivitySource{fakeSource: newFakeSource(nil), journal: journal}
	client := &advisorProbeClient{}
	activities := NewActivities(client, source, journal, nil, &testIDGen{})

	result, err := activities.ExecuteTool(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Result.IsError || client.calls != 0 || source.calls != 1 {
		t.Fatalf("recovered advisor result=%+v model=%d persistence=%d", result, client.calls, source.calls)
	}
	if source.consultation.UsageKnown || source.consultation.StopReason != "interrupted" {
		t.Fatalf("recovered consultation = %+v", source.consultation)
	}
}

func TestExecuteTool_CancelledAdvisorCompletesUnknownUsageWithoutReplay(t *testing.T) {
	in := advisorExecuteInput()
	journal := &memoryMCPJournal{}
	source := &advisorActivitySource{fakeSource: newFakeSource(nil), journal: journal}
	client := &advisorProbeClient{err: context.Canceled}
	activities := NewActivities(client, source, journal, nil, &testIDGen{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := activities.ExecuteTool(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Result.IsError || client.calls != 1 || source.calls != 1 ||
		journal.step.State != domain.ToolStepCompleted {
		t.Fatalf("cancelled advisor result=%+v model=%d persistence=%d step=%s",
			result, client.calls, source.calls, journal.step.State)
	}
	if source.consultation.UsageKnown || source.consultation.StopReason != "interrupted" {
		t.Fatalf("cancelled consultation = %+v", source.consultation)
	}
}

type permanentResourceReconciler struct{}

type failingWritebackReconciler struct{}

type permanentSandboxLease struct{}

type synchronizationObservingSandbox struct {
	sandbox.Sandbox
	operationActive atomic.Bool
	execSawLock     atomic.Bool
}

type synchronizationObservingReconciler struct {
	box                *synchronizationObservingSandbox
	writebackSawUnlock atomic.Bool
}

func (s *synchronizationObservingSandbox) Exec(
	ctx context.Context,
	command sandbox.Command,
) (*sandbox.Result, error) {
	if s.operationActive.Load() {
		s.execSawLock.Store(true)
	}
	return s.Sandbox.Exec(ctx, command)
}

func (s *synchronizationObservingSandbox) LockResourceOperation(
	context.Context,
) (func(), error) {
	if !s.operationActive.CompareAndSwap(false, true) {
		return nil, errors.New("resource operation lock already held")
	}
	return func() { s.operationActive.Store(false) }, nil
}

func (s *synchronizationObservingSandbox) TryLockResourceSync(
	ctx context.Context,
) (context.Context, func(), bool, error) {
	return ctx, func() {}, true, nil
}

func (s *synchronizationObservingSandbox) LockResourceSync(
	ctx context.Context,
) (context.Context, func(), error) {
	return ctx, func() {}, nil
}

func (r *synchronizationObservingReconciler) Reconcile(
	context.Context,
	string,
	sandbox.Sandbox,
) error {
	if r.box.operationActive.Load() {
		return errors.New("resource operation lock acquired before reconcile")
	}
	return nil
}

func (r *synchronizationObservingReconciler) Writeback(
	context.Context,
	string,
	sandbox.Sandbox,
) error {
	if r.box.operationActive.Load() {
		return errors.New("resource operation lock retained during writeback")
	}
	r.writebackSawUnlock.Store(true)
	return nil
}

func (permanentSandboxLease) Acquire(
	context.Context,
	string,
	sandbox.Spec,
) (sandbox.Sandbox, error) {
	return nil, sandbox.Permanent(errors.New("sandbox ownership mismatch"))
}

func (permanentSandboxLease) Release(context.Context, string) error { return nil }

func (permanentResourceReconciler) Reconcile(
	context.Context,
	string,
	sandbox.Sandbox,
) error {
	return sandbox.Permanent(errors.New("resource provider mismatch"))
}

func (failingWritebackReconciler) Reconcile(
	context.Context,
	string,
	sandbox.Sandbox,
) error {
	return nil
}

func (failingWritebackReconciler) Writeback(
	context.Context,
	string,
	sandbox.Sandbox,
) error {
	return errors.New("database temporarily unavailable")
}

func (l *countingSandboxLease) Acquire(
	context.Context,
	string,
	sandbox.Spec,
) (sandbox.Sandbox, error) {
	l.mu.Lock()
	l.count++
	l.mu.Unlock()
	return nil, nil
}

func (*countingSandboxLease) Release(context.Context, string) error { return nil }

func (l *countingSandboxLease) calls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count
}

type forwardingCountingLease struct {
	inner SandboxLease
	mu    sync.Mutex
	count int
	spec  sandbox.Spec
}

func (l *forwardingCountingLease) Acquire(
	ctx context.Context,
	sessionID string,
	spec sandbox.Spec,
) (sandbox.Sandbox, error) {
	l.mu.Lock()
	l.count++
	l.spec = spec
	l.mu.Unlock()
	return l.inner.Acquire(ctx, sessionID, spec)
}

func (l *forwardingCountingLease) Release(ctx context.Context, sessionID string) error {
	return l.inner.Release(ctx, sessionID)
}

func (l *forwardingCountingLease) calls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count
}

func (l *forwardingCountingLease) lastSpec() sandbox.Spec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.spec
}

type loseFirstCompletionAckJournal struct {
	JournalStore
	mu     sync.Mutex
	failed bool
}

func (j *loseFirstCompletionAckJournal) CompleteToolStep(
	ctx context.Context,
	stepID string,
	result domain.ToolStepResult,
) error {
	if err := j.JournalStore.CompleteToolStep(ctx, stepID, result); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.failed {
		j.failed = true
		return errors.New("tool result committed but Activity acknowledgement was lost")
	}
	return nil
}

func TestExecuteTool_CompletedStepReturnsWithoutReexecution(t *testing.T) {
	store := newToolTestStore(t)
	ctx := context.Background()
	const sessionID = "sess_workflow_tool_completed"
	trigger := toolSession(t, store, sessionID)

	attempt, err := store.EnsureAttempt(ctx, sessionID, trigger, "ratm_workflow_completed")
	if err != nil {
		t.Fatalf("ensure attempt: %v", err)
	}
	step, err := store.EnsureToolStep(
		ctx,
		attempt.ID,
		"tstep_workflow_completed",
		0,
		"sevt_workflow_completed",
		"bash",
		map[string]any{"command": "echo once"},
	)
	if err != nil {
		t.Fatalf("ensure step: %v", err)
	}
	if err := store.StartToolStep(ctx, step.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := store.CompleteToolStep(ctx, step.ID, domain.ToolStepResult{
		Content: []any{map[string]any{"type": "text", "text": "once"}},
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	lease := &countingSandboxLease{}
	source := storeSource{store: store}
	activities := NewActivities(nil, source, source, lease, domain.NewRandomIDGen())
	result, err := activities.ExecuteTool(ctx, ExecuteToolInput{
		SessionID:      sessionID,
		TriggerEventID: trigger,
		AttemptID:      attempt.ID,
		Ordinal:        0,
		ToolUseEventID: "sevt_workflow_completed",
		ToolStepID:     step.ID,
		ToolName:       "bash",
		Input:          map[string]any{"command": "echo once"},
	})
	if err != nil {
		t.Fatalf("execute retry: %v", err)
	}
	if result.Ambiguous || result.Result.Content[0].(map[string]any)["text"] != "once" {
		t.Fatalf("unexpected recovered result: %+v", result)
	}
	if lease.calls() != 0 {
		t.Fatalf("completed step reacquired a sandbox %d time(s)", lease.calls())
	}
}

func TestExecuteTool_StartedStepBecomesAmbiguousWithoutReexecution(t *testing.T) {
	store := newToolTestStore(t)
	ctx := context.Background()
	const sessionID = "sess_workflow_tool_ambiguous"
	trigger := toolSession(t, store, sessionID)

	attempt, err := store.EnsureAttempt(ctx, sessionID, trigger, "ratm_workflow_ambiguous")
	if err != nil {
		t.Fatalf("ensure attempt: %v", err)
	}
	step, err := store.EnsureToolStep(
		ctx,
		attempt.ID,
		"tstep_workflow_ambiguous",
		0,
		"sevt_workflow_ambiguous",
		"bash",
		map[string]any{"command": "side effect"},
	)
	if err != nil {
		t.Fatalf("ensure step: %v", err)
	}
	if err := store.StartToolStep(ctx, step.ID); err != nil {
		t.Fatalf("start: %v", err)
	}

	lease := &countingSandboxLease{}
	source := storeSource{store: store}
	activities := NewActivities(nil, source, source, lease, domain.NewRandomIDGen())
	result, err := activities.ExecuteTool(ctx, ExecuteToolInput{
		SessionID:      sessionID,
		TriggerEventID: trigger,
		AttemptID:      attempt.ID,
		Ordinal:        0,
		ToolUseEventID: "sevt_workflow_ambiguous",
		ToolStepID:     step.ID,
		ToolName:       "bash",
		Input:          map[string]any{"command": "side effect"},
	})
	if err != nil {
		t.Fatalf("execute retry: %v", err)
	}
	if !result.Ambiguous {
		t.Fatalf("started step must be reported ambiguous: %+v", result)
	}
	if lease.calls() != 0 {
		t.Fatalf("ambiguous step reacquired a sandbox %d time(s)", lease.calls())
	}
	state, ok, err := store.ToolStepStateByEventID(ctx, "sevt_workflow_ambiguous")
	if err != nil || !ok || state != domain.ToolStepAmbiguous {
		t.Fatalf("state = %s ok=%v err=%v", state, ok, err)
	}
}

func TestExecuteTool_ResourcePermanentErrorTerminatesTurn(t *testing.T) {
	store := newToolTestStore(t)
	ctx := context.Background()
	const sessionID = "sess_resource_permanent"
	trigger := toolSession(t, store, sessionID)
	lease := &countingSandboxLease{}
	source := storeSource{store: store}
	activities := NewActivities(nil, source, source, lease, domain.NewRandomIDGen()).
		WithSandboxResourceReconciler(permanentResourceReconciler{})

	result, err := activities.ExecuteTool(ctx, ExecuteToolInput{
		SessionID: sessionID, TriggerEventID: trigger,
		AttemptID: "ratm_resource_permanent", Ordinal: 0,
		ToolUseEventID: "sevt_resource_permanent",
		ToolStepID:     "tstep_resource_permanent",
		ToolName:       "bash",
		Input:          map[string]any{"command": "true"},
	})
	if err != nil {
		t.Fatalf("ExecuteTool error = %v, want terminal result", err)
	}
	if result.FatalError == "" {
		t.Fatalf("ExecuteTool result = %+v, want fatal error", result)
	}
}

func TestExecuteTool_SandboxPermanentErrorTerminatesTurn(t *testing.T) {
	store := newToolTestStore(t)
	ctx := context.Background()
	const sessionID = "sess_sandbox_permanent"
	trigger := toolSession(t, store, sessionID)
	source := storeSource{store: store}
	activities := NewActivities(
		nil, source, source, permanentSandboxLease{}, domain.NewRandomIDGen(),
	)

	result, err := activities.ExecuteTool(ctx, ExecuteToolInput{
		SessionID: sessionID, TriggerEventID: trigger,
		AttemptID: "ratm_sandbox_permanent", Ordinal: 0,
		ToolUseEventID: "sevt_sandbox_permanent",
		ToolStepID:     "tstep_sandbox_permanent",
		ToolName:       "bash",
		Input:          map[string]any{"command": "true"},
	})
	if err != nil {
		t.Fatalf("ExecuteTool error = %v, want terminal result", err)
	}
	if result.FatalError == "" {
		t.Fatalf("ExecuteTool result = %+v, want fatal error", result)
	}
}

func TestExecuteTool_WritebackFailurePreservesToolOutput(t *testing.T) {
	ctx := context.Background()
	box := sandboxtest.OpenSandbox(t)
	journal := &memoryMCPJournal{}
	activities := NewActivities(
		nil,
		newFakeSource(nil),
		journal,
		&fixedSandboxLease{box: box},
		&testIDGen{},
	).WithSandboxResourceReconciler(failingWritebackReconciler{})

	result, err := activities.ExecuteTool(ctx, ExecuteToolInput{
		SessionID: "sesn_writeback", TriggerEventID: "sevt_trigger",
		AttemptID: "ratm_writeback", Ordinal: 0,
		ToolUseEventID: "sevt_tool", ToolStepID: "tstep_writeback",
		ToolName: "bash", Input: map[string]any{"command": "printf tool-output"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Result.IsError || len(result.Result.Content) < 2 {
		t.Fatalf("writeback result = %+v", result.Result)
	}
	if got := result.Result.Content[0].(map[string]any)["text"]; got != "tool-output" {
		t.Fatalf("tool output = %#v", got)
	}
	if got := result.Result.Content[len(result.Result.Content)-1].(map[string]any)["text"]; got != "Memory Store writeback failed: database temporarily unavailable" {
		t.Fatalf("writeback error = %#v", got)
	}
	if !journal.result.IsError || len(journal.result.Content) != len(result.Result.Content) {
		t.Fatalf("journaled result = %+v", journal.result)
	}
}

func TestExecuteTool_CoordinatesResourceSyncAroundToolOperation(t *testing.T) {
	ctx := context.Background()
	inner := sandboxtest.OpenSandbox(t)
	box := &synchronizationObservingSandbox{Sandbox: inner}
	reconciler := &synchronizationObservingReconciler{box: box}
	journal := &memoryMCPJournal{}
	activities := NewActivities(
		nil,
		newFakeSource(nil),
		journal,
		&fixedSandboxLease{box: box},
		&testIDGen{},
	).WithSandboxResourceReconciler(reconciler)

	result, err := activities.ExecuteTool(ctx, ExecuteToolInput{
		SessionID: "sesn_resource_sync", TriggerEventID: "sevt_trigger",
		AttemptID: "ratm_resource_sync", Ordinal: 0,
		ToolUseEventID: "sevt_tool", ToolStepID: "tstep_resource_sync",
		ToolName: "bash", Input: map[string]any{"command": "printf synchronized"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Result.IsError {
		t.Fatalf("tool result = %+v", result.Result)
	}
	if !box.execSawLock.Load() {
		t.Fatal("tool execution was not protected by the shared resource lock")
	}
	if box.operationActive.Load() {
		t.Fatal("shared resource lock remained held after ExecuteTool")
	}
	if !reconciler.writebackSawUnlock.Load() {
		t.Fatal("writeback did not run after releasing the shared resource lock")
	}
}

func TestWorkflowTurn_ToolResultWriteRetryDoesNotReexecute(t *testing.T) {
	store := newToolTestStore(t)
	const sessionID = "sess_workflow_activity_retry"
	trigger := toolSession(t, store, sessionID)

	ids := domain.NewRandomIDGen()
	source := storeSource{store: store}
	journal := &loseFirstCompletionAckJournal{JournalStore: source}
	manager := sandbox.NewSessionManager(sandboxtest.OpenSandboxProvider(t), store)
	lease := &forwardingCountingLease{inner: manager}
	t.Cleanup(func() {
		_ = manager.Release(context.Background(), sessionID)
	})
	modelClient := model.NewFake()
	activities := NewActivities(modelClient, source, journal, lease, ids)

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)
	env.RegisterActivityWithOptions(activities.PrepareTurn, activity.RegisterOptions{Name: ActivityPrepareTurn})
	env.RegisterActivityWithOptions(activities.CallModel, activity.RegisterOptions{Name: ActivityCallModel})
	env.RegisterActivityWithOptions(activities.AdmitModelRequest, activity.RegisterOptions{Name: ActivityAdmitModelRequest})
	env.RegisterActivityWithOptions(activities.AccountModelRequest, activity.RegisterOptions{Name: ActivityAccountModelRequest})
	env.RegisterActivityWithOptions(activities.StartModelRequest, activity.RegisterOptions{Name: ActivityStartModelRequest})
	env.RegisterActivityWithOptions(activities.AppendWorkflowEvents, activity.RegisterOptions{Name: ActivityAppendWorkflowEvents})
	env.RegisterActivityWithOptions(activities.ExecuteTool, activity.RegisterOptions{Name: ActivityExecuteTool})
	env.RegisterActivityWithOptions(activities.CompleteWorkflowTurn, activity.RegisterOptions{Name: ActivityCompleteWorkflowTurn})

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: sessionID, TriggerEventID: trigger,
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow: %v", err)
	}
	if lease.calls() != 1 {
		t.Fatalf("completed tool was re-executed: sandbox acquired %d times", lease.calls())
	}
	if spec := lease.lastSpec(); spec.Network != defaultCloudSandboxNetwork {
		t.Fatalf("sandbox network = %q, want %q", spec.Network, defaultCloudSandboxNetwork)
	}
	events, err := store.EventsAfter(context.Background(), sessionID, 0, 100)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	assertHasType(t, events, domain.EvAgentToolUse)
	assertHasType(t, events, domain.EvAgentToolResult)
	assertHasType(t, events, domain.EvSessionStatusIdle)
}
