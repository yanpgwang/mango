package ui

import (
	"fmt"
	"io"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/yanpgwang/mango/examples/terminal-ui/internal/mango"
)

// sessionBrowser lets the official Bubbles list own selection, pagination,
// and fuzzy filtering while Mango keeps control of the surrounding layout.
type sessionBrowser struct {
	list.Model
}

type sessionListItem struct {
	session mango.Session
}

func (item sessionListItem) FilterValue() string {
	session := item.session
	return strings.Join([]string{
		session.Title,
		session.ID,
		session.Status,
		session.Agent.Name,
		session.Agent.Model.ID,
		session.EnvironmentID,
	}, " ")
}

type sessionListDelegate struct {
	theme          theme
	lastAttachedID string
	focused        bool
	now            time.Time
}

func (sessionListDelegate) Height() int  { return 2 }
func (sessionListDelegate) Spacing() int { return 1 }
func (sessionListDelegate) Update(tea.Msg, *list.Model) tea.Cmd {
	return nil
}

func (delegate sessionListDelegate) Render(writer io.Writer, browser list.Model, index int, raw list.Item) {
	item, ok := raw.(sessionListItem)
	if !ok {
		return
	}
	selected := delegate.focused && browser.FilterState() != list.Filtering && index == browser.Index()
	row := renderInboxSessionRow(delegate.theme, delegate.lastAttachedID, item.session, browser.Width(), selected, delegate.now)
	_, _ = fmt.Fprint(writer, row)
}

func newSessionBrowser(t theme) sessionBrowser {
	delegate := sessionListDelegate{theme: t, now: time.Now()}
	browser := list.New(nil, delegate, 1, 1)
	browser.SetShowTitle(false)
	browser.SetShowStatusBar(false)
	browser.SetShowHelp(false)
	browser.SetShowPagination(true)
	browser.SetStatusBarItemName("Session", "Sessions")
	browser.DisableQuitKeybindings()
	browser.InfiniteScrolling = false

	styles := list.DefaultStyles(true)
	styles.TitleBar = lipgloss.NewStyle().PaddingBottom(1)
	styles.Title = t.active
	styles.NoItems = t.dim
	styles.PaginationStyle = lipgloss.NewStyle().PaddingLeft(2)
	styles.ActivePaginationDot = lipgloss.NewStyle().Foreground(t.accent).SetString("•")
	styles.InactivePaginationDot = lipgloss.NewStyle().Foreground(t.muted).SetString("·")
	styles.ArabicPagination = t.dim
	browser.Styles = styles
	browser.Paginator.ActiveDot = styles.ActivePaginationDot.String()
	browser.Paginator.InactiveDot = styles.InactivePaginationDot.String()

	filterStyles := textinput.DefaultDarkStyles()
	filterStyles.Focused.Prompt = t.active
	filterStyles.Focused.Text = lipgloss.NewStyle().Foreground(t.text)
	filterStyles.Focused.Placeholder = t.dim
	filterStyles.Blurred = filterStyles.Focused
	browser.FilterInput.Prompt = "/ "
	browser.FilterInput.Placeholder = "Find Sessions"
	browser.FilterInput.CharLimit = 256
	browser.FilterInput.SetVirtualCursor(false)
	browser.FilterInput.SetStyles(filterStyles)
	return sessionBrowser{Model: browser}
}

func sessionListItems(sessions []mango.Session) []list.Item {
	items := make([]list.Item, len(sessions))
	for index, session := range sessions {
		items[index] = sessionListItem{session: session}
	}
	return items
}

func (m *Model) syncInboxList() tea.Cmd {
	selectedID := ""
	if selected, ok := m.selectedInboxSession(); ok {
		selectedID = selected.ID
	}
	command := m.inboxList.SetItems(sessionListItems(m.sessions))
	if len(m.sessions) == 0 {
		m.inboxCursor = 0
		return command
	}
	selectedIndex := clamp(m.inboxCursor-3, 0, len(m.sessions)-1)
	if selectedID != "" {
		for index, session := range m.sessions {
			if session.ID == selectedID {
				selectedIndex = index
				break
			}
		}
	}
	m.inboxList.Select(selectedIndex)
	if m.inboxCursor >= 3 {
		m.inboxCursor = selectedIndex + 3
	}
	return command
}

func (m *Model) syncInboxCursor() {
	if m.inboxCursor < 3 {
		return
	}
	if _, ok := m.inboxList.SelectedItem().(sessionListItem); !ok {
		// Keep focus in the list while a filter temporarily has no matches.
		m.inboxCursor = 3
		return
	}
	m.inboxCursor = m.inboxList.GlobalIndex() + 3
}

func (m *Model) updateInboxList(message tea.Msg) tea.Cmd {
	var command tea.Cmd
	m.inboxList.Model, command = m.inboxList.Update(message)
	showAppliedFilter := m.inboxList.IsFiltered()
	if m.inboxList.ShowTitle() != showAppliedFilter {
		m.inboxList.SetShowTitle(showAppliedFilter)
	}
	if showAppliedFilter {
		m.inboxList.Title = "FILTER  " + m.inboxList.FilterValue()
	}
	m.syncInboxCursor()
	return command
}

func (m *Model) beginInboxFilter() tea.Cmd {
	m.inboxCursor = 3
	m.inboxList.ResetFilter()
	return m.updateInboxList(tea.KeyPressMsg(tea.Key{Code: '/', Text: "/"}))
}

func (m Model) inboxListMatchesSessions() bool {
	items := m.inboxList.Items()
	if len(items) != len(m.sessions) {
		return false
	}
	for index, raw := range items {
		item, ok := raw.(sessionListItem)
		if !ok || item.session.ID != m.sessions[index].ID {
			return false
		}
	}
	return true
}

func (m Model) selectedInboxSession() (mango.Session, bool) {
	if m.inboxCursor < 3 {
		return mango.Session{}, false
	}
	if item, ok := m.inboxList.SelectedItem().(sessionListItem); ok {
		return item.session, true
	}
	if m.inboxList.FilterState() != list.Unfiltered {
		return mango.Session{}, false
	}
	index := m.inboxCursor - 3
	if index < 0 || index >= len(m.sessions) {
		return mango.Session{}, false
	}
	return m.sessions[index], true
}

func (m *Model) resizeInboxList() {
	width := max(1, m.width)
	height := max(1, m.height)
	gridHeight := max(6, height-5)
	panelWidth := width
	if !m.compact && width >= 120 && len(m.sessions) > 0 {
		panelWidth = width * 55 / 100
	}
	m.inboxList.SetSize(max(1, panelWidth-4), max(1, gridHeight-2))
}

func renderInboxSessionRow(t theme, lastAttachedID string, session mango.Session, contentWidth int, selected bool, now time.Time) string {
	marker := "  "
	titleStyle := lipgloss.NewStyle().Foreground(t.text)
	if selected {
		marker, titleStyle = "› ", t.active
	}
	pip := sessionStatePip(t, session.Status)
	returnMark := ""
	if session.ID != "" && session.ID == lastAttachedID {
		returnMark = "  " + t.active.Render("⤴")
	}
	nameWidth := max(12, contentWidth-ansi.StringWidth(marker)-ansi.StringWidth(pip)-1-
		ansi.StringWidth(ansi.Strip(returnMark))-1-ansi.StringWidth(stateText(t, session.Status))-1)
	name := truncate(first(session.Title, session.Agent.Name, session.ID), nameWidth)
	head := marker + pip + " " + titleStyle.Render(name) + "  " + stateText(t, session.Status) + returnMark

	metaParts := []string{first(session.Agent.Name, "Agent")}
	if model := strings.TrimSpace(session.Agent.Model.ID); model != "" {
		metaParts = append(metaParts, model)
	}
	if children := subagentCount(session); children > 0 {
		metaParts = append(metaParts, fmt.Sprintf("%d sub-agents", children))
	}
	if since := humanizeSince(recency(session), now); since != "" {
		metaParts = append(metaParts, since)
	}
	metaParts = append(metaParts, shortID(session.ID))
	meta := strings.Join(metaParts, " · ")
	return head + "\n    " + t.dim.Render(trimOneLine(meta, contentWidth-4))
}
