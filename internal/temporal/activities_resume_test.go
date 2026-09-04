package temporal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/yanpgwang/mango/internal/agentruntime"
	"github.com/yanpgwang/mango/internal/domain"
)

func TestPrepareTurnCompactsRequestButKeepsLosslessTranscriptDelta(t *testing.T) {
	processedAt := time.Now().UTC()
	base := newFakeSource([]domain.Event{
		{
			ID: "sevt_prior", Sequence: 1, Type: domain.EvUserMessage,
			Payload: map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "old request"},
			}},
			ProcessedAt: &processedAt,
		},
		{
			ID: "sevt_current", Sequence: 2, Type: domain.EvUserMessage,
			Payload: map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "current request"},
			}},
		},
	})
	largeImage := json.RawMessage(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + strings.Repeat("A", 30000) + `"}}`)
	source := &configuredTranscriptFakeSource{
		transcriptFakeSource: &transcriptFakeSource{
			fakeSource: base,
			transcript: domain.ProviderTranscript{
				TriggerEventIDs: []string{"sevt_prior"},
				Messages: []domain.Message{
					{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "image", Raw: largeImage}}},
					{Role: domain.RoleAssistant, Content: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("old response ", 3000)}}},
				},
			},
		},
		session: domain.Session{
			ID: "sess_context", Status: domain.StatusRunning,
			AgentSnapshot: domain.Agent{Model: domain.Model{ID: "model"}},
		},
	}

	prepared, err := NewActivities(
		nil, source, nil, nil, &testIDGen{},
	).WithContextTokenBudget(500).PrepareTurn(context.Background(), PrepareTurnInput{
		SessionID: "sess_context", TriggerEventID: "sevt_current",
	})
	require.NoError(t, err)
	require.True(t, prepared.UsesProviderTranscript)
	require.True(t, prepared.ContextProjection.Compacted)
	require.Equal(t, "current request", prepared.TranscriptDelta[0].Content[0].Text)
	require.Contains(t, prepared.Request.Messages[0].Content[0].Text, "compacted")
	require.NotEmpty(t, source.transcript.Messages[0].Content[0].Raw,
		"request projection must not mutate the durable provider transcript")
}

type transcriptFakeSource struct {
	*fakeSource
	transcript domain.ProviderTranscript
}

func (s *transcriptFakeSource) LoadProviderTranscript(
	context.Context,
	string,
) (domain.ProviderTranscript, error) {
	return s.transcript, nil
}

type configuredTranscriptFakeSource struct {
	*transcriptFakeSource
	session domain.Session
	skills  []domain.SkillVersion
}

type threadConfiguredTranscriptFakeSource struct {
	*configuredTranscriptFakeSource
	thread           domain.SessionThread
	runtime          domain.SkillRuntime
	snapshot         *domain.ContextSnapshot
	snapshotGetCalls int
	snapshotPutCalls int
}

type threadPendingFakeSource struct {
	*fakeSource
	thread domain.SessionThread
}

func (s *threadPendingFakeSource) GetSessionThread(
	context.Context,
	string,
	string,
) (domain.SessionThread, error) {
	return s.thread, nil
}

func (s *threadPendingFakeSource) UnresolvedThreadPendingActions(
	ctx context.Context,
	sessionID string,
	_ string,
) ([]domain.PendingAction, error) {
	return s.UnresolvedPendingActions(ctx, sessionID)
}

func (s *threadConfiguredTranscriptFakeSource) GetSessionThread(
	context.Context,
	string,
	string,
) (domain.SessionThread, error) {
	return s.thread, nil
}

func (s *threadConfiguredTranscriptFakeSource) LoadThreadProviderTranscript(
	context.Context,
	string,
	string,
) (domain.ProviderTranscript, error) {
	return s.transcript, nil
}

func (s *threadConfiguredTranscriptFakeSource) PutThreadContextSnapshot(
	_ context.Context,
	sessionID string,
	threadID string,
	triggerEventID string,
	transcriptTriggerEventIDs []string,
	messages []domain.Message,
	projection domain.ContextProjection,
) (domain.ContextSnapshot, error) {
	s.snapshotPutCalls++
	if s.snapshot != nil {
		return *s.snapshot, nil
	}
	s.snapshot = &domain.ContextSnapshot{
		ID: "csnp_test", SessionID: sessionID, ThreadID: threadID,
		TriggerEventID: triggerEventID,
		TranscriptTriggerEventIDs: append(
			[]string(nil), transcriptTriggerEventIDs...,
		),
		Messages: messages, Projection: projection,
		ContextPolicyVersion: domain.ContextPolicyVersion,
	}
	return *s.snapshot, nil
}

func (s *threadConfiguredTranscriptFakeSource) GetThreadContextSnapshotForTrigger(
	_ context.Context,
	sessionID string,
	threadID string,
	triggerEventID string,
) (domain.ContextSnapshot, bool, error) {
	s.snapshotGetCalls++
	if s.snapshot == nil || s.snapshot.SessionID != sessionID ||
		s.snapshot.ThreadID != threadID ||
		s.snapshot.TriggerEventID != triggerEventID {
		return domain.ContextSnapshot{}, false, nil
	}
	return *s.snapshot, true, nil
}

func (s *threadConfiguredTranscriptFakeSource) SessionThreadSkillRuntime(
	context.Context,
	string,
	string,
) (domain.SkillRuntime, error) {
	return s.runtime, nil
}

func (s *configuredTranscriptFakeSource) GetSession(
	context.Context,
	string,
) (domain.Session, error) {
	return s.session, nil
}

func (s *configuredTranscriptFakeSource) SessionSkillsForRuntime(
	context.Context,
	string,
) ([]domain.SkillVersion, error) {
	return append([]domain.SkillVersion(nil), s.skills...), nil
}

func TestPrepareTurn_ProjectsPinnedSkillDiscoveryMetadata(t *testing.T) {
	base := newFakeSource([]domain.Event{{
		ID: "sevt_skill", Sequence: 1, Type: domain.EvUserMessage,
		Payload: map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "analyze the report"},
		}},
	}})
	source := &configuredTranscriptFakeSource{
		transcriptFakeSource: &transcriptFakeSource{fakeSource: base},
		session: domain.Session{
			ID: "sess_skill", Status: domain.StatusRunning,
			AgentSnapshot: domain.Agent{
				Model: domain.Model{ID: "model"},
				Tools: []any{map[string]any{"type": domain.BuiltinToolsetType}},
				Skills: []domain.SkillReference{{
					Type: "custom", SkillID: "skill_reports", Version: "100",
				}},
			},
		},
		skills: []domain.SkillVersion{{
			SkillID: "skill_reports", Version: "100", Name: "report-tools",
			Description: "Analyze reports", UncompressedSizeBytes: 1024,
		}},
	}
	prepared, err := NewActivities(
		nil, source, nil, nil, &testIDGen{},
	).WithSkillRuntimeSupported(true).
		WithSkillInstructionLoader(staticSkillInstructionLoader{body: []byte("body")}).
		PrepareTurn(
			context.Background(),
			PrepareTurnInput{SessionID: "sess_skill", TriggerEventID: "sevt_skill"},
		)
	require.NoError(t, err)
	require.Empty(t, prepared.FatalError)
	require.Contains(t, prepared.Request.System, "<available_skills>")
	require.Contains(t, prepared.Request.System, `"name":"report-tools"`)
	require.Contains(t, prepared.Request.System, "/workspace/skills/report-tools/SKILL.md")
	require.Contains(t, summarizeModelTools(prepared.Request.Tools), modelToolSummary{
		Name: agentruntime.RuntimeSkillToolName,
	})
	require.Contains(t, prepared.Tools, TurnTool{
		Name:       agentruntime.RuntimeSkillToolName,
		Kind:       TurnToolRuntimeSkill,
		Permission: domain.PermissionPolicy{Type: "always_allow"},
	})

	unsupported, err := NewActivities(
		nil, source, nil, nil, &testIDGen{},
	).PrepareTurn(
		context.Background(),
		PrepareTurnInput{SessionID: "sess_skill", TriggerEventID: "sevt_skill"},
	)
	require.NoError(t, err)
	require.Contains(t, unsupported.FatalError, "configured sandbox provider")

	source.session.EnvironmentType = "self_hosted"
	selfHosted, err := NewActivities(
		nil, source, nil, nil, &testIDGen{},
	).WithSkillInstructionLoader(staticSkillInstructionLoader{body: []byte("body")}).
		PrepareTurn(
			context.Background(),
			PrepareTurnInput{SessionID: "sess_skill", TriggerEventID: "sevt_skill"},
		)
	require.NoError(t, err)
	require.Empty(t, selfHosted.FatalError)
	require.Contains(t, summarizeModelTools(selfHosted.Request.Tools), modelToolSummary{
		Name: agentruntime.RuntimeSkillToolName,
	})
	require.Equal(t, domain.SessionSkillsRelativeRoot, selfHosted.SkillRuntimeRoot)
	require.Contains(t, selfHosted.Request.System,
		`"skill_md":"skills/report-tools/SKILL.md"`)
}

func TestPrepareTurn_SelectsThreadAgentRuntimeConfiguration(t *testing.T) {
	root := domain.SessionSkillsRoot + "/.agents/0123456789abcdef01234567"
	primaryThreadID := "sthr_primary"
	base := newFakeSource([]domain.Event{{
		ID: "sevt_child_skill", SessionID: "sess_child_skill",
		ThreadID: "sthr_child", Sequence: 1, Type: domain.EvUserMessage,
		Payload: map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "review the report"},
		}},
	}})
	childSystem := "You are the child reviewer."
	source := &threadConfiguredTranscriptFakeSource{
		configuredTranscriptFakeSource: &configuredTranscriptFakeSource{
			transcriptFakeSource: &transcriptFakeSource{fakeSource: base},
			session: domain.Session{
				ID: "sess_child_skill", Status: domain.StatusRunning,
				AgentSnapshot: domain.Agent{
					ID: "agent_primary", Version: 1, Name: "coordinator",
					Model: domain.Model{ID: "primary-model"},
				},
			},
		},
		thread: domain.SessionThread{
			ID: "sthr_child", SessionID: "sess_child_skill",
			ParentThreadID: &primaryThreadID,
			Agent: domain.Agent{
				ID: "agent_child", Version: 2, Name: "reviewer",
				Model: domain.Model{ID: "child-model"}, System: &childSystem,
				Tools: []any{map[string]any{"type": domain.BuiltinToolsetType}},
				Skills: []domain.SkillReference{{
					Type: "custom", SkillID: "skill_child", Version: "200",
				}},
			},
		},
		runtime: domain.SkillRuntime{
			Root: root,
			Versions: []domain.SkillVersion{{
				SkillID: "skill_child", Version: "200", Name: "child-review",
				Description: "Review reports", UncompressedSizeBytes: 1024,
			}},
		},
	}
	prepared, err := NewActivities(
		nil, source, nil, nil, &testIDGen{},
	).WithSkillRuntimeSupported(true).
		WithSkillInstructionLoader(staticSkillInstructionLoader{body: []byte("body")}).
		PrepareTurn(
			context.Background(),
			PrepareTurnInput{
				SessionID: "sess_child_skill", TriggerEventID: "sevt_child_skill",
			},
		)
	require.NoError(t, err)
	require.Empty(t, prepared.FatalError)
	require.Equal(t, "sthr_child", prepared.ThreadID)
	require.Equal(t, root, prepared.SkillRuntimeRoot)
	require.Equal(t, "child-model", prepared.Request.Model)
	require.Contains(t, prepared.Request.System, childSystem)
	require.Contains(
		t, prepared.Request.System, root+"/child-review/SKILL.md",
	)
	require.True(t, prepared.IsChild)
	require.NotContains(t, prepared.Request.System, "<mango-coordinator>")
	require.NotContains(t, summarizeModelTools(prepared.Request.Tools), modelToolSummary{
		Name: agentruntime.SendToAgentToolName,
	})
}

func TestPrepareTurn_ChildCompactionRestoresFirstDurableSnapshot(t *testing.T) {
	processedAt := time.Now().UTC()
	const (
		sessionID = "sesn_child_snapshot"
		threadID  = "sthr_child_snapshot"
		priorID   = "sevt_child_prior"
		currentID = "sevt_child_current"
	)
	primaryThreadID := "sthr_primary_snapshot"
	base := newFakeSource([]domain.Event{
		{
			ID: priorID, SessionID: sessionID, ThreadID: threadID,
			Sequence: 1, Type: domain.EvAgentThreadMessageReceived,
			Payload: map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "first task"},
			}},
			ProcessedAt: &processedAt,
		},
		{
			ID: currentID, SessionID: sessionID, ThreadID: threadID,
			Sequence: 2, Type: domain.EvAgentThreadMessageReceived,
			Payload: map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "follow up"},
			}},
		},
	})
	source := &threadConfiguredTranscriptFakeSource{
		configuredTranscriptFakeSource: &configuredTranscriptFakeSource{
			transcriptFakeSource: &transcriptFakeSource{
				fakeSource: base,
				transcript: domain.ProviderTranscript{
					TriggerEventIDs: []string{priorID},
					Messages: []domain.Message{
						{Role: domain.RoleUser, Content: []domain.ContentBlock{{
							Type: "text", Text: strings.Repeat("old task ", 8_000),
						}}},
						{Role: domain.RoleAssistant, Content: []domain.ContentBlock{{
							Type: "text", Text: strings.Repeat("old answer ", 8_000),
						}}},
					},
				},
			},
			session: domain.Session{
				ID: sessionID, Status: domain.StatusRunning,
				AgentSnapshot: domain.Agent{
					ID: "agent_primary", Version: 1, Name: "coordinator",
					Model: domain.Model{ID: "primary-model"},
				},
			},
		},
		thread: domain.SessionThread{
			ID: threadID, SessionID: sessionID,
			ParentThreadID: &primaryThreadID,
			Agent: domain.Agent{
				ID: "agent_child", Version: 1, Name: "reviewer",
				Model: domain.Model{ID: "child-model"},
			},
			Status: domain.StatusRunning,
		},
	}

	first, err := NewActivities(
		nil, source, nil, nil, &testIDGen{},
	).WithContextTokenBudget(12_000).PrepareTurn(
		context.Background(),
		PrepareTurnInput{SessionID: sessionID, TriggerEventID: currentID},
	)
	require.NoError(t, err)
	require.True(t, first.IsChild)
	require.True(t, first.ContextProjection.Compacted)
	require.Equal(t, "csnp_test", first.ContextSnapshotID)
	require.Equal(t, []string{priorID, currentID},
		source.snapshot.TranscriptTriggerEventIDs)

	second, err := NewActivities(
		nil, source, nil, nil, &testIDGen{},
	).WithContextTokenBudget(1_000_000).PrepareTurn(
		context.Background(),
		PrepareTurnInput{SessionID: sessionID, TriggerEventID: currentID},
	)
	require.NoError(t, err)
	require.Equal(t, 2, source.snapshotGetCalls)
	require.Equal(t, 1, source.snapshotPutCalls)
	require.Equal(t, first.ContextSnapshotID, second.ContextSnapshotID)
	require.Equal(t, first.ContextProjection, second.ContextProjection)
	require.Equal(t, first.Request.Messages, second.Request.Messages)
}

func TestPrepareTurn_PrimaryCompactionRestoresFirstDurableSnapshot(t *testing.T) {
	processedAt := time.Now().UTC()
	const (
		sessionID = "sesn_primary_snapshot"
		threadID  = "sthr_primary_snapshot"
		priorID   = "sevt_primary_prior"
		currentID = "sevt_primary_current"
	)
	base := newFakeSource([]domain.Event{
		{
			ID: priorID, SessionID: sessionID, ThreadID: threadID,
			Sequence: 1, Type: domain.EvUserMessage,
			Payload: map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "first task"},
			}},
			ProcessedAt: &processedAt,
		},
		{
			ID: currentID, SessionID: sessionID, ThreadID: threadID,
			Sequence: 2, Type: domain.EvUserMessage,
			Payload: map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "follow up"},
			}},
		},
	})
	primaryAgent := domain.Agent{
		ID: "agent_primary", Version: 1, Name: "coordinator",
		Model: domain.Model{ID: "primary-model"},
	}
	source := &threadConfiguredTranscriptFakeSource{
		configuredTranscriptFakeSource: &configuredTranscriptFakeSource{
			transcriptFakeSource: &transcriptFakeSource{
				fakeSource: base,
				transcript: domain.ProviderTranscript{
					TriggerEventIDs: []string{priorID},
					Messages: []domain.Message{
						{Role: domain.RoleUser, Content: []domain.ContentBlock{{
							Type: "text", Text: strings.Repeat("old task ", 8_000),
						}}},
						{Role: domain.RoleAssistant, Content: []domain.ContentBlock{{
							Type: "text", Text: strings.Repeat("old answer ", 8_000),
						}}},
					},
				},
			},
			session: domain.Session{
				ID: sessionID, Status: domain.StatusRunning,
				AgentSnapshot: primaryAgent,
			},
		},
		thread: domain.SessionThread{
			ID: threadID, SessionID: sessionID, Agent: primaryAgent,
			Status: domain.StatusRunning,
		},
	}

	first, err := NewActivities(
		nil, source, nil, nil, &testIDGen{},
	).WithContextTokenBudget(12_000).PrepareTurn(
		context.Background(),
		PrepareTurnInput{SessionID: sessionID, TriggerEventID: currentID},
	)
	require.NoError(t, err)
	require.False(t, first.IsChild)
	require.Equal(t, threadID, first.ThreadID)
	require.True(t, first.ContextProjection.Compacted)
	require.Equal(t, "csnp_test", first.ContextSnapshotID)
	require.Equal(t, []string{priorID, currentID},
		source.snapshot.TranscriptTriggerEventIDs)

	second, err := NewActivities(
		nil, source, nil, nil, &testIDGen{},
	).WithContextTokenBudget(1_000_000).PrepareTurn(
		context.Background(),
		PrepareTurnInput{SessionID: sessionID, TriggerEventID: currentID},
	)
	require.NoError(t, err)
	require.Equal(t, 2, source.snapshotGetCalls)
	require.Equal(t, 1, source.snapshotPutCalls)
	require.Equal(t, first.ContextSnapshotID, second.ContextSnapshotID)
	require.Equal(t, first.ContextProjection, second.ContextProjection)
	require.Equal(t, first.Request.Messages, second.Request.Messages)
}

func TestPrepareTurn_AttachesPrivateCoordinatorToolsOnlyToPrimary(t *testing.T) {
	base := newFakeSource([]domain.Event{{
		ID: "sevt_coordinate", SessionID: "sess_coordinate", Sequence: 1,
		Type: domain.EvUserMessage,
		Payload: map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "delegate the review"},
		}},
	}})
	source := &configuredTranscriptFakeSource{
		transcriptFakeSource: &transcriptFakeSource{fakeSource: base},
		session: domain.Session{
			ID: "sess_coordinate", Status: domain.StatusRunning,
			AgentSnapshot: domain.Agent{
				ID: "agent_coordinator", Version: 1, Name: "coordinator",
				Model: domain.Model{ID: "model"},
				Multiagent: &domain.Multiagent{
					Type: "coordinator",
					Agents: []domain.AgentReference{{
						Type: "agent", ID: "agent_reviewer", Version: 1,
					}},
				},
			},
		},
	}
	prepared, err := NewActivities(
		nil, source, nil, nil, &testIDGen{},
	).PrepareTurn(context.Background(), PrepareTurnInput{
		SessionID: "sess_coordinate", TriggerEventID: "sevt_coordinate",
	})
	require.NoError(t, err)
	require.False(t, prepared.IsChild)
	require.Contains(t, prepared.Request.System, "<mango-coordinator>")
	require.Contains(t, prepared.Request.System, "<agent-thread-message>")
	tools := summarizeModelTools(prepared.Request.Tools)
	require.Contains(t, tools, modelToolSummary{Name: agentruntime.ListAgentsToolName})
	require.Contains(t, tools, modelToolSummary{Name: agentruntime.SendToAgentToolName})
	require.Contains(t, prepared.Tools, TurnTool{
		Name: agentruntime.SendToAgentToolName, Kind: TurnToolCoordinator,
		Permission: domain.PermissionPolicy{Type: "always_allow"},
	})
}

func TestPrepareTurn_AttachesAdvisorOnlyToPrimary(t *testing.T) {
	base := newFakeSource([]domain.Event{{
		ID: "sevt_advisor", SessionID: "sess_advisor", Sequence: 1,
		Type: domain.EvUserMessage,
		Payload: map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "review the plan"},
		}},
	}})
	primaryAgent := domain.Agent{
		ID: "agent_primary", Version: 1, Name: "coordinator",
		Model: domain.Model{ID: "claude-sonnet-5"},
		Multiagent: &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{{
			Type: "advisor", Model: "claude-opus-5",
		}}},
	}
	primarySource := &configuredTranscriptFakeSource{
		transcriptFakeSource: &transcriptFakeSource{fakeSource: base},
		session: domain.Session{
			ID: "sess_advisor", Status: domain.StatusRunning,
			AgentSnapshot: primaryAgent,
		},
	}
	prepared, err := NewActivities(
		nil, primarySource, nil, nil, &testIDGen{},
	).PrepareTurn(context.Background(), PrepareTurnInput{
		SessionID: "sess_advisor", TriggerEventID: "sevt_advisor",
	})
	require.NoError(t, err)
	require.False(t, prepared.IsChild)
	require.NotContains(t, prepared.Request.System, "<mango-coordinator>")
	require.Contains(t, prepared.Request.System, "<managed-advisor>")
	require.Len(t, prepared.Request.Tools, 1)
	require.Empty(t, prepared.Request.Tools[0].Type)
	require.Equal(t, agentruntime.AdvisorToolName, prepared.Request.Tools[0].Name)
	require.NotEmpty(t, prepared.Request.Tools[0].Description)
	require.Len(t, prepared.Tools, 1)
	require.Equal(t, TurnToolAdvisor, prepared.Tools[0].Kind)
	require.Equal(t, "claude-opus-5", prepared.Tools[0].Model)

	primaryID := "sthr_primary"
	childID := "sthr_child"
	childBase := newFakeSource([]domain.Event{{
		ID: "sevt_child_advisor", SessionID: "sess_advisor", ThreadID: childID,
		Sequence: 2, Type: domain.EvAgentThreadMessageReceived,
		Payload: map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "do the work"},
		}},
	}})
	childSource := &threadConfiguredTranscriptFakeSource{
		configuredTranscriptFakeSource: &configuredTranscriptFakeSource{
			transcriptFakeSource: &transcriptFakeSource{fakeSource: childBase},
			session: domain.Session{
				ID: "sess_advisor", Status: domain.StatusRunning,
				AgentSnapshot: primaryAgent,
			},
		},
		thread: domain.SessionThread{
			ID: childID, SessionID: "sess_advisor", ParentThreadID: &primaryID,
			Agent: domain.Agent{
				ID: "agent_child", Version: 1, Name: "worker",
				Model: domain.Model{ID: "claude-haiku-4-5"},
			},
			Status: domain.StatusRunning,
		},
	}
	childPrepared, err := NewActivities(
		nil, childSource, nil, nil, &testIDGen{},
	).PrepareTurn(context.Background(), PrepareTurnInput{
		SessionID: "sess_advisor", TriggerEventID: "sevt_child_advisor",
	})
	require.NoError(t, err)
	require.True(t, childPrepared.IsChild)
	for _, tool := range childPrepared.Request.Tools {
		require.NotEqual(t, agentruntime.AdvisorToolName, tool.Name)
	}
}

func TestTranscriptCoverageIgnoresInternallyConsumedAdvisorOutput(t *testing.T) {
	priorID := "sevt_prior_user"
	currentID := "sevt_current_user"
	advisorID := "sevt_advisor_received"
	history := []domain.Event{
		{ID: priorID, Type: domain.EvUserMessage},
		{
			ID: advisorID, Type: domain.EvAgentThreadMessageReceived,
			TurnEventID: &priorID,
		},
		{ID: currentID, Type: domain.EvUserMessage},
	}
	transcript := domain.ProviderTranscript{TriggerEventIDs: []string{priorID}}
	if !transcriptCoversPriorTurns(transcript, history, currentID, nil) {
		t.Fatal("internally consumed Advisor output invalidated private transcript coverage")
	}

	history[1].TurnEventID = nil
	if transcriptCoversPriorTurns(transcript, history, currentID, nil) {
		t.Fatal("independent child report was accepted without its own transcript turn")
	}
}

func TestPrepareTurn_LedgerFallbackPreservesSentThreadMessages(t *testing.T) {
	processedAt := time.Now().UTC()
	base := newFakeSource([]domain.Event{
		{
			ID: "sevt_request", SessionID: "sess_fallback", Sequence: 1,
			Type: domain.EvUserMessage, ProcessedAt: &processedAt,
			Payload: map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "research the release"},
			}},
		},
		{
			ID: "sevt_delegated", SessionID: "sess_fallback", Sequence: 2,
			Type: domain.EvAgentThreadMessageSent, ProcessedAt: &processedAt,
			Payload: map[string]any{
				"to_session_thread_id": "sthr_researcher",
				"to_agent_name":        "researcher",
				"content": []any{
					map[string]any{"type": "text", "text": "inspect the release notes"},
				},
			},
		},
		{
			ID: "sevt_report", SessionID: "sess_fallback", Sequence: 3,
			Type: domain.EvAgentThreadMessageReceived,
			Payload: map[string]any{
				"from_session_thread_id": "sthr_researcher",
				"from_agent_name":        "researcher",
				"content": []any{
					map[string]any{"type": "text", "text": "the release is ready"},
				},
			},
		},
	})
	source := &configuredTranscriptFakeSource{
		transcriptFakeSource: &transcriptFakeSource{fakeSource: base},
		session: domain.Session{
			ID: "sess_fallback", Status: domain.StatusRunning,
			AgentSnapshot: domain.Agent{
				ID: "agent_coordinator", Version: 1, Name: "coordinator",
				Model:      domain.Model{ID: "model"},
				Multiagent: &domain.Multiagent{Type: "coordinator"},
			},
		},
	}

	prepared, err := NewActivities(
		nil, source, nil, nil, &testIDGen{},
	).PrepareTurn(context.Background(), PrepareTurnInput{
		SessionID: "sess_fallback", TriggerEventID: "sevt_report",
	})
	require.NoError(t, err)
	require.False(t, prepared.UsesProviderTranscript)
	require.Len(t, prepared.Request.Messages, 3)
	require.Equal(t, domain.RoleAssistant, prepared.Request.Messages[1].Role)
	require.Contains(
		t,
		prepared.Request.Messages[1].Content[0].Text,
		`"to_session_thread_id":"sthr_researcher"`,
	)
	require.Equal(
		t,
		"inspect the release notes",
		prepared.Request.Messages[1].Content[1].Text,
	)
	require.Equal(t, domain.RoleUser, prepared.Request.Messages[2].Role)
	require.Equal(t, "the release is ready", prepared.Request.Messages[2].Content[1].Text)
}

func TestPrepareTurn_ReattachesInvokedSkillFromTranscriptAfterWorkerRestart(t *testing.T) {
	processedAt := time.Now().UTC()
	base := newFakeSource([]domain.Event{
		{
			ID: "sevt_prior_skill", Sequence: 1, Type: domain.EvUserMessage,
			Payload: map[string]any{"content": []any{map[string]any{
				"type": "text", "text": "first request",
			}}},
			ProcessedAt: &processedAt,
		},
		{
			ID: "sevt_current_skill", Sequence: 2, Type: domain.EvUserMessage,
			Payload: map[string]any{"content": []any{map[string]any{
				"type": "text", "text": "continue the report",
			}}},
		},
	})
	injection := agentruntime.RuntimeSkillInjection(
		"report-tools",
		[]byte("---\nname: report-tools\ndescription: Analyze reports\n---\ncanonical workflow\n"),
	)
	source := &configuredTranscriptFakeSource{
		transcriptFakeSource: &transcriptFakeSource{
			fakeSource: base,
			transcript: domain.ProviderTranscript{
				TriggerEventIDs: []string{"sevt_prior_skill"},
				Messages: []domain.Message{
					{Role: domain.RoleUser, Content: []domain.ContentBlock{{
						Type: "text", Text: "first request",
					}}},
					{Role: domain.RoleAssistant, Content: []domain.ContentBlock{{
						Type: "tool_use", ToolUseID: "provider_skill",
						ToolName: agentruntime.RuntimeSkillToolName,
						Input:    map[string]any{"skill": "report-tools"},
					}}},
					{Role: domain.RoleUser, Content: []domain.ContentBlock{
						{Type: "tool_result", ToolResultFor: "provider_skill", Text: "Launching skill: report-tools"},
						injection,
					}},
					{Role: domain.RoleAssistant, Content: []domain.ContentBlock{{
						Type: "text", Text: strings.Repeat("later analysis ", 6000),
					}}},
				},
			},
		},
		session: domain.Session{
			ID: "sess_restart_skill", Status: domain.StatusRunning,
			AgentSnapshot: domain.Agent{
				Model: domain.Model{ID: "model"},
				Tools: []any{map[string]any{"type": domain.BuiltinToolsetType}},
				Skills: []domain.SkillReference{{
					Type: "custom", SkillID: "skill_reports", Version: "100",
				}},
			},
		},
		skills: []domain.SkillVersion{{
			SkillID: "skill_reports", Version: "100", Name: "report-tools",
			Description: "Analyze reports", UncompressedSizeBytes: 1024,
		}},
	}
	prepared, err := NewActivities(
		nil, source, nil, nil, &testIDGen{},
	).WithSkillRuntimeSupported(true).
		WithSkillInstructionLoader(staticSkillInstructionLoader{body: []byte("body")}).
		WithContextTokenBudget(9000).PrepareTurn(
		context.Background(),
		PrepareTurnInput{
			SessionID: "sess_restart_skill", TriggerEventID: "sevt_current_skill",
		},
	)
	require.NoError(t, err)
	require.Empty(t, prepared.FatalError)
	require.True(t, prepared.UsesProviderTranscript)
	require.True(t, prepared.ContextProjection.Compacted)
	require.Contains(t, agentruntime.LoadedRuntimeSkills(prepared.Request.Messages), "report-tools")
	lastMessage := prepared.Request.Messages[len(prepared.Request.Messages)-1]
	require.Equal(
		t,
		"continue the report",
		lastMessage.Content[len(lastMessage.Content)-1].Text,
	)
}

func TestPrepareTurn_ProcessedOnReceiptCustomResultStillResumesPendingBarrier(t *testing.T) {
	processedAt := time.Now().UTC()
	resolutionID := "sevt_custom_result"
	const threadID = "sthr_child"
	base := newFakeSource([]domain.Event{
		{
			ID: "sevt_user", ThreadID: threadID,
			Sequence: 1, Type: domain.EvUserMessage,
			Payload: map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "inspect"}},
			},
		},
		{
			ID: "sevt_custom", ThreadID: threadID,
			Sequence: 2, Type: domain.EvAgentCustomToolUse,
			Payload: map[string]any{
				"name":  "ask_client",
				"input": map[string]any{"question": "continue?"},
			},
		},
		{
			ID: "sevt_custom_result", ThreadID: threadID, Sequence: 3,
			Type: domain.EvUserCustomToolResult,
			Payload: map[string]any{
				"custom_tool_use_id": "sevt_custom_crosspost",
				"session_thread_id":  threadID,
				"content": []any{
					map[string]any{"type": "text", "text": "yes"},
				},
			},
			ProcessedAt: &processedAt,
		},
	})
	base.pendingActions = []domain.PendingAction{{
		ID: "pact_1", SessionID: "sess_resume", ThreadID: threadID,
		ActionEventID: "sevt_custom", ClientActionEventID: "sevt_custom_crosspost",
		Kind: domain.PendingCustomToolResult, ResolvingEventID: &resolutionID,
	}}
	primaryID := "sthr_primary"
	source := &threadPendingFakeSource{
		fakeSource: base,
		thread: domain.SessionThread{
			ID: threadID, SessionID: "sess_resume", ParentThreadID: &primaryID,
			Status: domain.StatusRunning,
		},
	}
	activities := NewActivities(nil, source, nil, nil, &testIDGen{})

	selector, err := activities.LoadPendingActions(context.Background(), LoadPendingActionsInput{
		SessionID: "sess_resume", ThreadID: threadID,
	})
	require.NoError(t, err)
	require.Equal(t, []PendingActionRef{{
		ActionEventID:      "sevt_custom",
		ActionEventSeq:     2,
		Kind:               domain.PendingCustomToolResult,
		ResolutionEventID:  "sevt_custom_result",
		ResolutionEventSeq: 3,
	}}, selector.Actions)

	prepared, err := activities.PrepareTurn(context.Background(), PrepareTurnInput{
		SessionID:          "sess_resume",
		TriggerEventID:     "sevt_custom_result",
		ResolutionEventIDs: []string{"sevt_custom_result"},
	})
	require.NoError(t, err)
	require.False(t, prepared.AlreadyCompleted)
	require.Empty(t, prepared.FatalError)
	require.Equal(t, threadID, prepared.ThreadID)
	require.True(t, prepared.IsChild)
	require.Equal(t, []domain.Message{{
		Role: domain.RoleUser,
		Content: []domain.ContentBlock{{
			Type: "text", Text: "inspect",
		}},
	}}, prepared.Request.Messages, "parked action/result must be reconstructed by Workflow, not projected twice")
	require.Equal(t, []ResumeAction{{
		ActionEventID:     "sevt_custom",
		ActionEventType:   domain.EvAgentCustomToolUse,
		Kind:              domain.PendingCustomToolResult,
		ToolName:          "ask_client",
		Input:             map[string]any{"question": "continue?"},
		ResolutionEventID: "sevt_custom_result",
		Content: []any{
			map[string]any{"type": "text", "text": "yes"},
		},
	}}, prepared.ResumeActions)
}

func TestPrepareTurn_UsesLosslessTranscriptAndMapsResumeToProviderID(t *testing.T) {
	processedAt := time.Now().UTC()
	resolutionID := "sevt_custom_result"
	base := newFakeSource([]domain.Event{
		{
			ID: "sevt_user", Sequence: 1, Type: domain.EvUserMessage,
			Payload: map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "inspect"},
			}},
			ProcessedAt: &processedAt,
		},
		{
			ID: "sevt_custom", Sequence: 2, Type: domain.EvAgentCustomToolUse,
			Payload: map[string]any{
				"name":  "ask_client",
				"input": map[string]any{"question": "continue?"},
			},
		},
		{
			ID: "sevt_custom_result", Sequence: 3,
			Type: domain.EvUserCustomToolResult,
			Payload: map[string]any{
				"custom_tool_use_id": "sevt_custom",
				"content": []any{
					map[string]any{"type": "text", "text": "yes"},
				},
			},
			ProcessedAt: &processedAt,
		},
	})
	base.pendingActions = []domain.PendingAction{{
		ID:               "pact_1",
		SessionID:        "sess_resume",
		ActionEventID:    "sevt_custom",
		Kind:             domain.PendingCustomToolResult,
		ResolvingEventID: &resolutionID,
	}}
	rawToolUse := json.RawMessage(
		`{"type":"tool_use","id":"toolu_provider","name":"ask_client","input":{"question":"continue?"},"future_field":"keep"}`,
	)
	source := &transcriptFakeSource{
		fakeSource: base,
		transcript: domain.ProviderTranscript{
			TriggerEventIDs: []string{"sevt_user"},
			Messages: []domain.Message{
				{
					Role: domain.RoleUser,
					Content: []domain.ContentBlock{{
						Type: "text", Text: "inspect",
					}},
				},
				{
					Role: domain.RoleAssistant,
					Content: []domain.ContentBlock{{
						Type:      "tool_use",
						ToolUseID: "toolu_provider",
						ToolName:  "ask_client",
						Input: map[string]any{
							"question": "continue?",
						},
						Raw: rawToolUse,
					}},
				},
			},
			ToolUseMappings: []domain.ProviderToolUseMapping{{
				PublicEventID:     "sevt_custom",
				ProviderToolUseID: "toolu_provider",
				ToolName:          "ask_client",
			}},
		},
	}
	activities := NewActivities(nil, source, nil, nil, &testIDGen{})
	prepared, err := activities.PrepareTurn(
		context.Background(),
		PrepareTurnInput{
			SessionID:          "sess_resume",
			TriggerEventID:     "sevt_custom_result",
			ResolutionEventIDs: []string{"sevt_custom_result"},
		},
	)
	require.NoError(t, err)
	require.True(t, prepared.UsesProviderTranscript)
	require.Len(t, prepared.Request.Messages, 2)
	require.Equal(
		t,
		"toolu_provider",
		prepared.ResumeActions[0].ProviderToolUseID,
	)

	turn := &workflowTurnState{
		usesProviderTranscript: true,
	}
	messages, _, failure, err := resumeWorkflowTurn(
		turn,
		prepared,
		map[string]TurnTool{
			"ask_client": {Name: "ask_client", Kind: TurnToolCustom},
		},
		prepared.Request.Messages,
	)
	require.NoError(t, err)
	require.Empty(t, failure)
	require.Len(t, messages, 3)
	result := messages[2].Content[0]
	require.Equal(t, "toolu_provider", result.ToolResultFor)
	require.Len(t, turn.transcriptDelta, 1)
	require.Equal(
		t,
		"toolu_provider",
		turn.transcriptDelta[0].Content[0].ToolResultFor,
	)
}

func TestPrepareTurn_MultiActionResumeKeepsLosslessTranscript(t *testing.T) {
	processedAt := time.Now().UTC()
	resolutionA := "sevt_result_a"
	resolutionB := "sevt_result_b"
	base := newFakeSource([]domain.Event{
		{
			ID: "sevt_user", Sequence: 1, Type: domain.EvUserMessage,
			Payload: map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "do both"},
			}},
			ProcessedAt: &processedAt,
		},
		{
			ID: "sevt_action_a", Sequence: 2, Type: domain.EvAgentCustomToolUse,
			Payload: map[string]any{
				"name": "tool_a", "input": map[string]any{"value": "a"},
			},
		},
		{
			ID: "sevt_action_b", Sequence: 3, Type: domain.EvAgentCustomToolUse,
			Payload: map[string]any{
				"name": "tool_b", "input": map[string]any{"value": "b"},
			},
		},
		{
			ID: resolutionA, Sequence: 4, Type: domain.EvUserCustomToolResult,
			Payload: map[string]any{
				"custom_tool_use_id": "sevt_action_a",
				"content":            []any{map[string]any{"type": "text", "text": "A"}},
			},
			ProcessedAt: &processedAt,
		},
		{
			ID: resolutionB, Sequence: 5, Type: domain.EvUserCustomToolResult,
			Payload: map[string]any{
				"custom_tool_use_id": "sevt_action_b",
				"content":            []any{map[string]any{"type": "text", "text": "B"}},
			},
			ProcessedAt: &processedAt,
		},
	})
	base.pendingActions = []domain.PendingAction{
		{
			ID: "pact_a", SessionID: "sess_multi",
			ActionEventID: "sevt_action_a", Kind: domain.PendingCustomToolResult,
			ResolvingEventID: &resolutionA,
		},
		{
			ID: "pact_b", SessionID: "sess_multi",
			ActionEventID: "sevt_action_b", Kind: domain.PendingCustomToolResult,
			ResolvingEventID: &resolutionB,
		},
	}
	source := &configuredTranscriptFakeSource{
		transcriptFakeSource: &transcriptFakeSource{
			fakeSource: base,
			transcript: domain.ProviderTranscript{
				TriggerEventIDs: []string{"sevt_user"},
				Messages: []domain.Message{
					{
						Role:    domain.RoleUser,
						Content: []domain.ContentBlock{{Type: "text", Text: "do both"}},
					},
					{
						Role: domain.RoleAssistant,
						Content: []domain.ContentBlock{
							{Type: "tool_use", ToolUseID: "provider_a", ToolName: "tool_a", Input: map[string]any{"value": "a"}},
							{Type: "tool_use", ToolUseID: "provider_b", ToolName: "tool_b", Input: map[string]any{"value": "b"}},
						},
					},
				},
				ToolUseMappings: []domain.ProviderToolUseMapping{
					{PublicEventID: "sevt_action_a", ProviderToolUseID: "provider_a", ToolName: "tool_a"},
					{PublicEventID: "sevt_action_b", ProviderToolUseID: "provider_b", ToolName: "tool_b"},
				},
			},
		},
		session: domain.Session{
			ID: "sess_multi", Status: domain.StatusRunning,
			AgentSnapshot: domain.Agent{
				Model: domain.Model{ID: "model"},
				Tools: []any{
					map[string]any{"type": "custom", "name": "tool_a"},
					map[string]any{"type": "custom", "name": "tool_b"},
				},
			},
		},
	}

	prepared, err := NewActivities(
		nil,
		source,
		nil,
		nil,
		&testIDGen{},
	).PrepareTurn(context.Background(), PrepareTurnInput{
		SessionID:          "sess_multi",
		TriggerEventID:     resolutionB,
		ResolutionEventIDs: []string{resolutionA, resolutionB},
	})

	require.NoError(t, err)
	require.Empty(t, prepared.FatalError)
	require.True(t, prepared.UsesProviderTranscript)
	require.Len(t, prepared.ResumeActions, 2)
	require.Equal(t, "provider_a", prepared.ResumeActions[0].ProviderToolUseID)
	require.Equal(t, "provider_b", prepared.ResumeActions[1].ProviderToolUseID)
}
