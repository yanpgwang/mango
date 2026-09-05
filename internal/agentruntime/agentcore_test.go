package agentruntime

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/sandbox"
	"github.com/yanpgwang/mango/internal/sandbox/sandboxtest"
)

type captureSink struct {
	drafts []domain.EventDraft
	events []domain.Event
	n      int
}

func (s *captureSink) Emit(_ context.Context, d []domain.EventDraft) ([]domain.Event, error) {
	s.drafts = append(s.drafts, d...)
	out := make([]domain.Event, len(d))
	for i, dr := range d {
		s.n++
		id := dr.ID
		if id == "" {
			id = fmt.Sprintf("evt_%d", s.n)
		}
		out[i] = domain.Event{ID: id, Type: dr.Type, Payload: dr.Payload}
	}
	s.events = append(s.events, out...)
	return out, nil
}

type journalCall struct {
	operation      string
	stepID         string
	ordinal        int
	toolUseEventID string
	toolName       string
	input          map[string]any
	result         domain.ToolStepResult
}

type captureToolJournal struct {
	calls []journalCall
	next  int
	fail  string
	trace *[]string
}

func (j *captureToolJournal) traceOperation(operation string) {
	if j.trace != nil {
		*j.trace = append(*j.trace, operation)
	}
}

func (j *captureToolJournal) Prepare(
	_ context.Context,
	ordinal int,
	toolUseEventID string,
	toolName string,
	input map[string]any,
) (string, error) {
	j.next++
	stepID := fmt.Sprintf("tstep_%d", j.next)
	j.calls = append(j.calls, journalCall{
		operation:      "prepare",
		stepID:         stepID,
		ordinal:        ordinal,
		toolUseEventID: toolUseEventID,
		toolName:       toolName,
		input:          input,
	})
	j.traceOperation("prepare")
	if j.fail == "prepare" {
		return "", fmt.Errorf("journal prepare failed")
	}
	return stepID, nil
}

func (j *captureToolJournal) Start(_ context.Context, stepID string) error {
	j.calls = append(j.calls, journalCall{operation: "start", stepID: stepID})
	j.traceOperation("start")
	if j.fail == "start" {
		return fmt.Errorf("journal start failed")
	}
	return nil
}

func (j *captureToolJournal) Complete(
	_ context.Context,
	stepID string,
	result domain.ToolStepResult,
) error {
	j.calls = append(j.calls, journalCall{operation: "complete", stepID: stepID, result: result})
	j.traceOperation("complete")
	if j.fail == "complete" {
		return fmt.Errorf("journal complete failed")
	}
	return nil
}

// draftTypes returns the ordered list of emitted event types.
func draftTypes(s *captureSink) []string {
	types := make([]string, len(s.drafts))
	for i, d := range s.drafts {
		types[i] = d.Type
	}
	return types
}

// hasSeq reports whether want appears as an ordered (not necessarily
// contiguous) subsequence of got.
func hasSeq(got []string, want ...string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

func TestAgentCore_EmitsAgentMessageFromModel(t *testing.T) {
	core := NewAgentCore(model.NewFake(), domain.NewSeqIDGen())
	sink := &captureSink{}
	sys := "be helpful"
	_, err := core.Run(context.Background(), RunRequest{
		SessionID:     "sesn_1",
		Trigger:       domain.Event{Type: domain.EvUserMessage},
		Messages:      []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "ping"}}}},
		AgentSnapshot: domain.Agent{Model: domain.Model{ID: "m"}, System: &sys},
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.drafts) != 1 || sink.drafts[0].Type != domain.EvAgentMessage {
		t.Fatalf("drafts = %#v, want one agent.message", sink.drafts)
	}
	content, _ := sink.drafts[0].Payload["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %#v, want one block", sink.drafts[0].Payload["content"])
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "echo: ping" {
		t.Fatalf("block = %#v, want text 'echo: ping'", block)
	}
}

// previewCaptureSink implements both EventSink and PreviewEmitter. It records
// the preview start id, the number of deltas, the concatenated delta text, and
// every emitted draft (with its committed id) so a test can assert the preview
// stream and the persisted agent.message share one id.
type previewCaptureSink struct {
	startID    string
	startType  string
	deltaCount int
	deltaText  string
	emitted    []domain.EventDraft
}

func (s *previewCaptureSink) Emit(_ context.Context, drafts []domain.EventDraft) ([]domain.Event, error) {
	out := make([]domain.Event, len(drafts))
	for i, d := range drafts {
		s.emitted = append(s.emitted, d)
		out[i] = domain.Event{ID: d.ID, Type: d.Type, Payload: d.Payload}
	}
	return out, nil
}

func (s *previewCaptureSink) PreviewStart(eventID, eventType string) {
	s.startID = eventID
	s.startType = eventType
}

func (s *previewCaptureSink) PreviewDelta(_ string, _ int, text string) {
	s.deltaCount++
	s.deltaText += text
}

func TestAgentCore_EmitsPreviewThenPersistedMessage(t *testing.T) {
	core := NewAgentCore(model.NewFake(), domain.NewSeqIDGen())
	sink := &previewCaptureSink{}
	_, err := core.Run(context.Background(), RunRequest{
		Trigger:       domain.Event{Type: domain.EvUserMessage},
		Messages:      []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "hi"}}}},
		AgentSnapshot: domain.Agent{Model: domain.Model{ID: "m"}},
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	// one PreviewStart, >=1 PreviewDelta, then a persisted agent.message with the SAME id
	if sink.startID == "" || sink.deltaCount < 1 {
		t.Fatalf("preview: startID=%q deltas=%d", sink.startID, sink.deltaCount)
	}
	if sink.startType != domain.EvAgentMessage {
		t.Fatalf("preview start type = %q, want %q", sink.startType, domain.EvAgentMessage)
	}
	if len(sink.emitted) != 1 || sink.emitted[0].Type != domain.EvAgentMessage {
		t.Fatalf("emitted = %#v, want one agent.message", sink.emitted)
	}
	if sink.emitted[0].ID != sink.startID {
		t.Fatalf("persisted id %q != preview start id %q", sink.emitted[0].ID, sink.startID)
	}
	// delta text concatenates to the persisted message text
	if sink.deltaText != "echo: hi" {
		t.Fatalf("delta text = %q, want 'echo: hi'", sink.deltaText)
	}
}

func TestAgentCore_EmptyResponseEmitsNothing(t *testing.T) {
	core := NewAgentCore(emptyClient{}, domain.NewSeqIDGen())
	sink := &captureSink{}
	_, err := core.Run(context.Background(), RunRequest{
		Trigger:  domain.Event{Type: domain.EvUserMessage},
		Messages: []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "x"}}}},
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.drafts) != 0 {
		t.Fatalf("drafts = %#v, want none for empty model response", sink.drafts)
	}
}

type emptyClient struct{}

func (emptyClient) CreateMessage(_ context.Context, _ model.Request) (model.Response, error) {
	return model.Response{StopReason: "end_turn"}, nil
}

func (c emptyClient) CreateMessageStream(ctx context.Context, req model.Request, _ func(index int, text string)) (model.Response, error) {
	return c.CreateMessage(ctx, req)
}

type thinkingClient struct{}

func (thinkingClient) CreateMessage(context.Context, model.Request) (model.Response, error) {
	return model.Response{
		StopReason: "end_turn",
		Content: []domain.ContentBlock{
			{Type: "thinking", Text: "must remain private"},
			{Type: "text", Text: "public answer"},
		},
	}, nil
}

func (c thinkingClient) CreateMessageStream(
	ctx context.Context,
	req model.Request,
	_ func(index int, text string),
) (model.Response, error) {
	return c.CreateMessage(ctx, req)
}

func TestAgentCore_PublishesPrivacyPreservingThinkingEvent(t *testing.T) {
	core := NewAgentCore(thinkingClient{}, domain.NewSeqIDGen())
	sink := &captureSink{}
	system := "think"
	_, err := core.Run(context.Background(), RunRequest{
		SessionID:     "sesn_thinking",
		Trigger:       domain.Event{Type: domain.EvUserMessage},
		Messages:      []domain.Message{{Role: domain.RoleUser}},
		AgentSnapshot: domain.Agent{Model: domain.Model{ID: "m"}, System: &system},
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if got := draftTypes(sink); !slices.Equal(got, []string{
		domain.EvAgentThinking, domain.EvAgentMessage,
	}) {
		t.Fatalf("draft types = %v", got)
	}
	if len(sink.drafts[0].Payload) != 0 {
		t.Fatalf("thinking payload exposed private content: %#v", sink.drafts[0].Payload)
	}
}

func TestAgentCore_NonUserTriggerIsNoop(t *testing.T) {
	core := NewAgentCore(model.NewFake(), domain.NewSeqIDGen())
	sink := &captureSink{}
	if _, err := core.Run(context.Background(), RunRequest{
		Trigger: domain.Event{Type: domain.EvUserInterrupt},
	}, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.drafts) != 0 {
		t.Fatalf("drafts = %#v, want none for non-user trigger", sink.drafts)
	}
}

// TestEnabledBuiltinSchemas_AllOfferedSchemasAreObjects guards C1: every
// built-in schema offered to the model must be a non-nil JSON Schema object.
// With the default toolset all eight built-ins are enabled, including
// glob/grep and the provider-native web_fetch/web_search path. The real
// Anthropic API rejects a tool declared with "input_schema":null (400), so
// every configured built-in still needs a legal schema object.
func TestEnabledBuiltinSchemas_AllOfferedSchemasAreObjects(t *testing.T) {
	// Default toolset: {"type":"agent_toolset_20260401"} with no configs →
	// DefaultEnabled=true → all eight built-ins offered.
	ts := domain.ToolSet{Builtin: &domain.BuiltinToolset{
		DefaultEnabled: true,
		DefaultPolicy:  domain.PermissionPolicy{Type: "always_allow"},
	}}
	schemas := enabledBuiltinSchemas(ts)
	if len(schemas) != len(domain.BuiltinToolNames) {
		t.Fatalf("offered %d builtin schemas, want %d", len(schemas), len(domain.BuiltinToolNames))
	}
	for _, s := range schemas {
		if s.Type != "" {
			continue
		}
		if s.InputSchema == nil {
			t.Fatalf("tool %q offered with nil InputSchema (serializes to input_schema:null → 400)", s.Name)
		}
		if typ, _ := s.InputSchema["type"].(string); typ != "object" {
			t.Fatalf("tool %q InputSchema type = %v, want object", s.Name, s.InputSchema["type"])
		}
	}
}

func TestEnabledBuiltinSchemas_UsesNativeWebDeclarations(t *testing.T) {
	ts := domain.ToolSet{Builtin: &domain.BuiltinToolset{
		DefaultEnabled: true,
		DefaultPolicy:  domain.PermissionPolicy{Type: "always_allow"},
	}}
	native := make(map[string]model.ToolSchema)
	for _, schema := range enabledBuiltinSchemas(ts) {
		if schema.Type != "" {
			native[schema.Name] = schema
		}
	}
	if native["web_search"].Type != "web_search_20260318" ||
		native["web_fetch"].Type != "web_fetch_20260318" {
		t.Fatalf("native web schemas = %#v", native)
	}
	if native["web_search"].InputSchema != nil ||
		native["web_fetch"].InputSchema != nil {
		t.Fatalf("native tools must not carry client input schemas: %#v", native)
	}
}

func TestValidateToolCapabilities_RejectsApprovalForNativeWeb(t *testing.T) {
	ts := domain.ToolSet{Builtin: &domain.BuiltinToolset{
		DefaultEnabled: true,
		DefaultPolicy:  domain.PermissionPolicy{Type: "always_allow"},
		Configs: []domain.BuiltinConfig{{
			Name: "web_search",
			Policy: &domain.PermissionPolicy{
				Type: "always_ask",
			},
		}},
	}}
	if err := ValidateToolCapabilities(ts); err == nil {
		t.Fatal("expected provider-native web_search always_ask to be rejected")
	}
}

func TestEnabledSelfHostedToolSchemasKeepWebOnProviderAndBashOnWorker(t *testing.T) {
	toolSet, err := domain.ParseTools([]any{map[string]any{
		"type": domain.BuiltinToolsetType,
	}})
	if err != nil {
		t.Fatal(err)
	}
	webTools := 0
	for _, schema := range EnabledSelfHostedToolSchemas(toolSet) {
		if schema.Name == "bash" {
			properties := schema.InputSchema["properties"].(map[string]any)
			for _, field := range []string{"command", "restart", "timeout_ms"} {
				if _, ok := properties[field]; !ok {
					t.Fatalf("self-hosted bash schema omitted %q: %#v", field, schema.InputSchema)
				}
			}
		}
		if schema.Name != "web_search" && schema.Name != "web_fetch" {
			continue
		}
		webTools++
		if schema.Type != schema.Name+"_20260318" || schema.InputSchema != nil {
			t.Fatalf("self-hosted web schema = %+v, want provider-native tool", schema)
		}
	}
	if webTools != 2 {
		t.Fatalf("self-hosted schemas included %d web tools, want 2", webTools)
	}
}

func TestEnabledSelfHostedToolSchemasHonorDisabledWebAndCustomTools(t *testing.T) {
	toolSet, err := domain.ParseTools([]any{
		map[string]any{
			"type": domain.BuiltinToolsetType,
			"configs": []any{
				map[string]any{"name": "web_search", "enabled": false},
				map[string]any{"name": "web_fetch", "enabled": false},
			},
		},
		map[string]any{"type": "custom", "name": "lookup", "description": "Look up a record",
			"input_schema": map[string]any{"type": "object"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	schemas := EnabledSelfHostedToolSchemas(toolSet)
	if len(schemas) != 7 {
		t.Fatalf("schemas = %+v, want six sandbox tools and one custom tool", schemas)
	}
	for _, schema := range schemas {
		if schema.Type != "" || schema.Name == "web_search" || schema.Name == "web_fetch" {
			t.Fatalf("disabled Web tool was offered: %+v", schema)
		}
	}
	if last := schemas[len(schemas)-1]; last.Name != "lookup" || last.Description != "Look up a record" || last.InputSchema["type"] != "object" {
		t.Fatalf("custom tool changed: %+v", last)
	}
}

func TestAgentCore_ExecutesBuiltinToolLoop(t *testing.T) {
	sb := sandboxtest.Docker(t)
	if err := sb.WriteFile(context.Background(), "note.txt", []byte("hi")); err != nil {
		t.Fatal(err)
	}

	core := NewAgentCore(model.NewFake(), domain.NewSeqIDGen())
	sink := &captureSink{}
	journal := &captureToolJournal{}
	enabled := true
	ts := domain.ToolSet{Builtin: &domain.BuiltinToolset{
		DefaultEnabled: true,
		DefaultPolicy:  domain.PermissionPolicy{Type: "always_allow"},
		Configs:        []domain.BuiltinConfig{{Name: "bash", Enabled: &enabled}},
	}}
	_, err := core.Run(context.Background(), RunRequest{
		Trigger:       domain.Event{Type: domain.EvUserMessage},
		Messages:      []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "cat note.txt"}}}},
		ToolSet:       ts,
		Sandbox:       sb,
		ToolJournal:   journal,
		AgentSnapshot: domain.Agent{Model: domain.Model{ID: "m"}},
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	// Expect: agent.tool_use (bash), agent.tool_result, then agent.message (fake ends turn).
	types := draftTypes(sink)
	if !hasSeq(types, domain.EvAgentToolUse, domain.EvAgentToolResult, domain.EvAgentMessage) {
		t.Fatalf("draft types = %v", types)
	}

	// The tool_result must correlate to the committed id of the tool_use event.
	var useID, resultFor string
	var evaluatedPermission any
	for _, e := range sink.events {
		switch e.Type {
		case domain.EvAgentToolUse:
			useID = e.ID
			evaluatedPermission = e.Payload["evaluated_permission"]
		case domain.EvAgentToolResult:
			resultFor, _ = e.Payload["tool_use_id"].(string)
		}
	}
	if useID == "" || useID != resultFor {
		t.Fatalf("tool_result tool_use_id = %q, want committed use id %q", resultFor, useID)
	}
	if evaluatedPermission != "allow" {
		t.Fatalf("evaluated_permission = %#v, want allow", evaluatedPermission)
	}
	if len(journal.calls) != 3 ||
		journal.calls[0].operation != "prepare" ||
		journal.calls[1].operation != "start" ||
		journal.calls[2].operation != "complete" {
		t.Fatalf("journal calls = %#v, want prepare/start/complete", journal.calls)
	}
	if journal.calls[0].ordinal != 0 || journal.calls[0].toolUseEventID != useID ||
		journal.calls[0].toolName != "bash" {
		t.Fatalf("prepared call = %#v, want ordinal 0 and tool_use id %q", journal.calls[0], useID)
	}
	if len(journal.calls[2].result.Content) == 0 {
		t.Fatalf("completed result = %#v, want durable tool output", journal.calls[2].result)
	}
}

// TestAgentCore_CustomToolParksWithRequiresAction verifies that when the model
// calls a custom tool (one the core cannot execute), the run emits
// agent.custom_tool_use and returns a RunOutcome parked on requires_action with
// that committed event id. The core never emits a terminal status itself.
func TestAgentCore_CustomToolParksWithRequiresAction(t *testing.T) {
	core := NewAgentCore(model.NewFake(), domain.NewSeqIDGen())
	sink := &captureSink{}
	// A custom-only toolset: model.NewFake requests the first offered tool on the
	// first turn (no tool_result yet), so it calls the custom tool get_metrics.
	ts := domain.ToolSet{Custom: []domain.CustomTool{{
		Name: "get_metrics", InputSchema: map[string]any{"type": "object"},
	}}}
	outcome, err := core.Run(context.Background(), RunRequest{
		Trigger:       domain.Event{Type: domain.EvUserMessage},
		Messages:      []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "metrics?"}}}},
		ToolSet:       ts,
		AgentSnapshot: domain.Agent{Model: domain.Model{ID: "m"}},
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.RequiresAction {
		t.Fatalf("outcome = %#v, want RequiresAction", outcome)
	}

	// The parked event must be an agent.custom_tool_use and its committed id must
	// be exactly the id reported in ActionEventIDs.
	var useID string
	for _, e := range sink.events {
		if e.Type == domain.EvAgentCustomToolUse {
			useID = e.ID
		}
	}
	if useID == "" {
		t.Fatalf("no agent.custom_tool_use emitted; drafts = %v", draftTypes(sink))
	}
	if len(outcome.ActionEventIDs) != 1 || outcome.ActionEventIDs[0] != useID {
		t.Fatalf("ActionEventIDs = %v, want [%s]", outcome.ActionEventIDs, useID)
	}
}

// spySandbox records whether tool execution touched the sandbox. The
// confirmation-resume deny path must never invoke it; the allow path must.
type spySandbox struct {
	execN, writeN, readN int
	files                map[string][]byte
	trace                *[]string
}

func (s *spySandbox) Exec(_ context.Context, _ sandbox.Command) (*sandbox.Result, error) {
	s.execN++
	if s.trace != nil {
		*s.trace = append(*s.trace, "exec")
	}
	return &sandbox.Result{}, nil
}
func (s *spySandbox) ReadFile(_ context.Context, path string) ([]byte, error) {
	s.readN++
	if s.trace != nil {
		*s.trace = append(*s.trace, "read")
	}
	return s.files[path], nil
}
func (s *spySandbox) WriteFile(_ context.Context, path string, data []byte) error {
	s.writeN++
	if s.trace != nil {
		*s.trace = append(*s.trace, "write")
	}
	if s.files == nil {
		s.files = map[string][]byte{}
	}
	s.files[path] = data
	return nil
}
func (s *spySandbox) Root() string                    { return "/" }
func (s *spySandbox) Destroy(_ context.Context) error { return nil }

func (s *spySandbox) touched() bool { return s.execN+s.writeN+s.readN > 0 }

// askBuiltinToolSet enables exactly one built-in under an always_ask policy so a
// confirmation resume can execute it.
func askBuiltinToolSet(name string) domain.ToolSet {
	enabled := true
	return domain.ToolSet{Builtin: &domain.BuiltinToolset{
		DefaultEnabled: false,
		DefaultPolicy:  domain.PermissionPolicy{Type: "always_allow"},
		Configs: []domain.BuiltinConfig{{
			Name: name, Enabled: &enabled,
			Policy: &domain.PermissionPolicy{Type: "always_ask"},
		}},
	}}
}

// origToolUse builds the committed always_ask agent.tool_use event a
// confirmation references, as the app resolves it from causal history.
func origToolUse(id, name string, input map[string]any) *domain.Event {
	return &domain.Event{
		ID:   id,
		Type: domain.EvAgentToolUse,
		Payload: map[string]any{
			"name":                 name,
			"input":                input,
			"evaluated_permission": "ask",
		},
	}
}

// TestAgentCore_ConfirmationAllowExecutesAndContinues verifies the allow resume:
// the original built-in executes exactly once through the sandbox, an
// agent.tool_result correlated to the ORIGINAL committed event id carries the
// actual content, the paired assistant tool_use + user tool_result are threaded
// into the model conversation, and the continued turn reaches end_turn.
func TestAgentCore_ConfirmationAllowExecutesAndContinues(t *testing.T) {
	fake := model.NewFake()
	core := NewAgentCore(fake, domain.NewSeqIDGen())
	sink := &captureSink{}
	sb := &spySandbox{}
	journal := &captureToolJournal{}
	input := map[string]any{"path": "out.txt", "file_text": "hello"}
	outcome, err := core.Run(context.Background(), RunRequest{
		Trigger: domain.Event{Type: domain.EvUserToolConfirmation, Payload: map[string]any{
			"tool_use_id": "evt_use", "result": "allow",
		}},
		Messages:         []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "write it"}}}},
		ToolSet:          askBuiltinToolSet("write"),
		Sandbox:          sb,
		ConfirmedToolUse: origToolUse("evt_use", "write", input),
		ToolJournal:      journal,
		AgentSnapshot:    domain.Agent{Model: domain.Model{ID: "m"}},
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.RequiresAction {
		t.Fatalf("outcome = %#v, want normal end_turn", outcome)
	}
	// The write built-in ran exactly once against the sandbox.
	if sb.writeN != 1 {
		t.Fatalf("sandbox WriteFile calls = %d, want 1", sb.writeN)
	}
	if got := string(sb.files["out.txt"]); got != "hello" {
		t.Fatalf("written file = %q, want %q", got, "hello")
	}
	// The agent.tool_result correlates to the ORIGINAL committed id, not a new one.
	var result *domain.Event
	for i := range sink.events {
		if sink.events[i].Type == domain.EvAgentToolResult {
			result = &sink.events[i]
		}
	}
	if result == nil {
		t.Fatalf("no agent.tool_result emitted; drafts = %v", draftTypes(sink))
	}
	if result.Payload["tool_use_id"] != "evt_use" {
		t.Fatalf("tool_result tool_use_id = %v, want evt_use", result.Payload["tool_use_id"])
	}
	if isErr, _ := result.Payload["is_error"].(bool); isErr {
		t.Fatalf("allow result is_error = true, want false")
	}
	// The next model request saw a paired assistant tool_use + user tool_result.
	if !messagesContainPair(fake.LastRequest().Messages, "evt_use") {
		t.Fatalf("model request missing paired tool_use/tool_result for evt_use: %#v", fake.LastRequest().Messages)
	}
	// The continued turn reached end_turn (an agent.message was emitted).
	if !hasSeq(draftTypes(sink), domain.EvAgentToolResult, domain.EvAgentMessage) {
		t.Fatalf("draft types = %v, want tool_result then agent.message", draftTypes(sink))
	}
	if len(journal.calls) != 3 ||
		journal.calls[0].operation != "prepare" ||
		journal.calls[0].toolUseEventID != "evt_use" ||
		journal.calls[1].operation != "start" ||
		journal.calls[2].operation != "complete" {
		t.Fatalf("journal calls = %#v, want durable execution for evt_use", journal.calls)
	}
}

func TestAgentCore_JournalCompletionFailureStopsAfterSideEffectWithoutEmittingResult(t *testing.T) {
	core := NewAgentCore(model.NewFake(), domain.NewSeqIDGen())
	sink := &captureSink{}
	var trace []string
	sb := &spySandbox{trace: &trace}
	journal := &captureToolJournal{fail: "complete", trace: &trace}
	input := map[string]any{"path": "out.txt", "file_text": "hello"}

	_, err := core.Run(context.Background(), RunRequest{
		Trigger: domain.Event{Type: domain.EvUserToolConfirmation, Payload: map[string]any{
			"tool_use_id": "evt_use", "result": "allow",
		}},
		Messages:         []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "write it"}}}},
		ToolSet:          askBuiltinToolSet("write"),
		Sandbox:          sb,
		ConfirmedToolUse: origToolUse("evt_use", "write", input),
		ToolJournal:      journal,
		AgentSnapshot:    domain.Agent{Model: domain.Model{ID: "m"}},
	}, sink)
	if err == nil || !strings.Contains(err.Error(), "journal complete failed") {
		t.Fatalf("err = %v, want journal completion failure", err)
	}
	if sb.writeN != 1 {
		t.Fatalf("sandbox WriteFile calls = %d, want the side effect to have returned once", sb.writeN)
	}
	for _, event := range sink.events {
		if event.Type == domain.EvAgentToolResult {
			t.Fatalf("tool result emitted without a durable journal result: %#v", event)
		}
	}
	if len(journal.calls) != 3 ||
		journal.calls[0].operation != "prepare" ||
		journal.calls[1].operation != "start" ||
		journal.calls[2].operation != "complete" {
		t.Fatalf("journal calls = %#v, want prepare/start/complete", journal.calls)
	}
	if got := strings.Join(trace, ","); got != "prepare,start,write,complete" {
		t.Fatalf("execution trace = %q, want prepare,start,write,complete", got)
	}
}

// TestAgentCore_ConfirmationDenySkipsExecutorIncludesDenyMessage verifies the
// deny resume: the sandbox/executor is never invoked, the agent.tool_result is
// an error carrying the deny_message text, and the continued turn still reaches
// end_turn so the model can react.
func TestAgentCore_ConfirmationDenySkipsExecutorIncludesDenyMessage(t *testing.T) {
	fake := model.NewFake()
	core := NewAgentCore(fake, domain.NewSeqIDGen())
	sink := &captureSink{}
	sb := &spySandbox{}
	outcome, err := core.Run(context.Background(), RunRequest{
		Trigger: domain.Event{Type: domain.EvUserToolConfirmation, Payload: map[string]any{
			"tool_use_id": "evt_use", "result": "deny", "deny_message": "not allowed here",
		}},
		Messages:         []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "write it"}}}},
		ToolSet:          askBuiltinToolSet("write"),
		Sandbox:          sb,
		ConfirmedToolUse: origToolUse("evt_use", "write", map[string]any{"path": "out.txt", "file_text": "hello"}),
		AgentSnapshot:    domain.Agent{Model: domain.Model{ID: "m"}},
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.RequiresAction {
		t.Fatalf("outcome = %#v, want normal end_turn", outcome)
	}
	if sb.touched() {
		t.Fatalf("sandbox was invoked on deny (exec=%d write=%d read=%d), want none", sb.execN, sb.writeN, sb.readN)
	}
	var result *domain.Event
	for i := range sink.events {
		if sink.events[i].Type == domain.EvAgentToolResult {
			result = &sink.events[i]
		}
	}
	if result == nil {
		t.Fatalf("no agent.tool_result emitted; drafts = %v", draftTypes(sink))
	}
	if result.Payload["tool_use_id"] != "evt_use" {
		t.Fatalf("tool_result tool_use_id = %v, want evt_use", result.Payload["tool_use_id"])
	}
	if isErr, _ := result.Payload["is_error"].(bool); !isErr {
		t.Fatalf("deny result is_error = false, want true")
	}
	if !resultTextContains(result.Payload, "not allowed here") {
		t.Fatalf("deny result content = %#v, want it to include deny_message", result.Payload["content"])
	}
	if !messagesContainPair(fake.LastRequest().Messages, "evt_use") {
		t.Fatalf("model request missing paired tool_use/tool_result for evt_use")
	}
}

// TestAgentCore_ConfirmationInvalidFailsWithoutExecution verifies that a
// malformed or unresolvable confirmation fails safely: it never invokes the
// sandbox and never emits an agent.tool_result. Each case overrides the trigger
// payload and/or sandbox; the default trigger is a well-formed allow.
func TestAgentCore_ConfirmationInvalidFailsWithoutExecution(t *testing.T) {
	// alwaysAllowToolSet enables the named built-in but under always_allow, which
	// the confirmation path must reject: it only models an ask gate.
	alwaysAllowToolSet := func(name string) domain.ToolSet {
		enabled := true
		return domain.ToolSet{Builtin: &domain.BuiltinToolset{
			DefaultEnabled: false,
			DefaultPolicy:  domain.PermissionPolicy{Type: "always_allow"},
			Configs: []domain.BuiltinConfig{{
				Name: name, Enabled: &enabled,
				Policy: &domain.PermissionPolicy{Type: "always_allow"},
			}},
		}}
	}

	cases := []struct {
		name    string
		confirm *domain.Event
		ts      domain.ToolSet
		// trigger overrides the default well-formed allow trigger payload.
		trigger map[string]any
		// nilSandbox passes a nil Sandbox instead of the spy.
		nilSandbox bool
	}{
		{
			name:    "missing original action",
			confirm: nil,
			ts:      askBuiltinToolSet("write"),
		},
		{
			name: "original not evaluated ask",
			confirm: &domain.Event{ID: "evt_use", Type: domain.EvAgentToolUse, Payload: map[string]any{
				"name": "write", "input": map[string]any{}, "evaluated_permission": "allow",
			}},
			ts: askBuiltinToolSet("write"),
		},
		{
			name:    "original tool is not a built-in",
			confirm: origToolUse("evt_use", "get_metrics", map[string]any{}),
			ts:      askBuiltinToolSet("write"),
		},
		{
			name:    "built-in disabled in toolset",
			confirm: origToolUse("evt_use", "read", map[string]any{"path": "x"}),
			ts:      askBuiltinToolSet("write"), // read not enabled
		},
		{
			name:    "trigger references a different action",
			confirm: origToolUse("evt_use", "write", map[string]any{"path": "out.txt", "file_text": "hi"}),
			ts:      askBuiltinToolSet("write"),
			trigger: map[string]any{"tool_use_id": "evt_other", "result": "allow"},
		},
		{
			name:    "missing result",
			confirm: origToolUse("evt_use", "write", map[string]any{"path": "out.txt", "file_text": "hi"}),
			ts:      askBuiltinToolSet("write"),
			trigger: map[string]any{"tool_use_id": "evt_use"},
		},
		{
			name:    "invalid result value",
			confirm: origToolUse("evt_use", "write", map[string]any{"path": "out.txt", "file_text": "hi"}),
			ts:      askBuiltinToolSet("write"),
			trigger: map[string]any{"tool_use_id": "evt_use", "result": "maybe"},
		},
		{
			name:    "allow with deny_message",
			confirm: origToolUse("evt_use", "write", map[string]any{"path": "out.txt", "file_text": "hi"}),
			ts:      askBuiltinToolSet("write"),
			trigger: map[string]any{"tool_use_id": "evt_use", "result": "allow", "deny_message": "nope"},
		},
		{
			name:    "deny with non-string deny_message",
			confirm: origToolUse("evt_use", "write", map[string]any{"path": "out.txt", "file_text": "hi"}),
			ts:      askBuiltinToolSet("write"),
			trigger: map[string]any{"tool_use_id": "evt_use", "result": "deny", "deny_message": 42},
		},
		{
			name:    "current policy is not always_ask",
			confirm: origToolUse("evt_use", "write", map[string]any{"path": "out.txt", "file_text": "hi"}),
			ts:      alwaysAllowToolSet("write"),
		},
		{
			name: "missing input",
			confirm: &domain.Event{ID: "evt_use", Type: domain.EvAgentToolUse, Payload: map[string]any{
				"name": "write", "evaluated_permission": "ask",
			}},
			ts: askBuiltinToolSet("write"),
		},
		{
			name: "wrong-type input",
			confirm: &domain.Event{ID: "evt_use", Type: domain.EvAgentToolUse, Payload: map[string]any{
				"name": "write", "input": "not-an-object", "evaluated_permission": "ask",
			}},
			ts: askBuiltinToolSet("write"),
		},
		{
			name:       "allow with nil sandbox",
			confirm:    origToolUse("evt_use", "write", map[string]any{"path": "out.txt", "file_text": "hi"}),
			ts:         askBuiltinToolSet("write"),
			nilSandbox: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			core := NewAgentCore(model.NewFake(), domain.NewSeqIDGen())
			sink := &captureSink{}
			sb := &spySandbox{}
			trigger := tc.trigger
			if trigger == nil {
				trigger = map[string]any{"tool_use_id": "evt_use", "result": "allow"}
			}
			var box sandbox.Sandbox = sb
			if tc.nilSandbox {
				box = nil
			}
			_, err := core.Run(context.Background(), RunRequest{
				Trigger:          domain.Event{Type: domain.EvUserToolConfirmation, Payload: trigger},
				Messages:         []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "x"}}}},
				ToolSet:          tc.ts,
				Sandbox:          box,
				ConfirmedToolUse: tc.confirm,
				AgentSnapshot:    domain.Agent{Model: domain.Model{ID: "m"}},
			}, sink)
			if err == nil {
				t.Fatalf("Run returned nil error, want a safe failure")
			}
			if sb.touched() {
				t.Fatalf("sandbox was invoked on invalid confirmation, want none")
			}
			for _, d := range sink.drafts {
				if d.Type == domain.EvAgentToolResult {
					t.Fatalf("agent.tool_result emitted on invalid confirmation, want none")
				}
			}
		})
	}
}

// TestAgentCore_ConfirmationDenyWithNilSandbox proves deny never requires a
// sandbox: with a nil Sandbox the run still emits an is_error agent.tool_result
// and reaches end_turn without touching any executor.
func TestAgentCore_ConfirmationDenyWithNilSandbox(t *testing.T) {
	fake := model.NewFake()
	core := NewAgentCore(fake, domain.NewSeqIDGen())
	sink := &captureSink{}
	outcome, err := core.Run(context.Background(), RunRequest{
		Trigger: domain.Event{Type: domain.EvUserToolConfirmation, Payload: map[string]any{
			"tool_use_id": "evt_use", "result": "deny", "deny_message": "blocked",
		}},
		Messages:         []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "write it"}}}},
		ToolSet:          askBuiltinToolSet("write"),
		Sandbox:          nil,
		ConfirmedToolUse: origToolUse("evt_use", "write", map[string]any{"path": "out.txt", "file_text": "hello"}),
		AgentSnapshot:    domain.Agent{Model: domain.Model{ID: "m"}},
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.RequiresAction {
		t.Fatalf("outcome = %#v, want normal end_turn", outcome)
	}
	var result *domain.Event
	for i := range sink.events {
		if sink.events[i].Type == domain.EvAgentToolResult {
			result = &sink.events[i]
		}
	}
	if result == nil {
		t.Fatalf("no agent.tool_result emitted; drafts = %v", draftTypes(sink))
	}
	if isErr, _ := result.Payload["is_error"].(bool); !isErr {
		t.Fatalf("deny result is_error = false, want true")
	}
	if !resultTextContains(result.Payload, "blocked") {
		t.Fatalf("deny result content = %#v, want it to include deny_message", result.Payload["content"])
	}
}

// TestAgentCore_ConfirmationMergesRecoveredToolUseWithTrailingAssistantText
// covers the alternation invariant when the parked model response emitted
// assistant text alongside the always_ask tool_use. The dangling tool_use is
// dropped from projected history, so req.Messages ends with an assistant text
// message. Seeding the recovered tool_use must fold into that trailing assistant
// message (text then tool_use) rather than appending a second assistant message,
// so the next model request has strictly alternating roles.
func TestAgentCore_ConfirmationMergesRecoveredToolUseWithTrailingAssistantText(t *testing.T) {
	for _, result := range []string{"allow", "deny"} {
		t.Run(result, func(t *testing.T) {
			fake := model.NewFake()
			core := NewAgentCore(fake, domain.NewSeqIDGen())
			sink := &captureSink{}
			sb := &spySandbox{}
			journal := &captureToolJournal{}
			input := map[string]any{"path": "out.txt", "file_text": "hello"}
			// Projected history: user turn, then an assistant TEXT message that was
			// emitted alongside the parked (now-dropped) always_ask tool_use.
			msgs := []domain.Message{
				{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "write it"}}},
				{Role: domain.RoleAssistant, Content: []domain.ContentBlock{{Type: "text", Text: "I'll write the file."}}},
			}
			trigger := map[string]any{"tool_use_id": "evt_use", "result": result}
			if result == "deny" {
				trigger["deny_message"] = "no"
			}
			_, err := core.Run(context.Background(), RunRequest{
				Trigger:          domain.Event{Type: domain.EvUserToolConfirmation, Payload: trigger},
				Messages:         msgs,
				ToolSet:          askBuiltinToolSet("write"),
				Sandbox:          sb,
				ConfirmedToolUse: origToolUse("evt_use", "write", input),
				ToolJournal:      journal,
				AgentSnapshot:    domain.Agent{Model: domain.Model{ID: "m"}},
			}, sink)
			if err != nil {
				t.Fatal(err)
			}

			got := fake.LastRequest().Messages
			assertAlternatingRoles(t, got)

			// The existing assistant text and the recovered tool_use live in the SAME
			// assistant message, text first then tool_use, followed by the user
			// tool_result. Locate the assistant message that carries the tool_use.
			var asst *domain.Message
			for i := range got {
				for _, b := range got[i].Content {
					if b.Type == "tool_use" && b.ToolUseID == "evt_use" {
						asst = &got[i]
					}
				}
			}
			if asst == nil {
				t.Fatalf("no assistant message carried the recovered tool_use: %#v", got)
			}
			if asst.Role != domain.RoleAssistant {
				t.Fatalf("recovered tool_use is on a %s message, want assistant", asst.Role)
			}
			if len(asst.Content) != 2 ||
				asst.Content[0].Type != "text" || asst.Content[0].Text != "I'll write the file." ||
				asst.Content[1].Type != "tool_use" || asst.Content[1].ToolUseID != "evt_use" {
				t.Fatalf("merged assistant content = %#v, want [text, tool_use] in order", asst.Content)
			}
			if !messagesContainPair(got, "evt_use") {
				t.Fatalf("model request missing paired tool_use/tool_result for evt_use: %#v", got)
			}
			// The caller's slice must not have been mutated in place by the merge.
			if len(msgs) != 2 || len(msgs[1].Content) != 1 {
				t.Fatalf("caller Messages was mutated: %#v", msgs)
			}
		})
	}
}

// assertAlternatingRoles fails if msgs has two consecutive same-role messages,
// which the real Messages API rejects.
func assertAlternatingRoles(t *testing.T, msgs []domain.Message) {
	t.Helper()
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == msgs[i-1].Role {
			t.Fatalf("consecutive %s messages at %d: %#v", msgs[i].Role, i, msgs)
		}
	}
}

// messagesContainPair reports whether msgs hold an assistant tool_use and a user
// tool_result both correlated to toolUseID (the threaded resume pair).
func messagesContainPair(msgs []domain.Message, toolUseID string) bool {
	var use, result bool
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == "tool_use" && b.ToolUseID == toolUseID {
				use = true
			}
			if b.Type == "tool_result" && b.ToolResultFor == toolUseID {
				result = true
			}
		}
	}
	return use && result
}

// resultTextContains reports whether a tool_result payload's content array holds
// the given substring in any text block.
func resultTextContains(payload map[string]any, sub string) bool {
	content, _ := payload["content"].([]any)
	for _, item := range content {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if text, _ := m["text"].(string); strings.Contains(text, sub) {
			return true
		}
	}
	return false
}

// TestAgentCore_AlwaysAskBuiltinParks verifies that an enabled built-in whose
// permission policy is always_ask parks the run: it emits agent.tool_use with
// evaluated_permission "ask" and returns requires_action carrying that id.
func TestAgentCore_AlwaysAskBuiltinParks(t *testing.T) {
	core := NewAgentCore(model.NewFake(), domain.NewSeqIDGen())
	sink := &captureSink{}
	enabled := true
	ts := domain.ToolSet{Builtin: &domain.BuiltinToolset{
		DefaultEnabled: true,
		DefaultPolicy:  domain.PermissionPolicy{Type: "always_allow"},
		Configs: []domain.BuiltinConfig{{
			Name: "bash", Enabled: &enabled,
			Policy: &domain.PermissionPolicy{Type: "always_ask"},
		}},
	}}
	// Only bash is offered so model.NewFake requests it first.
	ts.Builtin.DefaultEnabled = false
	outcome, err := core.Run(context.Background(), RunRequest{
		Trigger:       domain.Event{Type: domain.EvUserMessage},
		Messages:      []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "run ls"}}}},
		ToolSet:       ts,
		AgentSnapshot: domain.Agent{Model: domain.Model{ID: "m"}},
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.RequiresAction || len(outcome.ActionEventIDs) != 1 {
		t.Fatalf("outcome = %#v, want RequiresAction with one id", outcome)
	}
	var evt domain.Event
	for _, e := range sink.events {
		if e.Type == domain.EvAgentToolUse {
			evt = e
		}
	}
	if evt.ID == "" {
		t.Fatalf("no agent.tool_use emitted; drafts = %v", draftTypes(sink))
	}
	if evt.Payload["evaluated_permission"] != "ask" {
		t.Fatalf("evaluated_permission = %v, want ask", evt.Payload["evaluated_permission"])
	}
	if evt.ID != outcome.ActionEventIDs[0] {
		t.Fatalf("ActionEventIDs = %v, want [%s]", outcome.ActionEventIDs, evt.ID)
	}
}
