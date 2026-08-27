package ui

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/yanpgwang/mango/examples/terminal-ui/internal/mango"
)

type sessionMutation string

const (
	mutationRename    sessionMutation = "rename"
	mutationInterrupt sessionMutation = "interrupt"
	mutationArchive   sessionMutation = "archive"
	mutationDelete    sessionMutation = "delete"
)

type sessionMutationDone struct {
	operationID uint64
	sessionID   string
	kind        sessionMutation
	session     mango.Session
	err         error
}

type sessionActionItem struct {
	id       sessionMutation
	title    string
	help     string
	danger   bool
	disabled bool
}

func (m Model) sessionActions() []sessionActionItem {
	target := m.sessionAction
	active := target.Status == "running" || target.Status == "rescheduling"
	closeHelp := "remove from the active Session list"
	deleteHelp := "permanently remove the Session and event history"
	if active {
		closeHelp = "interrupt active work first"
		deleteHelp = "interrupt active work first"
	}
	return []sessionActionItem{
		{id: mutationRename, title: "Rename", help: "change the Session title"},
		{id: mutationInterrupt, title: "Interrupt all", help: "stop active work; keep history", disabled: !active},
		{id: mutationArchive, title: "Archive", help: closeHelp, disabled: active},
		{id: mutationDelete, title: "Delete permanently", help: deleteHelp, danger: true, disabled: active},
	}
}

func (m Model) sessionManagerDialog() bool {
	switch m.dialog {
	case dialogSessionActions, dialogRenameSession, dialogInterruptSession, dialogArchiveSession, dialogDeleteSession:
		return true
	default:
		return false
	}
}

func (m *Model) openSessionManager(target mango.Session, parent dialog) {
	if target.ID == "" {
		return
	}
	m.sessionAction = target
	m.sessionActionParent = parent
	m.sessionActionCursor = 0
	m.dialog, m.dialogCursor, m.err = dialogSessionActions, 0, nil
	m.filter.Blur()
	m.editor.Blur()
}

func (m Model) updateSessionManagerDialog(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.loading {
		return m, nil
	}
	if m.dialog == dialogRenameSession {
		switch key.String() {
		case "esc":
			m.dialog, m.err = dialogSessionActions, nil
			m.filter.Blur()
			return m, nil
		case "ctrl+u":
			m.filter.Reset()
			return m, nil
		case "enter":
			title := strings.TrimSpace(m.filter.Value())
			if title == "" {
				m.err = fmt.Errorf("Session title cannot be empty")
				return m, nil
			}
			m.loading, m.loadingLabel, m.err = true, "Renaming Session", nil
			ctx, operationID := m.beginOperation()
			return m, m.renameSession(ctx, operationID, m.sessionAction.ID, title)
		default:
			var command tea.Cmd
			m.filter, command = m.filter.Update(key)
			return m, command
		}
	}

	if m.dialog == dialogSessionActions {
		actions := m.sessionActions()
		switch key.String() {
		case "esc":
			m.closeSessionManager()
		case "up", "k":
			m.sessionActionCursor = wrap(m.sessionActionCursor-1, len(actions))
		case "down", "j":
			m.sessionActionCursor = wrap(m.sessionActionCursor+1, len(actions))
		case "enter", "space":
			action := actions[clamp(m.sessionActionCursor, 0, len(actions)-1)]
			if action.disabled {
				if action.id == mutationInterrupt {
					m.err = fmt.Errorf("this Session has no active work to interrupt")
				} else {
					m.err = fmt.Errorf("interrupt active work before %s", action.id)
				}
				return m, nil
			}
			m.err = nil
			switch action.id {
			case mutationRename:
				m.dialog = dialogRenameSession
				m.beginFilter("Session title", m.sessionAction.Title)
			case mutationInterrupt:
				m.dialog, m.dialogCursor = dialogInterruptSession, 1
			case mutationArchive:
				m.dialog, m.dialogCursor = dialogArchiveSession, 1
			case mutationDelete:
				m.dialog, m.dialogCursor = dialogDeleteSession, 1
			}
		}
		return m, nil
	}

	switch key.String() {
	case "esc":
		m.dialog, m.dialogCursor, m.err = dialogSessionActions, m.sessionActionCursor, nil
		return m, nil
	case "left", "right", "h", "l", "tab", "shift+tab":
		m.dialogCursor = 1 - m.dialogCursor
	case "enter", "space":
		if m.dialogCursor == 1 {
			m.dialog, m.err = dialogSessionActions, nil
			return m, nil
		}
		kind := mutationInterrupt
		label := "Interrupting Session"
		switch m.dialog {
		case dialogArchiveSession:
			kind, label = mutationArchive, "Archiving Session"
		case dialogDeleteSession:
			kind, label = mutationDelete, "Deleting Session"
		}
		m.loading, m.loadingLabel, m.err = true, label, nil
		ctx, operationID := m.beginOperation()
		return m, m.mutateSession(ctx, operationID, m.sessionAction.ID, kind)
	}
	return m, nil
}

func (m *Model) closeSessionManager() {
	parent := m.sessionActionParent
	m.dialog, m.err = parent, nil
	m.sessionAction = mango.Session{}
	m.sessionActionParent = dialogNone
	if parent == dialogSessions {
		m.beginFilter("Filter Sessions", m.inboxFilter)
		return
	}
	m.filter.Blur()
	if m.screen == screenChat {
		m.setFocus(focusEditor)
	}
}

func (m Model) handleSessionMutation(msg sessionMutationDone) (tea.Model, tea.Cmd) {
	if msg.operationID != m.activeOperation || msg.sessionID != m.sessionAction.ID {
		return m, nil
	}
	m.loading, m.loadingLabel, m.err = false, "", msg.err
	m.cancelOperation()
	if msg.err != nil {
		return m, nil
	}
	switch msg.kind {
	case mutationRename:
		listCommand := m.updateSessionSnapshot(msg.session)
		m.sessionAction = msg.session
		m.status = "Session renamed"
		m.dialog = dialogSessionActions
		m.filter.Blur()
		return m, listCommand
	case mutationInterrupt:
		m.status = "interrupt requested"
		m.closeSessionManager()
		return m, nil
	case mutationArchive, mutationDelete:
		label := "Session archived"
		if msg.kind == mutationDelete {
			label = "Session deleted"
		}
		removedID := msg.sessionID
		attachedTarget := m.session != nil && m.session.ID == removedID
		listCommand := m.removeSessionSnapshot(removedID)
		if attachedTarget {
			m.stopStream()
			m.session = nil
			m.threads = nil
			m.events = map[string][]mango.Event{}
			m.previews = map[string]*preview{}
			m.pending = nil
			m.screen = screenInbox
			m.lastAttachedID = ""
			m.editor.Reset()
			m.editor.Blur()
		}
		m.dialog = dialogNone
		m.sessionAction = mango.Session{}
		m.sessionActionParent = dialogNone
		m.status, m.err = label, nil
		m.resize()
		return m, listCommand
	}
	return m, nil
}

func (m *Model) updateSessionSnapshot(updated mango.Session) tea.Cmd {
	for index := range m.sessions {
		if m.sessions[index].ID == updated.ID {
			m.sessions[index] = updated
		}
	}
	if m.session != nil && m.session.ID == updated.ID {
		*m.session = updated
	}
	return m.syncInboxList()
}

func (m *Model) removeSessionSnapshot(id string) tea.Cmd {
	filtered := m.sessions[:0]
	for _, session := range m.sessions {
		if session.ID != id {
			filtered = append(filtered, session)
		}
	}
	m.sessions = filtered
	if m.screen == screenInbox {
		m.inboxCursor = clamp(m.inboxCursor, 0, max(0, len(m.sessions)+2))
	}
	return m.syncInboxList()
}

func (m Model) renderSessionManagerDialog(innerWidth int) (string, string) {
	target := m.sessionAction
	name := truncate(first(target.Title, target.Agent.Name, target.ID), max(8, innerWidth-2))
	identity := m.theme.title.Render(name) + "\n" + m.theme.dim.Render(shortID(target.ID)+" · ") + stateText(m.theme, target.Status)
	content := identity + "\n\n"
	switch m.dialog {
	case dialogSessionActions:
		rows := make([]string, 0, len(m.sessionActions()))
		for index, action := range m.sessionActions() {
			marker := "  "
			style := lipgloss.NewStyle().Foreground(m.theme.text)
			if action.disabled {
				style = m.theme.dim
			} else if action.danger {
				style = m.theme.danger
			}
			if index == m.sessionActionCursor {
				marker = "› "
				if !action.disabled {
					style = m.theme.active
				}
			}
			rows = append(rows, marker+style.Render(action.title)+"\n    "+m.theme.dim.Render(action.help))
		}
		content += strings.Join(rows, "\n") + "\n\n" + m.theme.dim.Render("↑↓ choose  enter continue  esc close")
		if m.err != nil {
			content += "\n\n" + m.theme.danger.Render(trimOneLine(m.err.Error(), innerWidth))
		}
		return "Manage Session", content
	case dialogRenameSession:
		content += m.renderCreationSearch(m.filter.Value(), "Session title") + "\n\n" + m.theme.dim.Render("enter save  ctrl+u clear  esc back")
		if m.loading {
			content += "\n\n" + m.activity("Renaming Session")
		} else if m.err != nil {
			content += "\n\n" + m.theme.danger.Render(trimOneLine(m.err.Error(), innerWidth))
		}
		return "Rename Session", content
	case dialogInterruptSession:
		content += "Stop active work across every Agent?\nThe durable Session and event history stay intact.\n\n" +
			choice(m.theme, "Interrupt all", m.dialogCursor == 0, true) + "  " +
			choice(m.theme, "Keep running", m.dialogCursor == 1, false)
		if m.loading {
			content += "\n\n" + m.activity("Interrupting Session")
		} else if m.err != nil {
			content += "\n\n" + m.theme.danger.Render(trimOneLine(m.err.Error(), innerWidth))
		}
		return "Interrupt Session?", content
	case dialogArchiveSession:
		content += "Remove this Session from the active list?\nIts event history remains stored.\n\n" +
			choice(m.theme, "Archive", m.dialogCursor == 0, true) + "  " +
			choice(m.theme, "Keep Session", m.dialogCursor == 1, false)
		if m.loading {
			content += "\n\n" + m.activity("Archiving Session")
		} else if m.err != nil {
			content += "\n\n" + m.theme.danger.Render(trimOneLine(m.err.Error(), innerWidth))
		}
		return "Archive Session?", content
	case dialogDeleteSession:
		content += m.theme.danger.Render("Permanently delete this Session and its event history?") +
			"\nThis cannot be undone.\n\n" +
			choice(m.theme, "Delete permanently", m.dialogCursor == 0, true) + "  " +
			choice(m.theme, "Keep Session", m.dialogCursor == 1, false)
		if m.loading {
			content += "\n\n" + m.activity("Deleting Session")
		} else if m.err != nil {
			content += "\n\n" + m.theme.danger.Render(trimOneLine(m.err.Error(), innerWidth))
		}
		return "Delete Session?", content
	}
	return "Manage Session", content
}

func (m Model) renameSession(ctx context.Context, operationID uint64, sessionID, title string) tea.Cmd {
	return func() tea.Msg {
		session, err := m.backend.UpdateSessionTitle(ctx, sessionID, title)
		return sessionMutationDone{operationID: operationID, sessionID: sessionID, kind: mutationRename, session: session, err: err}
	}
}

func (m Model) mutateSession(ctx context.Context, operationID uint64, sessionID string, kind sessionMutation) tea.Cmd {
	return func() tea.Msg {
		result := sessionMutationDone{operationID: operationID, sessionID: sessionID, kind: kind}
		switch kind {
		case mutationInterrupt:
			_, result.err = m.backend.Interrupt(ctx, sessionID, "")
		case mutationArchive:
			result.session, result.err = m.backend.ArchiveSession(ctx, sessionID)
		case mutationDelete:
			result.err = m.backend.DeleteSession(ctx, sessionID)
		}
		return result
	}
}
