package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	mango "github.com/yanpgwang/mango/sdk/go"
)

func TestMangoSDKSessionThreadSurface(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC)
	service := &sdkThreadService{thread: domain.SessionThread{
		ID: "sthr_primary", SessionID: "sesn_thread_sdk",
		Agent: domain.Agent{
			ID: "agent_thread_sdk", Version: 3, Name: "Coordinator",
			Model: domain.NormalizeModel(domain.Model{ID: "claude-opus-4-8"}),
			Tools: []any{}, MCPServers: []any{}, Skills: []domain.SkillReference{},
		},
		Status: domain.StatusIdle, CreatedAt: now, UpdatedAt: now,
	}}
	service.next = service.thread
	service.next.ID = "sthr_child_fixture"
	service.next.ParentThreadID = &service.thread.ID
	service.next.CreatedAt = now.Add(time.Second)
	service.next.UpdatedAt = service.next.CreatedAt
	event := domain.Event{
		ID: "sevt_thread_sdk", SessionID: service.thread.SessionID,
		ThreadID: service.thread.ID, Sequence: 1,
		Type: domain.EvSessionThreadCreated,
		Payload: map[string]any{
			"agent_name": "reviewer", "session_thread_id": service.next.ID,
		},
		CreatedAt: now, ProcessedAt: &now,
	}
	nextEvent := event
	nextEvent.ID = "sevt_thread_sdk_next"
	nextEvent.Sequence = 2
	nextEvent.CreatedAt = now.Add(time.Second)
	nextEvent.ProcessedAt = &nextEvent.CreatedAt
	childEvent := event
	childEvent.ID = "sevt_child_thread_sdk"
	childEvent.ThreadID = service.next.ID
	childEvent.Sequence = 3
	childEvent.Type = domain.EvAgentThreadContextCompacted
	childEvent.Payload = map[string]any{}
	server := httptest.NewServer(NewServer(Deps{
		Sessions: &testSessionService{sessions: map[string]domain.Session{
			service.thread.SessionID: {ID: service.thread.SessionID},
		}},
		Threads: service,
		Events:  &sdkThreadEvents{event: event, next: nextEvent, child: childEvent},
		Stream:  &sdkThreadStream{event: event},
	}, Config{RequireAuth: true}).Handler())
	t.Cleanup(server.Close)
	client, responseJSON := recordedSDKClient(t, server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	thread, err := client.Sessions.Threads.Get(ctx, service.thread.SessionID, service.thread.ID)
	if err != nil || thread.ID != service.thread.ID || thread.ParentThreadID != nil ||
		thread.Agent.ManagedAgentThreadAgent.ID != service.thread.Agent.ID || thread.Agent.ManagedAgentThreadAgent.Version != 3 ||
		thread.Status != "idle" ||
		thread.Type != "session_thread" {
		t.Fatalf("Get Session Thread = %+v, err=%v", thread, err)
	}
	assertRawObjectHasFields(t, responseJSON(),
		"id", "agent", "archived_at", "created_at", "parent_thread_id",
		"session_id", "stats", "status", "type", "updated_at", "usage",
	)
	assertRawObjectHasFields(t, rawJSONField(t, responseJSON(), "usage"),
		"active_seconds", "cache_creation", "cache_read_input_tokens", "input_tokens",
		"list_cost", "output_tokens", "server_tool_use")
	if strings.Contains(rawJSONField(t, responseJSON(), "agent"), `"multiagent"`) {
		t.Fatal("thread agent repeated the coordinator multiagent roster")
	}

	page, err := client.Sessions.Threads.List(ctx, service.thread.SessionID,
		mango.ListSessionThreadsParams{Limit: mango.Some(int64(1))})
	if err != nil || len(page.Data) != 1 || page.Data[0].ID != service.thread.ID {
		t.Fatalf("List Session Threads = %+v, err=%v", page, err)
	}
	nextPage, err := client.Sessions.Threads.List(ctx, service.thread.SessionID, mango.ListSessionThreadsParams{Limit: mango.Some[int64](1), Page: mango.Some(*page.NextPage)})
	if err != nil || len(nextPage.Data) != 1 || nextPage.Data[0].ID != service.next.ID {
		t.Fatalf("List next Session Threads page = %+v, err=%v", nextPage, err)
	}
	childEvents, err := client.Sessions.Threads.Events.List(ctx, service.thread.SessionID, service.next.ID, mango.ListSessionThreadEventsParams{})
	if err != nil || len(childEvents.Data) != 1 || childEvents.Data[0].AgentThreadContextCompactedEvent == nil ||
		childEvents.Data[0].AgentThreadContextCompactedEvent.ID != childEvent.ID {
		t.Fatalf("List child Session Thread Events = %+v, err=%v", childEvents, err)
	}

	events, err := client.Sessions.Threads.Events.List(ctx, service.thread.SessionID, service.thread.ID, mango.ListSessionThreadEventsParams{Limit: mango.Some(int64(1))})
	if err != nil || len(events.Data) != 1 ||
		events.Data[0].SessionThreadCreatedEvent == nil ||
		events.Data[0].SessionThreadCreatedEvent.SessionThreadID != service.next.ID {
		t.Fatalf("List Session Thread Events = %+v, err=%v", events, err)
	}
	nextEvents, err := client.Sessions.Threads.Events.List(ctx, service.thread.SessionID, service.thread.ID, mango.ListSessionThreadEventsParams{Limit: mango.Some[int64](1), Page: mango.Some(*events.NextPage)})
	if err != nil || len(nextEvents.Data) != 1 || nextEvents.Data[0].SessionThreadCreatedEvent == nil || nextEvents.Data[0].SessionThreadCreatedEvent.ID != nextEvent.ID {
		t.Fatalf("List next Session Thread Events page = %+v, err=%v", nextEvents, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		server.URL+"/v1/sessions/"+service.thread.SessionID+"/events?order=asc&page="+
			url.QueryEscape(*events.NextPage), nil)
	if err != nil {
		t.Fatalf("build cross-resource cursor request: %v", err)
	}
	request.Header.Set("authorization", "Bearer test-key")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("cross-resource cursor request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("Thread cursor on Session Event list status = %d, want 400", response.StatusCode)
	}

	stream, err := client.Sessions.Threads.Events.Stream(ctx, service.thread.SessionID, service.thread.ID, mango.StreamSessionThreadEventsParams{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if !stream.Next() {
		t.Fatalf("Stream Session Thread Events yielded no event: %v", stream.Err())
	}
	var streamed mango.SessionEvent
	if err := stream.Event().Decode(&streamed); err != nil {
		t.Fatal(err)
	}
	if got := streamed.SessionThreadCreatedEvent; got == nil || got.ID != event.ID || got.Type != domain.EvSessionThreadCreated || got.AgentName != "reviewer" {
		t.Fatalf("streamed event = %+v", streamed)
	}

	archived, err := client.Sessions.Threads.Archive(ctx, service.thread.SessionID, service.thread.ID)
	if err != nil || archived.ArchivedAt == nil ||
		archived.Status != "terminated" {
		t.Fatalf("Archive Session Thread = %+v, err=%v", archived, err)
	}
}

func TestMangoSDKAdvisorSessionThreadAgentUnion(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 9, 9, 0, 0, 0, time.UTC)
	primaryID := "sthr_primary_advisor_sdk"
	advisor := domain.Advisor{Type: "advisor", Model: "claude-opus-5"}
	thread := domain.NewAdvisorSessionThread(
		"sthr_advisor_sdk", "sesn_advisor_sdk", primaryID, advisor,
		domain.TokenUsage{InputTokens: 10, OutputTokens: 2}, 100, true, now,
	)
	adviceEvent := domain.Event{
		ID: "sevt_advisor_advice_sdk", SessionID: thread.SessionID,
		ThreadID: thread.ID, Sequence: 1, Type: domain.EvAgentThreadMessageSent,
		Payload: map[string]any{
			"to_session_thread_id": primaryID,
			"to_agent_name":        "coordinator",
			"content": []any{map[string]any{
				"type": "text", "text": "check the shutdown race",
			}},
		},
		CreatedAt: now, ProcessedAt: &now,
	}
	service := &sdkThreadService{thread: thread, next: thread}
	server := httptest.NewServer(NewServer(Deps{
		Sessions: &testSessionService{sessions: map[string]domain.Session{
			thread.SessionID: {ID: thread.SessionID},
		}},
		Threads: service,
		Events: &sdkThreadEvents{
			event: adviceEvent, next: adviceEvent, child: adviceEvent,
		},
	}, Config{RequireAuth: true}).Handler())
	t.Cleanup(server.Close)
	client, responseJSON := recordedSDKClient(t, server.URL)
	got, err := client.Sessions.Threads.Get(
		context.Background(), thread.SessionID, thread.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	resolved := got.Agent.MultiagentAdvisor
	if resolved == nil || resolved.Model != "claude-opus-5" ||
		resolved.Type != "advisor" ||
		got.ParentThreadID == nil || *got.ParentThreadID != primaryID ||
		got.Status != "terminated" {
		t.Fatalf("Advisor Session Thread = %s", responseJSON())
	}
	if strings.Contains(rawJSONField(t, responseJSON(), "agent"), `"id"`) ||
		strings.Contains(rawJSONField(t, responseJSON(), "agent"), `"name"`) {
		t.Fatalf("Advisor union leaked Agent fields: %s", rawJSONField(t, responseJSON(), "agent"))
	}
	events, err := client.Sessions.Threads.Events.List(
		context.Background(), thread.SessionID, thread.ID, mango.ListSessionThreadEventsParams{},
	)
	if err != nil || len(events.Data) != 1 {
		t.Fatalf("Advisor Thread Events = %+v, err=%v", events, err)
	}
	sent := events.Data[0].AgentThreadMessageSentEvent
	if sent == nil || sent.ToSessionThreadID != primaryID || len(sent.Content) != 1 ||
		sent.Content[0].TextBlockInput.Text != "check the shutdown race" {
		t.Fatalf("Advisor advice Event = %s", responseJSON())
	}
}

type sdkThreadService struct {
	thread domain.SessionThread
	next   domain.SessionThread
}

func (s *sdkThreadService) Get(
	_ context.Context, sessionID, threadID string,
) (domain.SessionThread, error) {
	if sessionID != s.thread.SessionID {
		return domain.SessionThread{}, domain.NotFound("session thread not found")
	}
	switch threadID {
	case s.thread.ID:
		return s.thread, nil
	case s.next.ID:
		return s.next, nil
	default:
		return domain.SessionThread{}, domain.NotFound("session thread not found")
	}
}

func (s *sdkThreadService) List(
	_ context.Context, sessionID string, query app.SessionThreadListQuery,
) ([]domain.SessionThread, error) {
	if sessionID != s.thread.SessionID {
		return nil, domain.NotFound("session not found")
	}
	if query.Boundary != nil {
		return []domain.SessionThread{s.next}, nil
	}
	return []domain.SessionThread{s.thread, s.next}, nil
}

func (s *sdkThreadService) Archive(
	_ context.Context, sessionID, threadID string,
) (domain.SessionThread, error) {
	if _, err := s.Get(context.Background(), sessionID, threadID); err != nil {
		return domain.SessionThread{}, err
	}
	now := s.thread.UpdatedAt.Add(time.Second)
	s.thread.ArchivedAt = &now
	s.thread.TerminatedAt = &now
	s.thread.UpdatedAt = now
	s.thread.Status = domain.StatusTerminated
	return s.thread, nil
}

type sdkThreadEvents struct {
	event domain.Event
	next  domain.Event
	child domain.Event
}

func (s *sdkThreadEvents) Query(
	_ context.Context, sessionID string, query app.EventQuery,
) ([]domain.Event, error) {
	if sessionID != s.event.SessionID {
		return nil, domain.NotFound("session not found")
	}
	if query.ThreadID == s.child.ThreadID {
		return []domain.Event{s.child}, nil
	}
	if query.Boundary != nil {
		return []domain.Event{s.next}, nil
	}
	return []domain.Event{s.event, s.next}, nil
}

type sdkThreadStream struct{ event domain.Event }

func (s *sdkThreadStream) SubscribeContext(
	_ context.Context, sessionID string, _ map[string]bool,
) (<-chan app.Frame, func(), error) {
	if sessionID != s.event.SessionID {
		return nil, nil, domain.NotFound("session not found")
	}
	frames := make(chan app.Frame, 1)
	frames <- app.Frame{Event: &s.event}
	close(frames)
	return frames, func() {}, nil
}

func (s *sdkThreadStream) SubscribeThreadContext(
	ctx context.Context,
	sessionID string,
	_ string,
	deltaOptIn map[string]bool,
) (<-chan app.Frame, func(), error) {
	return s.SubscribeContext(ctx, sessionID, deltaOptIn)
}
