package temporal

// Independently authored synthetic workload exercising the public pattern in
// claude-cookbooks/claude_agent_sdk/08_Dynamic_workflows.ipynb (a162e018).
// This is NOT that SDK, its application/data, a live-model run, or public Mango
// admission. Explicit JSON parsing and a local pure pipeline replace its schema
// option/helper; filesystem reads, phase UI and primary lifecycle are absent.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/pg"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// Each item advances immediately; only Promise.all is a global barrier. The
// original item remains available to every stage, matching the documented
// pipeline(items, stage1, stage2) semantics without a host helper or scheduler.
const quickJSFactcheckSource = `
async function pipeline(items, ...stages) {
  return Promise.all(items.map(async item => {
    let value = item;
    for (const stage of stages) value = await stage(value, item);
    return value;
  }));
}
async function structured(stage, value) {
  return JSON.parse(await agent(JSON.stringify({stage, value}), {agent:"reviewer"}));
}
const extraction = await structured("extract", args.draft);
const claims = extraction.claims.sort((a,b) => a.number-b.number);
const rows = await pipeline(claims,
  claim => structured("verify", claim),
  async (verified, claim) => {
    if (verified.verdict !== "confirmed") return {...verified, skeptic_reviewed:false};
    const checked = await structured("skeptic", {claim, verified});
    if (checked.final_verdict === "contradicted") return {
      ...verified, verdict:"contradicted", source_file:checked.source_file || verified.source_file,
      quote:checked.quote || verified.quote, correct_figure:checked.correct_figure || verified.correct_figure,
      reasoning:"Skeptic overturned confirmation: " + checked.reasoning, skeptic_reviewed:true
    };
    return {...verified, skeptic_reviewed:true,
      reasoning:verified.reasoning + " (skeptic upheld: " + checked.reasoning + ")"};
  });
const finalRows = rows.filter(Boolean).sort((a,b) => a.number-b.number);
return await agent(JSON.stringify({stage:"report", value:finalRows}), {agent:"reviewer"});`

type factcheckClaim struct {
	Number int    `json:"number"`
	Text   string `json:"text"`
}
type factcheckRow struct {
	Number          int    `json:"number"`
	Text            string `json:"text"`
	Verdict         string `json:"verdict"`
	SourceFile      string `json:"source_file"`
	Quote           string `json:"quote"`
	CorrectFigure   string `json:"correct_figure"`
	Reasoning       string `json:"reasoning"`
	SkepticReviewed bool   `json:"skeptic_reviewed"`
}
type factcheckModel struct {
	mu                     sync.Mutex
	zero                   bool
	seen, finished         map[string]bool
	trace                  []string
	transportAttempts      map[string]int
	releaseOne, releaseTen chan struct{}
	onceOne, onceTen       sync.Once
	skepticTwo             chan struct{}
}

func factcheckExpected(zero bool) []factcheckRow {
	rows := make([]factcheckRow, 10)
	for i := 1; i <= 10; i++ {
		row := factcheckRow{Number: i, Text: fmt.Sprintf("synthetic claim %d", i), Verdict: "contradicted", SourceFile: "sources/fixture.txt", Quote: fmt.Sprintf("evidence %d", i), CorrectFigure: "corrected", Reasoning: "verify"}
		if i == 7 || i == 8 {
			row.Verdict = "unverifiable"
			row.SourceFile = ""
			row.Quote = ""
			row.CorrectFigure = ""
		}
		if !zero && (i == 2 || i == 3 || i == 9 || i == 10) {
			row.Verdict = "confirmed"
			row.CorrectFigure = ""
			row.SkepticReviewed = true
			row.Reasoning = "verify (skeptic upheld: upheld)"
			if i == 10 {
				row.Verdict = "contradicted"
				row.SourceFile = "sources/limiting.txt"
				row.Quote = "limiting evidence"
				row.CorrectFigure = "limited fact"
				row.Reasoning = "Skeptic overturned confirmation: context"
			}
		}
		rows[i-1] = row
	}
	return rows
}
func factcheckReport(zero bool) string {
	report := "# Synthetic fact check\n|Claim|Verdict|Evidence|\n|---|---|---|\n"
	summary := "\nSummary:\n"
	for _, row := range factcheckExpected(zero) {
		evidence := row.SourceFile + ": " + row.Quote
		if row.CorrectFigure != "" {
			evidence += "; correct fact: " + row.CorrectFigure
		}
		if row.Verdict == "unverifiable" {
			evidence = "no source covers this"
		}
		report += fmt.Sprintf("|%d: %s|%s|%s|\n", row.Number, row.Text, row.Verdict, evidence)
		if row.Verdict == "contradicted" {
			summary += fmt.Sprintf("- Claim %d: use %s.\n", row.Number, row.CorrectFigure)
		}
		if row.Verdict == "unverifiable" {
			summary += fmt.Sprintf("- Claim %d: provide a source or remove.\n", row.Number)
		}
	}
	return report + summary
}

func (m *factcheckModel) release() {
	m.onceOne.Do(func() { close(m.releaseOne) })
	m.onceTen.Do(func() { close(m.releaseTen) })
}
func (m *factcheckModel) CreateMessage(ctx context.Context, in model.Request) (model.Response, error) {
	if in.Model != "quickjs-oracle" {
		return model.Response{}, errors.New("unexpected primary inference")
	}
	var command struct {
		Stage string          `json:"stage"`
		Value json.RawMessage `json:"value"`
	}
	found := false
	for _, msg := range in.Messages {
		for _, b := range msg.Content {
			var candidate struct {
				Stage string          `json:"stage"`
				Value json.RawMessage `json:"value"`
			}
			if json.Unmarshal([]byte(b.Text), &candidate) == nil && candidate.Stage != "" {
				command = candidate
				found = true
			}
		}
	}
	if !found {
		return model.Response{}, errors.New("missing JS-driven command")
	}
	key := command.Stage
	var value any
	switch command.Stage {
	case "extract":
		if string(command.Value) != `"synthetic investor draft"` {
			return model.Response{}, errors.New("wrong extraction input")
		}
		claims := []factcheckClaim{}
		for i := 10; i >= 1; i-- {
			claims = append(claims, factcheckClaim{i, fmt.Sprintf("synthetic claim %d", i)})
		}
		value = map[string]any{"claims": claims}
	case "verify":
		var c factcheckClaim
		if json.Unmarshal(command.Value, &c) != nil || c.Number < 1 || c.Number > 10 || c.Text != fmt.Sprintf("synthetic claim %d", c.Number) {
			return model.Response{}, errors.New("incorrect claim input")
		}
		key = fmt.Sprintf("verify-%d", c.Number)
		row := factcheckExpected(m.zero)[c.Number-1]
		row.SkepticReviewed = false
		row.Reasoning = "verify"
		if !m.zero && (c.Number == 2 || c.Number == 3 || c.Number == 9 || c.Number == 10) {
			row.Verdict = "confirmed"
			row.SourceFile = "sources/fixture.txt"
			row.Quote = fmt.Sprintf("evidence %d", c.Number)
			row.CorrectFigure = ""
		}
		value = row
	case "skeptic":
		var v struct {
			Claim    factcheckClaim `json:"claim"`
			Verified factcheckRow   `json:"verified"`
		}
		if json.Unmarshal(command.Value, &v) != nil || m.zero || v.Verified.Verdict != "confirmed" || v.Claim.Number != v.Verified.Number || (v.Claim.Number != 2 && v.Claim.Number != 3 && v.Claim.Number != 9 && v.Claim.Number != 10) {
			return model.Response{}, errors.New("unconfirmed or mismatched skeptic input")
		}
		key = fmt.Sprintf("skeptic-%d", v.Claim.Number)
		value = map[string]any{"number": v.Claim.Number, "final_verdict": "confirmed", "source_file": "", "quote": "", "correct_figure": "", "reasoning": "upheld"}
		if v.Claim.Number == 10 {
			value = map[string]any{"number": 10, "final_verdict": "contradicted", "source_file": "sources/limiting.txt", "quote": "limiting evidence", "correct_figure": "limited fact", "reasoning": "context"}
		}
	case "report":
		var rows []factcheckRow
		if json.Unmarshal(command.Value, &rows) != nil {
			return model.Response{}, errors.New("invalid report input")
		}
		got, _ := json.Marshal(rows)
		want, _ := json.Marshal(factcheckExpected(m.zero))
		if string(got) != string(want) {
			return model.Response{}, fmt.Errorf("report input did not contain exactly ten sorted, fully merged rows: %s", got)
		}
		value = factcheckReport(m.zero)
	default:
		return model.Response{}, errors.New("unknown stage")
	}
	m.mu.Lock()
	first := !m.seen[key]
	// A provider response may return before its Activity ACK is durable.
	// Deterministic transport retries are allowed even after a prior return;
	// exact-once business admissions/receipts are asserted independently in PG.
	m.transportAttempts[key]++
	if key == "report" {
		for i := 1; i <= 10; i++ {
			if !m.finished[fmt.Sprintf("verify-%d", i)] {
				m.mu.Unlock()
				return model.Response{}, errors.New("report before verifier completion")
			}
		}
		if !m.zero {
			for _, i := range []int{2, 3, 9, 10} {
				if !m.finished[fmt.Sprintf("skeptic-%d", i)] {
					m.mu.Unlock()
					return model.Response{}, errors.New("report before skeptic completion")
				}
			}
		}
	}
	if first && key == "skeptic-2" && m.finished["verify-1"] {
		m.mu.Unlock()
		return model.Response{}, errors.New("global verify barrier replaced per-item pipeline")
	}
	m.seen[key] = true
	if first {
		m.trace = append(m.trace, "start:"+key)
	}
	m.mu.Unlock()
	if key == "skeptic-2" && first {
		close(m.skepticTwo)
	}
	var wait <-chan struct{}
	if key == "verify-1" {
		wait = m.releaseOne
	}
	if key == "skeptic-10" {
		wait = m.releaseTen
	}
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return model.Response{}, ctx.Err()
		}
	}
	m.mu.Lock()
	if !m.finished[key] {
		m.trace = append(m.trace, "finish:"+key)
	}
	m.finished[key] = true
	m.mu.Unlock()
	text, ok := value.(string)
	if !ok {
		encoded, _ := json.Marshal(value)
		text = string(encoded)
	}
	return model.Response{StopReason: "end_turn", Content: []domain.ContentBlock{{Type: "text", Text: text}}, Usage: domain.TokenUsage{InputTokens: 7, OutputTokens: 4}}, nil
}
func (m *factcheckModel) CreateMessageStream(ctx context.Context, in model.Request, _ func(int, string)) (model.Response, error) {
	return m.CreateMessage(ctx, in)
}

type factcheckLossStore struct {
	*pg.Store
	notice chan pg.WorkflowPumpRequest
	once   sync.Once
}

func (s *factcheckLossStore) CommitWorkflowPump(ctx context.Context, sid, rid string, in pg.WorkflowPumpRequest) (pg.WorkflowPumpDecision, error) {
	out, err := s.Store.CommitWorkflowPump(ctx, sid, rid, in)
	if err == nil && out.Delivery != nil {
		var v struct {
			Number  int    `json:"number"`
			Verdict string `json:"verdict"`
		}
		if json.Unmarshal([]byte(out.Delivery.Receipt.Text), &v) == nil && v.Number == 2 && v.Verdict != "" {
			s.once.Do(func() { s.notice <- in })
			return pg.WorkflowPumpDecision{}, errors.New("injected committed verification ACK loss")
		}
	}
	return out, err
}

func TestQuickJSFactcheckPipelineRealServices(t *testing.T) {
	if os.Getenv("MANGO_TEST_WORKFLOW_QUICKJS") != "1" {
		t.Skip("explicit real QuickJS/PG/Temporal tier")
	}
	image := os.Getenv("MANGO_TEST_QUICKJS_IMAGE")
	if image == "" {
		image = "mango-workflow-quickjs-vertical:study"
	}
	encoded, err := quickJSDocker(t.Context(), "image", "inspect", "--format", "{{.Id}}", image)
	require.NoError(t, err)
	image = strings.TrimSpace(string(encoded))
	evidence := map[string]any{"image": image, "live_provider": false, "public_admission": false}
	for _, mode := range []string{"mixed-restart", "mixed-delivery-order", "zero-confirmed"} {
		t.Run(mode, func(t *testing.T) {
			f := newQuickJSVerticalFixture(t, image)
			oracle := &factcheckModel{zero: mode == "zero-confirmed", seen: map[string]bool{}, finished: map[string]bool{}, transportAttempts: map[string]int{}, releaseOne: make(chan struct{}), releaseTen: make(chan struct{}), skepticTwo: make(chan struct{})}
			t.Cleanup(oracle.release)
			if oracle.zero {
				oracle.release()
			}
			src := storeSource{store: f.store}
			threadActs := NewActivities(oracle, src, src, f.ids)
			loss := &factcheckLossStore{Store: f.store, notice: make(chan pg.WorkflowPumpRequest, 1)}
			modelReturned := make(chan struct{}, 1)
			var returnedOnce sync.Once
			threadWorker := NewWorkerOnTaskQueue(f.client, threadActs, f.queue)
			if mode == "mixed-restart" {
				threadWorker.RegisterActivityWithOptions(func(ctx context.Context, in CallModelInput) (CallModelResult, error) {
					out, err := threadActs.callModel(ctx, in)
					if err != nil {
						return out, err
					}
					for _, message := range in.Request.Messages {
						for _, block := range message.Content {
							var command struct {
								Stage string         `json:"stage"`
								Value factcheckClaim `json:"value"`
							}
							if json.Unmarshal([]byte(block.Text), &command) == nil && command.Stage == "verify" && command.Value.Number == 3 {
								inject := false
								returnedOnce.Do(func() { inject = true; modelReturned <- struct{}{} })
								// Explicit retryable response loss, not an Activity cancellation.
								if inject {
									return CallModelResult{}, errors.New("injected provider response loss before Activity result")
								}
							}
						}
					}
					return out, nil
				}, activity.RegisterOptions{Name: ActivityCallModel, DisableAlreadyRegisteredCheck: true})
			}
			require.NoError(t, threadWorker.Start())
			t.Cleanup(threadWorker.Stop)
			// Replace the actual Run worker while owned Threads remain active on their
			// real runner. Distinct task queues prevent unknown-Activity poll races.
			runQueue := f.queue + "-run"
			create := func(inject bool) worker.Worker {
				w := worker.New(f.client, runQueue, worker.Options{})
				var store WorkflowRunStore = f.store
				if inject {
					store = loss
				}
				RegisterWorkflowRunCoordinator(w, &WorkflowRunActivities{Store: store, Evaluator: &quickJSTestEvaluator{image: image, profile: f.profile}})
				w.RegisterWorkflowWithOptions(func(ctx workflow.Context, in WorkflowRunWorkflowInput) error {
					return workflowRunWorkflow(ctx, in, 8, 100*time.Millisecond)
				}, workflow.RegisterOptions{Name: WorkflowRunWorkflowType, DisableAlreadyRegisteredCheck: true})
				return w
			}
			w := create(mode == "mixed-restart")
			require.NoError(t, w.Start())
			t.Cleanup(func() { w.Stop() })
			run := f.admitRequest(t, pg.WorkflowRunRequest{Source: quickJSFactcheckSource, RuntimeProfile: f.profile, Input: json.RawMessage(`{"draft":"synthetic investor draft"}`), Deadline: time.Now().UTC().Add(time.Hour), MaxInvocations: 20, MaxResultBytes: 8192})
			signaler := NewSignalerOnTaskQueue(f.client, f.queue)
			runSignaler := NewSignalerOnTaskQueue(f.client, runQueue)
			ctx, cancel := context.WithCancel(f.ctx)
			var relays sync.WaitGroup
			for _, isRun := range []bool{true, false} {
				relays.Go(func() {
					if isRun {
						_ = NewWorkflowRunRelay(f.store, runSignaler, RelayConfig{PollInterval: 50 * time.Millisecond}).Run(ctx)
					} else {
						_ = NewRelay(f.store, signaler, RelayConfig{PollInterval: 50 * time.Millisecond}).Run(ctx)
					}
				})
			}
			t.Cleanup(func() { cancel(); relays.Wait() })
			var old pg.WorkflowPumpRequest
			if mode == "mixed-restart" {
				old = threadRecoveryReceive(t, f.ctx, loss.notice)
				threadRecoveryReceive(t, f.ctx, modelReturned)
				w.Stop()
				worker.PurgeStickyWorkflowCache()
				w = create(false)
				require.NoError(t, w.Start())
			}
			if !oracle.zero {
				threadRecoveryReceive(t, f.ctx, oracle.skepticTwo)
				oracle.mu.Lock()
				firstFinished, reportStarted := oracle.finished["verify-1"], oracle.seen["report"]
				oracle.mu.Unlock()
				require.False(t, firstFinished)
				require.False(t, reportStarted)
				var admittedSkeptic, pendingFirst, admittedReport int
				require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT
 (SELECT count(*) FROM workflow_invocations WHERE run_id=$1 AND (command->>'prompt')::jsonb->>'stage'='skeptic' AND ((command->>'prompt')::jsonb->'value'->'claim'->>'number')::int=2),
 (SELECT count(*) FROM workflow_invocations i JOIN workflow_invocation_attempts a ON a.invocation_id=i.id WHERE i.run_id=$1 AND (i.command->>'prompt')::jsonb->>'stage'='verify' AND ((i.command->>'prompt')::jsonb->'value'->>'number')::int=1 AND a.state='active'),
 (SELECT count(*) FROM workflow_invocations WHERE run_id=$1 AND (command->>'prompt')::jsonb->>'stage'='report')`, run.ID).Scan(&admittedSkeptic, &pendingFirst, &admittedReport))
				require.Equal(t, 1, admittedSkeptic)
				require.Equal(t, 1, pendingFirst, "actual verifier Thread remains pending when skeptic starts")
				require.Zero(t, admittedReport)
				if mode == "mixed-restart" {
					oracle.onceOne.Do(func() { close(oracle.releaseOne) })
				} else {
					oracle.onceTen.Do(func() { close(oracle.releaseTen) })
				}
				require.Eventually(t, func() bool { oracle.mu.Lock(); defer oracle.mu.Unlock(); return oracle.seen["skeptic-10"] }, 20*time.Second, 25*time.Millisecond)
				oracle.mu.Lock()
				reportStarted = oracle.seen["report"]
				oracle.mu.Unlock()
				require.False(t, reportStarted, "report waits for last item chain")
				if mode == "mixed-restart" {
					require.Eventually(t, func() bool {
						var settled int
						return f.pool.QueryRow(f.ctx, `SELECT count(*) FROM workflow_invocations i JOIN workflow_invocation_attempts a ON a.invocation_id=i.id WHERE i.run_id=$1 AND (i.command->>'prompt')::jsonb->>'stage'='verify' AND a.state='settled'`, run.ID).Scan(&settled) == nil && settled == 10
					}, 10*time.Second, 25*time.Millisecond)
				}
				require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT count(*) FROM workflow_invocations WHERE run_id=$1 AND (command->>'prompt')::jsonb->>'stage'='report'`, run.ID).Scan(&admittedReport))
				require.Zero(t, admittedReport, "no report admission before final item chain")
				oracle.release()
			}
			var result []byte
			require.Eventually(t, func() bool {
				return f.pool.QueryRow(f.ctx, `SELECT decision->'value' FROM workflow_pump_decisions WHERE run_id=$1 AND decision->>'type'='return'`, run.ID).Scan(&result) == nil
			}, 45*time.Second, 50*time.Millisecond)
			var report string
			require.NoError(t, json.Unmarshal(result, &report))
			require.Equal(t, factcheckReport(oracle.zero), report)
			calls := 16
			if oracle.zero {
				calls = 12
			}
			var actual, decisions int
			var state, owner, chain string
			var epoch int64
			require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM workflow_invocations WHERE run_id=$1),(SELECT count(*) FROM workflow_pump_decisions WHERE run_id=$1),state,coordinator_execution_id,coordinator_chain_id,evaluator_epoch FROM workflow_runs WHERE id=$1`, run.ID).Scan(&actual, &decisions, &state, &owner, &chain, &epoch))
			require.Equal(t, calls, actual)
			require.Equal(t, 2*calls+1, decisions)
			// Independently inspect real invocation/Thread terminal receipts, not
			// just the model double's callbacks or the JS helper's output.
			ledger, err := f.pool.Query(f.ctx, `SELECT i.command,a.receipt FROM workflow_invocations i JOIN workflow_invocation_attempts a ON a.invocation_id=i.id WHERE i.run_id=$1 AND a.state='settled' AND a.terminal_event_id IS NOT NULL ORDER BY i.ordinal`, run.ID)
			require.NoError(t, err)
			var stages []string
			var skepticIDs []int
			var reportRows []factcheckRow
			for ledger.Next() {
				var rawCommand, rawReceipt []byte
				require.NoError(t, ledger.Scan(&rawCommand, &rawReceipt))
				var command pg.WorkflowInvocationCommand
				var receipt pg.WorkflowInvocationReceipt
				require.NoError(t, json.Unmarshal(rawCommand, &command))
				require.NoError(t, json.Unmarshal(rawReceipt, &receipt))
				require.Equal(t, "text", receipt.Type)
				var input struct {
					Stage string          `json:"stage"`
					Value json.RawMessage `json:"value"`
				}
				require.NoError(t, json.Unmarshal([]byte(command.Prompt), &input))
				stages = append(stages, input.Stage)
				switch input.Stage {
				case "verify":
					var claim factcheckClaim
					require.NoError(t, json.Unmarshal(input.Value, &claim))
					require.Equal(t, len(stages)-1, claim.Number, "JS sorted reverse extraction before fan-out")
				case "skeptic":
					var in struct {
						Claim factcheckClaim `json:"claim"`
					}
					require.NoError(t, json.Unmarshal(input.Value, &in))
					skepticIDs = append(skepticIDs, in.Claim.Number)
				case "report":
					require.NoError(t, json.Unmarshal(input.Value, &reportRows))
					require.Equal(t, report, receipt.Text, "returned value comes from actual report Thread receipt")
				}
			}
			require.NoError(t, ledger.Err())
			ledger.Close()
			require.Len(t, stages, calls)
			require.Equal(t, factcheckExpected(oracle.zero), reportRows)
			if oracle.zero {
				require.Empty(t, skepticIDs)
			} else {
				require.ElementsMatch(t, []int{2, 3, 9, 10}, skepticIDs)
			}
			oracle.mu.Lock()
			seenCalls := len(oracle.seen)
			trace := append([]string(nil), oracle.trace...)
			transportAttempts := make(map[string]int, len(oracle.transportAttempts))
			for key, n := range oracle.transportAttempts {
				transportAttempts[key] = n
			}
			oracle.mu.Unlock()
			require.Equal(t, calls, seenCalls)
			session, err := f.store.GetSession(f.ctx, run.SessionID)
			require.NoError(t, err)
			require.EqualValues(t, calls*7, session.Usage.InputTokens)
			require.EqualValues(t, calls*4, session.Usage.OutputTokens)
			if mode == "mixed-restart" {
				require.GreaterOrEqual(t, transportAttempts["verify-3"], 2, "force returned provider response/Activity ACK uncertainty")
				_, err = f.store.CommitWorkflowPump(f.ctx, run.SessionID, run.ID, pg.WorkflowPumpRequest{RequestID: "stale-factcheck", Epoch: old.Epoch, ExpectedCursor: int64(decisions), CoordinatorExecutionID: owner, Command: pg.WorkflowPumpCommand{Kind: "return", Value: json.RawMessage(`"wrong"`)}})
				require.ErrorContains(t, err, "stale")
			}
			histories := 0
			for current := chain; current != ""; {
				events := coordinatorHistory(t, f.ctx, f.client, workflowRunWorkflowID(run.ID), current)
				next := ""
				for _, ev := range events {
					if can := ev.GetWorkflowExecutionContinuedAsNewEventAttributes(); can != nil {
						next = can.NewExecutionRunId
					}
				}
				histories++
				current = next
			}
			require.Greater(t, histories, 1)
			// Explicit negative product evidence: candidate completion did not close
			// the Run or deliver its report to the primary. Session idle is insufficient.
			require.Equal(t, "open", state)
			var primaryReports int
			require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT count(*) FROM events e JOIN session_threads t ON t.id=e.thread_id WHERE e.session_id=$1 AND t.parent_thread_id IS NULL AND e.type IN ('agent.message','agent.thread_message_received')`, run.SessionID).Scan(&primaryReports))
			require.Zero(t, primaryReports)
			evidence[mode] = map[string]any{"result": report, "report_input": reportRows, "stages": stages, "skeptic_ids": skepticIDs, "trace": trace, "provider_transport_attempts": transportAttempts, "calls": actual, "journal_decisions": decisions, "usage": session.Usage, "run_state": state, "session_status": session.Status, "primary_reports": primaryReports, "temporal_histories": histories, "epoch": epoch}
		})
	}
	if path := os.Getenv("MANGO_TEST_FACTCHECK_REPORT"); path != "" {
		data, err := json.MarshalIndent(evidence, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, append(data, '\n'), 0600))
	}
}

// A passing computational workload is not a successful product session. Keep
// the current missing lifecycle contract executable and visible while admission
// is unexposed; do not manufacture Run terminal events/primary notifications.
func TestQuickJSFactcheckLifecycleBoundaryRealServices(t *testing.T) {
	if os.Getenv("MANGO_TEST_WORKFLOW_QUICKJS") != "1" {
		t.Skip("explicit disposable service boundary probe")
	}
	f := newQuickJSVerticalFixture(t, "test/no-evaluator-needed")
	run := f.admit(t, `return "candidate";`)
	for _, mode := range []string{"early-primary-end-turn", "second-open-run", "paused-run"} {
		switch mode {
		case "second-open-run":
			_, err := f.store.AdmitWorkflowRun(f.ctx, run.SessionID, "second", pg.WorkflowRunRequest{Source: `return "second";`, RuntimeProfile: f.profile, Input: json.RawMessage(`{}`), Deadline: time.Now().UTC().Add(time.Hour), MaxInvocations: 1, MaxResultBytes: 128})
			require.NoError(t, err)
		case "paused-run":
			require.NoError(t, f.store.SetWorkflowRunPause(f.ctx, run.SessionID, run.ID, "interrupt", true))
			_, err := f.store.ClaimWorkflowEvaluator(f.ctx, run.SessionID, run.ID, "paused-must-not-progress", 0)
			require.ErrorContains(t, err, "paused")
		}
		admission, err := f.store.AdmitEvents(f.ctx, run.SessionID, []domain.EventDraft{{Type: domain.EvUserMessage, Payload: map[string]any{"content": "primary boundary probe"}}})
		require.NoError(t, err)
		_, err = f.store.CompleteTurn(f.ctx, run.SessionID, admission.Events[0].ID, []domain.EventDraft{{Type: domain.EvSessionStatusIdle, Payload: map[string]any{"stop_reason": map[string]any{"type": "end_turn"}}}}, domain.StatusIdle)
		require.NoError(t, err)
		session, err := f.store.GetSession(f.ctx, run.SessionID)
		require.NoError(t, err)
		require.Equal(t, domain.StatusRunning, session.Status)
		var open, ended int
		require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM workflow_runs WHERE session_id=$1 AND state='open'),(SELECT count(*) FROM events WHERE session_id=$1 AND type='workflow_run.status_ended')`, run.SessionID).Scan(&open, &ended))
		require.Positive(t, open)
		require.Zero(t, ended)
		var idle int
		require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT count(*) FROM events WHERE session_id=$1 AND type='session.status_idle'`, run.SessionID).Scan(&idle))
		require.Zero(t, idle)
		t.Logf("%s: primary end_turn suppressed with %d open Runs and zero Run terminal events; aggregate remains blocked", mode, open)
	}
}
