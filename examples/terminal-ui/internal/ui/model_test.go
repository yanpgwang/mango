package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/yanpgwang/mango/examples/terminal-ui/internal/demo"
	"github.com/yanpgwang/mango/examples/terminal-ui/internal/feed"
	"github.com/yanpgwang/mango/examples/terminal-ui/internal/mango"
)

type failingSendBackend struct{ mango.Backend }

func (f failingSendBackend) SendMessage(context.Context, string, string) ([]mango.Event, error) {
	return nil, errors.New("control plane unavailable")
}

type cancelAwareAttachBackend struct {
	mango.Backend
	started  chan struct{}
	canceled chan struct{}
}

func (b *cancelAwareAttachBackend) Attach(ctx context.Context, _ string) (mango.Attachment, error) {
	close(b.started)
	<-ctx.Done()
	close(b.canceled)
	return mango.Attachment{}, ctx.Err()
}

func TestNewAttachCancelsAndRejectsOlderAttach(t *testing.T) {
	backend := &cancelAwareAttachBackend{
		Backend: demo.New(), started: make(chan struct{}), canceled: make(chan struct{}),
	}
	model := New(backend, "")
	oldCommand := model.beginAttach("sesn_old")
	oldResult := make(chan tea.Msg, 1)
	go func() { oldResult <- oldCommand() }()
	<-backend.started
	_ = model.beginAttach("sesn_new")
	select {
	case <-backend.canceled:
	case <-time.After(time.Second):
		t.Fatal("starting a new attach did not cancel the older request")
	}
	updated, _ := model.Update(<-oldResult)
	model = updated.(Model)
	if model.session != nil || model.attachTarget != "sesn_new" || model.activeAttach == 0 {
		t.Fatalf("stale attach mutated model: session=%#v target=%q generation=%d", model.session, model.attachTarget, model.activeAttach)
	}
	model.cancelAttach()
}

func TestStaleSuccessfulAttachIsClosedWithoutReplacingSelection(t *testing.T) {
	model := New(demo.New(), "")
	model.session = &mango.Session{ID: "sesn_current"}
	model.activeAttach, model.attachTarget = 2, "sesn_new"
	canceled := false
	updated, _ := model.Update(attachedLoaded{
		sessionID: "sesn_old", attachID: 1,
		attachment: mango.Attachment{Session: mango.Session{ID: "sesn_old"}, Cancel: func() { canceled = true }},
	})
	model = updated.(Model)
	if !canceled || model.session == nil || model.session.ID != "sesn_current" {
		t.Fatalf("canceled=%v session=%#v", canceled, model.session)
	}
}

func TestThreadSelectionIsExplicit(t *testing.T) {
	model := New(demo.New(), "")
	model.loading = false
	parent := "sthr_primary"
	model.threads = []mango.Thread{{ID: parent}, {ID: "sthr_child", ParentThreadID: &parent}}
	model.threadCursor = 0
	model.selectThread(1)
	if model.currentThreadID() != "sthr_child" {
		t.Fatalf("thread = %q", model.currentThreadID())
	}
	model.setFocus(focusChat)
	if model.focus != focusChat || model.editor.Focused() {
		t.Fatal("chat focus did not blur editor")
	}
}

func TestTabNeverChangesSelectedAgent(t *testing.T) {
	model := New(demo.New(), "")
	parent := "sthr_primary"
	model.threads = []mango.Thread{{ID: parent}, {ID: "sthr_child", ParentThreadID: &parent}}
	model.threadCursor, model.screen, model.width, model.height = 0, screenChat, 140, 42
	model.resize()

	updated, _ := model.updateChat(tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))
	model = updated.(Model)
	if model.focus != focusChat || model.currentThreadID() != parent {
		t.Fatalf("after tab focus=%v thread=%q", model.focus, model.currentThreadID())
	}
	updated, _ = model.updateChat(tea.KeyPressMsg(tea.Key{Code: 'l', Text: "l"}))
	model = updated.(Model)
	if model.focus != focusChat || model.currentThreadID() != parent {
		t.Fatalf("after l focus=%v thread=%q", model.focus, model.currentThreadID())
	}
	updated, _ = model.updateChat(tea.KeyPressMsg(tea.Key{Code: 'g', Mod: tea.ModCtrl}))
	model = updated.(Model)
	if model.dialog != dialogAgents {
		t.Fatalf("ctrl+g dialog=%v", model.dialog)
	}
	updated, _ = model.updateDialog(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	model = updated.(Model)
	updated, _ = model.updateDialog(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if model.currentThreadID() != "sthr_child" {
		t.Fatalf("agent picker thread=%q", model.currentThreadID())
	}
}

func TestAttachedSessionShowsConversationWithSubagentWorkspace(t *testing.T) {
	model := New(demo.New(), "")
	model.loading = false
	model.width, model.height, model.screen = 140, 42, screenChat
	model.session = &mango.Session{ID: "sesn_1", Title: "Launch"}
	parent := "sthr_primary"
	model.threads = []mango.Thread{
		{ID: parent, Status: "idle", Agent: mango.ThreadAgent{Name: "coordinator"}},
		{ID: "sthr_child", ParentThreadID: &parent, Status: "idle", Agent: mango.ThreadAgent{Name: "researcher"}},
	}
	model.threadCursor = 0
	model.resize()
	model.renderChat()
	view := ansi.Strip(model.renderWorkspace())
	for _, want := range []string{"Launch", "Subagent workspace", "coordinator", "researcher", "main conversation"} {
		if !strings.Contains(view, want) {
			t.Fatalf("workspace missing %q: %q", want, view)
		}
	}
}

func TestSessionFocusCyclesThroughSubagentWorkspace(t *testing.T) {
	model := New(demo.New(), "")
	model.loading = false
	model.width, model.height, model.screen = 140, 42, screenChat
	parent := "sthr_primary"
	model.threads = []mango.Thread{{ID: parent}, {ID: "sthr_child", ParentThreadID: &parent}}
	model.threadCursor = 0
	model.resize()

	updated, _ := model.updateChat(tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))
	model = updated.(Model)
	if model.focus != focusChat {
		t.Fatalf("first tab focus=%v, want conversation", model.focus)
	}
	updated, _ = model.updateChat(tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))
	model = updated.(Model)
	if model.focus != focusAgents {
		t.Fatalf("second tab focus=%v, want subagent workspace", model.focus)
	}
	updated, _ = model.updateChat(tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))
	model = updated.(Model)
	if model.focus != focusEditor || !model.editor.Focused() {
		t.Fatalf("third tab focus=%v editor=%v", model.focus, model.editor.Focused())
	}
}

func TestSubagentWorkspacePreviewsAndOpensChildTranscript(t *testing.T) {
	model := New(demo.New(), "")
	model.loading = false
	model.width, model.height, model.screen = 140, 42, screenChat
	parent := "sthr_primary"
	model.threads = []mango.Thread{{ID: parent}, {ID: "sthr_child", ParentThreadID: &parent}}
	model.threadCursor, model.railCursor, model.focus = 0, 1, focusAgents
	model.resize()

	updated, _ := model.updateChat(tea.KeyPressMsg(tea.Key{Code: tea.KeySpace}))
	model = updated.(Model)
	if model.currentThreadID() != "sthr_child" || model.focus != focusAgents {
		t.Fatalf("preview thread=%q focus=%v", model.currentThreadID(), model.focus)
	}
	updated, _ = model.updateChat(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if model.currentThreadID() != "sthr_child" || model.focus != focusChat {
		t.Fatalf("open thread=%q focus=%v", model.currentThreadID(), model.focus)
	}
	updated, _ = model.updateChat(tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	model = updated.(Model)
	if model.currentThreadID() != parent || model.focus != focusEditor || model.screen != screenChat {
		t.Fatalf("back thread=%q focus=%v screen=%v", model.currentThreadID(), model.focus, model.screen)
	}
}

func TestSubagentWorkspaceInterruptTargetsSelectedChild(t *testing.T) {
	model := New(demo.New(), "")
	model.loading = false
	model.width, model.height, model.screen = 140, 42, screenChat
	parent := "sthr_primary"
	model.threads = []mango.Thread{{ID: parent}, {ID: "sthr_child", ParentThreadID: &parent}}
	model.threadCursor, model.railCursor, model.focus = 0, 1, focusAgents
	model.resize()

	updated, _ := model.updateChat(tea.KeyPressMsg(tea.Key{Code: 'x'}))
	model = updated.(Model)
	if model.dialog != dialogInterrupt || model.currentThreadID() != "sthr_child" {
		t.Fatalf("dialog=%v target=%q", model.dialog, model.currentThreadID())
	}
}

func TestSendingFromChildViewReturnsToCoordinator(t *testing.T) {
	model := New(demo.New(), "")
	model.loading = false
	model.width, model.height, model.screen = 140, 42, screenChat
	parent := "sthr_primary"
	model.session = &mango.Session{ID: "sesn_1"}
	model.threads = []mango.Thread{{ID: parent}, {ID: "sthr_child", ParentThreadID: &parent}}
	model.threadCursor, model.railCursor = 1, 1
	model.editor.SetValue("follow up with the reviewer")
	model.resize()

	updated, command := model.updateChat(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if command == nil || model.currentThreadID() != parent || model.previews[parent] == nil || model.previews["sthr_child"] != nil {
		t.Fatalf("command=%v thread=%q previews=%#v", command, model.currentThreadID(), model.previews)
	}
}

func TestCompactSessionCollapsesWorkspaceIntoAgentStrip(t *testing.T) {
	model := New(demo.New(), "")
	model.loading = false
	model.width, model.height, model.screen = 100, 32, screenChat
	parent := "sthr_primary"
	model.session = &mango.Session{ID: "sesn_1", Title: "Compact review"}
	model.threads = []mango.Thread{
		{ID: parent, Agent: mango.ThreadAgent{Name: "coordinator"}},
		{ID: "sthr_child", ParentThreadID: &parent, Agent: mango.ThreadAgent{Name: "researcher"}},
	}
	model.resize()
	model.renderChat()
	view := ansi.Strip(model.renderWorkspace())
	if !strings.Contains(view, "Agents") || !strings.Contains(view, "researcher") || strings.Contains(view, "Subagent workspace") {
		t.Fatalf("compact workspace = %q", view)
	}
}

func TestInboxPreviewSummarizesSelectedSession(t *testing.T) {
	model := New(demo.New(), "")
	model.width, model.height, model.screen, model.inboxCursor = 150, 42, screenInbox, 3
	session := mango.Session{
		ID:            "sesn_product_launch",
		Title:         "Ship the managed Agent launch",
		Status:        "idle",
		UpdatedAt:     time.Now().Add(-4 * time.Minute),
		EnvironmentID: "env_demo_cloud",
		Usage:         mango.Usage{InputTokens: 18420, OutputTokens: 3286},
		Stats:         mango.Stats{ActiveSeconds: 147.8},
	}
	session.Agent.ID, session.Agent.Name, session.Agent.Model.ID = "agent_coordinator", "coordinator", "claude-sonnet-4-5"
	session.Agent.Multiagent = &mango.Multiagent{Agents: []mango.AgentReference{
		{ID: "agent_researcher", Name: "researcher"},
		{ID: "agent_reviewer", Name: "reviewer"},
	}}
	model.sessions = []mango.Session{session}
	model.resize()

	preview := ansi.Strip(model.renderInboxPreview(58, 30))
	if strings.ContainsAny(preview, "╭╮╰╯│") {
		t.Fatalf("preview still renders a card border: %q", preview)
	}
	if lipgloss.Width(preview) != 58 || lipgloss.Height(preview) != 30 {
		t.Fatalf("preview dimensions=%dx%d", lipgloss.Width(preview), lipgloss.Height(preview))
	}
	for _, want := range []string{
		"Ship the managed Agent launch", "idle · 4m ago", "AGENT", "coordinator", "claude-sonnet-4-5",
		"env_demo_cloud", "SUBAGENTS · 2", "researcher  ·  reviewer", "USAGE", "18.4K in", "3.3K out",
		"147.8s active", "SESSION", "sesn_product_launch", "enter attach", "m manage",
	} {
		if !strings.Contains(preview, want) {
			t.Fatalf("preview missing %q: %q", want, preview)
		}
	}
}

func TestInboxBubblesListOwnsNavigationPaginationAndFilter(t *testing.T) {
	model := New(demo.New(), "")
	model.width, model.height, model.screen, model.inboxCursor = 150, 20, screenInbox, 3
	model.sessions = make([]mango.Session, 8)
	for index := range model.sessions {
		model.sessions[index] = mango.Session{
			ID:     fmt.Sprintf("sesn_%d", index),
			Title:  fmt.Sprintf("Session %d", index),
			Status: "idle",
			Agent:  mango.Agent{Name: "coordinator"},
		}
	}
	model.sessions[6].Title = "Reviewer launch room"
	model.sessions[6].Agent.Name = "reviewer"
	model.resize()

	for range 4 {
		updated, _ := model.updateInbox(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
		model = updated.(Model)
	}
	selected, ok := model.selectedInboxSession()
	if !ok || selected.ID != "sesn_4" || model.inboxList.Paginator.Page == 0 {
		t.Fatalf("selection=%q page=%d cursor=%d", selected.ID, model.inboxList.Paginator.Page, model.inboxCursor)
	}

	model.inboxCursor = 1
	updated, _ := model.updateInbox(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if !model.inboxList.SettingFilter() || model.dialog != dialogNone || model.viewCursor() == nil {
		t.Fatalf("filtering=%v dialog=%v cursor=%v", model.inboxList.SettingFilter(), model.dialog, model.viewCursor())
	}
	if view := ansi.Strip(model.renderInbox()); !strings.Contains(view, "Find Sessions") {
		t.Fatalf("filter input is not visible: %q", view)
	}

	model.inboxList.SetFilterText("reviewer")
	model.updateInboxList(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	selected, ok = model.selectedInboxSession()
	if !ok || selected.ID != "sesn_6" || !model.inboxList.IsFiltered() {
		t.Fatalf("filtered selection=%q state=%v", selected.ID, model.inboxList.FilterState())
	}
	if view := ansi.Strip(model.renderInbox()); !strings.Contains(view, "FILTER  reviewer") ||
		!strings.Contains(view, "Reviewer launch room") {
		t.Fatalf("applied filter is not visible: %q", view)
	}
	model.inboxList.SetFilterText("does-not-exist")
	model.updateInboxList(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if _, ok := model.selectedInboxSession(); ok {
		t.Fatal("a filter with no matches retained an invisible Session selection")
	}
	if preview := ansi.Strip(model.renderInboxPreview(58, 12)); !strings.Contains(preview, "No matching Session") {
		t.Fatalf("no-match preview=%q", preview)
	}

	updated, _ = model.updateInbox(tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	model = updated.(Model)
	if model.screen != screenInbox || model.inboxList.FilterState().String() != "unfiltered" {
		t.Fatalf("screen=%v filter=%v", model.screen, model.inboxList.FilterState())
	}
}

func TestInboxToolbarBalancesFleetStatusAboveSpacedHelp(t *testing.T) {
	model := New(demo.New(), "")
	model.width, model.height, model.screen = 150, 36, screenInbox
	model.sessions = []mango.Session{
		{ID: "sesn_running", Title: "Running", Status: "running"},
		{ID: "sesn_idle", Title: "Idle", Status: "idle"},
	}
	model.resize()

	lines := strings.Split(ansi.Strip(model.renderInbox()), "\n")
	actionLine, helpLine := -1, -1
	for index, line := range lines {
		if strings.Contains(line, "+ New") {
			actionLine = index
			if !strings.Contains(line, "CONNECTED") || !strings.Contains(line, "1 running") || !strings.Contains(line, "1 idle") {
				t.Fatalf("toolbar status is not balanced on the action row: %q", line)
			}
		}
		if strings.Contains(line, "pill/page") {
			helpLine = index
			if strings.Contains(line, "CONNECTED") {
				t.Fatalf("fleet status remained in the footer: %q", line)
			}
		}
	}
	if actionLine < 0 || helpLine < 1 || strings.TrimSpace(lines[helpLine-1]) != "" {
		t.Fatalf("actionLine=%d helpLine=%d linesBeforeHelp=%q", actionLine, helpLine, lines[max(0, helpLine-1)])
	}
}

func TestSessionManagerRenamesAndDeletesWithSafeConfirmation(t *testing.T) {
	backend := demo.New()
	sessions, err := backend.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	model := New(backend, "")
	model.width, model.height, model.screen = 120, 36, screenInbox
	model.sessions, model.inboxCursor = sessions, 3
	model.resize()

	updated, _ := model.updateInbox(tea.KeyPressMsg(tea.Key{Code: 'm'}))
	model = updated.(Model)
	if model.dialog != dialogSessionActions || model.sessionAction.ID != sessions[0].ID {
		t.Fatalf("dialog=%v target=%q", model.dialog, model.sessionAction.ID)
	}
	manager := ansi.Strip(model.renderDialog())
	for _, want := range []string{"Manage Session", "Rename", "Archive", "Delete permanently"} {
		if !strings.Contains(manager, want) {
			t.Fatalf("manager missing %q: %q", want, manager)
		}
	}

	updated, _ = model.updateDialog(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	model.filter.SetValue("Launch decision room")
	updated, command := model.updateDialog(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if command == nil {
		t.Fatal("rename produced no command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if model.sessions[0].Title != "Launch decision room" || model.dialog != dialogSessionActions {
		t.Fatalf("renamed=%q dialog=%v", model.sessions[0].Title, model.dialog)
	}

	model.sessionActionCursor = 3
	updated, _ = model.updateDialog(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if model.dialog != dialogDeleteSession || model.dialogCursor != 1 {
		t.Fatalf("delete dialog=%v cursor=%d", model.dialog, model.dialogCursor)
	}
	// Enter starts on the safe choice and must not delete anything.
	updated, command = model.updateDialog(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if command != nil || model.dialog != dialogSessionActions || len(model.sessions) != 2 {
		t.Fatalf("safe cancel command=%v dialog=%v sessions=%d", command, model.dialog, len(model.sessions))
	}

	model.sessionActionCursor = 3
	updated, _ = model.updateDialog(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	updated, _ = model.updateDialog(tea.KeyPressMsg(tea.Key{Code: tea.KeyLeft}))
	model = updated.(Model)
	updated, command = model.updateDialog(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if command == nil {
		t.Fatal("confirmed delete produced no command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if len(model.sessions) != 1 || model.dialog != dialogNone || model.status != "Session deleted" {
		t.Fatalf("sessions=%d dialog=%v status=%q", len(model.sessions), model.dialog, model.status)
	}
}

func TestRunningSessionManagerRequiresInterruptBeforeArchive(t *testing.T) {
	model := New(demo.New(), "")
	model.screen = screenInbox
	target := mango.Session{ID: "sesn_running", Title: "Running", Status: "running"}
	model.openSessionManager(target, dialogNone)
	model.sessionActionCursor = 2

	updated, command := model.updateDialog(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if command != nil || model.dialog != dialogSessionActions || model.err == nil ||
		!strings.Contains(model.err.Error(), "interrupt active work") {
		t.Fatalf("command=%v dialog=%v err=%v", command, model.dialog, model.err)
	}
	model.sessionActionCursor = 1
	updated, _ = model.updateDialog(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if model.dialog != dialogInterruptSession || model.dialogCursor != 1 {
		t.Fatalf("interrupt dialog=%v cursor=%d", model.dialog, model.dialogCursor)
	}
}

func TestDeletingAttachedSessionReturnsToInbox(t *testing.T) {
	backend := demo.New()
	sessions, err := backend.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	model := New(backend, "")
	model.width, model.height, model.screen = 120, 36, screenChat
	model.sessions = sessions
	model.session = &model.sessions[0]
	model.threads = []mango.Thread{{ID: "sthr_primary"}}
	model.openSessionManager(*model.session, dialogNone)
	model.sessionActionCursor = 3

	updated, _ := model.updateDialog(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	updated, _ = model.updateDialog(tea.KeyPressMsg(tea.Key{Code: tea.KeyLeft}))
	model = updated.(Model)
	updated, command := model.updateDialog(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	updated, _ = model.Update(command())
	model = updated.(Model)
	if model.screen != screenInbox || model.session != nil || len(model.threads) != 0 || model.dialog != dialogNone {
		t.Fatalf("screen=%v session=%v threads=%d dialog=%v", model.screen, model.session, len(model.threads), model.dialog)
	}
}

func TestWideSessionCursorAndLayoutStayInsideLeftConversation(t *testing.T) {
	model := New(demo.New(), "")
	model.loading = false
	model.width, model.height, model.screen = 140, 42, screenChat
	model.session = &mango.Session{ID: "sesn_1", Title: "Layout"}
	parent := "sthr_primary"
	model.threads = []mango.Thread{{ID: parent}, {ID: "sthr_child", ParentThreadID: &parent}}
	model.editor.SetValue("中文")
	model.resize()
	model.renderChat()
	view := model.View()
	if got := lipgloss.Width(view.Content); got > model.width {
		t.Fatalf("workspace width=%d terminal=%d", got, model.width)
	}
	if got := lipgloss.Height(view.Content); got > model.height {
		t.Fatalf("workspace height=%d terminal=%d", got, model.height)
	}
	if view.Cursor == nil || view.Cursor.X >= model.workspaceMainWidth() || view.Cursor.Y >= model.height {
		t.Fatalf("cursor=%#v mainWidth=%d terminalHeight=%d", view.Cursor, model.workspaceMainWidth(), model.height)
	}
}

func TestSubagentWorkspaceShowsGhostRosterAndPendingAction(t *testing.T) {
	model := New(demo.New(), "")
	model.width, model.height, model.screen = 140, 42, screenChat
	model.session = &mango.Session{ID: "sesn_1"}
	model.session.Agent.ID = "agent_main"
	model.session.Agent.Multiagent = &mango.Multiagent{Agents: []mango.AgentReference{
		{ID: "agent_researcher", Name: "researcher"},
		{ID: "agent_writer", Name: "writer"},
	}}
	parent := "sthr_primary"
	model.threads = []mango.Thread{
		{ID: parent, Agent: mango.ThreadAgent{ID: "agent_main", Name: "coordinator"}},
		{ID: "sthr_child", ParentThreadID: &parent, Agent: mango.ThreadAgent{ID: "agent_researcher", Name: "researcher"}},
	}
	model.pending = []mango.Action{{ID: "sevt_action", ThreadID: "sthr_child", Name: "bash"}}
	model.resize()
	rail := ansi.Strip(model.renderAgentWorkspace(model.workspaceRailWidth(), 38))
	for _, want := range []string{"1 live / 2 roster", "writer", "not delegated yet", "Needs your input", "bash", "researcher"} {
		if !strings.Contains(rail, want) {
			t.Fatalf("rail missing %q: %q", want, rail)
		}
	}
}

func TestSubagentWorkspaceKeepsSelectedAgentVisibleInLongRoster(t *testing.T) {
	model := New(demo.New(), "")
	model.width, model.height, model.screen = 140, 30, screenChat
	model.session = &mango.Session{ID: "sesn_1"}
	model.threads = []mango.Thread{{ID: "sthr_primary", Agent: mango.ThreadAgent{Name: "coordinator"}}}
	parent := model.threads[0].ID
	for index := 1; index <= 9; index++ {
		model.threads = append(model.threads, mango.Thread{
			ID: fmt.Sprintf("sthr_%d", index), ParentThreadID: &parent,
			Agent: mango.ThreadAgent{Name: fmt.Sprintf("agent-%d", index)},
		})
	}
	model.focus, model.railCursor = focusAgents, 9
	model.resize()
	rail := ansi.Strip(model.renderAgentWorkspace(model.workspaceRailWidth(), 26))
	if !strings.Contains(rail, "agent-9") || !strings.Contains(rail, "earlier") {
		t.Fatalf("selected long-roster agent not visible: %q", rail)
	}
}

func TestCoordinatorActivityIgnoresChildActionCrosspost(t *testing.T) {
	model := New(demo.New(), "")
	parent := mango.Thread{ID: "sthr_primary", Agent: mango.ThreadAgent{Name: "coordinator"}}
	model.events[parent.ID] = []mango.Event{
		{"id": "sevt_report", "type": "agent.thread_message_received", "from_agent_name": "researcher", "content": "cohort report ready"},
		{"id": "sevt_child_tool", "type": "agent.tool_use", "name": "bash", "session_thread_id": "sthr_child"},
	}
	activity, _ := model.agentActivity(parent)
	if !strings.Contains(activity, "cohort report ready") || strings.Contains(activity, "bash") {
		t.Fatalf("coordinator activity=%q", activity)
	}
}

func TestRunningAgentActivitySupersedesHistoricalReport(t *testing.T) {
	model := New(demo.New(), "")
	thread := mango.Thread{ID: "sthr_child", Status: "running", Agent: mango.ThreadAgent{Name: "researcher"}}
	model.events[thread.ID] = []mango.Event{
		{"id": "sevt_report", "type": "agent.thread_message_received", "from_agent_name": "researcher", "content": "previous report"},
	}
	activity, _ := model.agentActivity(thread)
	if activity != "running" {
		t.Fatalf("activity=%q, want live Thread status", activity)
	}
}

func TestSubagentCountDeduplicatesConfiguredRoles(t *testing.T) {
	session := mango.Session{Agent: mango.Agent{ID: "agent_main"}}
	session.Agent.Multiagent = &mango.Multiagent{Agents: []mango.AgentReference{
		{ID: "agent_main", Name: "coordinator"},
		{ID: "agent_researcher", Name: "researcher"},
		{ID: "agent_researcher", Name: "researcher"},
		{ID: "agent_reviewer", Name: "researcher"},
		{ID: "agent_writer", Name: "writer"},
	}}
	if count := subagentCount(session); count != 2 {
		t.Fatalf("subagentCount=%d, want 2 unique configured roles", count)
	}
}

func TestConnectionLogoUsesPixelCloudWelcomeScene(t *testing.T) {
	model := New(demo.New(), "")
	logo := model.brandLogo(false)
	plain := ansi.Strip(logo)
	if logo == plain {
		t.Fatal("connection logo has no ANSI color treatment")
	}
	if !strings.Contains(plain, "Welcome to Mango") || !strings.Contains(plain, "managed agents, one window") {
		t.Fatalf("connection logo lost the Mango welcome copy: %q", plain)
	}
	// The welcome scene is now a chunky pixel cloud, so it must carry block
	// glyphs and a face rather than the old thin line-art clouds.
	if !strings.Contains(plain, "█") || !strings.Contains(plain, "●") {
		t.Fatalf("connection logo is not a pixel cloud: %q", plain)
	}
	if width := lipgloss.Width(logo); width > 58 {
		t.Fatalf("connection logo width=%d, want <=58", width)
	}
	if height := lipgloss.Height(logo); height != 7 {
		t.Fatalf("connection logo height=%d, want 7", height)
	}
}

func TestConnectionLogoFitsMinimumSupportedTerminal(t *testing.T) {
	model := New(demo.New(), "")
	model.width, model.height, model.screen = 60, 20, screenConnect
	view := model.renderConnect()
	if width := lipgloss.Width(view); width > model.width {
		t.Fatalf("connection view width=%d terminal=%d: %q", width, model.width, ansi.Strip(view))
	}
	if height := lipgloss.Height(view); height > model.height {
		t.Fatalf("connection view height=%d terminal=%d", height, model.height)
	}
	if plain := ansi.Strip(view); !strings.Contains(plain, "Mango Cloud") || !strings.Contains(plain, "Welcome to Mango") {
		t.Fatalf("connection view lost controls or welcome: %q", plain)
	}
}

func TestEndpointListConnectsOnHighlightedEnter(t *testing.T) {
	selected := ""
	model := NewWithOptions(demo.New(), "", Options{
		Endpoint: "http://first.example.com",
		Endpoints: []EndpointOption{
			{URL: "http://first.example.com", Source: "configured"},
			{URL: "https://second.example.com", Source: "configured"},
		},
		BackendForEndpoint: func(target string) (mango.Backend, error) {
			selected = target
			return demo.New(), nil
		},
	})
	// The endpoint list is always visible; the cursor starts on the configured
	// endpoint. There is no separate Connect button — Enter connects the
	// highlighted row directly, matching the Crush-style picker.
	if model.connect.current().url != "http://first.example.com" {
		t.Fatalf("initial highlight = %q", model.connect.current().url)
	}
	updated, _ := model.updateConnect(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	model = updated.(Model)
	if model.connect.current().url != "https://second.example.com" {
		t.Fatalf("after down highlight = %q", model.connect.current().url)
	}
	updated, command := model.updateConnect(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if command == nil || !model.loading || selected != "https://second.example.com" {
		t.Fatalf("command=%v loading=%v selected=%q", command, model.loading, selected)
	}
}

func TestEndpointManualEntryNormalizesAndHighlights(t *testing.T) {
	model := NewWithOptions(demo.New(), "", Options{
		Endpoint:  "demo://local",
		Endpoints: []EndpointOption{{URL: "demo://local", Label: "Local demo", Available: true, SkipProbe: true}},
	})
	updated, _ := model.updateConnect(tea.KeyPressMsg(tea.Key{Code: 'e', Text: "e"}))
	model = updated.(Model)
	if !model.connect.editing {
		t.Fatal("manual endpoint editor did not open")
	}
	for _, character := range "localhost:9090" {
		updated, _ = model.updateConnect(tea.KeyPressMsg(tea.Key{Code: character, Text: string(character)}))
		model = updated.(Model)
	}
	updated, _ = model.updateConnect(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	// After adding a manual endpoint the editor closes and the cursor lands on
	// the freshly added endpoint, ready for a single Enter to connect.
	if model.connect.editing || model.connect.current().url != "http://localhost:9090" {
		t.Fatalf("editing=%v endpoint=%q", model.connect.editing, model.connect.current().url)
	}
}

func TestEndpointEditorAcceptsBracketedPaste(t *testing.T) {
	model := NewWithOptions(demo.New(), "", Options{
		Endpoint:  "demo://local",
		Endpoints: []EndpointOption{{URL: "demo://local", Label: "Local demo", Available: true, SkipProbe: true}},
	})
	updated, _ := model.updateConnect(tea.KeyPressMsg(tea.Key{Code: 'e', Text: "e"}))
	model = updated.(Model)
	updated, _ = model.Update(tea.PasteMsg{Content: "https://mango.example.com"})
	model = updated.(Model)
	if got := model.connect.endpointInput.Value(); got != "https://mango.example.com" {
		t.Fatalf("pasted endpoint=%q", got)
	}
}

func TestEndpointSaveFailureIsVisibleAfterConnecting(t *testing.T) {
	model := NewWithOptions(demo.New(), "", Options{
		Endpoint:  "https://mango.example.com",
		Endpoints: []EndpointOption{{URL: "https://mango.example.com"}},
		SaveEndpoint: func(string) error {
			return errors.New("read-only config")
		},
	})
	updated, load := model.updateConnect(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	updated, save := model.Update(load())
	model = updated.(Model)
	if save == nil || model.screen != screenInbox {
		t.Fatalf("save=%v screen=%v", save, model.screen)
	}
	updated, _ = model.Update(save())
	model = updated.(Model)
	if model.err == nil || !strings.Contains(model.err.Error(), "not saved") {
		t.Fatalf("save error was not visible: %v", model.err)
	}
}

func TestEndpointListAndEditorFitMinimumTerminal(t *testing.T) {
	model := NewWithOptions(demo.New(), "", Options{
		Endpoint: "http://127.0.0.1:8080",
		Endpoints: []EndpointOption{
			{URL: "http://127.0.0.1:8080", Source: "local default"},
			{URL: "https://mango.example.com", Source: "saved"},
		},
	})
	model.width, model.height = 60, 20
	model.resize()
	if view := model.renderConnect(); lipgloss.Width(view) > model.width || lipgloss.Height(view) > model.height {
		t.Fatalf("list is %dx%d in %dx%d", lipgloss.Width(view), lipgloss.Height(view), model.width, model.height)
	}
	model.connect.editing = true
	model.connect.endpointInput.Focus()
	model.connect.endpointInput.SetValue("https://a-very-long-endpoint-name-for-a-small-terminal.example.com")
	view := model.renderConnect()
	if lipgloss.Width(view) > model.width || lipgloss.Height(view) > model.height {
		t.Fatalf("editor is %dx%d in %dx%d", lipgloss.Width(view), lipgloss.Height(view), model.width, model.height)
	}
	cursor := model.View().Cursor
	if cursor == nil || cursor.X < 0 || cursor.X >= model.width || cursor.Y < 0 || cursor.Y >= model.height {
		t.Fatalf("endpoint editor cursor=%#v", cursor)
	}
	_, _, connectWidth, compact := connectDimensions(model.width, model.height)
	wantInputWidth, _, _ := connectCardMetrics(connectWidth, compact)
	if model.connect.endpointInput.Width() != wantInputWidth {
		t.Fatalf("endpoint input width=%d card width=%d", model.connect.endpointInput.Width(), wantInputWidth)
	}
}

func TestNormalizeEndpointRejectsEmbeddedCredentials(t *testing.T) {
	if _, err := normalizeEndpoint("https://secret@example.com"); err == nil {
		t.Fatal("endpoint accepted embedded credentials")
	}
	if got, err := normalizeEndpoint("localhost:8080/"); err != nil || got != "http://localhost:8080" {
		t.Fatalf("normalize=%q err=%v", got, err)
	}
	if _, err := normalizeEndpoint("https://example.com?api_key=secret"); err == nil {
		t.Fatal("endpoint accepted query parameters")
	}
}

func TestConnectCloudsAnimateWithoutChangingBounds(t *testing.T) {
	model := New(demo.New(), "")
	firstFrame := ansi.Strip(model.cloudScene(58, false))
	model.motion = 18
	secondFrame := ansi.Strip(model.cloudScene(58, false))
	if firstFrame == secondFrame {
		t.Fatal("cloud scene did not move")
	}
	if lipgloss.Width(firstFrame) != lipgloss.Width(secondFrame) || lipgloss.Height(firstFrame) != lipgloss.Height(secondFrame) {
		t.Fatalf("animation changed bounds: %dx%d -> %dx%d", lipgloss.Width(firstFrame), lipgloss.Height(firstFrame), lipgloss.Width(secondFrame), lipgloss.Height(secondFrame))
	}
}

func TestTextareaUsesRealCursorForIME(t *testing.T) {
	model := New(demo.New(), "")
	if model.editor.VirtualCursor() {
		t.Fatal("textarea virtual cursor leaves terminal IME without an anchor")
	}
	model.loading = false
	model.width, model.height, model.screen = 100, 32, screenChat
	model.session = &mango.Session{ID: "sesn_ime"}
	model.threads = []mango.Thread{{ID: "sthr_primary", Status: "idle"}}
	model.threadCursor = 0
	model.editor.SetValue("凡")
	model.resize()
	model.renderChat()

	view := model.View()
	if view.Cursor == nil {
		t.Fatal("focused chat textarea did not expose a terminal cursor")
	}
	if view.Cursor.X >= model.width/2 || view.Cursor.Y < model.height/2 || view.Cursor.Y >= model.height {
		t.Fatalf("chat cursor = (%d,%d), want composer near lower-left", view.Cursor.X, view.Cursor.Y)
	}
	model.screen = screenConnect
	if cursor := model.View().Cursor; cursor != nil {
		t.Fatalf("connection screen exposed editor cursor at (%d,%d)", cursor.X, cursor.Y)
	}
}

func TestChatAndDialogCursorOffsetsStayInsideTheirEditors(t *testing.T) {
	model := New(demo.New(), "")
	model.loading = false
	model.width, model.height, model.screen = 140, 42, screenChat
	model.session = &mango.Session{ID: "sesn_1", Title: "Cursor test"}
	model.threads = []mango.Thread{{ID: "sthr_primary", Status: "idle"}}
	model.threadCursor = 0
	model.editor.SetValue("中文")
	model.resize()
	model.renderChat()

	chat := model.View()
	if chat.Cursor == nil || chat.Cursor.X >= 24 || chat.Cursor.Y < model.height/2 || chat.Cursor.Y >= model.height {
		if chat.Cursor == nil {
			t.Fatal("chat textarea did not expose a terminal cursor")
		}
		t.Fatalf("chat cursor = (%d,%d), want lower-left composer", chat.Cursor.X, chat.Cursor.Y)
	}

	model.dialog = dialogResult
	model.editor.Focus()
	model.resize()
	dialog := model.View()
	if dialog.Cursor == nil {
		t.Fatal("dialog textarea did not expose a terminal cursor")
	}
	modal := model.renderDialog()
	left := max(0, (model.width-lipgloss.Width(modal))/2)
	top := max(0, (model.height-lipgloss.Height(modal))/2)
	if dialog.Cursor.X <= left || dialog.Cursor.X >= left+lipgloss.Width(modal) ||
		dialog.Cursor.Y <= top || dialog.Cursor.Y >= top+lipgloss.Height(modal) {
		t.Fatalf("dialog cursor = (%d,%d), modal=(%d,%d)-(%d,%d)",
			dialog.Cursor.X, dialog.Cursor.Y, left, top, left+lipgloss.Width(modal), top+lipgloss.Height(modal))
	}
}

func TestSelectionDialogsUseRealTextInputCursorForIME(t *testing.T) {
	model := New(demo.New(), "")
	model.loading = false
	model.width, model.height, model.screen = 120, 36, screenInbox
	model.openSessions()
	model.filter.SetValue("中文")
	model.inboxFilter = model.filter.Value()
	model.resize()

	view := model.View()
	if view.Cursor == nil {
		t.Fatal("Session picker did not expose a real textinput cursor")
	}
	modal := model.renderDialog()
	left := max(0, (model.width-lipgloss.Width(modal))/2)
	top := max(0, (model.height-lipgloss.Height(modal))/2)
	if view.Cursor.X <= left || view.Cursor.X >= left+lipgloss.Width(modal) ||
		view.Cursor.Y <= top || view.Cursor.Y >= top+lipgloss.Height(modal) {
		t.Fatalf("filter cursor = (%d,%d), modal=(%d,%d)-(%d,%d)",
			view.Cursor.X, view.Cursor.Y, left, top, left+lipgloss.Width(modal), top+lipgloss.Height(modal))
	}
}

func TestPendingChildActionKeepsVisibleCrosspostID(t *testing.T) {
	model := New(demo.New(), "")
	model.threads = []mango.Thread{{ID: "sthr_primary"}}
	model.events["sthr_primary"] = []mango.Event{
		{"id": "sevt_crosspost", "type": "agent.tool_use", "name": "bash", "evaluated_permission": "ask", "session_thread_id": "sthr_child"},
		{"id": "sevt_idle", "type": "session.status_idle", "stop_reason": map[string]any{"type": "requires_action", "event_ids": []any{"sevt_crosspost"}}},
	}
	model.pending = feed.PendingActions(model.primaryThreadID(), model.events)
	if len(model.pending) != 1 || model.pending[0].ID != "sevt_crosspost" || model.pending[0].ThreadID != "sthr_child" {
		t.Fatalf("pending = %#v", model.pending)
	}
}

func TestPendingActionOpensDecisionDialog(t *testing.T) {
	model := New(demo.New(), "")
	model.width, model.height, model.screen = 120, 36, screenChat
	model.threads = []mango.Thread{{ID: "sthr_primary"}}
	model.threadCursor = 0
	model.resize()
	model.applyStream(mango.StreamUpdate{ThreadID: "sthr_primary", Frame: mango.Event{
		"id": "sevt_tool", "type": "agent.tool_use", "name": "bash",
		"input": map[string]any{"command": "go test ./..."}, "evaluated_permission": "ask",
	}})
	model.applyStream(mango.StreamUpdate{ThreadID: "sthr_primary", Frame: mango.Event{
		"id": "sevt_idle", "type": "session.status_idle",
		"stop_reason": map[string]any{"type": "requires_action", "event_ids": []any{"sevt_tool"}},
	}})
	if model.dialog != dialogAction || len(model.pending) != 1 {
		t.Fatalf("dialog=%v pending=%#v", model.dialog, model.pending)
	}
	if !model.dialogIsOverlay() {
		t.Fatal("permission decision should be an overlay")
	}
	dialog := model.renderDialog()
	if !strings.Contains(dialog, "bash") || !strings.Contains(dialog, "Allow once") {
		t.Fatalf("dialog = %q", dialog)
	}
	model.applyStream(mango.StreamUpdate{ThreadID: "sthr_primary", Frame: mango.Event{
		"id": "sevt_done", "type": "session.status_idle",
		"stop_reason": map[string]any{"type": "end_turn"},
	}})
	if model.dialog != dialogNone || len(model.pending) != 0 || !model.editor.Focused() {
		t.Fatalf("resolved dialog=%v pending=%#v focused=%v", model.dialog, model.pending, model.editor.Focused())
	}
}

func TestSessionLifecycleEventsDriveActionsAndClearBarrier(t *testing.T) {
	model := New(demo.New(), "")
	model.screen = screenChat
	model.session = &mango.Session{ID: "sesn_1", Status: "idle"}
	model.sessions = []mango.Session{{ID: "sesn_1", Status: "idle"}}
	model.sessionAction = mango.Session{ID: "sesn_1", Status: "idle"}
	model.threads = []mango.Thread{{ID: "sthr_primary", Status: "idle"}}
	model.pending = []mango.Action{{ID: "sevt_tool", Kind: mango.ActionConfirmation}}
	model.pendingSignature = "tool_confirmation:sevt_tool"
	model.dialog = dialogAction

	model.applyStream(mango.StreamUpdate{ThreadID: "sthr_primary", Frame: mango.Event{
		"id": "sevt_running", "type": "session.status_running",
	}})
	if model.session.Status != "running" || model.sessions[0].Status != "running" || model.sessionAction.Status != "running" {
		t.Fatalf("statuses session=%q list=%q action=%q", model.session.Status, model.sessions[0].Status, model.sessionAction.Status)
	}
	if len(model.pending) != 0 || model.dialog != dialogNone {
		t.Fatalf("running barrier pending=%#v dialog=%v", model.pending, model.dialog)
	}
	actions := model.sessionActions()
	if actions[1].disabled || !actions[2].disabled || !actions[3].disabled {
		t.Fatalf("running actions = %#v", actions)
	}

	model.applyStream(mango.StreamUpdate{ThreadID: "sthr_primary", Frame: mango.Event{
		"id": "sevt_idle", "type": "session.status_idle", "stop_reason": map[string]any{"type": "end_turn"},
	}})
	if model.session.Status != "idle" || model.sessionAction.Status != "idle" {
		t.Fatalf("idle statuses session=%q action=%q", model.session.Status, model.sessionAction.Status)
	}
	actions = model.sessionActions()
	if !actions[1].disabled || actions[2].disabled || actions[3].disabled {
		t.Fatalf("idle actions = %#v", actions)
	}
}

func TestChildPartialActionResolutionAdvancesToRemainingAction(t *testing.T) {
	model := New(demo.New(), "")
	model.screen = screenChat
	model.session = &mango.Session{ID: "sesn_1", Status: "idle"}
	parent := "sthr_primary"
	model.threads = []mango.Thread{
		{ID: parent, Status: "idle"},
		{ID: "sthr_child", ParentThreadID: &parent, Status: "idle"},
	}
	model.events[parent] = []mango.Event{
		{"id": "sevt_action_1", "type": "agent.tool_use", "name": "bash", "evaluated_permission": "ask", "session_thread_id": "sthr_child"},
		{"id": "sevt_action_2", "type": "agent.tool_use", "name": "write", "evaluated_permission": "ask", "session_thread_id": "sthr_child"},
		{"id": "sevt_idle", "type": "session.status_idle", "stop_reason": map[string]any{
			"type": "requires_action", "event_ids": []any{"sevt_action_1", "sevt_action_2"},
		}},
	}
	model.refreshPending()
	model.dialog = dialogAction
	model.activeOperation = 1

	updated, _ := model.Update(operationDone{
		sessionID: "sesn_1", operationID: 1, label: "action resolved", resolvedID: "sevt_action_1",
		events: []mango.Event{{
			"id": "sevt_resolution_1", "type": "user.tool_confirmation", "tool_use_id": "sevt_action_1",
			"result": "allow", "session_thread_id": "sthr_child",
		}},
	})
	model = updated.(Model)
	if len(model.pending) != 1 || model.pending[0].ID != "sevt_action_2" || model.dialog != dialogAction {
		t.Fatalf("partial pending=%#v dialog=%v", model.pending, model.dialog)
	}
	if len(model.events["sthr_child"]) != 1 || model.events["sthr_child"][0].ID() != "sevt_resolution_1" {
		t.Fatalf("child events = %#v", model.events["sthr_child"])
	}

	model.activeOperation = 2
	updated, _ = model.Update(operationDone{
		sessionID: "sesn_1", operationID: 2, label: "action resolved", resolvedID: "sevt_action_2",
		events: []mango.Event{{
			"id": "sevt_resolution_2", "type": "user.tool_confirmation", "tool_use_id": "sevt_action_2",
			"result": "allow", "session_thread_id": "sthr_child",
		}},
	})
	model = updated.(Model)
	if len(model.pending) != 0 || model.dialog != dialogNone || model.session.Status != "idle" {
		t.Fatalf("resolved pending=%#v dialog=%v status=%q", model.pending, model.dialog, model.session.Status)
	}
	model.applyStream(mango.StreamUpdate{ThreadID: parent, Frame: mango.Event{
		"id": "sevt_running", "type": "session.status_running",
	}})
	if model.session.Status != "running" || len(model.pending) != 0 || model.dialog != dialogNone {
		t.Fatalf("SSE status=%q pending=%#v dialog=%v", model.session.Status, model.pending, model.dialog)
	}
}

func TestExternalSessionDeletionReturnsToInbox(t *testing.T) {
	model := New(demo.New(), "")
	updates := make(chan mango.StreamUpdate)
	streamCanceled := false
	model.screen = screenChat
	model.session = &mango.Session{ID: "sesn_deleted"}
	model.sessions = []mango.Session{{ID: "sesn_deleted"}}
	model.threads = []mango.Thread{{ID: "sthr_primary"}}
	model.stream = updates
	model.streamCancel = func() { streamCanceled = true }
	model.reconnecting = true

	updated, command := model.Update(streamUpdate{
		source: updates, open: true,
		update: mango.StreamUpdate{ThreadID: "sthr_primary", Frame: mango.Event{"id": "sevt_deleted", "type": "session.deleted"}},
	})
	model = updated.(Model)
	if command == nil || !streamCanceled || model.session != nil || model.screen != screenInbox || model.reconnecting {
		t.Fatalf("command=%v canceled=%v session=%#v screen=%v reconnecting=%v", command, streamCanceled, model.session, model.screen, model.reconnecting)
	}
	if len(model.sessions) != 0 || model.status != "Session deleted by another client" {
		t.Fatalf("sessions=%#v status=%q", model.sessions, model.status)
	}
}

func TestReconnectNotFoundStopsRetryLoop(t *testing.T) {
	model := New(demo.New(), "")
	model.session = &mango.Session{ID: "sesn_deleted"}
	model.reconnecting = true
	model.activeAttach, model.attachTarget = 7, "sesn_deleted"
	requestCanceled := false
	model.attachCancel = func() { requestCanceled = true }
	updated, command := model.Update(attachedLoaded{
		sessionID: "sesn_deleted", attachID: 7,
		err: &mango.APIError{StatusCode: 404, Status: "404 Not Found", Detail: "not found"},
	})
	model = updated.(Model)
	if command == nil || !requestCanceled || model.session != nil || model.reconnecting || model.screen != screenInbox {
		t.Fatalf("command=%v canceled=%v session=%#v reconnecting=%v screen=%v", command, requestCanceled, model.session, model.reconnecting, model.screen)
	}
}

func TestConversationUsesRailsWithoutRoleHeaders(t *testing.T) {
	model := New(demo.New(), "")
	user := model.renderFeedItem(feed.Item{Kind: feed.User, Title: "You", Body: "hello"}, false, 80)
	agent := model.renderFeedItem(feed.Item{Kind: feed.Agent, Title: "coordinator", Body: "hi"}, false, 80)
	if strings.Contains(user, "You") || strings.Contains(agent, "coordinator") || !strings.Contains(user, "▌") {
		t.Fatalf("user=%q agent=%q", user, agent)
	}
}

func TestMarkdownCacheEvictsIncrementallyAtCapacity(t *testing.T) {
	model := New(demo.New(), "")
	for index := range markdownCacheLimit + 2 {
		model.renderMarkdown(fmt.Sprintf("message %d", index), 80, true)
	}
	if len(model.markdown.entries) != markdownCacheLimit || len(model.markdown.order) != markdownCacheLimit {
		t.Fatalf("entries=%d order=%d", len(model.markdown.entries), len(model.markdown.order))
	}
	if _, ok := model.markdown.entries[markdownCacheKey{width: 80, content: "message 0"}]; ok {
		t.Fatal("oldest cache entry was not evicted")
	}
	if _, ok := model.markdown.entries[markdownCacheKey{width: 80, content: fmt.Sprintf("message %d", markdownCacheLimit+1)}]; !ok {
		t.Fatal("newest cache entry is missing")
	}
}

func TestStreamingPreviewGrowsInPlaceThenBecomesDurable(t *testing.T) {
	model := New(demo.New(), "")
	model.width, model.height, model.screen = 100, 30, screenChat
	model.threads = []mango.Thread{{ID: "sthr_primary"}}
	model.threadCursor = 0
	model.resize()
	model.applyStream(mango.StreamUpdate{ThreadID: "sthr_primary", Frame: mango.Event{
		"type": "event_start", "event": map[string]any{"id": "sevt_reply", "type": "agent.message"},
	}})
	for _, text := range []string{"hello ", "world"} {
		model.applyStream(mango.StreamUpdate{ThreadID: "sthr_primary", Frame: mango.Event{
			"type": "event_delta", "event_id": "sevt_reply",
			"delta": map[string]any{"content": map[string]any{"text": text}},
		}})
	}
	if model.previews["sthr_primary"] == nil || model.previews["sthr_primary"].content != "hello world" {
		t.Fatalf("preview = %#v", model.previews["sthr_primary"])
	}
	model.applyStream(mango.StreamUpdate{ThreadID: "sthr_primary", Frame: mango.Event{
		"id": "sevt_reply", "type": "agent.message",
		"content": []any{map[string]any{"type": "text", "text": "hello world"}},
	}})
	if model.previews["sthr_primary"] != nil || len(model.events["sthr_primary"]) != 1 {
		t.Fatalf("preview=%#v events=%#v", model.previews["sthr_primary"], model.events["sthr_primary"])
	}
}

func TestThinkingFruitTransitionsIntoStreamingReply(t *testing.T) {
	model := New(demo.New(), "")
	model.width, model.height, model.screen = 100, 30, screenChat
	model.session = &mango.Session{ID: "sesn_live"}
	model.threads = []mango.Thread{{ID: "sthr_primary"}}
	model.threadCursor = 0
	model.resize()

	model.beginLiveTurn("sthr_primary")
	if plain := ansi.Strip(model.chat.View()); !strings.Contains(plain, "🥭") || !strings.Contains(plain, "Thinking") {
		t.Fatalf("optimistic thinking state = %q", plain)
	}
	model.applyStream(mango.StreamUpdate{ThreadID: "sthr_primary", Frame: mango.Event{
		"type": "event_start", "event": map[string]any{"id": "sevt_think", "type": "agent.thinking"},
	}})
	if model.previews["sthr_primary"].thinkingID != "sevt_think" {
		t.Fatalf("thinking preview = %#v", model.previews["sthr_primary"])
	}
	model.applyStream(mango.StreamUpdate{ThreadID: "sthr_primary", Frame: mango.Event{
		"type": "event_start", "event": map[string]any{"id": "sevt_reply", "type": "agent.message"},
	}})
	model.applyStream(mango.StreamUpdate{ThreadID: "sthr_primary", Frame: mango.Event{
		"type": "event_delta", "event_id": "sevt_reply",
		"delta": map[string]any{"content": map[string]any{"text": "streaming now"}},
	}})
	current := model.previews["sthr_primary"]
	if current == nil || current.thinkingID != "" || current.messageID != "sevt_reply" || current.content != "streaming now" {
		t.Fatalf("reply preview = %#v", current)
	}
	if plain := ansi.Strip(model.chat.View()); !strings.Contains(plain, "streaming now") || !strings.Contains(plain, "▌") {
		t.Fatalf("streaming view = %q", plain)
	}
}

func TestStreamingDeltaRecoversWhenStartWasMissed(t *testing.T) {
	model := New(demo.New(), "")
	model.width, model.height, model.screen = 100, 30, screenChat
	model.threads = []mango.Thread{{ID: "sthr_primary"}}
	model.threadCursor = 0
	model.resize()
	model.applyStream(mango.StreamUpdate{ThreadID: "sthr_primary", Frame: mango.Event{
		"type": "event_delta", "event_id": "sevt_reply",
		"delta": map[string]any{"content": map[string]any{"text": "recovered"}},
	}})
	current := model.previews["sthr_primary"]
	if current == nil || current.messageID != "sevt_reply" || current.content != "recovered" {
		t.Fatalf("recovered preview = %#v", current)
	}
}

func TestConversationFooterOwnsModelAndLiveUsage(t *testing.T) {
	model := New(demo.New(), "")
	model.width, model.height, model.screen = 140, 42, screenChat
	model.session = &mango.Session{ID: "sesn_usage"}
	model.threads = []mango.Thread{{ID: "sthr_primary"}}
	model.threads[0].Agent.Model.ID = "deepseek-v4-flash"
	model.threadCursor = 0
	model.resize()
	model.applyStream(mango.StreamUpdate{ThreadID: "sthr_primary", Frame: mango.Event{
		"id": "sevt_usage", "type": "span.model_request_end",
		"model_usage": map[string]any{"input_tokens": float64(1200), "output_tokens": float64(345), "cache_read_input_tokens": float64(90)},
	}})

	footer := ansi.Strip(model.renderConversationInfo(108))
	if !strings.Contains(footer, "deepseek-v4-flash") || !strings.Contains(footer, "1.2K in") || !strings.Contains(footer, "345 out") {
		t.Fatalf("conversation footer = %q", footer)
	}
	if model.session.Usage.InputTokens != 1200 || model.threads[0].Usage.CacheReadInputTokens != 90 {
		t.Fatalf("usage session=%#v thread=%#v", model.session.Usage, model.threads[0].Usage)
	}
}

func TestNewSessionWizardIsFocusedCenterCard(t *testing.T) {
	model := New(demo.New(), "")
	model.width, model.height, model.screen = 140, 42, screenInbox
	updated, command := model.startNewSession()
	model = updated.(Model)
	if command == nil || !model.dialogIsOverlay() || model.dialog != dialogNewSession {
		t.Fatalf("dialog=%v overlay=%v command=%v", model.dialog, model.dialogIsOverlay(), command)
	}
	if width := model.dialogWidth(); width != 66 {
		t.Fatalf("creation card width=%d, want 66", width)
	}
	modal := model.renderDialog()
	if lipgloss.Width(modal) >= model.width-20 {
		t.Fatalf("creation card width=%d terminal=%d", lipgloss.Width(modal), model.width)
	}

	model.loading = false
	model.creation.step = createSessionTitle
	model.creation.agent.Name = "researcher"
	model.creation.agent.Model.ID = "deepseek-v4-flash"
	model.creation.environment.Name = "Mango cloud"
	model.editor.SetValue("研究历")
	model.editor.Focus()
	model.resize()
	modal = model.renderDialog()
	left := max(0, (model.width-lipgloss.Width(modal))/2)
	top := max(0, (model.height-lipgloss.Height(modal))/2)
	view := model.View()
	if view.Cursor == nil {
		t.Fatal("creation card did not expose its textarea cursor")
	}
	source := model.editor.Cursor()
	wantX, wantY := left+5+source.X, top+model.dialogEditorY()+source.Y
	if view.Cursor.X != wantX || view.Cursor.Y != wantY {
		t.Fatalf("creation cursor=(%d,%d), want text position (%d,%d)", view.Cursor.X, view.Cursor.Y, wantX, wantY)
	}

	plainLines := strings.Split(ansi.Strip(view.Content), "\n")
	titleX := -1
	for _, line := range plainLines {
		if index := strings.Index(line, "New Session"); index >= 0 {
			titleX = ansi.StringWidth(line[:index])
			break
		}
	}
	if titleX != left+3 {
		t.Fatalf("creation card title x=%d, want centered card x=%d", titleX, left+3)
	}
}

func TestNewSessionWizardCreatesAndAttaches(t *testing.T) {
	model := New(demo.New(), "")
	model.loading = false
	model.width, model.height = 100, 32
	model.resize()

	updated, command := model.updateInbox(tea.KeyPressMsg(tea.Key{Code: 'n', Mod: tea.ModCtrl}))
	model = updated.(Model)
	if model.dialog != dialogNewSession || command == nil {
		t.Fatalf("dialog=%v command=%v", model.dialog, command)
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if model.creation.step != createChooseAgent || len(model.creation.agents) == 0 {
		t.Fatalf("creation = %#v", model.creation)
	}

	updated, _ = model.updateNewSession(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	updated, _ = model.updateNewSession(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if model.creation.step != createSessionTitle {
		t.Fatalf("step = %v", model.creation.step)
	}
	model.editor.SetValue("Created in TUI")
	updated, _ = model.updateNewSession(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	model.editor.SetValue("hello from the wizard")
	updated, _ = model.updateNewSession(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if model.creation.step != createConfirm {
		t.Fatalf("step = %v", model.creation.step)
	}
	updated, command = model.updateNewSession(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	updated, attach := model.Update(command())
	model = updated.(Model)
	if attach == nil {
		t.Fatal("created Session was not attached")
	}
	updated, _ = model.Update(attach())
	model = updated.(Model)
	if model.screen != screenChat || model.session == nil || model.session.Title != "Created in TUI" {
		t.Fatalf("screen=%v session=%#v err=%v", model.screen, model.session, model.err)
	}
	if got := model.events[model.primaryThreadID()]; len(got) != 1 || got[0].Type() != "user.message" {
		t.Fatalf("initial events = %#v", got)
	}
}

func TestCreationPickersUseFuzzyFiltering(t *testing.T) {
	model := New(demo.New(), "")
	model.dialog = dialogNewSession
	updated, _ := model.resourcesLoaded(creationResourcesLoaded{agents: []mango.Agent{
		{ID: "agent_reviewer", Name: "reviewer"},
		{ID: "agent_researcher", Name: "researcher"},
	}})
	model = updated.(Model)
	for _, char := range "rvr" {
		updated, _ = model.updateNewSession(tea.KeyPressMsg(tea.Key{Code: char, Text: string(char)}))
		model = updated.(Model)
	}
	filtered := model.filteredCreationAgents()
	if len(filtered) != 1 || filtered[0].Name != "reviewer" {
		t.Fatalf("filtered Agents = %#v", filtered)
	}
}

func TestConnectThenHomeStartsWithVisibleCreateChoice(t *testing.T) {
	model := New(demo.New(), "")
	if model.screen != screenConnect || model.loading {
		t.Fatalf("initial screen=%v loading=%v", model.screen, model.loading)
	}
	updated, command := model.updateConnect(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if command == nil || !model.loading {
		t.Fatalf("connect command=%v loading=%v", command, model.loading)
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if model.screen != screenInbox || len(model.sessions) == 0 || model.inboxCursor != 0 {
		t.Fatalf("screen=%v sessions=%d cursor=%d err=%v", model.screen, len(model.sessions), model.inboxCursor, model.err)
	}
	updated, command = model.updateInbox(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if model.dialog != dialogNewSession || command == nil {
		t.Fatalf("create choice dialog=%v command=%v", model.dialog, command)
	}
}

func TestCommandsUseSearchablePicker(t *testing.T) {
	model := New(demo.New(), "")
	model.session = &mango.Session{ID: "sesn_1"}
	model.openCommands()
	for _, char := range "swtchagt" {
		updated, _ := model.updateDialog(tea.KeyPressMsg(tea.Key{Code: char, Text: string(char)}))
		model = updated.(Model)
	}
	commands := model.filteredCommands()
	if len(commands) != 1 || commands[0].id != "agents" {
		t.Fatalf("commands = %#v", commands)
	}
}

func TestBackgroundTurnCompletionUsesTerminalNotification(t *testing.T) {
	model := NewWithOptions(demo.New(), "", Options{Notifications: NotificationOSC777})
	model.session = &mango.Session{Title: "Launch review"}
	model.windowFocused = false
	command := model.notificationFor(mango.StreamUpdate{Frame: mango.Event{
		"id": "sevt_idle", "type": "session.status_idle",
		"stop_reason": map[string]any{"type": "end_turn"},
	}})
	if command == nil {
		t.Fatal("missing notification command")
	}
	raw, ok := command().(tea.RawMsg)
	if !ok || !strings.Contains(raw.Msg.(string), "Mango finished a turn") {
		t.Fatalf("notification = %#v", raw)
	}
	model.windowFocused = true
	if command := model.notificationFor(mango.StreamUpdate{Frame: mango.Event{"id": "sevt_idle", "type": "session.status_idle"}}); command != nil {
		t.Fatal("focused window should not notify")
	}
}

func TestNotificationsAreDisabledUnlessExplicitlyEnabled(t *testing.T) {
	model := New(demo.New(), "")
	model.session = &mango.Session{Title: "Launch review"}
	model.windowFocused = false
	if command := model.notificationFor(mango.StreamUpdate{Frame: mango.Event{
		"id": "sevt_idle", "type": "session.status_idle",
	}}); command != nil {
		t.Fatal("default notification mode must not emit terminal escape sequences")
	}
}

func TestScrollThumbGeometry(t *testing.T) {
	// Everything fits: the thumb fills the gutter.
	if pos, size := scrollThumb(10, 8, 0); pos != 0 || size != 10 {
		t.Fatalf("all-visible thumb=(%d,%d), want (0,10)", pos, size)
	}
	// 10% visible: a one-row thumb that starts at the top and reaches the
	// bottom row at the maximum offset.
	if _, size := scrollThumb(10, 100, 0); size != 1 {
		t.Fatalf("thumb size=%d, want 1", size)
	}
	if pos, _ := scrollThumb(10, 100, 0); pos != 0 {
		t.Fatalf("top thumb pos=%d, want 0", pos)
	}
	if pos, _ := scrollThumb(10, 100, 90); pos != 9 {
		t.Fatalf("bottom thumb pos=%d, want 9", pos)
	}
}

func TestConversationScrollbarTracksFocusAndPosition(t *testing.T) {
	model := New(demo.New(), "")
	model.loading = false
	model.width, model.height, model.screen = 140, 42, screenChat
	model.threads = []mango.Thread{{ID: "sthr_primary"}}
	model.threadCursor = 0
	model.resize()
	model.chat.SetContent(strings.Repeat("conversation line\n", 300))
	model.refreshChatMetrics()
	model.focus = focusChat

	width := model.workspaceMainWidth()
	model.chat.GotoTop()
	top := model.renderConversationViewport(width)
	if lipgloss.Width(top) != width {
		t.Fatalf("viewport width=%d, want %d", lipgloss.Width(top), width)
	}
	// The focused conversation carries an accent scrollbar with a track and a
	// thumb, mirroring the Crush gutter.
	if !strings.Contains(top, "255;138;61") {
		t.Fatalf("focused scrollbar is not accent: %q", ansi.Strip(top))
	}
	plainTop := ansi.Strip(top)
	if !strings.Contains(plainTop, "█") || !strings.Contains(plainTop, "│") {
		t.Fatalf("scrollbar missing thumb/track: %q", plainTop)
	}

	model.chat.GotoBottom()
	plainBottom := ansi.Strip(model.renderConversationViewport(width))
	if firstRowWith(plainTop, '█') == firstRowWith(plainBottom, '█') {
		t.Fatalf("scrollbar thumb did not move between top and bottom")
	}
}

func firstRowWith(block string, target rune) int {
	for index, line := range strings.Split(block, "\n") {
		if strings.ContainsRune(line, target) {
			return index
		}
	}
	return -1
}

func TestManageDialogTitleHasNoGradient(t *testing.T) {
	model := New(demo.New(), "")
	model.width, model.height, model.screen = 100, 30, screenInbox
	model.openSessionManager(mango.Session{ID: "sesn_x", Title: "Launch"}, dialogNone)
	model.resize()
	for _, line := range strings.Split(model.renderDialog(), "\n") {
		if strings.Contains(ansi.Strip(line), "Manage Session") {
			if strings.Contains(line, "255;157;24") || strings.Contains(line, "49;87;213") {
				t.Fatalf("manage title still uses the fruit→link gradient: %q", line)
			}
			return
		}
	}
	t.Fatal("manage dialog has no title line")
}

func TestActivitySpinnerHasNoGradient(t *testing.T) {
	model := New(demo.New(), "")
	// theme.blue (#6F8CFF → 111;140;255) is the far end of the old spinner
	// gradient; a solid spinner must not blend toward it.
	if spinner := model.activity("Connecting"); strings.Contains(spinner, "111;140;255") {
		t.Fatalf("activity spinner still blends toward blue: %q", spinner)
	}
}

func TestQuitDialogTitleHasNoGradient(t *testing.T) {
	model := New(demo.New(), "")
	model.width, model.height, model.screen = 100, 30, screenChat
	model.resize()
	model.openQuitDialog()
	modal := model.renderDialog()
	titleLine := ""
	for _, line := range strings.Split(modal, "\n") {
		if strings.Contains(ansi.Strip(line), "Quit Mango?") {
			titleLine = line
			break
		}
	}
	if titleLine == "" {
		t.Fatal("quit dialog has no title line")
	}
	// The brand wordmark blends the fruit (#FF9D18) and link (#3157D5) colors
	// across the title. Quitting should feel calm, so neither gradient endpoint
	// should appear on the title line.
	if strings.Contains(titleLine, "255;157;24") || strings.Contains(titleLine, "49;87;213") {
		t.Fatalf("quit title still uses the fruit→link gradient: %q", titleLine)
	}
}

func TestQuitUsesSafeCentralConfirmation(t *testing.T) {
	model := New(demo.New(), "")
	model.width, model.height, model.screen = 100, 30, screenChat
	model.resize()

	updated, command := model.Update(tea.KeyPressMsg(tea.Key{Code: 'c', Mod: tea.ModCtrl}))
	model = updated.(Model)
	if command != nil || model.dialog != dialogQuit || model.dialogCursor != 1 || !model.dialogIsOverlay() {
		t.Fatalf("quit prompt command=%v dialog=%v cursor=%d", command, model.dialog, model.dialogCursor)
	}
	dialog := ansi.Strip(model.renderDialog())
	if !strings.Contains(dialog, "Quit Mango?") || !strings.Contains(dialog, "keep running in the") ||
		!strings.Contains(dialog, "cloud.") ||
		!strings.Contains(dialog, "Keep working") {
		t.Fatalf("quit dialog = %q", dialog)
	}

	updated, command = model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if command != nil || model.dialog != dialogNone {
		t.Fatalf("safe default command=%v dialog=%v", command, model.dialog)
	}
}

func TestQuitConfirmationSupportsChoiceAndDoubleControlC(t *testing.T) {
	model := New(demo.New(), "")
	model.width, model.height, model.screen = 100, 30, screenChat
	model.resize()
	model.openQuitDialog()

	updated, _ := model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyLeft}))
	model = updated.(Model)
	updated, command := model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if command == nil {
		t.Fatal("selected Quit did not return a quit command")
	}
	if _, ok := command().(tea.QuitMsg); !ok {
		t.Fatal("selected Quit did not emit tea.QuitMsg")
	}

	model = New(demo.New(), "")
	model.openQuitDialog()
	updated, command = model.Update(tea.KeyPressMsg(tea.Key{Code: 'c', Mod: tea.ModCtrl}))
	if command == nil {
		t.Fatal("second ctrl+c did not return a quit command")
	}
	if _, ok := command().(tea.QuitMsg); !ok {
		t.Fatal("second ctrl+c did not emit tea.QuitMsg")
	}
}

func TestQuitCancelRestoresUnderlyingDialog(t *testing.T) {
	model := New(demo.New(), "")
	model.dialog = dialogHelp
	model.openQuitDialog()
	updated, command := model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	model = updated.(Model)
	if command != nil || model.dialog != dialogHelp {
		t.Fatalf("cancel command=%v dialog=%v", command, model.dialog)
	}
}

func TestHomeRefreshIsAVisibleChoice(t *testing.T) {
	model := New(demo.New(), "")
	model.screen, model.loading, model.inboxCursor = screenInbox, false, 2
	updated, command := model.updateInbox(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if command == nil || !model.loading || model.loadingLabel != "Refreshing Sessions" {
		t.Fatalf("command=%v loading=%v label=%q", command, model.loading, model.loadingLabel)
	}
}

func TestTextDialogRendersEditorOnlyOnce(t *testing.T) {
	model := New(demo.New(), "")
	model.width, model.height, model.screen = 100, 30, screenChat
	model.session = &mango.Session{ID: "sesn_1"}
	model.threads = []mango.Thread{{ID: "sthr_primary"}}
	model.pending = []mango.Action{{ID: "sevt_tool", Kind: mango.ActionToolResult}}
	model.dialog = dialogResult
	model.editor.SetValue("unique-modal-draft")
	model.resize()
	if got := strings.Count(model.View().Content, "unique-modal-draft"); got != 1 {
		t.Fatalf("editor rendered %d times", got)
	}
}

func TestFailedSendPreservesDraftAndReadyPlaceholder(t *testing.T) {
	backend := failingSendBackend{Backend: demo.New()}
	model := New(backend, "")
	model.screen, model.loading = screenChat, false
	model.session = &mango.Session{ID: "sesn_1"}
	model.threads = []mango.Thread{{ID: "sthr_primary"}}
	model.editor.SetValue("do not lose this")
	updated, command := model.updateChat(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if command == nil || model.editor.Value() != "" {
		t.Fatalf("command=%v draft=%q", command, model.editor.Value())
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if model.editor.Value() != "do not lose this" || model.editor.Placeholder != "Message coordinator…" || model.err == nil {
		t.Fatalf("draft=%q placeholder=%q err=%v", model.editor.Value(), model.editor.Placeholder, model.err)
	}
}

func TestOldOperationCannotMutateNewSession(t *testing.T) {
	model := New(demo.New(), "")
	model.session = &mango.Session{ID: "sesn_new"}
	updated, _ := model.Update(operationDone{
		sessionID: "sesn_old", label: "message sent",
		events: []mango.Event{{"id": "sevt_old", "type": "agent.message"}},
	})
	model = updated.(Model)
	if len(model.events) != 0 || model.status != "" {
		t.Fatalf("stale operation mutated model: events=%#v status=%q", model.events, model.status)
	}
}

func TestWideCharacterTruncationFitsTerminalCells(t *testing.T) {
	got := truncate("研究者🍊协调员", 8)
	if width := ansi.StringWidth(got); width > 8 {
		t.Fatalf("truncate width=%d value=%q", width, got)
	}
}

func TestSmallTerminalUsesBoundedFallback(t *testing.T) {
	model := New(demo.New(), "")
	model.width, model.height = 30, 10
	view := model.View().Content
	for _, line := range strings.Split(view, "\n") {
		if width := ansi.StringWidth(line); width > model.width {
			t.Fatalf("line width=%d exceeds %d: %q", width, model.width, line)
		}
	}
}

func TestLargeActionInputCannotGrowDialogPastTerminal(t *testing.T) {
	model := New(demo.New(), "")
	model.width, model.height, model.screen = 60, 20, screenChat
	model.threads = []mango.Thread{{ID: "sthr_primary"}}
	model.pending = []mango.Action{{
		ID: "sevt_tool", Kind: mango.ActionConfirmation, Name: "write_file",
		Input: strings.Repeat("a very long tool argument that must wrap safely ", 2000),
	}}
	model.dialog = dialogAction
	model.resize()
	dialog := model.renderDialog()
	if height := lipgloss.Height(dialog); height > model.height {
		t.Fatalf("dialog height=%d terminal=%d", height, model.height)
	}
	if width := lipgloss.Width(dialog); width > model.width {
		t.Fatalf("dialog width=%d terminal=%d", width, model.width)
	}
	if !strings.Contains(dialog, "more line") {
		t.Fatalf("large input was not summarized: %q", dialog)
	}
}

func TestTextDialogsStayInsideMinimumTerminal(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*Model)
	}{
		{
			name: "tool result",
			setup: func(model *Model) {
				model.pending = []mango.Action{{ID: "sevt_tool", Kind: mango.ActionToolResult}}
				model.dialog = dialogResult
			},
		},
		{
			name: "creation field",
			setup: func(model *Model) {
				model.dialog = dialogNewSession
				model.creation.step = createInitialPrompt
				model.creation.agent = mango.Agent{ID: "agent_1", Name: "coordinator"}
				model.creation.environment = mango.Environment{ID: "env_1", Name: "Mango cloud"}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := New(demo.New(), "")
			model.width, model.height, model.screen = 60, 20, screenChat
			test.setup(&model)
			model.editor.SetValue(strings.Repeat("line of text\n", 50))
			model.resize()
			dialog := model.renderDialog()
			if height := lipgloss.Height(dialog); height > model.height {
				t.Fatalf("dialog height=%d terminal=%d", height, model.height)
			}
			if width := lipgloss.Width(dialog); width > model.width {
				t.Fatalf("dialog width=%d terminal=%d", width, model.width)
			}
		})
	}
}

func TestLongNamesStayInsideMinimumTerminalDialogs(t *testing.T) {
	longName := strings.Repeat("研究型协调 Agent 🍊 ", 30)
	tests := []struct {
		name  string
		setup func(*Model)
	}{
		{
			name: "Agent picker",
			setup: func(model *Model) {
				model.dialog = dialogAgents
				model.threads = []mango.Thread{{ID: "sthr_1", Status: "idle", Agent: mango.ThreadAgent{Name: longName}}}
			},
		},
		{
			name: "creation picker",
			setup: func(model *Model) {
				model.dialog = dialogNewSession
				model.creation.step = createChooseAgent
				model.creation.agents = []mango.Agent{{ID: "agent_1", Name: longName}}
			},
		},
		{
			name: "interrupt",
			setup: func(model *Model) {
				model.dialog = dialogInterrupt
				model.threads = []mango.Thread{{ID: "sthr_1", Agent: mango.ThreadAgent{Name: longName}}}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := New(demo.New(), "")
			model.width, model.height, model.screen = 60, 20, screenChat
			test.setup(&model)
			model.resize()
			dialog := model.renderDialog()
			if width, height := lipgloss.Width(dialog), lipgloss.Height(dialog); width > model.width || height > model.height {
				t.Fatalf("dialog=%dx%d terminal=%dx%d", width, height, model.width, model.height)
			}
		})
	}
}

func TestThreadCreatedRequestsAnEventDrivenRosterRefresh(t *testing.T) {
	model := New(demo.New(), "")
	model.threads = []mango.Thread{{ID: "sthr_primary"}}
	changed := model.applyStream(mango.StreamUpdate{ThreadID: "sthr_primary", Frame: mango.Event{
		"id": "sevt_created", "type": "session.thread_created",
		"session_thread_id": "sthr_child", "agent_name": "reviewer",
	}})
	if !changed {
		t.Fatal("thread creation did not request a roster refresh")
	}
}

func TestAsyncActionDialogHasBoundedGraceAndSkipsImmediateReopen(t *testing.T) {
	model := New(demo.New(), "")
	model.pending = []mango.Action{{ID: "sevt_tool", Kind: mango.ActionConfirmation}}
	model.openActionDialog(true)
	updated, command := model.updateDialog(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if command != nil || model.dialog != dialogAction {
		t.Fatal("in-flight enter should be absorbed")
	}
	model.dialogGraceOpenedAt = time.Now().Add(-actionGraceMaxDelay - time.Millisecond)
	model.dialogGraceLastInputAt = time.Now()
	if model.actionDialogInGrace() {
		t.Fatal("absolute grace ceiling did not arm dialog")
	}
	model.closeActionDialog()
	model.dialog = dialogNone
	model.openActionDialog(true)
	if model.actionDialogInGrace() {
		t.Fatal("immediate same-dialog reopen should skip grace")
	}
}

func TestFreshAgentEventGetsOneShotSpark(t *testing.T) {
	model := New(demo.New(), "")
	model.threads = []mango.Thread{{ID: "sthr_primary"}}
	model.applyStream(mango.StreamUpdate{ThreadID: "sthr_primary", Frame: mango.Event{
		"id": "sevt_reply", "type": "agent.message",
		"content": []any{map[string]any{"type": "text", "text": "done"}},
	}})
	if model.fresh["sevt_reply"] == 0 || model.spark("sevt_reply") == "" {
		t.Fatalf("fresh = %#v", model.fresh)
	}
}

func TestNewSessionWizardCanCreateMissingResources(t *testing.T) {
	model := New(demo.New(), "")
	model.dialog = dialogNewSession
	updated, _ := model.resourcesLoaded(creationResourcesLoaded{})
	model = updated.(Model)
	if model.creation.step != createChooseAgent || model.creation.agentCursor != 0 {
		t.Fatalf("step = %v", model.creation.step)
	}
	updated, _ = model.updateNewSession(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if model.creation.step != createAgentName {
		t.Fatalf("create Agent choice step = %v", model.creation.step)
	}

	model.editor.SetValue("fresh coder")
	updated, _ = model.updateNewSession(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	model.editor.SetValue("deepseek-v4-flash")
	updated, _ = model.updateNewSession(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	model.editor.SetValue("Work carefully.")
	updated, _ = model.updateNewSession(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if model.creation.step != createAgentTools {
		t.Fatalf("step after system = %v", model.creation.step)
	}
	updated, _ = model.updateNewSession(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	updated, _ = model.updateNewSession(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	updated, command := model.updateNewSession(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if command == nil {
		t.Fatal("Agent review did not produce a create command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if model.creation.agent.ID == "" || model.creation.step != createChooseEnvironment || model.creation.environmentCursor != 0 {
		t.Fatalf("Agent creation = %#v", model.creation)
	}
	updated, _ = model.updateNewSession(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if model.creation.step != createEnvironmentName {
		t.Fatalf("create Environment choice step = %v", model.creation.step)
	}

	model.editor.SetValue("fresh cloud")
	updated, command = model.updateNewSession(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	updated, _ = model.Update(command())
	model = updated.(Model)
	if model.creation.environment.ID == "" || model.creation.step != createSessionTitle {
		t.Fatalf("Environment creation = %#v", model.creation)
	}
}
