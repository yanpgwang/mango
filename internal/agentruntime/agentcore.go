package agentruntime

import (
	"context"

	"github.com/yanpgwang/mango/internal/agentruntime/tools"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
)

var _ AgentRuntime = (*AgentCore)(nil)

// maxToolTurns bounds the model<->tool loop within a single Run so a model that
// keeps requesting tools can never spin forever. Reaching the cap ends the turn
// as if the model had stopped; the app layer appends the terminal idle status.
const maxToolTurns = 20

// AgentCore is the self-hosted agent runtime: a bounded orchestration loop that
// replays the projected conversation to the model, emits typed events, and runs
// enabled built-in tools inside the request sandbox. It owns no history (the app
// layer projects it) and touches no database or HTTP. The app layer appends the
// terminal session.status_idle after Run returns.
//
// Each turn calls the model once. Text blocks are emitted as agent.message. If
// the model requests always_allow built-in tools, the core executes each in the
// sandbox, emits the paired agent.tool_use/agent.tool_result events, threads the
// tool_use and tool_result blocks into a local running message list, and loops
// so the model sees the results within the same run. When a turn produces no
// tool_use (end_turn) the loop returns.
//
// With no toolset the loop runs exactly once and behaves like the S1 single
// round: a tool_use stop reason is impossible (no tools are offered), so the
// produced text is emitted and the turn ends.
type AgentCore struct {
	client model.Client
	ids    domain.IDGenerator
}

// NewAgentCore builds the agent core over a model client and an id generator.
// The generator names the committed event ids the core pre-assigns to drafts,
// most importantly the assistant agent.message: when the sink is a
// PreviewEmitter the core generates this id up front so the preview stream
// (PreviewStart / PreviewDelta) and the persisted agent.message share one id.
// It is the same generator kind the store/admission layer uses, so ids the core
// mints are drawn from the same space the sink would otherwise assign.
func NewAgentCore(c model.Client, ids domain.IDGenerator) *AgentCore {
	return &AgentCore{client: c, ids: ids}
}

// drivesModelTurn reports whether a trigger should start a model turn. A plain
// user.message does; so does a user.custom_tool_result that resolves a parked
// custom tool; and so does a user.tool_confirmation that resolves a parked
// always_ask built-in. In each resolution case the projected history now pairs
// the awaited result with its agent.(custom_)tool_use, so the model sees the
// outcome and the loop continues.
//
// For a confirmation the pairing is produced by the core itself (seedConfirmation
// below): the original agent.tool_use is dangling in projected history until its
// tool_result exists, so the core executes-or-denies the recovered built-in,
// emits the agent.tool_result correlated to the original committed id, and
// threads the paired tool_use/tool_result into the run's local conversation
// before the model loop runs. Interrupts and outcome definitions never by
// themselves drive a turn.
func drivesModelTurn(triggerType string) bool {
	switch triggerType {
	case domain.EvUserMessage, domain.EvUserDefineOutcome,
		domain.EvUserCustomToolResult, domain.EvUserToolResult,
		domain.EvUserToolConfirmation:
		return true
	}
	return false
}

func (a *AgentCore) Run(ctx context.Context, req RunRequest, sink EventSink) (RunOutcome, error) {
	if !drivesModelTurn(req.Trigger.Type) {
		return RunOutcome{}, nil
	}
	if err := ValidateToolCapabilities(req.ToolSet); err != nil {
		return RunOutcome{}, err
	}
	system := ""
	if req.AgentSnapshot.System != nil {
		system = *req.AgentSnapshot.System
	}
	toolSchemas := EnabledToolSchemas(req.ToolSet)

	// messages is the local running conversation, seeded from the projected
	// history and grown with assistant tool_use / user tool_result blocks so the
	// model sees tool outcomes within this run.
	messages := req.Messages
	toolOrdinal := 0

	// A user.tool_confirmation resumes a parked always_ask built-in. Before the
	// model loop, resolve the original committed agent.tool_use, execute it (allow)
	// or reject it (deny), emit the agent.tool_result correlated to the original
	// committed event id, and thread the paired tool_use/tool_result into the local
	// conversation. A malformed/unresolvable confirmation fails safely here without
	// executing anything, so the loop below never replays a dangling tool_use.
	if req.Trigger.Type == domain.EvUserToolConfirmation {
		seeded, err := a.seedConfirmation(ctx, req, sink, &toolOrdinal)
		if err != nil {
			return RunOutcome{}, err
		}
		// Merge the seeded pair into the running conversation with the same
		// role-collapsing semantics ProjectMessages uses. When the parked model
		// response emitted assistant text alongside the always_ask tool_use, the
		// dangling tool_use is dropped from projected history, so req.Messages ends
		// with an assistant text message. Blindly appending the seeded assistant
		// tool_use message would then produce two consecutive assistant messages,
		// which the Messages API rejects. Merging folds the recovered tool_use into
		// that trailing assistant message (text then tool_use), preserving strict
		// role alternation before the model loop runs.
		messages = AppendMerging(messages, seeded)
	}

	for turn := 0; turn < maxToolTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return RunOutcome{}, err
		}

		// Assistant text is streamed as a preview when the sink supports it: the
		// core mints the committed agent.message id up front, announces it with
		// PreviewStart, streams each text delta through PreviewDelta, then emits
		// the full agent.message draft carrying that same id. Preview and
		// persisted event are one event correlated by the shared id. When the sink
		// is not a PreviewEmitter the turn falls back to the non-streaming
		// CreateMessage + plain Emit (S1 behavior). Only assistant text previews;
		// the tool loop below is unchanged.
		var resp model.Response
		var err error
		previewer, canPreview := sink.(PreviewEmitter)
		if canPreview {
			messageID := a.ids.NewID(domain.PrefixEvent)
			started := false
			resp, err = a.client.CreateMessageStream(ctx, model.Request{
				Model:    req.AgentSnapshot.Model.ID,
				System:   system,
				Messages: messages,
				Tools:    toolSchemas,
			}, func(index int, text string) {
				if !started {
					previewer.PreviewStart(messageID, domain.EvAgentMessage)
					started = true
				}
				previewer.PreviewDelta(messageID, index, text)
			})
			if err != nil {
				return RunOutcome{}, err
			}
			if HasThinkingBlocks(resp.Content) {
				if _, err := sink.Emit(ctx, []domain.EventDraft{{
					Type: domain.EvAgentThinking, Payload: map[string]any{},
				}}); err != nil {
					return RunOutcome{}, err
				}
			}
			if content := TextBlocksToContent(resp.Content); len(content) > 0 {
				// A text turn that produced no deltas (e.g. a client that never
				// streams) still needs a PreviewStart so the preview id is announced
				// before the persisted agent.message carrying it.
				if !started {
					previewer.PreviewStart(messageID, domain.EvAgentMessage)
				}
				if _, err := sink.Emit(ctx, []domain.EventDraft{{
					ID:      messageID,
					Type:    domain.EvAgentMessage,
					Payload: map[string]any{"content": content},
				}}); err != nil {
					return RunOutcome{}, err
				}
			}
		} else {
			resp, err = a.client.CreateMessage(ctx, model.Request{
				Model:    req.AgentSnapshot.Model.ID,
				System:   system,
				Messages: messages,
				Tools:    toolSchemas,
			})
			if err != nil {
				return RunOutcome{}, err
			}
			if HasThinkingBlocks(resp.Content) {
				if _, err := sink.Emit(ctx, []domain.EventDraft{{
					Type: domain.EvAgentThinking, Payload: map[string]any{},
				}}); err != nil {
					return RunOutcome{}, err
				}
			}
			// Emit any assistant text as an agent.message (S1 behavior).
			if content := TextBlocksToContent(resp.Content); len(content) > 0 {
				if _, err := sink.Emit(ctx, []domain.EventDraft{{
					Type:    domain.EvAgentMessage,
					Payload: map[string]any{"content": content},
				}}); err != nil {
					return RunOutcome{}, err
				}
			}
		}

		// Collect the tool_use blocks this turn requested.
		var toolUses []domain.ContentBlock
		for _, b := range resp.Content {
			if b.Type == "tool_use" {
				toolUses = append(toolUses, b)
			}
		}
		if len(toolUses) == 0 {
			return RunOutcome{}, nil // end_turn: no tools requested.
		}

		// Execute each tool_use. always_allow built-ins run inline and thread their
		// result back into this run. custom tools and always_ask built-ins cannot
		// be resolved by the core: they emit the use event and park the run with
		// requires_action so the app layer stops at idle and a later
		// user.custom_tool_result / user.tool_confirmation resumes a fresh run.
		var assistantBlocks, resultBlocks []domain.ContentBlock
		var actionEventIDs []string
		for _, use := range toolUses {
			enabled, policy := req.ToolSet.BuiltinEnabled(use.ToolName)
			exec, isBuiltin := tools.Registry()[use.ToolName]

			switch {
			case isBuiltin && enabled && policy.Type == "always_allow":
				// Allocate the public correlation id before execution so the durable
				// journal and the eventual event pair refer to the same tool call.
				id := a.ids.NewID(domain.PrefixEvent)
				stepID, err := a.prepareBuiltin(ctx, req, toolOrdinal, id, use.ToolName, use.Input)
				if err != nil {
					return RunOutcome{}, err
				}
				toolOrdinal++
				if _, err := sink.Emit(ctx, []domain.EventDraft{{
					ID:   id,
					Type: domain.EvAgentToolUse,
					Payload: map[string]any{
						"name":                 use.ToolName,
						"input":                use.Input,
						"evaluated_permission": "allow",
					},
				}}); err != nil {
					return RunOutcome{}, err
				}
				result, err := a.executePreparedBuiltin(ctx, req, stepID, use.Input, exec)
				if err != nil {
					return RunOutcome{}, err
				}

				if _, err := sink.Emit(ctx, []domain.EventDraft{{
					Type: domain.EvAgentToolResult,
					Payload: map[string]any{
						"tool_use_id": id,
						"content":     result.Content,
						"is_error":    result.IsError,
					},
				}}); err != nil {
					return RunOutcome{}, err
				}

				assistantBlocks = append(assistantBlocks, domain.ContentBlock{
					Type:      "tool_use",
					ToolUseID: id,
					ToolName:  use.ToolName,
					Input:     use.Input,
				})
				resultBlocks = append(resultBlocks, domain.ContentBlock{
					Type:          "tool_result",
					ToolResultFor: id,
					Text:          FlattenResultText(result.Content),
					IsError:       result.IsError,
				})

			case isBuiltin && enabled:
				// Enabled built-in whose policy is not always_allow (always_ask):
				// emit agent.tool_use carrying the evaluated permission and park for
				// a user.tool_confirmation referencing the committed event id.
				out, err := sink.Emit(ctx, []domain.EventDraft{{
					Type: domain.EvAgentToolUse,
					Payload: map[string]any{
						"name":                 use.ToolName,
						"input":                use.Input,
						"evaluated_permission": "ask",
					},
				}})
				if err != nil {
					return RunOutcome{}, err
				}
				actionEventIDs = append(actionEventIDs, out[0].ID)

			default:
				// Custom tool (not a built-in the core can execute): emit
				// agent.custom_tool_use and park for a user.custom_tool_result
				// referencing the committed event id.
				out, err := sink.Emit(ctx, []domain.EventDraft{{
					Type: domain.EvAgentCustomToolUse,
					Payload: map[string]any{
						"name":  use.ToolName,
						"input": use.Input,
					},
				}})
				if err != nil {
					return RunOutcome{}, err
				}
				actionEventIDs = append(actionEventIDs, out[0].ID)
			}
		}

		// Any parked tool ends the run: the core cannot make progress until the
		// app admits the awaited result as a new trigger. The app appends the
		// terminal session.status_idle{stop_reason: requires_action} referencing
		// these ids.
		if len(actionEventIDs) > 0 {
			return RunOutcome{RequiresAction: true, ActionEventIDs: actionEventIDs}, nil
		}

		// If nothing executed, there is no result to feed back; end the turn to
		// avoid an unbounded no-progress loop.
		if len(assistantBlocks) == 0 {
			return RunOutcome{}, nil
		}

		messages = append(messages,
			domain.Message{Role: domain.RoleAssistant, Content: assistantBlocks},
			domain.Message{Role: domain.RoleUser, Content: resultBlocks},
		)
	}
	return RunOutcome{}, nil
}

// ValidateToolCapabilities rejects permission semantics the selected execution
// owner cannot honor. Native server tools execute inside the provider request,
// so this runtime cannot durably pause between request and execution.
func ValidateToolCapabilities(ts domain.ToolSet) error {
	for _, name := range []string{"web_search", "web_fetch"} {
		enabled, policy := ts.BuiltinEnabled(name)
		if enabled && policy.Type != "always_allow" {
			return domain.Validation(
				name + " requires always_allow while it is provider-native",
			)
		}
	}
	return nil
}

// seedConfirmation resolves a user.tool_confirmation into the paired
// assistant tool_use / user tool_result blocks that unblock the parked
// always_ask built-in, emitting the public agent.tool_result correlated to the
// ORIGINAL committed agent.tool_use event id. It returns those two messages to
// append to the run's local conversation before the model loop runs.
//
// The original action is recovered from server-owned causal history
// (req.ConfirmedToolUse), never from client-supplied name/input. The core
// re-validates every durable assumption the admission gate cannot, BEFORE any
// executor/sandbox call or sink.Emit, so a malformed confirmation fails safely
// at this boundary rather than executing a tool or emitting a result:
//   - the trigger resolves a tool_confirmation whose tool_use_id references
//     exactly this original event (domain.ResolutionReference);
//   - the referenced event is a persisted agent.tool_use whose
//     evaluated_permission is "ask", naming a registered built-in with a present
//     input object (an empty object is valid; missing/wrong-type is not);
//   - the built-in is still enabled AND its current immutable session policy is
//     always_ask (never always_allow);
//   - result is exactly allow|deny, and deny_message is only present on deny.
//
// Any failure returns an error and emits nothing.
//
// On allow the built-in executes through the same durable journal and
// tools.Registry/Sandbox path a normal turn uses. Allow requires a non-nil
// sandbox and journal. On deny the executor is never invoked and neither is
// required; the result is marked is_error and carries the deny_message text so
// the model can react.
func (a *AgentCore) seedConfirmation(
	ctx context.Context,
	req RunRequest,
	sink EventSink,
	toolOrdinal *int,
) ([]domain.Message, error) {
	orig := req.ConfirmedToolUse
	if orig == nil || orig.ID == "" {
		return nil, domain.Validation("tool_confirmation has no resolvable original action")
	}

	// Re-derive the trigger's own reference from server-owned payload rather than
	// trusting that the app resolved the correct original: the trigger must be a
	// user.tool_confirmation whose tool_use_id references exactly this original
	// event. A mismatch means the recovered action does not answer the trigger.
	refID, refKind, ok := domain.ResolutionReference(req.Trigger.Type, req.Trigger.Payload)
	if !ok || refKind != domain.PendingToolConfirmation {
		return nil, domain.Validation("trigger is not a tool_confirmation resolution")
	}
	if refID != orig.ID {
		return nil, domain.Validation("tool_confirmation references a different action than the recovered original")
	}

	// The original must be a persisted always_ask agent.tool_use (the helper
	// requires evaluated_permission == "ask"), naming a registered built-in with a
	// present input object. An empty object is valid; a missing/wrong-type input is
	// not — a malformed action must never reach an executor.
	kind, ok := domain.PendingActionKindForEvent(orig.Type, orig.Payload)
	if !ok || kind != domain.PendingToolConfirmation {
		return nil, domain.Validation("confirmed event is not an always_ask agent.tool_use")
	}
	name, _ := orig.Payload["name"].(string)
	if name == "" {
		return nil, domain.Validation("confirmed tool_use has no name")
	}
	input, ok := orig.Payload["input"].(map[string]any)
	if !ok {
		return nil, domain.Validation("confirmed tool_use input is missing or not an object")
	}
	exec, isBuiltin := tools.Registry()[name]
	if !isBuiltin {
		return nil, domain.Validation("confirmed tool is not a built-in")
	}

	// Re-resolve the CURRENT immutable session policy for this built-in: it must be
	// enabled AND still always_ask. If the policy is now (or was mistakenly)
	// always_allow/anything else, this confirmation path must not execute — the
	// only decision surface it models is an ask gate.
	enabled, policy := req.ToolSet.BuiltinEnabled(name)
	if !enabled {
		return nil, domain.Validation("confirmed built-in is not enabled in this session")
	}
	if policy.Type != "always_ask" {
		return nil, domain.Validation("confirmed built-in is not under an always_ask policy")
	}

	// The confirmation decision comes from the trigger payload. Re-validate the
	// HTTP invariants at the runtime boundary so an internal caller that bypasses
	// the edge cannot execute on a malformed result: result must be exactly
	// allow|deny, and deny_message is only permitted on deny.
	result, _ := req.Trigger.Payload["result"].(string)
	if result != "allow" && result != "deny" {
		return nil, domain.Validation("tool_confirmation result must be allow or deny")
	}
	if raw, present := req.Trigger.Payload["deny_message"]; present {
		if result != "deny" {
			return nil, domain.Validation("deny_message is only allowed when result is deny")
		}
		if _, ok := raw.(string); !ok {
			return nil, domain.Validation("deny_message must be a string")
		}
	}
	denyMessage, _ := req.Trigger.Payload["deny_message"].(string)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var content []any
	var isError bool
	if result == "deny" {
		// Never touch the sandbox/executor on deny. Deliver a rejection tool
		// result including the deny_message so the model sees why it was refused.
		text := "Tool call denied by user."
		if denyMessage != "" {
			text += " " + denyMessage
		}
		content = []any{map[string]any{"type": "text", "text": text}}
		isError = true
	} else {
		stepID, err := a.prepareBuiltin(ctx, req, *toolOrdinal, orig.ID, name, input)
		if err != nil {
			return nil, err
		}
		(*toolOrdinal)++
		res, err := a.executePreparedBuiltin(ctx, req, stepID, input, exec)
		if err != nil {
			return nil, err
		}
		content = res.Content
		isError = res.IsError
	}

	// Emit the public agent.tool_result correlated to the ORIGINAL committed
	// event id, so agent.tool_use/agent.tool_result re-project as a valid pair.
	if _, err := sink.Emit(ctx, []domain.EventDraft{{
		Type: domain.EvAgentToolResult,
		Payload: map[string]any{
			"tool_use_id": orig.ID,
			"content":     content,
			"is_error":    isError,
		},
	}}); err != nil {
		return nil, err
	}

	return []domain.Message{
		{Role: domain.RoleAssistant, Content: []domain.ContentBlock{{
			Type: "tool_use", ToolUseID: orig.ID, ToolName: name, Input: input,
		}}},
		{Role: domain.RoleUser, Content: []domain.ContentBlock{{
			Type: "tool_result", ToolResultFor: orig.ID, Text: FlattenResultText(content), IsError: isError,
		}}},
	}, nil
}

func (a *AgentCore) prepareBuiltin(
	ctx context.Context,
	req RunRequest,
	ordinal int,
	toolUseEventID string,
	toolName string,
	input map[string]any,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if req.Sandbox == nil {
		return "", domain.Validation("cannot execute built-in without a sandbox")
	}
	if req.ToolJournal == nil {
		return "", domain.Validation("cannot execute built-in without a tool journal")
	}
	return req.ToolJournal.Prepare(ctx, ordinal, toolUseEventID, toolName, input)
}

func (a *AgentCore) executePreparedBuiltin(
	ctx context.Context,
	req RunRequest,
	stepID string,
	input map[string]any,
	exec tools.Executor,
) (tools.Result, error) {
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	if err := req.ToolJournal.Start(ctx, stepID); err != nil {
		return tools.Result{}, err
	}
	result := exec(ctx, req.Sandbox, input)
	result, materializeErr := tools.MaterializeLargeResult(
		context.WithoutCancel(ctx),
		req.Sandbox,
		stepID,
		result,
	)
	if materializeErr != nil {
		result = tools.Result{
			Content: []any{map[string]any{
				"type": "text",
				"text": materializeErr.Error(),
			}},
			IsError: true,
		}
	}
	if err := req.ToolJournal.Complete(ctx, stepID, domain.ToolStepResult{
		Content: result.Content,
		IsError: result.IsError,
	}); err != nil {
		return tools.Result{}, err
	}
	return result, nil
}

// AppendMerging appends added messages to base, folding an added message into
// the previous message when their roles match — the same role-collapsing rule
// ProjectMessages applies to committed events. This keeps the running
// conversation strictly role-alternating even when the seeded confirmation pair
// begins with the same role as base's final message (e.g. base ends with
// assistant text and the recovered tool_use is also assistant).
//
// It is exported for the Temporal workflow loop, which must apply the identical
// pure message projection without invoking AgentCore's legacy in-process loop.
func AppendMerging(base, added []domain.Message) []domain.Message {
	out := base
	for _, m := range added {
		if n := len(out); n > 0 && out[n-1].Role == m.Role {
			// Replace the trailing message with a merged copy rather than mutating
			// it in place: base may alias the caller's slice, whose elements must
			// not change under it.
			merged := make([]domain.ContentBlock, 0, len(out[n-1].Content)+len(m.Content))
			merged = append(merged, out[n-1].Content...)
			merged = append(merged, m.Content...)
			dup := append([]domain.Message(nil), out...)
			anchor := out[n-1].ContextUsage
			if m.ContextUsage != nil {
				anchor = m.ContextUsage
			}
			dup[n-1] = domain.Message{
				Role: out[n-1].Role, Content: merged, ContextUsage: anchor,
			}
			out = dup
			continue
		}
		out = append(out, m)
	}
	return out
}

// EnabledToolSchemas returns the model-facing tool schemas the session offers:
// every enabled built-in in canonical order, followed by the session's custom
// tools. Offering custom tools lets the model request them; the core parks the
// run when it does, since only the app/client can resolve a custom tool.
//
// It is deterministic and side-effect free so both AgentCore and the Temporal
// preparation Activity can share one schema projection.
func EnabledToolSchemas(ts domain.ToolSet) []model.ToolSchema {
	schemas := enabledBuiltinSchemas(ts)
	for _, ct := range ts.Custom {
		schemas = append(schemas, model.ToolSchema{
			Name:        ct.Name,
			Description: ct.Description,
			InputSchema: ct.InputSchema,
		})
	}
	return schemas
}

// EnabledSelfHostedToolSchemas declares every built-in as a client tool. In a
// self_hosted Environment the worker client, not the Messages provider,
// executes sandbox-routed tools and returns user.tool_result. Web Search/Fetch
// therefore must not be declared as provider-native server tools on this path.
func EnabledSelfHostedToolSchemas(ts domain.ToolSet) []model.ToolSchema {
	var schemas []model.ToolSchema
	for _, name := range domain.BuiltinToolNames {
		if enabled, _ := ts.BuiltinEnabled(name); !enabled {
			continue
		}
		schema := tools.Schema(name)
		if schema == nil {
			continue
		}
		schemas = append(schemas, model.ToolSchema{Name: name, InputSchema: schema})
	}
	for _, custom := range ts.Custom {
		schemas = append(schemas, model.ToolSchema{
			Name: custom.Name, Description: custom.Description,
			InputSchema: custom.InputSchema,
		})
	}
	return schemas
}

// enabledBuiltinSchemas returns the model-facing tool schemas for every enabled
// built-in in the toolset, in the canonical BuiltinToolNames order. A built-in
// whose schema is nil is skipped rather than offered: declaring a tool with a
// null input_schema is an illegal Messages request (400). Every built-in name
// currently returns a non-nil object schema, so this is a defensive safeguard.
func enabledBuiltinSchemas(ts domain.ToolSet) []model.ToolSchema {
	var schemas []model.ToolSchema
	for _, name := range domain.BuiltinToolNames {
		if enabled, _ := ts.BuiltinEnabled(name); !enabled {
			continue
		}
		switch name {
		case "web_search":
			schemas = append(schemas, model.ToolSchema{
				Type: "web_search_20260318",
				Name: name,
			})
			continue
		case "web_fetch":
			schemas = append(schemas, model.ToolSchema{
				Type: "web_fetch_20260318",
				Name: name,
			})
			continue
		}
		schema := tools.Schema(name)
		if schema == nil {
			continue
		}
		schemas = append(schemas, model.ToolSchema{Name: name, InputSchema: schema})
	}
	return schemas
}

// textContent projects the non-empty text blocks of a model response into the
// agent.message wire content array.
func TextBlocksToContent(blocks []domain.ContentBlock) []any {
	content := make([]any, 0, len(blocks))
	for _, b := range blocks {
		if b.Type != "text" || b.Text == "" {
			continue
		}
		content = append(content, map[string]any{"type": "text", "text": b.Text})
	}
	return content
}

// HasThinkingBlocks reports whether the provider returned ordinary or redacted
// extended-thinking content. Public history records only that thinking
// occurred; the sensitive provider block remains in private continuation state.
func HasThinkingBlocks(blocks []domain.ContentBlock) bool {
	for _, block := range blocks {
		if block.Type == "thinking" || block.Type == "redacted_thinking" {
			return true
		}
	}
	return false
}

// FlattenResultText extracts the concatenated text of a tool result's content
// block array for threading into the local tool_result message block.
func FlattenResultText(content []any) string {
	var s string
	for _, item := range content {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != "text" {
			continue
		}
		if text, _ := m["text"].(string); text != "" {
			s += text
		}
	}
	return s
}
