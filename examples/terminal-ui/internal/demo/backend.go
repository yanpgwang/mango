// Package demo provides an in-memory Mango ledger for exploring the TUI
// without a running control plane. It implements the same Backend contract.
package demo

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yanpgwang/mango/examples/terminal-ui/internal/mango"
)

type Backend struct {
	mu             sync.Mutex
	session        mango.Session
	threads        []mango.Thread
	events         map[string][]mango.Event
	agents         []mango.Agent
	environments   []mango.Environment
	created        map[string]mango.Session
	createdTrees   map[string][]mango.Thread
	createdLogs    map[string]map[string][]mango.Event
	titles         map[string]string
	status         map[string]string
	archived       map[string]time.Time
	deleted        map[string]bool
	subscribers    map[int]chan mango.StreamUpdate
	nextSubscriber int
	nextID         int
}

func New() *Backend {
	now := time.Now().Add(-9 * time.Minute).UTC()
	session := mango.Session{
		ID: "sesn_demo_product_launch", Title: "Ship the managed Agent launch", Status: "idle",
		CreatedAt: now, UpdatedAt: time.Now().UTC().Add(-4 * time.Minute), EnvironmentID: "env_demo_cloud",
	}
	session.Agent.ID = "agent_demo_coordinator"
	session.Agent.Name = "coordinator"
	session.Agent.Version = 1
	session.Agent.Model.ID = "claude-sonnet-4-5"
	session.Usage.InputTokens = 18420
	session.Usage.OutputTokens = 3286
	session.Stats.ActiveSeconds = 147.8
	primary := thread("sthr_demo_primary", "", "coordinator", "idle")
	researcher := thread("sthr_demo_researcher", primary.ID, "researcher", "idle")
	reviewer := thread("sthr_demo_reviewer", primary.ID, "reviewer", "idle")
	researcher.Stats.ActiveSeconds = 31.4
	researcher.Usage.InputTokens, researcher.Usage.OutputTokens = 5720, 684
	reviewer.Stats.ActiveSeconds = 78.2
	reviewer.Usage.InputTokens, reviewer.Usage.OutputTokens = 9040, 1130
	session.Agent.Multiagent = &mango.Multiagent{Type: "supervisor", Agents: []mango.AgentReference{
		{Type: "agent", ID: researcher.Agent.ID, Version: 1, Name: researcher.Agent.Name},
		{Type: "agent", ID: reviewer.Agent.ID, Version: 1, Name: reviewer.Agent.Name},
		{Type: "agent", ID: "agent_demo_writer", Version: 1, Name: "writer"},
	}}
	timeText := func(offset time.Duration) string { return now.Add(offset).Format(time.RFC3339Nano) }
	text := func(value string) []any { return []any{map[string]any{"type": "text", "text": value}} }
	events := map[string][]mango.Event{
		primary.ID: {
			{"id": "sevt_01", "type": "user.message", "content": text("Review the launch plan, verify the risky assumptions, and give me a concrete go/no-go recommendation."), "processed_at": timeText(time.Minute)},
			{"id": "sevt_02", "type": "agent.message", "content": text("I’ll split this into evidence gathering and an independent risk review, then synthesize the decision."), "processed_at": timeText(2 * time.Minute)},
			{"id": "sevt_03", "type": "session.thread_created", "agent_name": "researcher", "session_thread_id": researcher.ID, "processed_at": timeText(2*time.Minute + 5*time.Second)},
			{"id": "sevt_04", "type": "agent.thread_message_sent", "to_agent_name": "researcher", "to_session_thread_id": researcher.ID, "content": text("Validate adoption metrics and identify unsupported claims."), "processed_at": timeText(2*time.Minute + 7*time.Second)},
			{"id": "sevt_05", "type": "session.thread_created", "agent_name": "reviewer", "session_thread_id": reviewer.ID, "processed_at": timeText(2*time.Minute + 10*time.Second)},
			{"id": "sevt_06", "type": "agent.thread_message_sent", "to_agent_name": "reviewer", "to_session_thread_id": reviewer.ID, "content": text("Stress-test rollback readiness and operating risks."), "processed_at": timeText(2*time.Minute + 12*time.Second)},
			{"id": "sevt_07", "type": "agent.thread_message_received", "from_agent_name": "researcher", "from_session_thread_id": researcher.ID, "content": text("Adoption is above the launch threshold in 4/5 cohorts. The enterprise cohort has only seven days of data, so the long-term retention claim should be softened."), "processed_at": timeText(5 * time.Minute)},
			{"id": "sevt_08", "type": "agent.tool_use", "name": "bash", "input": map[string]any{"command": "./scripts/verify-rollback.sh --production"}, "evaluated_permission": "ask", "session_thread_id": reviewer.ID, "processed_at": timeText(6 * time.Minute)},
			{"id": "sevt_09", "type": "session.status_idle", "stop_reason": map[string]any{"type": "requires_action", "event_ids": []any{"sevt_08"}}, "processed_at": timeText(6*time.Minute + time.Second)},
		},
		researcher.ID: {
			{"id": "sevt_r1", "type": "agent.thread_message_received", "from_agent_name": "coordinator", "from_session_thread_id": primary.ID, "content": text("Validate adoption metrics and identify unsupported claims."), "processed_at": timeText(2*time.Minute + 7*time.Second)},
			{"id": "sevt_r2", "type": "agent.tool_use", "name": "read", "input": map[string]any{"path": "reports/adoption.csv"}, "evaluated_permission": "allow", "processed_at": timeText(3 * time.Minute)},
			{"id": "sevt_r3", "type": "agent.tool_result", "tool_use_id": "sevt_r2", "content": text("5 cohorts, 4 above threshold"), "processed_at": timeText(3*time.Minute + 3*time.Second)},
			{"id": "sevt_r4", "type": "agent.message", "content": text("The launch threshold is met, but the enterprise retention claim outruns the available observation window."), "processed_at": timeText(4 * time.Minute)},
			{"id": "sevt_r5", "type": "agent.thread_message_sent", "to_agent_name": "coordinator", "to_session_thread_id": primary.ID, "content": text("Adoption is above threshold in 4/5 cohorts; soften the enterprise retention claim."), "processed_at": timeText(5 * time.Minute)},
		},
		reviewer.ID: {
			{"id": "sevt_v1", "type": "agent.thread_message_received", "from_agent_name": "coordinator", "from_session_thread_id": primary.ID, "content": text("Stress-test rollback readiness and operating risks."), "processed_at": timeText(2*time.Minute + 12*time.Second)},
			{"id": "sevt_v2", "type": "agent.tool_use", "name": "bash", "input": map[string]any{"command": "./scripts/verify-rollback.sh --production"}, "evaluated_permission": "ask", "processed_at": timeText(6 * time.Minute)},
			{"id": "sevt_v3", "type": "session.thread_status_idle", "stop_reason": map[string]any{"type": "requires_action", "event_ids": []any{"sevt_v2"}}, "processed_at": timeText(6*time.Minute + time.Second)},
		},
	}
	environment := mango.Environment{ID: "env_demo_cloud", Name: "Mango cloud"}
	environment.Config.Type = "cloud"
	return &Backend{
		session: session, threads: []mango.Thread{primary, researcher, reviewer}, events: events,
		agents: []mango.Agent{session.Agent}, environments: []mango.Environment{environment},
		created: map[string]mango.Session{}, createdTrees: map[string][]mango.Thread{},
		createdLogs: map[string]map[string][]mango.Event{},
		titles:      map[string]string{}, status: map[string]string{},
		archived: map[string]time.Time{}, deleted: map[string]bool{},
		subscribers: make(map[int]chan mango.StreamUpdate), nextID: 100,
	}
}

func thread(id, parentID, name, status string) mango.Thread {
	thread := mango.Thread{ID: id, SessionID: "sesn_demo_product_launch", Status: status}
	if parentID != "" {
		thread.ParentThreadID = &parentID
	}
	thread.Agent.Name = name
	thread.Agent.ID = "agent_demo_" + name
	thread.Agent.Version = 1
	thread.Agent.Model.ID = "claude-sonnet-4-5"
	return thread
}

func (b *Backend) ListSessions(context.Context) ([]mango.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := []string{b.session.ID, "sesn_demo_incident_review"}
	for id := range b.created {
		ids = append(ids, id)
	}
	sessions := make([]mango.Session, 0, len(ids))
	for _, id := range ids {
		if session, ok := b.sessionLocked(id); ok {
			sessions = append(sessions, session)
		}
	}
	return sessions, nil
}

func (b *Backend) ListAgents(context.Context) ([]mango.Agent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]mango.Agent(nil), b.agents...), nil
}

func (b *Backend) ListEnvironments(context.Context) ([]mango.Environment, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]mango.Environment(nil), b.environments...), nil
}

func (b *Backend) CreateAgent(_ context.Context, input mango.CreateAgentInput) (mango.Agent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	agent := mango.Agent{
		ID: fmt.Sprintf("agent_demo_%d", b.nextID), Name: input.Name, System: input.System, Version: 1,
	}
	agent.Model.ID = input.Model
	agent.Tools = append([]map[string]any(nil), input.Tools...)
	agent.MCPServers = append([]map[string]any(nil), input.MCPServers...)
	if input.Multiagent != nil {
		agent.Multiagent = &mango.Multiagent{Type: input.Multiagent.Type}
		for _, id := range input.Multiagent.Agents {
			agent.Multiagent.Agents = append(agent.Multiagent.Agents, mango.AgentReference{Type: "agent", ID: id, Version: 1})
		}
	}
	b.agents = append(b.agents, agent)
	return agent, nil
}

func (b *Backend) CreateEnvironment(_ context.Context, input mango.CreateEnvironmentInput) (mango.Environment, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	environment := mango.Environment{
		ID: fmt.Sprintf("env_demo_%d", b.nextID), Name: input.Name, Description: input.Description,
	}
	environment.Config.Type = "cloud"
	b.environments = append(b.environments, environment)
	return environment, nil
}

func (b *Backend) CreateSession(_ context.Context, input mango.CreateSessionInput) (mango.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var selected mango.Agent
	for _, agent := range b.agents {
		if agent.ID == input.AgentID {
			selected = agent
			break
		}
	}
	if selected.ID == "" {
		return mango.Session{}, fmt.Errorf("demo Agent %s not found", input.AgentID)
	}
	validEnvironment := false
	for _, environment := range b.environments {
		if environment.ID == input.EnvironmentID {
			validEnvironment = true
			break
		}
	}
	if !validEnvironment {
		return mango.Session{}, fmt.Errorf("demo Environment %s not found", input.EnvironmentID)
	}
	b.nextID++
	sessionID := fmt.Sprintf("sesn_demo_%d", b.nextID)
	session := mango.Session{
		ID: sessionID, Title: input.Title, Status: "idle",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		EnvironmentID: input.EnvironmentID, Agent: selected,
	}
	if session.Title == "" {
		session.Title = "New Session"
	}
	threadID := fmt.Sprintf("sthr_demo_%d", b.nextID)
	primary := mango.Thread{
		ID: threadID, SessionID: sessionID, Status: "idle",
		Agent: mango.ThreadAgentFromAgent(selected),
	}
	logs := map[string][]mango.Event{threadID: {}}
	if input.InitialPrompt != "" {
		logs[threadID] = append(logs[threadID], b.event("user.message", map[string]any{
			"content": []any{map[string]any{"type": "text", "text": input.InitialPrompt}},
		}))
	}
	b.created[sessionID] = session
	b.createdTrees[sessionID] = []mango.Thread{primary}
	b.createdLogs[sessionID] = logs
	return session, nil
}

func (b *Backend) UpdateSessionTitle(_ context.Context, sessionID, title string) (mango.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	session, ok := b.sessionLocked(sessionID)
	if !ok {
		return mango.Session{}, fmt.Errorf("demo Session %s not found", sessionID)
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return mango.Session{}, fmt.Errorf("Session title cannot be empty")
	}
	b.titles[sessionID] = title
	session.Title, session.UpdatedAt = title, time.Now().UTC()
	return session, nil
}

func (b *Backend) ArchiveSession(_ context.Context, sessionID string) (mango.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	session, ok := b.sessionLocked(sessionID)
	if !ok {
		return mango.Session{}, fmt.Errorf("demo Session %s not found", sessionID)
	}
	if session.Status == "running" || session.Status == "rescheduling" {
		return mango.Session{}, fmt.Errorf("interrupt active work before archiving this Session")
	}
	now := time.Now().UTC()
	b.archived[sessionID] = now
	session.ArchivedAt = &now
	return session, nil
}

func (b *Backend) DeleteSession(_ context.Context, sessionID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	session, ok := b.sessionLocked(sessionID)
	if !ok {
		return fmt.Errorf("demo Session %s not found", sessionID)
	}
	if session.Status == "running" || session.Status == "rescheduling" {
		return fmt.Errorf("interrupt active work before deleting this Session")
	}
	b.deleted[sessionID] = true
	delete(b.created, sessionID)
	delete(b.createdTrees, sessionID)
	delete(b.createdLogs, sessionID)
	return nil
}

func (b *Backend) Attach(ctx context.Context, sessionID string) (mango.Attachment, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	session, ok := b.sessionLocked(sessionID)
	if !ok {
		return mango.Attachment{}, fmt.Errorf("demo Session %s not found", sessionID)
	}
	if _, created := b.created[sessionID]; created {
		return b.attachment(ctx, session, b.createdTrees[sessionID], b.createdLogs[sessionID]), nil
	}
	return b.attachment(ctx, session, b.threads, b.events), nil
}

func (b *Backend) attachment(ctx context.Context, session mango.Session, threads []mango.Thread, events map[string][]mango.Event) mango.Attachment {
	streamCtx, cancel := context.WithCancel(ctx)
	updates := make(chan mango.StreamUpdate, 128)
	b.nextSubscriber++
	subscriberID := b.nextSubscriber
	b.subscribers[subscriberID] = updates
	var closeOnce sync.Once
	closeSubscription := func() {
		closeOnce.Do(func() {
			cancel()
			b.mu.Lock()
			defer b.mu.Unlock()
			if _, ok := b.subscribers[subscriberID]; ok {
				delete(b.subscribers, subscriberID)
				close(updates)
			}
		})
	}
	go func() {
		<-streamCtx.Done()
		closeSubscription()
	}()
	return mango.Attachment{
		Session: session, Threads: append([]mango.Thread(nil), threads...), Events: cloneEvents(events),
		Updates: updates, Cancel: closeSubscription,
	}
}

func (b *Backend) Refresh(_ context.Context, sessionID string) (mango.Summary, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	session, ok := b.sessionLocked(sessionID)
	if !ok {
		return mango.Summary{}, fmt.Errorf("demo Session %s not found", sessionID)
	}
	if _, created := b.created[sessionID]; created {
		return mango.Summary{Session: session, Threads: append([]mango.Thread(nil), b.createdTrees[sessionID]...)}, nil
	}
	return mango.Summary{Session: session, Threads: append([]mango.Thread(nil), b.threads...)}, nil
}

func (b *Backend) SendMessage(_ context.Context, sessionID string, content string) ([]mango.Event, error) {
	b.mu.Lock()
	event := b.event("user.message", map[string]any{"content": []any{map[string]any{"type": "text", "text": content}}})
	primary, logs := b.sessionLedger(sessionID)
	if primary == "" {
		b.mu.Unlock()
		return nil, fmt.Errorf("demo Session %s not found", sessionID)
	}
	logs[primary] = append(logs[primary], event)
	b.broadcastLocked(mango.StreamUpdate{ThreadID: primary, Frame: event})
	b.mu.Unlock()
	go b.simulateTurn(sessionID, primary)
	return []mango.Event{event}, nil
}

// simulateTurn makes --demo exercise the same privacy-safe thinking and text
// preview contract as the HTTP backend. It is intentionally small, but it
// prevents the demo from looking like an echo-only transport.
func (b *Backend) simulateTurn(sessionID, threadID string) {
	b.mu.Lock()
	_, logs := b.sessionLedger(sessionID)
	if logs == nil {
		b.mu.Unlock()
		return
	}
	running := b.event("session.thread_status_running", map[string]any{"session_thread_id": threadID})
	logs[threadID] = append(logs[threadID], running)
	b.nextID++
	thinkingID := fmt.Sprintf("sevt_demo_%d", b.nextID)
	b.broadcastLocked(mango.StreamUpdate{ThreadID: threadID, Frame: running})
	b.broadcastLocked(mango.StreamUpdate{ThreadID: threadID, Frame: mango.Event{
		"type": "event_start", "event": map[string]any{"id": thinkingID, "type": "agent.thinking"},
	}})
	b.mu.Unlock()

	time.Sleep(650 * time.Millisecond)

	b.mu.Lock()
	_, logs = b.sessionLedger(sessionID)
	if logs == nil {
		b.mu.Unlock()
		return
	}
	thinking := mango.Event{"id": thinkingID, "type": "agent.thinking", "processed_at": time.Now().UTC().Format(time.RFC3339Nano)}
	logs[threadID] = append(logs[threadID], thinking)
	b.nextID++
	messageID := fmt.Sprintf("sevt_demo_%d", b.nextID)
	b.broadcastLocked(mango.StreamUpdate{ThreadID: threadID, Frame: thinking})
	b.broadcastLocked(mango.StreamUpdate{ThreadID: threadID, Frame: mango.Event{
		"type": "event_start", "event": map[string]any{"id": messageID, "type": "agent.message"},
	}})
	b.mu.Unlock()

	chunks := []string{
		"Mango is connected to the cloud Agent. ",
		"This reply is arriving through the same ",
		"thinking and streaming event path used by the real client.",
	}
	var reply strings.Builder
	for _, chunk := range chunks {
		time.Sleep(160 * time.Millisecond)
		reply.WriteString(chunk)
		b.mu.Lock()
		b.broadcastLocked(mango.StreamUpdate{ThreadID: threadID, Frame: mango.Event{
			"type": "event_delta", "event_id": messageID,
			"delta": map[string]any{"type": "content_delta", "index": 0, "content": map[string]any{"type": "text", "text": chunk}},
		}})
		b.mu.Unlock()
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	_, logs = b.sessionLedger(sessionID)
	if logs == nil {
		return
	}
	message := mango.Event{
		"id": messageID, "type": "agent.message", "processed_at": time.Now().UTC().Format(time.RFC3339Nano),
		"content": []any{map[string]any{"type": "text", "text": reply.String()}},
	}
	usage := b.event("span.model_request_end", map[string]any{
		"is_error": false, "model_usage": map[string]any{
			"input_tokens": 42, "output_tokens": 28, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0,
		},
	})
	idleThread := b.event("session.thread_status_idle", map[string]any{"session_thread_id": threadID})
	idle := b.event("session.status_idle", map[string]any{"stop_reason": map[string]any{"type": "end_turn"}})
	logs[threadID] = append(logs[threadID], message, usage, idleThread, idle)
	for _, frame := range []mango.Event{message, usage, idleThread, idle} {
		b.broadcastLocked(mango.StreamUpdate{ThreadID: threadID, Frame: frame})
	}
}

func (b *Backend) Interrupt(_ context.Context, sessionID string, threadID string) ([]mango.Event, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	primary, logs := b.sessionLedger(sessionID)
	if primary == "" {
		return nil, fmt.Errorf("demo Session %s not found", sessionID)
	}
	if threadID == "" {
		threadID = primary
		b.status[sessionID] = "idle"
	}
	event := b.event("user.interrupt", map[string]any{"session_thread_id": threadID})
	logs[threadID] = append(logs[threadID], event)
	b.broadcastLocked(mango.StreamUpdate{ThreadID: threadID, Frame: event})
	return []mango.Event{event}, nil
}

func (b *Backend) sessionLocked(sessionID string) (mango.Session, bool) {
	if b.deleted[sessionID] {
		return mango.Session{}, false
	}
	if _, archived := b.archived[sessionID]; archived {
		return mango.Session{}, false
	}
	var session mango.Session
	switch {
	case sessionID == b.session.ID:
		session = b.session
	case sessionID == "sesn_demo_incident_review":
		session = b.session
		session.ID, session.Title, session.Status = sessionID, "Investigate checkout latency", "running"
		session.CreatedAt = session.CreatedAt.Add(-2 * time.Hour)
		session.UpdatedAt = time.Now().UTC().Add(-25 * time.Second)
	default:
		var ok bool
		session, ok = b.created[sessionID]
		if !ok {
			return mango.Session{}, false
		}
	}
	if title := b.titles[sessionID]; title != "" {
		session.Title = title
	}
	if status := b.status[sessionID]; status != "" {
		session.Status = status
	}
	return session, true
}

func (b *Backend) ResolveAction(_ context.Context, sessionID string, action mango.Action, response mango.ActionResponse) ([]mango.Event, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	primary, logs := b.sessionLedger(sessionID)
	if primary == "" {
		return nil, fmt.Errorf("demo Session %s not found", sessionID)
	}
	payload := map[string]any{}
	typ := "user.tool_confirmation"
	if action.Kind == mango.ActionConfirmation {
		payload["tool_use_id"], payload["result"] = action.ID, response.Result
	} else {
		typ, payload["content"] = "user.tool_result", []any{map[string]any{"type": "text", "text": response.Content}}
		payload["tool_use_id"] = action.ID
		if action.Kind == mango.ActionCustomResult {
			typ = "user.custom_tool_result"
			delete(payload, "tool_use_id")
			payload["custom_tool_use_id"] = action.ID
		}
	}
	if action.ThreadID != "" {
		payload["session_thread_id"] = action.ThreadID
	}
	event := b.event(typ, payload)
	logs[primary] = append(logs[primary], event)
	idle := b.event("session.status_idle", map[string]any{"stop_reason": map[string]any{"type": "end_turn"}})
	logs[primary] = append(logs[primary], idle)
	b.broadcastLocked(mango.StreamUpdate{ThreadID: primary, Frame: event})
	b.broadcastLocked(mango.StreamUpdate{ThreadID: primary, Frame: idle})
	return []mango.Event{event}, nil
}

func (b *Backend) broadcastLocked(update mango.StreamUpdate) {
	for _, subscriber := range b.subscribers {
		subscriber <- update
	}
}

func (b *Backend) sessionLedger(sessionID string) (string, map[string][]mango.Event) {
	if trees, ok := b.createdTrees[sessionID]; ok && len(trees) > 0 {
		return trees[0].ID, b.createdLogs[sessionID]
	}
	if sessionID == b.session.ID || sessionID == "sesn_demo_incident_review" {
		return b.threads[0].ID, b.events
	}
	return "", nil
}

func (b *Backend) event(typ string, payload map[string]any) mango.Event {
	b.nextID++
	event := mango.Event{"id": fmt.Sprintf("sevt_demo_%d", b.nextID), "type": typ, "processed_at": time.Now().UTC().Format(time.RFC3339Nano)}
	for key, value := range payload {
		event[key] = value
	}
	return event
}

func cloneEvents(source map[string][]mango.Event) map[string][]mango.Event {
	clone := make(map[string][]mango.Event, len(source))
	for id, events := range source {
		clone[id] = append([]mango.Event(nil), events...)
	}
	return clone
}
