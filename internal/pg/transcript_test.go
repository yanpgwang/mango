package pg

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

func TestCompleteWorkflowTurn_CommitsLosslessTranscriptAtomically(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sess_transcript")
	if _, err := store.CreateSession(ctx, session, nil); err != nil {
		t.Fatal(err)
	}
	admission, err := store.AdmitEvents(ctx, session.ID, []domain.EventDraft{{
		Type: domain.EvUserMessage,
		Payload: map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "search"},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	triggerID := admission.Events[0].ID
	opaque := json.RawMessage(
		`{"type":"web_search_tool_result","tool_use_id":"srv_1","content":[{"type":"web_search_result","encrypted_content":"opaque"}]}`,
	)
	delta := []domain.Message{
		{
			Role: domain.RoleUser,
			Content: []domain.ContentBlock{{
				Type: "text", Text: "search",
			}},
		},
		{
			Role: domain.RoleAssistant,
			Content: []domain.ContentBlock{{
				Type: "web_search_tool_result", Raw: opaque,
			}},
		},
	}
	mappings := []domain.ProviderToolUseMapping{{
		PublicEventID:     "sevt_public",
		ProviderToolUseID: "toolu_provider",
		ToolName:          "read",
	}}
	_, err = store.CompleteWorkflowTurnWithTranscript(
		ctx,
		session.ID,
		triggerID,
		[]domain.EventDraft{{
			Type: domain.EvSessionStatusIdle,
			Payload: map[string]any{
				"stop_reason": map[string]any{"type": "end_turn"},
			},
		}},
		domain.StatusIdle,
		"",
		"",
		nil,
		nil,
		nil,
		delta,
		mappings,
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.LoadProviderTranscript(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.TriggerEventIDs) != 1 || got.TriggerEventIDs[0] != triggerID {
		t.Fatalf("trigger ids = %#v", got.TriggerEventIDs)
	}
	if len(got.Messages) != 2 || len(got.Messages[1].Content) != 1 ||
		!equivalentJSON(got.Messages[1].Content[0].Raw, opaque) {
		t.Fatalf("transcript = %#v", got.Messages)
	}
	if len(got.ToolUseMappings) != 1 ||
		got.ToolUseMappings[0] != mappings[0] {
		t.Fatalf("mappings = %#v", got.ToolUseMappings)
	}
}

// PostgreSQL jsonb intentionally normalizes insignificant whitespace and key
// order. Provider-native blocks are lossless at the JSON value level, not at
// the original byte-serialization level.
func equivalentJSON(left, right []byte) bool {
	var leftValue, rightValue any
	if err := json.Unmarshal(left, &leftValue); err != nil {
		return false
	}
	if err := json.Unmarshal(right, &rightValue); err != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func TestProviderTranscriptContextIsIsolatedByThread(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sesn_thread_transcript")
	session.AgentSnapshot = domain.Agent{
		ID: session.AgentID, Version: session.AgentVersion, Name: "coordinator",
		Model: domain.NormalizeModel(domain.Model{ID: "claude-test"}),
		Multiagent: &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{{
			Type: "agent", ID: "agent_peer", Version: 2,
		}}},
	}
	session.MultiagentRoster = []domain.Agent{{
		ID: "agent_peer", Version: 2, Name: "reviewer",
		Model: domain.NormalizeModel(domain.Model{ID: "claude-test"}),
	}}
	if _, err := store.CreateSession(ctx, session, nil); err != nil {
		t.Fatal(err)
	}
	threads, err := store.ListSessionThreads(ctx, session.ID, app.SessionThreadListQuery{Limit: 1})
	if err != nil || len(threads) != 1 {
		t.Fatalf("primary Thread = %+v, err=%v", threads, err)
	}
	primary := threads[0]
	child, createdEvent, err := store.CreateChildSessionThread(
		ctx, session.ID, primary.ID, "reviewer",
	)
	if err != nil {
		t.Fatal(err)
	}
	childTriggers, err := store.AppendThreadEvents(ctx, session.ID, child.ID, []domain.EventDraft{{
		Type: domain.EvAgentThreadMessageReceived,
		Payload: map[string]any{
			"from_session_thread_id": primary.ID,
			"from_agent_name":        nil,
			"content":                []any{map[string]any{"type": "text", "text": "review this"}},
		},
	}})
	if err != nil || len(childTriggers) != 1 {
		t.Fatalf("child trigger = %+v, err=%v", childTriggers, err)
	}
	childTrigger := childTriggers[0]
	primaryMessages, _ := json.Marshal([]domain.Message{{
		Role:    domain.RoleAssistant,
		Content: []domain.ContentBlock{{Type: "text", Text: "delegated"}},
	}})
	childMessages, _ := json.Marshal([]domain.Message{{
		Role:    domain.RoleUser,
		Content: []domain.ContentBlock{{Type: "text", Text: "review this"}},
	}})
	insert := func(trigger domain.Event, messages []byte) error {
		represented, _ := json.Marshal([]string{trigger.ID})
		return store.q.InsertProviderTranscriptTurn(ctx, pgstore.InsertProviderTranscriptTurnParams{
			SessionID: session.ID, TriggerEventID: trigger.ID,
			CommittedThroughSeq: trigger.Sequence, RepresentedEventIds: represented,
			Messages: messages, ToolUseMappings: []byte("[]"),
			CreatedAt: tsUTC(trigger.CreatedAt),
		})
	}
	if err := insert(createdEvent, primaryMessages); err != nil {
		t.Fatal(err)
	}
	if err := insert(childTrigger, childMessages); err != nil {
		t.Fatal(err)
	}

	primaryTranscript, err := store.LoadProviderTranscript(ctx, session.ID)
	if err != nil || len(primaryTranscript.Messages) != 1 ||
		primaryTranscript.Messages[0].Content[0].Text != "delegated" {
		t.Fatalf("primary transcript = %+v, err=%v", primaryTranscript, err)
	}
	childTranscript, err := store.LoadThreadProviderTranscript(ctx, session.ID, child.ID)
	if err != nil || len(childTranscript.Messages) != 1 ||
		childTranscript.Messages[0].Content[0].Text != "review this" {
		t.Fatalf("child transcript = %+v, err=%v", childTranscript, err)
	}
}

func TestAppendProviderMessagesPreservesLatestContextUsage(t *testing.T) {
	first := &domain.ContextUsageAnchor{
		Usage:              domain.ContextWindowUsage{InputTokens: 10},
		RequestFingerprint: "request-1",
		PrefixFingerprint:  "prefix-1",
		ContentBlocks:      1,
	}
	latest := &domain.ContextUsageAnchor{
		Usage:              domain.ContextWindowUsage{InputTokens: 20},
		RequestFingerprint: "request-2",
		PrefixFingerprint:  "prefix-2",
		ContentBlocks:      2,
	}
	got := appendProviderMessages(
		[]domain.Message{{
			Role:         domain.RoleAssistant,
			Content:      []domain.ContentBlock{{Type: "text", Text: "first"}},
			ContextUsage: first,
		}},
		[]domain.Message{{
			Role:         domain.RoleAssistant,
			Content:      []domain.ContentBlock{{Type: "text", Text: "latest"}},
			ContextUsage: latest,
		}},
	)

	if len(got) != 1 || len(got[0].Content) != 2 {
		t.Fatalf("merged transcript = %#v", got)
	}
	if got[0].ContextUsage == nil ||
		got[0].ContextUsage.RequestFingerprint != latest.RequestFingerprint ||
		got[0].ContextUsage.PrefixFingerprint != latest.PrefixFingerprint {
		t.Fatalf("latest context anchor = %#v", got[0].ContextUsage)
	}
	if got[0].ContextUsage == latest {
		t.Fatal("merged transcript retained the caller's mutable anchor pointer")
	}
}

func TestCloseInterruptedProviderTranscript_PairsDanglingTools(t *testing.T) {
	anchor := &domain.ContextUsageAnchor{
		Usage:              domain.ContextWindowUsage{InputTokens: 25},
		RequestFingerprint: "request",
		PrefixFingerprint:  "prefix",
		ContentBlocks:      3,
	}
	messages := []domain.Message{
		{
			Role: domain.RoleAssistant,
			Content: []domain.ContentBlock{
				{Type: "tool_use", ToolUseID: "provider_done"},
				{Type: "tool_use", ToolUseID: "provider_pending"},
				{Type: "server_tool_use", ToolUseID: "server_native"},
			},
			ContextUsage: anchor,
		},
		{
			Role: domain.RoleUser,
			Content: []domain.ContentBlock{{
				Type: "tool_result", ToolResultFor: "provider_done",
				Text: "done",
			}},
		},
	}

	got := closeInterruptedProviderTranscript(messages)

	if len(got) != 2 || got[1].Role != domain.RoleUser {
		t.Fatalf("closed transcript = %#v", got)
	}
	if len(got[1].Content) != 2 {
		t.Fatalf("result blocks = %#v", got[1].Content)
	}
	synthetic := got[1].Content[1]
	if synthetic.ToolResultFor != "provider_pending" ||
		!synthetic.IsError {
		t.Fatalf("synthetic result = %#v", synthetic)
	}
	if len(messages[1].Content) != 1 {
		t.Fatal("helper mutated its input transcript")
	}
	if got[0].ContextUsage == nil ||
		got[0].ContextUsage.RequestFingerprint != anchor.RequestFingerprint ||
		got[0].ContextUsage == anchor {
		t.Fatalf("context anchor was not cloned: %#v", got[0].ContextUsage)
	}
	if got := closeInterruptedProviderTranscript(nil); got != nil {
		t.Fatalf("nil transcript became represented: %#v", got)
	}
}

func TestRetainCommittedProviderMappings_DropsInterruptedActions(t *testing.T) {
	mappings := []domain.ProviderToolUseMapping{
		{
			PublicEventID:     "public_done",
			ProviderToolUseID: "provider_done",
		},
		{
			PublicEventID:     "public_pending",
			ProviderToolUseID: "provider_pending",
		},
	}
	got := retainCommittedProviderMappings(
		mappings,
		[]domain.EventDraft{
			{ID: "public_done", Type: domain.EvAgentToolUse},
			{Type: domain.EvAgentToolResult},
		},
	)
	if len(got) != 1 || got[0] != mappings[0] {
		t.Fatalf("retained mappings = %#v", got)
	}
}
