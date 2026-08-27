package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/yanpgwang/mango/internal/domain"
)

// These tests drive the server through the Anthropic Go SDK as optional design
// research. Mango's OpenAPI and raw HTTP tests define the contract; successful
// SDK calls only show that selected request and response shapes remain reusable.
//
// SDK-expressible JSON is asserted here; wire details the SDK cannot express
// (e.g. exact top-level event union flattening) are covered by the raw-HTTP
// golden tests in sdk_golden_test.go.

func sdkClientAndServer(t *testing.T) (anthropic.Client, *httptest.Server) {
	t.Helper()
	client, server, _ := sdkClientServerAndSessions(t)
	return client, server
}

func sdkClientServerAndSessions(
	t *testing.T,
) (anthropic.Client, *httptest.Server, *testSessionService) {
	t.Helper()
	handler, sessions := newTestHandlerWithSessions(t, Config{
		RequireAuth: true,
	}, false)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	client := anthropic.NewClient(
		option.WithBaseURL(ts.URL),
		option.WithAuthToken("sk-test"),
	)
	return client, ts, sessions
}

func TestSDK_SessionFileResourceLifecycle(t *testing.T) {
	client, ts, _ := sdkClientServerAndSessions(t)
	ctx := context.Background()
	agent := mustAgent(t, client, "opus", "sys")
	environmentID := mustEnv(t, ts.URL)

	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: environmentID,
		Resources: []anthropic.BetaSessionNewParamsResourceUnion{{
			OfFile: &anthropic.BetaManagedAgentsFileResourceParams{
				FileID: "file_create_source",
				Type:   anthropic.BetaManagedAgentsFileResourceParamsTypeFile,
			},
		}},
	})
	if err != nil {
		t.Fatalf("create session with resource: %v", err)
	}
	if len(session.Resources) != 1 {
		t.Fatalf("create-time resources = %d, want 1", len(session.Resources))
	}
	created := session.Resources[0].AsFile()
	assertRawObjectHasFields(
		t, created.RawJSON(), "id", "created_at", "file_id", "mount_path", "type", "updated_at",
	)
	if created.MountPath != "/mnt/session/uploads/file_create_source" ||
		created.FileID == "file_create_source" {
		t.Fatalf("create-time resource = %s", created.RawJSON())
	}

	added, err := client.Beta.Sessions.Resources.Add(
		ctx,
		session.ID,
		anthropic.BetaSessionResourceAddParams{
			BetaManagedAgentsFileResourceParams: anthropic.BetaManagedAgentsFileResourceParams{
				FileID:    "file_runtime_source",
				Type:      anthropic.BetaManagedAgentsFileResourceParamsTypeFile,
				MountPath: anthropic.String("/reports/receipt.pdf"),
			},
		},
	)
	if err != nil {
		t.Fatalf("add resource: %v", err)
	}
	if added.MountPath != "/mnt/session/uploads/reports/receipt.pdf" ||
		added.Type != anthropic.BetaManagedAgentsFileResourceTypeFile {
		t.Fatalf("added resource = %s", added.RawJSON())
	}

	got, err := client.Beta.Sessions.Resources.Get(
		ctx,
		added.ID,
		anthropic.BetaSessionResourceGetParams{SessionID: session.ID},
	)
	if err != nil {
		t.Fatalf("get resource: %v", err)
	}
	if file := got.AsFile(); file.ID != added.ID || file.FileID != added.FileID {
		t.Fatalf("get resource = %s", got.RawJSON())
	}

	firstPage, err := client.Beta.Sessions.Resources.List(
		ctx,
		session.ID,
		anthropic.BetaSessionResourceListParams{Limit: anthropic.Int(1)},
	)
	if err != nil {
		t.Fatalf("list first page: %v", err)
	}
	if len(firstPage.Data) != 1 || firstPage.NextPage == "" {
		t.Fatalf("first resource page = %#v", firstPage)
	}
	secondPage, err := client.Beta.Sessions.Resources.List(
		ctx,
		session.ID,
		anthropic.BetaSessionResourceListParams{
			Limit: anthropic.Int(1), Page: anthropic.String(firstPage.NextPage),
		},
	)
	if err != nil || len(secondPage.Data) != 1 || secondPage.Data[0].AsFile().ID != added.ID {
		t.Fatalf("list second page = %#v, err=%v", secondPage, err)
	}

	_, err = client.Beta.Sessions.Resources.Update(
		ctx,
		added.ID,
		anthropic.BetaSessionResourceUpdateParams{
			SessionID: session.ID, AuthorizationToken: "not-applicable",
		},
	)
	assertAPIStatus(t, err, http.StatusNotFound)

	deleted, err := client.Beta.Sessions.Resources.Delete(
		ctx,
		added.ID,
		anthropic.BetaSessionResourceDeleteParams{SessionID: session.ID},
	)
	if err != nil {
		t.Fatalf("delete resource: %v", err)
	}
	if deleted.ID != added.ID ||
		deleted.Type != anthropic.BetaManagedAgentsDeleteSessionResourceTypeSessionResourceDeleted {
		t.Fatalf("delete response = %s", deleted.RawJSON())
	}
}

func TestSDK_FileBackedOutcomeRubricShape(t *testing.T) {
	client, ts, _ := sdkClientServerAndSessions(t)
	ctx := context.Background()
	agent := mustAgent(t, client, "opus", "sys")
	environmentID := mustEnv(t, ts.URL)

	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent: anthropic.BetaSessionNewParamsAgentUnion{
			OfString: anthropic.String(agent.ID),
		},
		EnvironmentID: environmentID,
		InitialEvents: []anthropic.BetaSessionNewParamsInitialEventUnion{{
			OfUserDefineOutcome: &anthropic.BetaManagedAgentsUserDefineOutcomeEventParams{
				Type:        anthropic.BetaManagedAgentsUserDefineOutcomeEventParamsTypeUserDefineOutcome,
				Description: "produce report.md",
				Rubric: anthropic.BetaManagedAgentsUserDefineOutcomeEventParamsRubricUnion{
					OfFile: &anthropic.BetaManagedAgentsFileRubricParams{
						Type:   anthropic.BetaManagedAgentsFileRubricParamsTypeFile,
						FileID: "file_rubric",
					},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("create Session with file rubric through SDK: %v", err)
	}
	if session.ID == "" {
		t.Fatal("SDK decoded an empty Session id")
	}

	conversation, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent: anthropic.BetaSessionNewParamsAgentUnion{
			OfString: anthropic.String(agent.ID),
		},
		EnvironmentID: environmentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Beta.Sessions.Events.Send(
		ctx,
		conversation.ID,
		anthropic.BetaSessionEventSendParams{Events: []anthropic.BetaManagedAgentsEventParamsUnion{
			anthropic.BetaManagedAgentsEventParamsOfUserDefineOutcome(
				"produce report.md",
				anthropic.BetaManagedAgentsFileRubricParams{
					Type:   anthropic.BetaManagedAgentsFileRubricParamsTypeFile,
					FileID: "file_rubric",
				},
				anthropic.BetaManagedAgentsUserDefineOutcomeEventParamsTypeUserDefineOutcome,
			),
		}},
	)
	if err != nil || len(result.Data) == 0 {
		t.Fatalf("send file rubric through SDK: result=%+v err=%v", result, err)
	}
}

func TestSDK_SessionMemoryStoreResource(t *testing.T) {
	client, ts, _ := sdkClientServerAndSessions(t)
	ctx := context.Background()
	agent := mustAgent(t, client, "opus", "sys")
	environmentID := mustEnv(t, ts.URL)

	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: environmentID,
		Resources: []anthropic.BetaSessionNewParamsResourceUnion{{
			OfMemoryStore: &anthropic.BetaManagedAgentsMemoryStoreResourceParam{
				MemoryStoreID: "memstore_project",
				Type:          anthropic.BetaManagedAgentsMemoryStoreResourceParamTypeMemoryStore,
				Access:        anthropic.BetaManagedAgentsMemoryStoreResourceParamAccessReadOnly,
				Instructions:  anthropic.String("Prefer established project conventions."),
			},
		}},
	})
	if err != nil {
		t.Fatalf("create session with Memory Store: %v", err)
	}
	if len(session.Resources) != 1 {
		t.Fatalf("create-time resources = %d, want 1", len(session.Resources))
	}
	memory := session.Resources[0].AsMemoryStore()
	if memory.MemoryStoreID != "memstore_project" ||
		memory.Type != anthropic.BetaManagedAgentsMemoryStoreResourceTypeMemoryStore ||
		memory.Access != anthropic.BetaManagedAgentsMemoryStoreResourceAccessReadOnly ||
		memory.MountPath != "/mnt/memory/project-memory" ||
		memory.Instructions != "Prefer established project conventions." {
		t.Fatalf("Memory Store resource = %s", memory.RawJSON())
	}
	assertRawObjectHasFields(
		t, memory.RawJSON(), "memory_store_id", "type", "access", "description",
		"instructions", "mount_path", "name",
	)
}

func TestSDK_TerminalSessionErrorEvent(t *testing.T) {
	client, ts, sessions := sdkClientServerAndSessions(t)
	ctx := context.Background()

	agent := mustAgent(t, client, "opus", "sys")
	environmentID := mustEnv(t, ts.URL)
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: environmentID,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	sessions.mu.Lock()
	sessions.appendEventLocked(session.ID, domain.EventDraft{
		Type: domain.EvSessionError,
		Payload: map[string]any{"error": map[string]any{
			"type": "unknown_error", "message": "turn failed",
			"retry_status": map[string]any{"type": "terminal"},
		}},
	})
	sessions.mu.Unlock()

	page, err := client.Beta.Sessions.Events.List(
		ctx,
		session.ID,
		anthropic.BetaSessionEventListParams{Types: []string{domain.EvSessionError}},
	)
	if err != nil {
		t.Fatalf("list session errors: %v", err)
	}
	if len(page.Data) != 1 {
		t.Fatalf("session.error count = %d, want 1", len(page.Data))
	}
	event := page.Data[0].AsSessionError()
	unknown := event.Error.AsUnknownError()
	if unknown.Type != anthropic.BetaManagedAgentsUnknownErrorTypeUnknownError ||
		unknown.RetryStatus.Type != "terminal" || unknown.Message != "turn failed" {
		t.Fatalf("session.error = %s", event.RawJSON())
	}
}

func TestSDK_ModelRetryLifecycleEvents(t *testing.T) {
	client, ts, sessions := sdkClientServerAndSessions(t)
	ctx := context.Background()

	agent := mustAgent(t, client, "opus", "sys")
	environmentID := mustEnv(t, ts.URL)
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: environmentID,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	sessions.mu.Lock()
	sessions.appendEventLocked(session.ID, domain.EventDraft{
		Type: domain.EvSessionError,
		Payload: map[string]any{"error": map[string]any{
			"type": "model_rate_limited_error", "message": "slow down",
			"retry_status": map[string]any{"type": "retrying"},
		}},
	})
	sessions.appendEventLocked(session.ID, domain.EventDraft{
		Type: domain.EvSessionStatusRescheduling, Payload: map[string]any{},
	})
	sessions.appendEventLocked(session.ID, domain.EventDraft{
		Type: domain.EvSessionStatusRunning, Payload: map[string]any{},
	})
	sessions.mu.Unlock()

	page, err := client.Beta.Sessions.Events.List(
		ctx,
		session.ID,
		anthropic.BetaSessionEventListParams{Types: []string{
			domain.EvSessionError,
			domain.EvSessionStatusRescheduling,
			domain.EvSessionStatusRunning,
		}},
	)
	if err != nil {
		t.Fatalf("list retry lifecycle: %v", err)
	}
	if len(page.Data) != 3 {
		t.Fatalf("retry lifecycle event count = %d, want 3", len(page.Data))
	}
	retry := page.Data[0].AsSessionError().Error.AsModelRateLimitedError()
	if retry.Type != anthropic.BetaManagedAgentsModelRateLimitedErrorTypeModelRateLimitedError ||
		retry.Message != "slow down" || retry.RetryStatus.AsRetrying().Type != "retrying" {
		t.Fatalf("retry event = %s", page.Data[0].RawJSON())
	}
	if page.Data[1].AsSessionStatusRescheduled().Type !=
		anthropic.BetaManagedAgentsSessionStatusRescheduledEventTypeSessionStatusRescheduled {
		t.Fatalf("rescheduled event = %s", page.Data[1].RawJSON())
	}
	if page.Data[2].AsSessionStatusRunning().Type !=
		anthropic.BetaManagedAgentsSessionStatusRunningEventTypeSessionStatusRunning {
		t.Fatalf("running event = %s", page.Data[2].RawJSON())
	}
}

func TestSDK_ThinkingAndBillingEvents(t *testing.T) {
	client, ts, sessions := sdkClientServerAndSessions(t)
	ctx := context.Background()
	agent := mustAgent(t, client, "opus", "sys")
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: mustEnv(t, ts.URL),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	sessions.mu.Lock()
	sessions.appendEventLocked(session.ID, domain.EventDraft{
		Type: domain.EvAgentThinking, Payload: map[string]any{},
	})
	sessions.appendEventLocked(session.ID, domain.EventDraft{
		Type: domain.EvSessionError,
		Payload: map[string]any{"error": map[string]any{
			"type": "billing_error", "message": "credits exhausted",
			"retry_status": map[string]any{"type": "terminal"},
		}},
	})
	sessions.mu.Unlock()

	page, err := client.Beta.Sessions.Events.List(ctx, session.ID,
		anthropic.BetaSessionEventListParams{Types: []string{
			domain.EvAgentThinking, domain.EvSessionError,
		}},
	)
	if err != nil {
		t.Fatalf("list thinking and billing events: %v", err)
	}
	if len(page.Data) != 2 {
		t.Fatalf("event count = %d, want 2", len(page.Data))
	}
	thinking := page.Data[0].AsAgentThinking()
	if thinking.Type != anthropic.BetaManagedAgentsAgentThinkingEventTypeAgentThinking {
		t.Fatalf("thinking event = %s", page.Data[0].RawJSON())
	}
	billing := page.Data[1].AsSessionError().Error.AsBillingError()
	if billing.Type != anthropic.BetaManagedAgentsBillingErrorTypeBillingError ||
		billing.Message != "credits exhausted" ||
		billing.RetryStatus.AsTerminal().Type != "terminal" {
		t.Fatalf("billing event = %s", page.Data[1].RawJSON())
	}
}

func TestSDK_AgentLifecycle(t *testing.T) {
	client, _ := sdkClientAndServer(t)
	ctx := context.Background()
	peer := mustAgent(t, client, "peer", "peer")
	peerV2 := mustAgent(t, client, "peer-v2", "peer-v2")
	peerV2, err := client.Beta.Agents.Update(ctx, peerV2.ID, anthropic.BetaAgentUpdateParams{
		Name: anthropic.String("Peer v2"),
	})
	if err != nil {
		t.Fatalf("prepare versioned peer: %v", err)
	}

	// Create.
	agent, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
		Name: "SRE Agent",
		Model: anthropic.BetaManagedAgentsModelConfigParams{
			ID: anthropic.BetaManagedAgentsModelClaudeOpus4_8,
		},
		System:   anthropic.String("help"),
		Metadata: map[string]string{"team": "sre"},
		Tools: []anthropic.BetaAgentNewParamsToolUnion{{
			OfCustom: &anthropic.BetaManagedAgentsCustomToolParams{
				Description: "Look up the current service status.",
				InputSchema: anthropic.BetaManagedAgentsCustomToolInputSchemaParam{
					Properties: map[string]any{"service": map[string]any{"type": "string"}},
					Required:   []string{"service"},
				},
				Name: "get_service_status",
				Type: anthropic.BetaManagedAgentsCustomToolParamsTypeCustom,
			},
		}},
		Multiagent: anthropic.BetaManagedAgentsMultiagentParams{
			Type: anthropic.BetaManagedAgentsMultiagentParamsTypeCoordinator,
			Agents: []anthropic.BetaManagedAgentsMultiagentRosterEntryParamsUnion{{
				OfBetaManagedAgentsAgents: &anthropic.BetaManagedAgentsAgentParams{
					ID:      peer.ID,
					Type:    anthropic.BetaManagedAgentsAgentParamsTypeAgent,
					Version: anthropic.Int(1),
				},
			}},
		},
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if agent.ID == "" {
		t.Fatal("created agent has empty id")
	}
	if agent.Type != "agent" {
		t.Fatalf("agent type = %q, want agent", agent.Type)
	}
	if agent.Version != 1 {
		t.Fatalf("new agent version = %d, want 1", agent.Version)
	}
	if agent.Model.ID != anthropic.BetaManagedAgentsModelClaudeOpus4_8 {
		t.Fatalf("model id = %q", agent.Model.ID)
	}
	if agent.System != "help" {
		t.Fatalf("system = %q, want help", agent.System)
	}
	if agent.Metadata["team"] != "sre" {
		t.Fatalf("metadata = %#v, want team=sre", agent.Metadata)
	}
	if len(agent.Tools) != 1 || agent.Tools[0].Type != "custom" ||
		agent.Tools[0].Name != "get_service_status" ||
		agent.Tools[0].Description != "Look up the current service status." ||
		agent.Tools[0].InputSchema.Type != "object" {
		t.Fatalf("custom tool response = %#v", agent.Tools)
	}
	if agent.Multiagent.Type != anthropic.BetaManagedAgentsMultiagentTypeCoordinator ||
		len(agent.Multiagent.Agents) != 1 ||
		agent.Multiagent.Agents[0].ID != peer.ID ||
		agent.Multiagent.Agents[0].Version != 1 {
		t.Fatalf("multiagent response = %#v", agent.Multiagent)
	}
	assertRawObjectHasFields(t, agent.RawJSON(), "multiagent")
	if agent.JSON.Multiagent.Raw() == "" {
		t.Fatal("agent response omitted multiagent")
	}

	// Get.
	got, err := client.Beta.Agents.Get(ctx, agent.ID, anthropic.BetaAgentGetParams{})
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.ID != agent.ID || got.Version != 1 {
		t.Fatalf("get returned %s v%d", got.ID, got.Version)
	}

	// Update -> version increments to 2 (official `version` optimistic field).
	updated, err := client.Beta.Agents.Update(ctx, agent.ID, anthropic.BetaAgentUpdateParams{
		Name:    anthropic.String("SRE Agent v2"),
		Version: anthropic.Int(1),
		Multiagent: anthropic.BetaManagedAgentsMultiagentParams{
			Type: anthropic.BetaManagedAgentsMultiagentParamsTypeCoordinator,
			Agents: []anthropic.BetaManagedAgentsMultiagentRosterEntryParamsUnion{{
				OfBetaManagedAgentsAgents: &anthropic.BetaManagedAgentsAgentParams{
					ID:      peerV2.ID,
					Type:    anthropic.BetaManagedAgentsAgentParamsTypeAgent,
					Version: anthropic.Int(2),
				},
			}},
		},
	})
	if err != nil {
		t.Fatalf("update agent: %v", err)
	}
	if updated.Version != 2 {
		t.Fatalf("updated version = %d, want 2", updated.Version)
	}
	if updated.Name != "SRE Agent v2" {
		t.Fatalf("updated name = %q", updated.Name)
	}
	if len(updated.Multiagent.Agents) != 1 ||
		updated.Multiagent.Agents[0].ID != peerV2.ID ||
		updated.Multiagent.Agents[0].Version != 2 {
		t.Fatalf("updated multiagent = %#v", updated.Multiagent)
	}

	// Update with a stale version must conflict.
	_, err = client.Beta.Agents.Update(ctx, agent.ID, anthropic.BetaAgentUpdateParams{
		Name:    anthropic.String("stale"),
		Version: anthropic.Int(1),
	})
	if err == nil {
		t.Fatal("expected version-conflict error on stale update, got nil")
	}
	assertAPIStatus(t, err, 409)

	// List.
	list, err := client.Beta.Agents.List(ctx, anthropic.BetaAgentListParams{})
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	if len(list.Data) != 3 {
		t.Fatalf("list returned %d agents, want coordinator and two peers", len(list.Data))
	}
	listedCoordinator := false
	for _, listed := range list.Data {
		if listed.ID == agent.ID {
			listedCoordinator = true
			if listed.Version != 2 {
				t.Fatalf("listed coordinator version = %d, want latest 2", listed.Version)
			}
		}
	}
	if !listedCoordinator {
		t.Fatal("list omitted coordinator")
	}

	// Archive.
	archived, err := client.Beta.Agents.Archive(ctx, agent.ID, anthropic.BetaAgentArchiveParams{})
	if err != nil {
		t.Fatalf("archive agent: %v", err)
	}
	if archived.ArchivedAt.IsZero() {
		t.Fatal("archived agent has zero archived_at")
	}
	if archived.Version != updated.Version {
		t.Fatalf("archive changed configuration version: got %d want %d", archived.Version, updated.Version)
	}
	_, err = client.Beta.Agents.Update(ctx, agent.ID, anthropic.BetaAgentUpdateParams{
		Name: anthropic.String("must fail"),
	})
	if err == nil {
		t.Fatal("expected archived agent update to fail")
	}
	assertAPIStatus(t, err, 400)
}

func TestSDK_AgentMultiagentSelfResolvesAndTracksCoordinatorVersion(t *testing.T) {
	client, _ := sdkClientAndServer(t)
	ctx := context.Background()
	selfEntry := anthropic.BetaManagedAgentsMultiagentRosterEntryParamsOfBetaManagedAgentsMultiagentSelfs(
		anthropic.BetaManagedAgentsMultiagentSelfParamsTypeSelf,
	)
	agent, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
		Name:  "Recursive coordinator",
		Model: anthropic.BetaManagedAgentsModelConfigParams{ID: anthropic.BetaManagedAgentsModelClaudeOpus4_8},
		Multiagent: anthropic.BetaManagedAgentsMultiagentParams{
			Type: anthropic.BetaManagedAgentsMultiagentParamsTypeCoordinator, Agents: []anthropic.BetaManagedAgentsMultiagentRosterEntryParamsUnion{selfEntry},
		},
	})
	if err != nil {
		t.Fatalf("create self coordinator: %v", err)
	}
	if len(agent.Multiagent.Agents) != 1 || agent.Multiagent.Agents[0].ID != agent.ID ||
		agent.Multiagent.Agents[0].Version != 1 || agent.Multiagent.Agents[0].Type != "agent" {
		t.Fatalf("resolved self roster = %s", agent.Multiagent.RawJSON())
	}

	updated, err := client.Beta.Agents.Update(ctx, agent.ID, anthropic.BetaAgentUpdateParams{
		Name: anthropic.String("Recursive coordinator v2"),
	})
	if err != nil {
		t.Fatalf("update self coordinator: %v", err)
	}
	if updated.Version != 2 || updated.Multiagent.Agents[0].ID != agent.ID ||
		updated.Multiagent.Agents[0].Version != 2 {
		t.Fatalf("updated self roster = %s", updated.Multiagent.RawJSON())
	}
}

func TestSDK_AgentAdvisorRosterRoundTripsLast(t *testing.T) {
	client, _ := sdkClientAndServer(t)
	ctx := context.Background()
	selfEntry := anthropic.BetaManagedAgentsMultiagentRosterEntryParamsOfBetaManagedAgentsMultiagentSelfs(
		anthropic.BetaManagedAgentsMultiagentSelfParamsTypeSelf,
	)
	advisorEntry := anthropic.BetaManagedAgentsMultiagentRosterEntryParamsOfBetaManagedAgentsAdvisors(
		"claude-opus-5", anthropic.BetaManagedAgentsAdvisorParamsTypeAdvisor,
	)
	agent, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
		Name: "Advisor coordinator",
		Model: anthropic.BetaManagedAgentsModelConfigParams{
			ID: anthropic.BetaManagedAgentsModelClaudeSonnet5,
		},
		Multiagent: anthropic.BetaManagedAgentsMultiagentParams{
			Type: anthropic.BetaManagedAgentsMultiagentParamsTypeCoordinator,
			Agents: []anthropic.BetaManagedAgentsMultiagentRosterEntryParamsUnion{
				advisorEntry, selfEntry,
			},
		},
	})
	if err != nil {
		t.Fatalf("create Advisor coordinator: %v", err)
	}
	if len(agent.Multiagent.Agents) != 2 {
		t.Fatalf("Advisor roster = %s", agent.Multiagent.RawJSON())
	}
	if self := agent.Multiagent.Agents[0].AsAgent(); self.ID != agent.ID || self.Version != 1 || self.Type != "agent" {
		t.Fatalf("resolved self = %s", agent.Multiagent.Agents[0].RawJSON())
	}
	advisor := agent.Multiagent.Agents[1].AsAdvisor()
	if advisor.Type != anthropic.BetaManagedAgentsAdvisorTypeAdvisor ||
		advisor.Model != "claude-opus-5" {
		t.Fatalf("Advisor response = %s", agent.Multiagent.Agents[1].RawJSON())
	}
}

func TestSDK_SessionMultiagentRosterExpandsAndFreezesAgentSnapshots(t *testing.T) {
	client, server := sdkClientAndServer(t)
	ctx := context.Background()
	peer, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
		Name:        "Reviewer",
		Description: anthropic.String("Reviews changes before merge."),
		Model: anthropic.BetaManagedAgentsModelConfigParams{
			ID: anthropic.BetaManagedAgentsModelClaudeOpus4_8,
		},
		System: anthropic.String("review-system-v1"),
	})
	if err != nil {
		t.Fatalf("create peer: %v", err)
	}
	selfEntry := anthropic.BetaManagedAgentsMultiagentRosterEntryParamsOfBetaManagedAgentsMultiagentSelfs(
		anthropic.BetaManagedAgentsMultiagentSelfParamsTypeSelf,
	)
	coordinator, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
		Name: "Coordinator",
		Model: anthropic.BetaManagedAgentsModelConfigParams{
			ID: anthropic.BetaManagedAgentsModelClaudeOpus4_8,
		},
		System: anthropic.String("coordinator-system-v1"),
		Multiagent: anthropic.BetaManagedAgentsMultiagentParams{
			Type: anthropic.BetaManagedAgentsMultiagentParamsTypeCoordinator,
			Agents: []anthropic.BetaManagedAgentsMultiagentRosterEntryParamsUnion{
				{
					OfBetaManagedAgentsAgents: &anthropic.BetaManagedAgentsAgentParams{
						ID: peer.ID, Type: anthropic.BetaManagedAgentsAgentParamsTypeAgent,
						Version: anthropic.Int(1),
					},
				},
				selfEntry,
			},
		},
	})
	if err != nil {
		t.Fatalf("create coordinator: %v", err)
	}
	environmentID := mustEnv(t, server.URL)
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent: anthropic.BetaSessionNewParamsAgentUnion{
			OfBetaManagedAgentsAgentWithOverridess: &anthropic.BetaManagedAgentsAgentWithOverridesParams{
				ID:     coordinator.ID,
				Type:   anthropic.BetaManagedAgentsAgentWithOverridesParamsTypeAgentWithOverrides,
				System: anthropic.String("session-coordinator-system"),
			},
		},
		EnvironmentID: environmentID,
	})
	if err != nil {
		t.Fatalf("create Session: %v", err)
	}
	if len(session.Agent.Multiagent.Agents) != 2 {
		t.Fatalf("resolved roster = %s", session.Agent.Multiagent.RawJSON())
	}
	resolvedPeer := session.Agent.Multiagent.Agents[0].AsAgent()
	if resolvedPeer.ID != peer.ID || resolvedPeer.Version != 1 ||
		resolvedPeer.Name != "Reviewer" || resolvedPeer.System != "review-system-v1" ||
		resolvedPeer.Description != "Reviews changes before merge." {
		t.Fatalf("resolved peer = %s", resolvedPeer.RawJSON())
	}
	resolvedSelf := session.Agent.Multiagent.Agents[1].AsAgent()
	if resolvedSelf.ID != coordinator.ID || resolvedSelf.Version != 1 ||
		resolvedSelf.System != "session-coordinator-system" {
		t.Fatalf("resolved self = %s", resolvedSelf.RawJSON())
	}

	if _, err := client.Beta.Agents.Update(ctx, peer.ID, anthropic.BetaAgentUpdateParams{
		System:  anthropic.String("review-system-v2"),
		Version: anthropic.Int(1),
	}); err != nil {
		t.Fatalf("update peer: %v", err)
	}
	reloaded, err := client.Beta.Sessions.Get(ctx, session.ID, anthropic.BetaSessionGetParams{})
	if err != nil {
		t.Fatalf("reload Session: %v", err)
	}
	frozenPeer := reloaded.Agent.Multiagent.Agents[0].AsAgent()
	if frozenPeer.Version != 1 || frozenPeer.System != "review-system-v1" {
		t.Fatalf("Session roster drifted after Agent update: %s", frozenPeer.RawJSON())
	}
}

func TestSDK_SkillReferencesPinAcrossAgentAndSessionSnapshots(t *testing.T) {
	client, server, sessions := sdkClientServerAndSessions(t)
	ctx := context.Background()
	const skillID = "skill_sdk_pin"
	sessions.setLatestSkillVersion(skillID, "100")
	agent, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
		Name: "Skill Agent",
		Model: anthropic.BetaManagedAgentsModelConfigParams{
			ID: anthropic.BetaManagedAgentsModelClaudeOpus4_8,
		},
		Tools: []anthropic.BetaAgentNewParamsToolUnion{{
			OfAgentToolset20260401: &anthropic.BetaManagedAgentsAgentToolset20260401Params{
				Type: anthropic.BetaManagedAgentsAgentToolset20260401ParamsTypeAgentToolset20260401,
			},
		}},
		Skills: []anthropic.BetaManagedAgentsSkillParamsUnion{{
			OfCustom: &anthropic.BetaManagedAgentsCustomSkillParams{
				Type:    anthropic.BetaManagedAgentsCustomSkillParamsTypeCustom,
				SkillID: skillID,
				Version: anthropic.String("latest"),
			},
		}},
	})
	if err != nil {
		t.Fatalf("create Agent with Skill: %v", err)
	}
	if len(agent.Skills) != 1 || agent.Skills[0].Version != "100" {
		t.Fatalf("resolved Agent Skills = %#v", agent.Skills)
	}

	sessions.setLatestSkillVersion(skillID, "200")
	environmentID := mustEnv(t, server.URL)
	inherited, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: environmentID,
	})
	if err != nil {
		t.Fatalf("create inherited Session: %v", err)
	}
	if len(inherited.Agent.Skills) != 1 || inherited.Agent.Skills[0].Version != "100" {
		t.Fatalf("inherited Session Skills = %#v, want Agent pin 100", inherited.Agent.Skills)
	}

	overridden, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent: anthropic.BetaSessionNewParamsAgentUnion{
			OfBetaManagedAgentsAgentWithOverridess: &anthropic.BetaManagedAgentsAgentWithOverridesParams{
				ID:   agent.ID,
				Type: anthropic.BetaManagedAgentsAgentWithOverridesParamsTypeAgentWithOverrides,
				Skills: []anthropic.BetaManagedAgentsSkillParamsUnion{{
					OfCustom: &anthropic.BetaManagedAgentsCustomSkillParams{
						Type:    anthropic.BetaManagedAgentsCustomSkillParamsTypeCustom,
						SkillID: skillID,
						Version: anthropic.String("latest"),
					},
				}},
			},
		},
		EnvironmentID: environmentID,
	})
	if err != nil {
		t.Fatalf("create overridden Session: %v", err)
	}
	if len(overridden.Agent.Skills) != 1 || overridden.Agent.Skills[0].Version != "200" {
		t.Fatalf("overridden Session Skills = %#v, want resolved latest 200", overridden.Agent.Skills)
	}
}

func TestSDK_SessionLifecycleAndSnapshot(t *testing.T) {
	client, ts := sdkClientAndServer(t)
	ctx := context.Background()

	agent := mustAgent(t, client, "opus", "system prompt A")
	env := mustEnv(t, ts.URL)

	// Create with the string (latest) agent form.
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: env,
		Title:         anthropic.String("Order #1234 inquiry"),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if session.Type != "session" {
		t.Fatalf("session type = %q", session.Type)
	}
	if session.Status != anthropic.BetaManagedAgentsSessionStatusIdle {
		t.Fatalf("new session status = %q, want idle", session.Status)
	}
	if session.Agent.ID != agent.ID || session.Agent.Version != 1 {
		t.Fatalf("session agent snapshot = %s v%d", session.Agent.ID, session.Agent.Version)
	}
	if session.Agent.System != "system prompt A" {
		t.Fatalf("snapshot system = %q", session.Agent.System)
	}
	if session.Title != "Order #1234 inquiry" {
		t.Fatalf("title = %q", session.Title)
	}
	assertRawObjectHasFields(t, session.RawJSON(),
		"budget", "outcome_evaluations", "resources", "stats", "usage", "vault_ids", "deployment_id")
	assertRawObjectHasFields(t, session.Agent.RawJSON(), "multiagent")
	if !session.JSON.OutcomeEvaluations.Valid() || !session.JSON.Resources.Valid() ||
		!session.JSON.Stats.Valid() || !session.JSON.Usage.Valid() || !session.JSON.VaultIDs.Valid() {
		t.Fatal("session response contains a missing or invalid required collection/stats field")
	}
	if session.JSON.DeploymentID.Raw() == "" {
		t.Fatal("session response omitted nullable deployment_id")
	}
	if session.JSON.Budget.Raw() != "null" {
		t.Fatalf("session response budget = %q, want explicit null", session.JSON.Budget.Raw())
	}
	assertRawObjectHasFields(t, session.Usage.RawJSON(),
		"active_seconds", "cache_creation", "cache_read_input_tokens", "input_tokens",
		"list_cost", "output_tokens", "server_tool_use")
	if session.Usage.ListCost.Amount != "0" ||
		session.Usage.ServerToolUse.WebSearchRequests != 0 {
		t.Fatalf("new Session usage = %s", session.Usage.RawJSON())
	}
	if session.Agent.JSON.Multiagent.Raw() == "" {
		t.Fatal("session agent snapshot omitted multiagent")
	}

	// Mutating the underlying agent must not change the existing snapshot.
	if _, err := client.Beta.Agents.Update(ctx, agent.ID, anthropic.BetaAgentUpdateParams{
		System:  anthropic.String("system prompt B"),
		Version: anthropic.Int(1),
	}); err != nil {
		t.Fatalf("update agent: %v", err)
	}
	got, err := client.Beta.Sessions.Get(ctx, session.ID, anthropic.BetaSessionGetParams{})
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Agent.System != "system prompt A" {
		t.Fatalf("snapshot mutated after agent update: system = %q, want stable A", got.Agent.System)
	}
	if got.Agent.Version != 1 {
		t.Fatalf("snapshot version drifted to %d, want pinned 1", got.Agent.Version)
	}

	// List sessions.
	list, err := client.Beta.Sessions.List(ctx, anthropic.BetaSessionListParams{})
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != session.ID {
		t.Fatalf("list sessions returned %d rows", len(list.Data))
	}

	archived, err := client.Beta.Sessions.Archive(
		ctx, session.ID, anthropic.BetaSessionArchiveParams{},
	)
	if err != nil {
		t.Fatalf("archive session: %v", err)
	}
	if archived.ArchivedAt.IsZero() || archived.ID != session.ID ||
		archived.Agent.ID != agent.ID || archived.Title != session.Title {
		t.Fatalf("archived session = %+v", archived)
	}
	archivedAgain, err := client.Beta.Sessions.Archive(
		ctx, session.ID, anthropic.BetaSessionArchiveParams{},
	)
	if err != nil {
		t.Fatalf("archive session again: %v", err)
	}
	if !archivedAgain.ArchivedAt.Equal(archived.ArchivedAt) {
		t.Fatalf("idempotent archive changed archived_at: %s -> %s",
			archived.ArchivedAt, archivedAgain.ArchivedAt)
	}

	deleted, err := client.Beta.Sessions.Delete(ctx, session.ID, anthropic.BetaSessionDeleteParams{})
	if err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if deleted.ID != session.ID || deleted.Type != anthropic.BetaManagedAgentsDeletedSessionTypeSessionDeleted {
		t.Fatalf("deleted session response = %+v", deleted)
	}
}

func TestSDK_SessionBudgetLifecycle(t *testing.T) {
	client, ts, sessions := sdkClientServerAndSessions(t)
	ctx := context.Background()
	agent := mustAgent(t, client, "budget", "sys")
	environmentID := mustEnv(t, ts.URL)

	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: environmentID,
		Budget:        param.NullStruct[anthropic.BetaManagedAgentsBudgetLimitParam](),
	})
	if err != nil {
		t.Fatalf("explicit null budget should mean no ceiling: %v", err)
	}
	if session.JSON.Budget.Raw() != "null" {
		t.Fatalf("session budget = %q, want null", session.JSON.Budget.Raw())
	}
	limit := anthropic.BetaManagedAgentsBudgetLimitParam{
		Type: anthropic.BetaManagedAgentsBudgetLimitTypeLimit,
		MaxListCost: anthropic.BetaMonetaryAmountParam{
			Amount: "2500", Currency: anthropic.BetaCurrencyUsd,
		},
	}
	budgeted, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: environmentID,
		Budget:        limit,
	})
	if err != nil {
		t.Fatalf("create budgeted session: %v", err)
	}
	if !strings.Contains(budgeted.RawJSON(), `"amount":"2500"`) {
		t.Fatalf("budgeted session JSON = %s", budgeted.RawJSON())
	}
	sessions.mu.Lock()
	stored := sessions.sessions[budgeted.ID]
	sessions.appendEventLocked(budgeted.ID, domain.EventDraft{
		Type:    domain.EvSessionUsage,
		Payload: stored.UsageEventPayload(time.Now()),
	})
	sessions.mu.Unlock()
	usageEvents, err := client.Beta.Sessions.Events.List(
		ctx, budgeted.ID, anthropic.BetaSessionEventListParams{
			Types: []string{domain.EvSessionUsage},
		},
	)
	if err != nil || len(usageEvents.Data) != 1 {
		t.Fatalf("list session.usage = %+v, err=%v", usageEvents, err)
	}
	usageEvent := usageEvents.Data[0].AsSessionUsage()
	if usageEvent.Type != anthropic.BetaManagedAgentsSessionUsageEventTypeSessionUsage ||
		usageEvent.Budget.MaxListCost.Amount != "2500" ||
		usageEvent.Usage.ListCost.Amount != "0" {
		t.Fatalf("decoded session.usage = %s", usageEvent.RawJSON())
	}

	if _, err := client.Beta.Sessions.Update(ctx, session.ID, anthropic.BetaSessionUpdateParams{
		Budget: param.NullStruct[anthropic.BetaManagedAgentsBudgetLimitParam](),
	}); err != nil {
		t.Fatalf("explicit null budget update should remain a no-op: %v", err)
	}
	_, err = client.Beta.Sessions.Update(ctx, session.ID, anthropic.BetaSessionUpdateParams{Budget: limit})
	assertAPIStatus(t, err, http.StatusBadRequest)

	higher := limit
	higher.MaxListCost.Amount = "3000"
	updated, err := client.Beta.Sessions.Update(
		ctx, budgeted.ID, anthropic.BetaSessionUpdateParams{Budget: higher},
	)
	if err != nil || !strings.Contains(updated.RawJSON(), `"amount":"3000"`) {
		t.Fatalf("raise session budget: session=%+v err=%v", updated, err)
	}
	if _, err := client.Beta.Sessions.Update(ctx, budgeted.ID, anthropic.BetaSessionUpdateParams{
		Budget: param.NullStruct[anthropic.BetaManagedAgentsBudgetLimitParam](),
	}); err != nil {
		t.Fatalf("remove session budget: %v", err)
	}
	_, err = client.Beta.Sessions.Update(
		ctx, budgeted.ID, anthropic.BetaSessionUpdateParams{Budget: higher},
	)
	assertAPIStatus(t, err, http.StatusBadRequest)
}

func TestSDK_SessionPinnedVersion(t *testing.T) {
	client, ts := sdkClientAndServer(t)
	ctx := context.Background()

	agent := mustAgent(t, client, "opus", "v1 system")
	// Bump to version 2 with a different system prompt.
	if _, err := client.Beta.Agents.Update(ctx, agent.ID, anthropic.BetaAgentUpdateParams{
		System:  anthropic.String("v2 system"),
		Version: anthropic.Int(1),
	}); err != nil {
		t.Fatalf("update agent: %v", err)
	}
	env := mustEnv(t, ts.URL)

	// Pin the session to version 1.
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent: anthropic.BetaSessionNewParamsAgentUnion{
			OfBetaManagedAgentsAgents: &anthropic.BetaManagedAgentsAgentParams{
				Type:    anthropic.BetaManagedAgentsAgentParamsTypeAgent,
				ID:      agent.ID,
				Version: anthropic.Int(1),
			},
		},
		EnvironmentID: env,
	})
	if err != nil {
		t.Fatalf("create pinned session: %v", err)
	}
	if session.Agent.Version != 1 {
		t.Fatalf("pinned snapshot version = %d, want 1", session.Agent.Version)
	}
	if session.Agent.System != "v1 system" {
		t.Fatalf("pinned snapshot system = %q, want v1 system", session.Agent.System)
	}
}

func TestSDK_SessionTitleUpdateEmitsChangedFieldsEvent(t *testing.T) {
	client, ts := sdkClientAndServer(t)
	ctx := context.Background()

	agent := mustAgent(t, client, "opus", "sys")
	env := mustEnv(t, ts.URL)
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: env,
		Title:         anthropic.String("before"),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	updated, err := client.Beta.Sessions.Update(ctx, session.ID, anthropic.BetaSessionUpdateParams{
		Title: anthropic.String("after"),
	})
	if err != nil {
		t.Fatalf("update session: %v", err)
	}
	if updated.Title != "after" {
		t.Fatalf("updated title = %q", updated.Title)
	}
	// A same-value request is a no-op and must not emit a second event.
	if _, err := client.Beta.Sessions.Update(ctx, session.ID, anthropic.BetaSessionUpdateParams{
		Title: anthropic.String("after"),
	}); err != nil {
		t.Fatalf("no-op update session: %v", err)
	}

	page, err := client.Beta.Sessions.Events.List(ctx, session.ID, anthropic.BetaSessionEventListParams{
		Types: []string{"session.updated"},
	})
	if err != nil {
		t.Fatalf("list update events: %v", err)
	}
	if len(page.Data) != 1 {
		t.Fatalf("session.updated count = %d, want 1", len(page.Data))
	}
	event := page.Data[0].AsSessionUpdated()
	if event.Title != "after" || !event.JSON.Title.Valid() {
		t.Fatalf("session.updated title = %q, raw=%s", event.Title, event.RawJSON())
	}
}

func TestSDK_SessionAgentAndMetadataUpdate(t *testing.T) {
	client, ts := sdkClientAndServer(t)
	ctx := context.Background()

	agent := mustAgent(t, client, "opus", "sys")
	env := mustEnv(t, ts.URL)
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: env,
		Metadata:      map[string]string{"keep": "yes"},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	updated, err := client.Beta.Sessions.Update(ctx, session.ID, anthropic.BetaSessionUpdateParams{
		Metadata: map[string]string{"added": "new"},
		Agent: anthropic.BetaManagedAgentsSessionAgentUpdateParam{
			Tools: []anthropic.BetaManagedAgentsSessionAgentUpdateToolUnionParam{
				{
					OfAgentToolset20260401: &anthropic.BetaManagedAgentsAgentToolset20260401Params{
						Type: anthropic.BetaManagedAgentsAgentToolset20260401ParamsTypeAgentToolset20260401,
					},
				},
				{
					OfMCPToolset: &anthropic.BetaManagedAgentsMCPToolsetParams{
						Type:          anthropic.BetaManagedAgentsMCPToolsetParamsTypeMCPToolset,
						MCPServerName: "linear",
					},
				},
			},
			MCPServers: []anthropic.BetaManagedAgentsURLMCPServerParams{
				{
					Type: anthropic.BetaManagedAgentsURLMCPServerParamsTypeURL,
					Name: "linear",
					URL:  "https://mcp.example.com/sse",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("update session: %v", err)
	}
	if len(updated.Agent.Tools) != 2 || len(updated.Agent.MCPServers) != 1 {
		t.Fatalf("updated snapshot tools=%d servers=%d, raw=%s",
			len(updated.Agent.Tools), len(updated.Agent.MCPServers), updated.RawJSON())
	}
	if updated.Agent.Version != 1 {
		t.Fatalf("session-local update renumbered the agent: %d", updated.Agent.Version)
	}
	if updated.Metadata["keep"] != "yes" || updated.Metadata["added"] != "new" {
		t.Fatalf("metadata patch = %v", updated.Metadata)
	}

	// The underlying agent resource is unchanged.
	resource, err := client.Beta.Agents.Get(ctx, agent.ID, anthropic.BetaAgentGetParams{})
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if resource.Version != 1 || len(resource.Tools) != 0 {
		t.Fatalf("session update propagated to the agent: version=%d tools=%d",
			resource.Version, len(resource.Tools))
	}

	page, err := client.Beta.Sessions.Events.List(ctx, session.ID,
		anthropic.BetaSessionEventListParams{Types: []string{"session.updated"}})
	if err != nil {
		t.Fatalf("list update events: %v", err)
	}
	if len(page.Data) != 1 {
		t.Fatalf("session.updated count = %d, want 1", len(page.Data))
	}
	event := page.Data[0].AsSessionUpdated()
	if !event.JSON.Agent.Valid() || event.Agent.ID != agent.ID {
		t.Fatalf("session.updated agent = %s", event.RawJSON())
	}
	if !event.JSON.Metadata.Valid() || event.Metadata["added"] != "new" {
		t.Fatalf("session.updated metadata = %s", event.RawJSON())
	}
	if event.JSON.Title.Valid() {
		t.Fatalf("session.updated carries an unchanged title: %s", event.RawJSON())
	}
}

func TestSDK_SessionUpdateRejectsVaultIDs(t *testing.T) {
	client, ts := sdkClientAndServer(t)
	ctx := context.Background()

	agent := mustAgent(t, client, "opus", "sys")
	env := mustEnv(t, ts.URL)
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: env,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	_, err = client.Beta.Sessions.Update(ctx, session.ID, anthropic.BetaSessionUpdateParams{
		VaultIDs: []string{"vlt_1"},
	})
	if err == nil {
		t.Fatal("expected vault_ids to be rejected")
	}
	assertAPIStatus(t, err, 422)
}

func TestSDK_SessionCreateAcceptsOrderedVaultIDs(t *testing.T) {
	client, ts := sdkClientAndServer(t)
	ctx := context.Background()
	agent := mustAgent(t, client, "opus", "sys")
	environment := mustEnv(t, ts.URL)
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: environment,
		VaultIDs:      []string{"vlt_first", "vlt_second"},
	})
	if err != nil {
		t.Fatalf("create Session with Vaults: %v", err)
	}
	if len(session.VaultIDs) != 2 || session.VaultIDs[0] != "vlt_first" || session.VaultIDs[1] != "vlt_second" {
		t.Fatalf("Session Vault order = %#v", session.VaultIDs)
	}
}

func TestSDK_EventSendAndList(t *testing.T) {
	client, ts := sdkClientAndServer(t)
	ctx := context.Background()

	agent := mustAgent(t, client, "opus", "sys")
	env := mustEnv(t, ts.URL)
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: env,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Send a user.message with content[] blocks.
	sent, err := client.Beta.Sessions.Events.Send(ctx, session.ID, anthropic.BetaSessionEventSendParams{
		Events: []anthropic.BetaManagedAgentsEventParamsUnion{{
			OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
				Type: anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage,
				Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{
					OfText: &anthropic.BetaManagedAgentsTextBlockParam{
						Type: anthropic.BetaManagedAgentsTextBlockTypeText,
						Text: "Where is my order #1234?",
					},
				}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("send event: %v", err)
	}
	if len(sent.Data) != 1 {
		t.Fatalf("send echoed %d events, want 1", len(sent.Data))
	}
	if sent.Data[0].Type != "user.message" || sent.Data[0].ID == "" {
		t.Fatalf("echoed event = %+v", sent.Data[0])
	}

	// Poll list until the fake runtime's agent.message + status_idle land.
	deadline := time.Now().Add(3 * time.Second)
	var userText string
	var sawIdle bool
	for time.Now().Before(deadline) {
		page, err := client.Beta.Sessions.Events.List(ctx, session.ID, anthropic.BetaSessionEventListParams{})
		if err != nil {
			t.Fatalf("list events: %v", err)
		}
		userText, sawIdle = "", false
		for _, ev := range page.Data {
			switch ev.Type {
			case "user.message":
				for _, block := range ev.AsUserMessage().Content {
					if block.Type == "text" {
						userText = block.AsText().Text
					}
				}
			case "session.status_idle":
				sawIdle = true
			}
		}
		if userText != "" && sawIdle {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if userText != "Where is my order #1234?" {
		t.Fatalf("listed user.message text = %q", userText)
	}
	if !sawIdle {
		t.Fatal("never observed session.status_idle in listed events")
	}
}

func TestSDK_EventSendTargetedInterrupt(t *testing.T) {
	client, ts := sdkClientAndServer(t)
	ctx := context.Background()
	agent := mustAgent(t, client, "opus", "sys")
	environment := mustEnv(t, ts.URL)
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent: anthropic.BetaSessionNewParamsAgentUnion{
			OfString: anthropic.String(agent.ID),
		},
		EnvironmentID: environment,
	})
	if err != nil {
		t.Fatalf("create Session: %v", err)
	}
	const threadID = "sthr_sdk_interrupt_target"
	sent, err := client.Beta.Sessions.Events.Send(
		ctx, session.ID, anthropic.BetaSessionEventSendParams{
			Events: []anthropic.BetaManagedAgentsEventParamsUnion{{
				OfUserInterrupt: &anthropic.BetaManagedAgentsUserInterruptEventParams{
					Type:            anthropic.BetaManagedAgentsUserInterruptEventParamsTypeUserInterrupt,
					SessionThreadID: param.NewOpt(threadID),
				},
			}},
		},
	)
	if err != nil {
		t.Fatalf("send targeted interrupt: %v", err)
	}
	if len(sent.Data) != 1 || sent.Data[0].Type != domain.EvUserInterrupt ||
		sent.Data[0].AsUserInterrupt().SessionThreadID != threadID {
		t.Fatalf("targeted interrupt response = %+v", sent.Data)
	}
}

func TestSDK_EventSendRichContentShapes(t *testing.T) {
	client, ts := sdkClientAndServer(t)
	ctx := context.Background()

	agent := mustAgent(t, client, "opus", "sys")
	env := mustEnv(t, ts.URL)
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: env,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	sent, err := client.Beta.Sessions.Events.Send(ctx, session.ID, anthropic.BetaSessionEventSendParams{
		Events: []anthropic.BetaManagedAgentsEventParamsUnion{{
			OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
				Type: anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage,
				Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{
					{
						OfImage: &anthropic.BetaManagedAgentsImageBlockParam{
							Type: anthropic.BetaManagedAgentsImageBlockTypeImage,
							Source: anthropic.BetaManagedAgentsImageBlockSourceUnionParam{
								OfURL: &anthropic.BetaManagedAgentsURLImageSourceParam{
									Type: anthropic.BetaManagedAgentsURLImageSourceTypeURL,
									URL:  "https://example.com/image.png",
								},
							},
						},
					},
					{
						OfDocument: &anthropic.BetaManagedAgentsDocumentBlockParam{
							Type:    anthropic.BetaManagedAgentsDocumentBlockTypeDocument,
							Title:   anthropic.String("Evidence"),
							Context: anthropic.String("Supporting material"),
							Source: anthropic.BetaManagedAgentsDocumentBlockSourceUnionParam{
								OfText: &anthropic.BetaManagedAgentsPlainTextDocumentSourceParam{
									Type:      anthropic.BetaManagedAgentsPlainTextDocumentSourceTypeText,
									MediaType: anthropic.BetaManagedAgentsPlainTextDocumentSourceMediaTypeTextPlain,
									Data:      "evidence",
								},
							},
						},
					},
					{
						OfDocument: &anthropic.BetaManagedAgentsDocumentBlockParam{
							Type: anthropic.BetaManagedAgentsDocumentBlockTypeDocument,
							Source: anthropic.BetaManagedAgentsDocumentBlockSourceUnionParam{
								OfFile: &anthropic.BetaManagedAgentsFileDocumentSourceParam{
									Type:   anthropic.BetaManagedAgentsFileDocumentSourceTypeFile,
									FileID: "file_uploaded_text",
								},
							},
						},
					},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("send rich event: %v", err)
	}
	if len(sent.Data) != 1 {
		t.Fatalf("send echoed %d events, want 1", len(sent.Data))
	}
	message := sent.Data[0].AsUserMessage()
	if len(message.Content) != 3 {
		t.Fatalf("echoed content = %+v", message.Content)
	}
	if got := message.Content[0].AsImage().Source.AsURL().URL; got != "https://example.com/image.png" {
		t.Fatalf("echoed image URL = %q", got)
	}
	document := message.Content[1].AsDocument()
	if document.Title != "Evidence" || document.Context != "Supporting material" ||
		document.Source.AsText().Data != "evidence" {
		t.Fatalf("echoed document = %+v", document)
	}
	if got := message.Content[2].AsDocument().Source.AsFile().FileID; got != "file_uploaded_text" {
		t.Fatalf("echoed File document ID = %q", got)
	}
}

func TestSDK_EventStream(t *testing.T) {
	client, ts := sdkClientAndServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	agent := mustAgent(t, client, "opus", "sys")
	env := mustEnv(t, ts.URL)
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: env,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	stream := client.Beta.Sessions.Events.StreamEvents(
		ctx, session.ID, anthropic.BetaSessionEventStreamParams{},
	)
	defer stream.Close()

	sent, err := client.Beta.Sessions.Events.Send(ctx, session.ID, anthropic.BetaSessionEventSendParams{
		Events: []anthropic.BetaManagedAgentsEventParamsUnion{{
			OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
				Type: anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage,
				Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{
					OfText: &anthropic.BetaManagedAgentsTextBlockParam{
						Type: anthropic.BetaManagedAgentsTextBlockTypeText,
						Text: "stream me",
					},
				}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("send event: %v", err)
	}
	if len(sent.Data) != 1 {
		t.Fatalf("sent event count = %d, want 1", len(sent.Data))
	}

	if !stream.Next() {
		t.Fatalf("official SDK returned no streamed event: %v", stream.Err())
	}
	got := stream.Current()
	if got.Type != "user.message" || got.ID != sent.Data[0].ID {
		t.Fatalf("streamed event = %s %s, want user.message %s", got.Type, got.ID, sent.Data[0].ID)
	}
}

func TestSDK_EventListPaginationAndTypesFilter(t *testing.T) {
	client, ts := sdkClientAndServer(t)
	ctx := context.Background()

	agent := mustAgent(t, client, "opus", "sys")
	env := mustEnv(t, ts.URL)
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agent.ID)},
		EnvironmentID: env,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Three user.message events, no runtime trigger side effects to reason about
	// (fake still runs, but we filter to user.message below).
	for i := 0; i < 3; i++ {
		if _, err := client.Beta.Sessions.Events.Send(ctx, session.ID, anthropic.BetaSessionEventSendParams{
			Events: []anthropic.BetaManagedAgentsEventParamsUnion{{
				OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
					Type: anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage,
					Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{
						OfText: &anthropic.BetaManagedAgentsTextBlockParam{
							Type: anthropic.BetaManagedAgentsTextBlockTypeText,
							Text: "msg",
						},
					}},
				},
			}},
		}); err != nil {
			t.Fatalf("send event %d: %v", i, err)
		}
	}

	// Filter to user.message and page with limit=2: first page has 2 + a cursor.
	first, err := client.Beta.Sessions.Events.List(ctx, session.ID, anthropic.BetaSessionEventListParams{
		Types: []string{"user.message"},
		Limit: anthropic.Int(2),
	})
	if err != nil {
		t.Fatalf("list page 1: %v", err)
	}
	if len(first.Data) != 2 {
		t.Fatalf("page 1 returned %d events, want 2", len(first.Data))
	}
	for _, ev := range first.Data {
		if ev.Type != "user.message" {
			t.Fatalf("types filter leaked %q", ev.Type)
		}
	}
	if first.NextPage == "" {
		t.Fatal("expected a next_page cursor on page 1")
	}

	// Second page: the remaining user.message.
	second, err := client.Beta.Sessions.Events.List(ctx, session.ID, anthropic.BetaSessionEventListParams{
		Types: []string{"user.message"},
		Limit: anthropic.Int(2),
		Page:  anthropic.String(first.NextPage),
	})
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	if len(second.Data) != 1 {
		t.Fatalf("page 2 returned %d events, want 1", len(second.Data))
	}
	if second.NextPage != "" {
		t.Fatalf("page 2 next_page = %q, want empty (last page)", second.NextPage)
	}
	// No overlap between pages.
	if first.Data[0].ID == second.Data[0].ID || first.Data[1].ID == second.Data[0].ID {
		t.Fatal("page 2 overlaps page 1")
	}
}

func TestSDK_AgentListParamsAndPaging(t *testing.T) {
	client, _ := sdkClientAndServer(t)
	ctx := context.Background()
	created := map[string]bool{}
	for range 3 {
		agent := mustAgent(t, client, "opus", "system")
		created[agent.ID] = true
	}

	first, err := client.Beta.Agents.List(ctx, anthropic.BetaAgentListParams{
		Limit:           anthropic.Int(2),
		IncludeArchived: anthropic.Bool(false),
		CreatedAtGte:    anthropic.Time(time.Unix(0, 0).UTC()),
		CreatedAtLte:    anthropic.Time(time.Unix(1<<31, 0).UTC()),
	})
	if err != nil {
		t.Fatalf("list agents page 1: %v", err)
	}
	if len(first.Data) != 2 || first.NextPage == "" {
		t.Fatalf("page 1 = %d agents, next_page %q", len(first.Data), first.NextPage)
	}
	second, err := first.GetNextPage()
	if err != nil {
		t.Fatalf("follow agent next_page: %v", err)
	}
	if second == nil || len(second.Data) != 1 || second.NextPage != "" {
		t.Fatalf("page 2 = %+v, want one terminal row", second)
	}
	for _, agent := range append(first.Data, second.Data...) {
		if !created[agent.ID] {
			t.Fatalf("unexpected agent %s", agent.ID)
		}
		delete(created, agent.ID)
	}
	if len(created) != 0 {
		t.Fatalf("agents missing from paged SDK result: %v", created)
	}
	if _, err := client.Beta.Agents.List(ctx, anthropic.BetaAgentListParams{
		Limit: anthropic.Int(101),
	}); err == nil {
		t.Fatal("limit=101 was accepted")
	} else {
		assertAPIStatus(t, err, 400)
	}
}

func TestSDK_AgentVersionListParamsAndPaging(t *testing.T) {
	client, _ := sdkClientAndServer(t)
	ctx := context.Background()
	agent := mustAgent(t, client, "opus", "system")
	for _, name := range []string{"Agent v2", "Agent v3"} {
		updated, err := client.Beta.Agents.Update(ctx, agent.ID, anthropic.BetaAgentUpdateParams{
			Name: anthropic.String(name), Version: anthropic.Int(agent.Version),
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		agent = updated
	}

	first, err := client.Beta.Agents.Versions.List(
		ctx, agent.ID, anthropic.BetaAgentVersionListParams{Limit: anthropic.Int(2)},
	)
	if err != nil {
		t.Fatalf("list Agent versions page 1: %v", err)
	}
	if len(first.Data) != 2 || first.Data[0].Version != 1 ||
		first.Data[1].Version != 2 || first.NextPage == "" {
		t.Fatalf("page 1 = %+v, want versions 1,2 and next_page", first)
	}
	second, err := first.GetNextPage()
	if err != nil {
		t.Fatalf("follow Agent versions next_page: %v", err)
	}
	if second == nil || len(second.Data) != 1 || second.Data[0].Version != 3 ||
		second.NextPage != "" {
		t.Fatalf("page 2 = %+v, want terminal version 3", second)
	}
	if _, err := client.Beta.Agents.Versions.List(
		ctx, agent.ID, anthropic.BetaAgentVersionListParams{Limit: anthropic.Int(101)},
	); err == nil {
		t.Fatal("Agent Versions limit=101 was accepted")
	} else {
		assertAPIStatus(t, err, 400)
	}
}

func TestSDK_EnvironmentLifecycle(t *testing.T) {
	client, _ := sdkClientAndServer(t)
	ctx := context.Background()

	environment, err := client.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
		Name:        "SDK environment",
		Description: anthropic.String("created through the official SDK"),
		Metadata:    map[string]string{"team": "platform"},
	})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	if environment.ID == "" || environment.Type != "environment" ||
		environment.Name != "SDK environment" ||
		environment.Description != "created through the official SDK" ||
		environment.Metadata["team"] != "platform" ||
		environment.Config.Type != "cloud" || environment.Config.Networking.Type != "unrestricted" {
		t.Fatalf("created environment = %#v", environment)
	}
	assertRawObjectHasFields(t, environment.RawJSON(),
		"id", "archived_at", "config", "created_at", "description", "metadata", "name", "type", "updated_at")
	assertRawObjectHasFields(t, environment.Config.RawJSON(), "type", "networking", "packages")
	assertRawObjectHasFields(t, environment.Config.Packages.RawJSON(),
		"type", "apt", "cargo", "gem", "go", "npm", "pip")

	got, err := client.Beta.Environments.Get(ctx, environment.ID, anthropic.BetaEnvironmentGetParams{})
	if err != nil {
		t.Fatalf("get environment: %v", err)
	}
	if got.ID != environment.ID || got.Description != environment.Description ||
		got.Metadata["team"] != "platform" || got.Config.Networking.Type != "unrestricted" {
		t.Fatalf("retrieved environment = %#v", got)
	}

	archived, err := client.Beta.Environments.Archive(
		ctx, environment.ID, anthropic.BetaEnvironmentArchiveParams{},
	)
	if err != nil {
		t.Fatalf("archive environment: %v", err)
	}
	if archived.ArchivedAt == "" || archived.Description != environment.Description ||
		archived.Metadata["team"] != "platform" {
		t.Fatalf("archived environment = %#v", archived)
	}

	deleted, err := client.Beta.Environments.Delete(
		ctx, environment.ID, anthropic.BetaEnvironmentDeleteParams{},
	)
	if err != nil {
		t.Fatalf("delete environment: %v", err)
	}
	if deleted.ID != environment.ID ||
		deleted.Type != anthropic.BetaEnvironmentDeleteResponseTypeEnvironmentDeleted {
		t.Fatalf("delete response = %#v", deleted)
	}
}

func TestSDK_EnvironmentExplicitCloudDefaults(t *testing.T) {
	client, _ := sdkClientAndServer(t)
	ctx := context.Background()
	networking := anthropic.NewBetaUnrestrictedNetworkParam()
	cloud := anthropic.BetaCloudConfigParams{
		Networking: anthropic.BetaCloudConfigParamsNetworkingUnion{
			OfUnrestricted: &networking,
		},
		Packages: anthropic.BetaPackagesParams{
			Type: anthropic.BetaPackagesParamsTypePackages,
		},
	}

	environment, err := client.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
		Name: "Explicit cloud defaults",
		Config: anthropic.BetaEnvironmentNewParamsConfigUnion{
			OfCloud: &cloud,
		},
	})
	if err != nil {
		t.Fatalf("create environment with explicit defaults: %v", err)
	}
	if environment.Config.Networking.Type != "unrestricted" ||
		len(environment.Config.Packages.Pip) != 0 {
		t.Fatalf("created environment config = %#v", environment.Config)
	}

	updated, err := client.Beta.Environments.Update(
		ctx,
		environment.ID,
		anthropic.BetaEnvironmentUpdateParams{
			Config: anthropic.BetaEnvironmentUpdateParamsConfigUnion{
				OfCloud: &cloud,
			},
		},
	)
	if err != nil {
		t.Fatalf("update environment with explicit defaults: %v", err)
	}
	if updated.Config.Networking.Type != "unrestricted" ||
		len(updated.Config.Packages.Pip) != 0 {
		t.Fatalf("updated environment config = %#v", updated.Config)
	}
}

func TestSDK_EnvironmentPackagesRoundTrip(t *testing.T) {
	client, _ := sdkClientAndServer(t)
	ctx := context.Background()
	cloud := anthropic.BetaCloudConfigParams{
		Packages: anthropic.BetaPackagesParams{
			Apt:  []string{"git"},
			Npm:  []string{"typescript@5.9.2"},
			Pip:  []string{"httpx==0.28.1"},
			Type: anthropic.BetaPackagesParamsTypePackages,
		},
	}

	environment, err := client.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
		Name: "SDK package environment",
		Config: anthropic.BetaEnvironmentNewParamsConfigUnion{
			OfCloud: &cloud,
		},
	})
	if err != nil {
		t.Fatalf("create package environment: %v", err)
	}
	if len(environment.Config.Packages.Apt) != 1 || environment.Config.Packages.Apt[0] != "git" ||
		len(environment.Config.Packages.Npm) != 1 || environment.Config.Packages.Npm[0] != "typescript@5.9.2" ||
		len(environment.Config.Packages.Pip) != 1 || environment.Config.Packages.Pip[0] != "httpx==0.28.1" {
		t.Fatalf("created packages = %#v", environment.Config.Packages)
	}
}

func TestSDK_EnvironmentLimitedNetworkingRoundTrip(t *testing.T) {
	client, _ := sdkClientAndServer(t)
	ctx := context.Background()
	limited := anthropic.BetaLimitedNetworkParams{
		AllowMCPServers:      anthropic.Bool(true),
		AllowPackageManagers: anthropic.Bool(false),
		AllowedHosts:         []string{"api.example.com", "*.assets.example.com"},
	}
	cloud := anthropic.BetaCloudConfigParams{
		Networking: anthropic.BetaCloudConfigParamsNetworkingUnion{OfLimited: &limited},
	}

	environment, err := client.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
		Name: "SDK limited network environment",
		Config: anthropic.BetaEnvironmentNewParamsConfigUnion{
			OfCloud: &cloud,
		},
	})
	if err != nil {
		t.Fatalf("create limited network environment: %v", err)
	}
	networking := environment.Config.Networking.AsLimited()
	if networking.Type != "limited" || !networking.AllowMCPServers ||
		networking.AllowPackageManagers || len(networking.AllowedHosts) != 2 ||
		networking.AllowedHosts[0] != "api.example.com" {
		t.Fatalf("created limited networking = %#v", networking)
	}
	assertRawObjectHasFields(t, networking.RawJSON(),
		"type", "allow_mcp_servers", "allow_package_managers", "allowed_hosts")

	patch := anthropic.BetaLimitedNetworkParams{AllowedHosts: []string{"next.example.com"}}
	cloud.Networking = anthropic.BetaCloudConfigParamsNetworkingUnion{OfLimited: &patch}
	updated, err := client.Beta.Environments.Update(
		ctx,
		environment.ID,
		anthropic.BetaEnvironmentUpdateParams{
			Config: anthropic.BetaEnvironmentUpdateParamsConfigUnion{OfCloud: &cloud},
		},
	)
	if err != nil {
		t.Fatalf("update limited network environment: %v", err)
	}
	networking = updated.Config.Networking.AsLimited()
	if !networking.AllowMCPServers || len(networking.AllowedHosts) != 1 ||
		networking.AllowedHosts[0] != "next.example.com" {
		t.Fatalf("updated limited networking = %#v", networking)
	}
}

func TestSDK_SelfHostedEnvironmentScope(t *testing.T) {
	client, _ := sdkClientAndServer(t)
	ctx := context.Background()
	config := anthropic.NewBetaSelfHostedConfigParams()

	environment, err := client.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
		Name:        "Self-hosted SDK environment",
		Description: anthropic.String("before update"),
		Metadata:    map[string]string{"keep": "old", "drop": "value"},
		Config: anthropic.BetaEnvironmentNewParamsConfigUnion{
			OfSelfHosted: &config,
		},
		Scope: anthropic.BetaEnvironmentNewParamsScopeAccount,
	})
	if err != nil {
		t.Fatalf("create self-hosted environment: %v", err)
	}
	if environment.Config.Type != "self_hosted" ||
		environment.Scope != anthropic.BetaEnvironmentScopeAccount {
		t.Fatalf("self-hosted environment = %#v", environment)
	}
	assertRawObjectHasFields(t, environment.RawJSON(), "scope")

	updated, err := client.Beta.Environments.Update(
		ctx,
		environment.ID,
		anthropic.BetaEnvironmentUpdateParams{
			Name:        anthropic.String("Updated SDK environment"),
			Description: anthropic.String("after update"),
			Metadata:    map[string]string{"keep": "updated", "drop": ""},
			Scope:       anthropic.BetaEnvironmentUpdateParamsScopeOrganization,
			Config: anthropic.BetaEnvironmentUpdateParamsConfigUnion{
				OfSelfHosted: &config,
			},
		},
	)
	if err != nil {
		t.Fatalf("update self-hosted environment: %v", err)
	}
	if updated.Name != "Updated SDK environment" || updated.Description != "after update" ||
		updated.Scope != anthropic.BetaEnvironmentScopeOrganization ||
		len(updated.Metadata) != 1 || updated.Metadata["keep"] != "updated" {
		t.Fatalf("updated self-hosted environment = %#v", updated)
	}

	got, err := client.Beta.Environments.Get(ctx, environment.ID, anthropic.BetaEnvironmentGetParams{})
	if err != nil {
		t.Fatalf("get self-hosted environment: %v", err)
	}
	if got.Config.Type != "self_hosted" || got.Name != updated.Name ||
		got.Scope != anthropic.BetaEnvironmentScopeOrganization || got.Metadata["keep"] != "updated" {
		t.Fatalf("retrieved self-hosted environment = %#v", got)
	}
}

func TestSDK_EnvironmentListParamsAndPaging(t *testing.T) {
	client, server := sdkClientAndServer(t)
	ctx := context.Background()
	created := map[string]bool{}
	for range 3 {
		id := mustEnv(t, server.URL)
		created[id] = true
	}

	first, err := client.Beta.Environments.List(ctx, anthropic.BetaEnvironmentListParams{
		Limit:           anthropic.Int(2),
		IncludeArchived: anthropic.Bool(false),
	})
	if err != nil {
		t.Fatalf("list environments page 1: %v", err)
	}
	if len(first.Data) != 2 || first.NextPage == "" {
		t.Fatalf("page 1 = %d environments, next_page %q", len(first.Data), first.NextPage)
	}
	second, err := first.GetNextPage()
	if err != nil {
		t.Fatalf("follow environment next_page: %v", err)
	}
	if second == nil || len(second.Data) != 1 || second.NextPage != "" {
		t.Fatalf("page 2 = %+v, want one terminal row", second)
	}
	for _, environment := range append(first.Data, second.Data...) {
		if !created[environment.ID] {
			t.Fatalf("unexpected environment %s", environment.ID)
		}
		delete(created, environment.ID)
	}
	if len(created) != 0 {
		t.Fatalf("environments missing from paged SDK result: %v", created)
	}
}

func mustAgent(t *testing.T, client anthropic.Client, _ string, system string) *anthropic.BetaManagedAgentsAgent {
	t.Helper()
	agent, err := client.Beta.Agents.New(context.Background(), anthropic.BetaAgentNewParams{
		Name: "Agent",
		Model: anthropic.BetaManagedAgentsModelConfigParams{
			ID: anthropic.BetaManagedAgentsModelClaudeOpus4_8,
		},
		System: anthropic.String(system),
	})
	if err != nil {
		t.Fatalf("mustAgent: %v", err)
	}
	return agent
}
