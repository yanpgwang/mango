package ui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/yanpgwang/mango/examples/terminal-ui/internal/feed"
	"github.com/yanpgwang/mango/examples/terminal-ui/internal/mango"
)

const (
	minTerminalWidth  = 60
	minTerminalHeight = 20
)

type markdownCacheKey struct {
	width   int
	content string
}

type markdownState struct {
	renderers map[int]*glamour.TermRenderer
	entries   map[markdownCacheKey]string
	order     []markdownCacheKey
	next      int
}

const markdownCacheLimit = 2048

func newMarkdownState() markdownState {
	return markdownState{
		renderers: make(map[int]*glamour.TermRenderer),
		entries:   make(map[markdownCacheKey]string),
	}
}

func (m Model) View() tea.View {
	if m.width > 0 && m.height > 0 && (m.width < minTerminalWidth || m.height < minTerminalHeight) {
		view := tea.NewView(m.renderTooSmall())
		view.AltScreen = true
		view.WindowTitle = "Mango"
		view.ReportFocus = true
		return view
	}
	content := m.renderConnect()
	switch m.screen {
	case screenInbox:
		content = m.renderInbox()
	case screenChat:
		content = m.renderWorkspace()
	}
	if m.dialogIsOverlay() && m.width > 0 && m.height > 0 {
		modal := m.renderDialog()
		left := max(0, (m.width-lipgloss.Width(modal))/2)
		top := max(0, (m.height-lipgloss.Height(modal))/2)
		canvas := lipgloss.NewCanvas(max(1, m.width), max(1, m.height))
		// A Layer's X/Y coordinates are interpreted by a Compositor. Drawing a
		// Layer directly onto a Canvas ignores them and paints at (0,0), which
		// left the modal in the corner while the real IME cursor was correctly
		// offset to the intended center position.
		canvas.Compose(lipgloss.NewCompositor(
			lipgloss.NewLayer(content),
			lipgloss.NewLayer(modal).X(left).Y(top).Z(10),
		))
		content = canvas.Render()
	}
	view := tea.NewView(content)
	view.AltScreen = true
	view.WindowTitle = "Mango"
	view.MouseMode = tea.MouseModeCellMotion
	view.ReportFocus = true
	view.Cursor = m.viewCursor()
	return view
}

// viewCursor translates native textarea/textinput cursors into terminal
// coordinates. Besides drawing the caret, this is the anchor terminals expose
// to Chinese, Japanese, and Korean IMEs.
func (m Model) viewCursor() *tea.Cursor {
	if !m.windowFocused || m.width <= 0 || m.height <= 0 ||
		m.width < minTerminalWidth || m.height < minTerminalHeight {
		return nil
	}
	var source *tea.Cursor
	xOffset, yOffset := 0, 0
	if m.dialogIsOverlay() {
		if m.dialogUsesEditor() {
			source = m.editor.Cursor()
			xOffset, yOffset = 5, m.dialogEditorY()
		} else if m.dialogUsesFilter() {
			source = m.filter.Cursor()
			xOffset, yOffset = 3, m.dialogFilterY()
		} else {
			return nil
		}
		modal := m.renderDialog()
		xOffset += max(0, (m.width-lipgloss.Width(modal))/2)
		yOffset += max(0, (m.height-lipgloss.Height(modal))/2)
	} else if m.screen == screenConnect && m.connect.editing {
		source = m.connect.endpointInput.Cursor()
		layout := m.buildConnectLayout()
		xOffset, yOffset = layout.inputX, layout.inputY
	} else {
		if m.focus != focusEditor {
			if m.screen != screenInbox || !m.inboxList.SettingFilter() {
				return nil
			}
		}
		if m.screen == screenInbox && m.inboxList.SettingFilter() {
			source = m.inboxList.FilterInput.Cursor()
			// Toolbar (2 rows), blank separator (1), then the shared surface's
			// top/left padding (1/2). The filter starts at x=2, y=4.
			xOffset, yOffset = 2, 4
		} else {
			if m.screen != screenChat {
				return nil
			}
			source = m.editor.Cursor()
			above := []string{
				m.renderSessionHeader(m.width),
				m.renderConversationViewport(m.workspaceMainWidth()),
				m.renderConversationInfo(m.workspaceMainWidth()),
			}
			// The composer is part of the left workspace column. The right rail has
			// no bearing on its native terminal cursor or IME anchor.
			yOffset = lipgloss.Height(strings.Join(above, "\n"))
		}
	}
	if source == nil {
		return nil
	}
	cur := *source
	cur.X += xOffset
	cur.Y += yOffset

	// A cursor beyond the canvas can make some terminals scroll the alternate
	// screen. Hide it instead when a very short viewport clips its editor.
	if cur.X < 0 || cur.X >= m.width || cur.Y < 0 || cur.Y >= m.height {
		return nil
	}
	return &cur
}

func (m Model) dialogEditorY() int {
	// Dialog border + vertical padding + title/rule/blank + editor box border.
	const dialogChrome = 6
	switch m.dialog {
	case dialogResult:
		// "Result type", blank line, then the editor box.
		return dialogChrome + 2
	case dialogNewSession:
		// Summary, blank, field label, blank, then the editor box.
		return dialogChrome + lipgloss.Height(m.creationEditorSummary(m.dialogWidth())) + 3
	default:
		return dialogChrome
	}
}

func (m Model) dialogFilterY() int {
	// Border + top padding + title + rule + blank line.
	const contentStart = 5
	if m.dialog == dialogRenameSession {
		// Session identity, blank line, then the title field.
		return contentStart + 3
	}
	if m.dialog != dialogNewSession {
		return contentStart
	}
	summaryHeight := lipgloss.Height(m.selectionSummary())
	if m.creation.step == createChooseAgent {
		return contentStart + summaryHeight + 2
	}
	// Environment selection includes a blank line on both sides of its title.
	return contentStart + summaryHeight + 3
}

func (m *Model) resize() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	m.compact = m.width < 120 || m.height < 30
	innerWidth := max(20, m.workspaceMainWidth()-2)
	m.editor.MaxHeight = 15
	if m.dialogUsesEditor() {
		innerWidth = max(12, dialogInnerWidth(m.dialogWidth())-4)
		m.editor.MaxHeight = max(3, min(10, m.height-15))
	}
	m.editor.SetWidth(innerWidth)
	m.filter.SetWidth(max(12, dialogInnerWidth(m.dialogWidth())-3))
	_, _, connectWidth, connectCompact := connectDimensions(m.width, m.height)
	connectInputWidth, _, _ := connectCardMetrics(connectWidth, connectCompact)
	m.connect.endpointInput.SetWidth(connectInputWidth)
	composerHeight := m.composerHeight()
	// Chat viewport consumes the left column below the shared Session header.
	// Conversation info and help each own one line beneath it.
	headerHeight := lipgloss.Height(m.renderSessionHeader(m.width))
	chatHeight := max(3, m.height-headerHeight-composerHeight-2)
	m.chat.SetWidth(innerWidth)
	m.chat.SetHeight(chatHeight)
	if !m.inboxListMatchesSessions() {
		m.syncInboxList()
	}
	m.resizeInboxList()
	m.refreshChatMetrics()
}

func (m Model) composerHeight() int {
	return m.editor.Height()
}

func (m Model) dialogIsOverlay() bool {
	return m.dialog == dialogCommands || m.dialog == dialogAgents || m.dialog == dialogInterrupt ||
		m.dialog == dialogNewSession || m.dialog == dialogSessions || m.dialog == dialogAction ||
		m.dialog == dialogResult || m.dialog == dialogHelp || m.dialog == dialogQuit ||
		m.sessionManagerDialog()
}

func (m Model) dialogUsesEditor() bool {
	if m.dialog == dialogResult {
		return true
	}
	if m.dialog != dialogNewSession {
		return false
	}
	switch m.creation.step {
	case createAgentName, createAgentModel, createAgentSystem, createAgentMCPName, createAgentMCPURL,
		createEnvironmentName, createSessionTitle, createInitialPrompt:
		return true
	default:
		return false
	}
}

func (m Model) dialogUsesFilter() bool {
	switch m.dialog {
	case dialogCommands, dialogAgents, dialogSessions, dialogRenameSession:
		return true
	case dialogNewSession:
		return m.creation.step == createChooseAgent || m.creation.step == createChooseEnvironment
	default:
		return false
	}
}

func (m Model) renderWorkspace() string {
	width, height := max(1, m.width), max(1, m.height)
	header := m.renderSessionHeader(width)
	mainWidth := m.workspaceMainWidth()
	bodyHeight := max(1, height-lipgloss.Height(header))
	main := m.renderConversationColumn(mainWidth, bodyHeight)
	if !m.hasAgentRail() {
		return lipgloss.NewStyle().Width(width).Height(height).Render(header + "\n" + main)
	}
	rail := m.renderAgentWorkspace(m.workspaceRailWidth(), bodyHeight)
	body := lipgloss.JoinHorizontal(lipgloss.Top, main, rail)
	return lipgloss.NewStyle().Width(width).Height(height).Render(header + "\n" + body)
}

func (m Model) hasAgentRail() bool {
	return !m.compact && m.width >= 120 && m.height >= 30
}

func (m Model) workspaceRailWidth() int {
	if !m.hasAgentRail() {
		return 0
	}
	return clamp(m.width*28/100, 34, 40)
}

func (m Model) workspaceMainWidth() int {
	return max(20, m.width-m.workspaceRailWidth())
}

func (m Model) renderConversationColumn(width, height int) string {
	parts := []string{
		m.renderConversationViewport(width),
		m.renderConversationInfo(width),
		m.renderComposer(width),
		m.renderHelp(width),
	}
	return lipgloss.NewStyle().Width(width).Height(height).Render(strings.Join(parts, "\n"))
}

func (m Model) renderConversationViewport(width int) string {
	height := m.chat.Height()
	bar := strings.Join(m.conversationScrollbar(height), "\n")
	body := lipgloss.NewStyle().Width(max(1, width-1)).Height(height).PaddingLeft(1).Render(m.chat.View())
	return lipgloss.JoinHorizontal(lipgloss.Top, bar, body)
}

// conversationScrollbar draws a one-column Crush-style gutter: a dim track with
// a thumb whose size reflects how much of the transcript is visible and whose
// position follows the scroll offset. The thumb turns accent while the
// conversation holds focus, doubling as the pane's focus indicator.
//
// It reads only cached/cheap values (chatLines, YOffset, Height) so scrolling
// never triggers the viewport's O(total lines) soft-wrap recomputation.
func (m Model) conversationScrollbar(height int) []string {
	lines := make([]string, max(0, height))
	if height <= 0 {
		return lines
	}
	pos, thumb := scrollThumb(height, m.chatLines, m.chat.YOffset())

	thumbColor := m.theme.muted
	if m.focus == focusChat {
		thumbColor = m.theme.accent
	}
	track := lipgloss.NewStyle().Foreground(m.theme.border).Render("│")
	head := lipgloss.NewStyle().Foreground(thumbColor).Render("█")
	for row := range lines {
		if row >= pos && row < pos+thumb {
			lines[row] = head
		} else {
			lines[row] = track
		}
	}
	return lines
}

// scrollThumb sizes and positions a scrollbar thumb for a gutter of the given
// height, a transcript of total wrapped lines, scrolled to yoffset. When the
// content fits, the thumb fills the gutter.
func scrollThumb(height, total, yoffset int) (pos, size int) {
	if height <= 0 {
		return 0, 0
	}
	if total <= height {
		return 0, height
	}
	size = max(1, height*height/total)
	if size > height {
		size = height
	}
	pos = clamp((height-size)*yoffset/(total-height), 0, height-size)
	return pos, size
}

// renderSessionHeader keeps the durable Session and the observed Agent visible
// at once. On compact terminals the right rail collapses into a one-line roster
// strip instead of forcing a second navigation screen.
func (m Model) renderSessionHeader(width int) string {
	name := "Agent"
	status := ""
	role := ""
	if m.threadCursor >= 0 && m.threadCursor < len(m.threads) {
		thread := m.threads[m.threadCursor]
		name = first(thread.Agent.Name, name)
		status = stateText(m.theme, thread.Status)
		if thread.Primary() {
			role = "coordinator"
		} else {
			role = "sub-agent"
		}
	}
	backLabel := "‹ Sessions"
	if m.currentThreadID() != "" && !m.currentThreadPrimary() {
		backLabel = "‹ coordinator"
	}
	back := m.theme.dim.Render(backLabel) + "  " + m.theme.dim.Render("esc")
	sessionTitle := "Managed Session"
	if m.session != nil {
		sessionTitle = first(m.session.Title, sessionTitle)
	}
	right := m.theme.title.Render(name)
	if !strings.EqualFold(name, role) {
		right += "  " + m.theme.dim.Render(role)
	}
	if status != "" {
		right += "  " + status
	}
	leftWidth := max(8, width-ansi.StringWidth(ansi.Strip(right))-ansi.StringWidth(ansi.Strip(back))-8)
	left := back + "  " + m.theme.title.Render(truncate(sessionTitle, leftWidth))
	line := lipgloss.NewStyle().Width(width).Padding(0, 2).Render(joinSides(left, right, width-4))
	lines := []string{line, m.theme.dim.Render(strings.Repeat("─", width))}
	if !m.hasAgentRail() && len(m.threads)+len(m.unspawnedRoster(m.specialistThreads())) > 1 {
		lines = append(lines, m.renderCompactAgentStrip(width))
	}
	return strings.Join(lines, "\n")
}

func (m Model) renderComposer(width int) string {
	if m.dialogUsesEditor() {
		return lipgloss.NewStyle().Width(width).Height(m.composerHeight()).Render("")
	}
	return lipgloss.NewStyle().Width(width).Height(m.composerHeight()).Render(m.editor.View())
}

// renderConversationInfo keeps model and metering context beside the work it
// describes. The Agent rail is for navigation; the conversation footer is for
// the currently selected Thread's runtime details.
func (m Model) renderConversationInfo(width int) string {
	if m.session == nil {
		return lipgloss.NewStyle().Width(width).Render("")
	}
	modelName := first(m.session.Agent.Model.ID, "unknown model")
	usage := m.session.Usage
	activeSeconds := m.session.Stats.ActiveSeconds
	if m.threadCursor >= 0 && m.threadCursor < len(m.threads) {
		thread := m.threads[m.threadCursor]
		modelName = first(thread.Agent.Model.ID, modelName)
		if !thread.Primary() || thread.Usage.InputTokens != 0 || thread.Usage.OutputTokens != 0 || thread.Usage.CacheReadInputTokens != 0 {
			usage = thread.Usage
		}
		if !thread.Primary() || thread.Stats.ActiveSeconds != 0 {
			activeSeconds = thread.Stats.ActiveSeconds
		}
	}
	text := "◇ " + modelName + "  ·  " + compactTokens(usage.InputTokens) + " in  /  " +
		compactTokens(usage.OutputTokens) + " out"
	if activeSeconds > 0 {
		text += fmt.Sprintf("  ·  %.1fs", activeSeconds)
	}
	if m.currentThreadID() != "" && !m.currentThreadPrimary() {
		text = "viewing child transcript  ·  replies go to coordinator  ·  " + text
	}
	text = truncate(text, max(1, width-4))
	return lipgloss.NewStyle().Width(width).Padding(0, 2).Align(lipgloss.Right).Render(m.theme.dim.Render(text))
}

func (m Model) renderHelp(width int) string {
	var text string
	switch m.focus {
	case focusEditor:
		text = "enter send to coordinator  shift+enter newline  tab conversation  ctrl+p commands  esc back"
		if width < 80 {
			text = "enter send  tab conversation  ctrl+p commands  esc back"
		}
	case focusAgents:
		text = "↑↓ pick  enter open  space preview  x interrupt  tab compose  esc back"
		if width < 40 {
			text = "↑↓ pick  enter open  tab compose"
		}
	case focusChat:
		next := "editor"
		if m.hasAgentRail() {
			next = "subagents"
		}
		text = "↑↓ scroll  enter expand  tab " + next + "  esc back"
		if width < 80 {
			text = "↑↓ scroll  enter inspect  tab editor  esc back"
		}
		if width >= 90 {
			text = "↑↓ scroll  shift+↑↓ select  enter inspect  tab " + next + "  esc back"
		}
	}
	if m.err != nil {
		text = m.theme.danger.Render(trimOneLine(m.err.Error(), width-4))
		return lipgloss.NewStyle().Width(width).Padding(0, 2).Render(text)
	}
	if m.loading || m.reconnecting {
		label := first(m.loadingLabel, "Reconnecting")
		return lipgloss.NewStyle().Width(width).Padding(0, 2).Render(m.activity(label))
	}
	return lipgloss.NewStyle().Width(width).Padding(0, 2).Render(m.theme.dim.Render(text))
}

// delegationPreview returns the first task that landed on this Thread, or the
// empty string if the child has not been briefed yet. It is intentionally the
// earliest inbound message because Managed coordinators delegate once at the
// start of a Thread and only chase up with clarifications.
func (m Model) delegationPreview(threadID string) string {
	for _, event := range m.events[threadID] {
		if event.Type() != "agent.thread_message_received" {
			continue
		}
		if text := strings.TrimSpace(feed.ContentText(event["content"])); text != "" {
			if newline := strings.IndexByte(text, '\n'); newline >= 0 {
				text = text[:newline]
			}
			return text
		}
	}
	return ""
}

func (m *Model) renderChat() {
	if m.threadCursor < 0 || m.threadCursor >= len(m.threads) {
		m.chat.SetContent(m.theme.dim.Render("Waiting for the primary Agent…"))
		m.refreshChatMetrics()
		return
	}
	items := feed.Project(m.threads[m.threadCursor], m.events[m.currentThreadID()])
	if len(items) > 0 && m.itemCursor >= len(items) {
		m.itemCursor = len(items) - 1
	}
	parts := make([]string, 0, len(items)+1)
	contentWidth := min(100, max(30, m.chat.Width()))
	for index, item := range items {
		parts = append(parts, m.renderFeedItem(item, index == m.itemCursor, contentWidth))
	}
	if current := m.previews[m.currentThreadID()]; current != nil {
		live := ""
		if strings.TrimSpace(current.content) != "" {
			streamed := strings.TrimRight(m.renderMarkdown(current.content, contentWidth-4, false), "\n")
			live = lipgloss.NewStyle().PaddingLeft(2).Render(streamed + " " + m.streamCaret())
		} else if current.messageID != "" {
			live = lipgloss.NewStyle().PaddingLeft(2).Render(m.replyActivity())
		} else if current.waiting || current.thinkingID != "" {
			live = lipgloss.NewStyle().PaddingLeft(2).Render(m.fruitThinking())
		}
		if live != "" {
			parts = append(parts, live)
		}
	}
	if len(parts) == 0 {
		parts = append(parts, m.theme.dim.Render("No messages yet. Ask the coordinator to begin."))
	}
	m.chat.SetContent(strings.Join(parts, "\n\n"))
	m.refreshChatMetrics()
	if m.follow {
		m.chat.GotoBottom()
	}
}

// refreshChatMetrics caches the transcript's wrapped line count so the
// scrollbar can be drawn from cheap field reads. It runs only when the content
// or viewport size changes — never on every scroll frame.
func (m *Model) refreshChatMetrics() {
	m.chatLines = m.chat.TotalLineCount()
}

func (m *Model) renderFeedItem(item feed.Item, selected bool, width int) string {
	body := ""
	if strings.TrimSpace(item.Body) != "" {
		body = m.renderMarkdown(item.Body, width-3, true)
	}
	selectedStyle := func(content string) string {
		content = m.spark(item.ID) + content
		if !selected {
			return lipgloss.NewStyle().PaddingLeft(2).Render(content)
		}
		return lipgloss.NewStyle().PaddingLeft(1).BorderLeft(true).
			BorderStyle(lipgloss.Border{Left: "▌"}).BorderForeground(m.theme.green).Render(content)
	}

	switch item.Kind {
	case feed.User:
		if body == "" {
			return ""
		}
		return lipgloss.NewStyle().PaddingLeft(1).BorderLeft(true).
			BorderStyle(lipgloss.Border{Left: "▌"}).BorderForeground(m.theme.accent).Render(body)
	case feed.Agent:
		return selectedStyle(body)
	case feed.Tool:
		status := item.Status
		statusStyle := m.theme.dim
		if status == "waiting" {
			statusStyle = m.theme.warning
		}
		if status == "error" {
			statusStyle = m.theme.danger
		}
		if status == "done" {
			statusStyle = m.theme.success
		}
		label := m.theme.dim.Render("✱") + " " + m.theme.title.Render(item.Title) + " " + statusStyle.Render(status)
		if m.expanded[item.ID] {
			details := []string{label}
			if strings.TrimSpace(item.Detail) != "" {
				details = append(details, m.theme.dim.Render(item.Detail))
			}
			if body != "" {
				details = append(details, body)
			}
			return selectedStyle(strings.Join(details, "\n"))
		}
		return selectedStyle(label)
	case feed.Delegation:
		label := m.theme.warning.Render("↗ " + item.Title)
		if body != "" {
			label += "\n" + body
		}
		return selectedStyle(label)
	case feed.Report:
		label := m.theme.success.Render("↙ " + item.Title)
		if body != "" {
			label += "\n" + body
		}
		return selectedStyle(label)
	case feed.Thinking, feed.Notice:
		return selectedStyle(m.theme.dim.Render(item.Title))
	case feed.Failure:
		label := m.theme.danger.Render(item.Title)
		if body != "" {
			label += "\n" + body
		}
		return selectedStyle(label)
	}
	return selectedStyle(body)
}

func (m Model) renderInbox() string {
	width, height := max(1, m.width), max(1, m.height)
	main := m.renderInboxMain(width, height-3)
	helpText := "←→ pill/page  ↑↓ list  enter open  m manage  n new  / find  r refresh  esc disconnect  ? help"
	if width < 90 {
		helpText = "←→ pill  ↑↓ list  enter open  m manage  n new  esc"
	}
	if m.inboxList.SettingFilter() {
		helpText = "type to filter  enter apply  esc cancel"
	} else if m.inboxList.IsFiltered() {
		helpText = "↑↓ choose  enter open  m manage  esc clear filter  / find again"
	}
	help := m.theme.dim.Render(helpText)
	footer := lipgloss.NewStyle().Width(width).Padding(0, 2).Render(help)
	return lipgloss.NewStyle().Width(width).Height(height).Render(main + "\n\n" + footer)
}

func (m Model) renderInboxMain(width, height int) string {
	// Toolbar sits at the very top of the interactive area — nothing above it
	// except the terminal's own top margin.
	innerWidth := max(1, width-4)
	actions := m.renderInboxToolbar()
	statusWidth := max(1, innerWidth-ansi.StringWidth(actions)-2)
	status := truncate(m.renderInboxFleetSummary(), statusWidth)
	toolbarLine := joinSides(actions, status, innerWidth)
	toolbar := lipgloss.NewStyle().Width(width).Padding(1, 2, 0, 2).Render(toolbarLine)

	used := lipgloss.Height(toolbar) + 1 // toolbar + blank line
	gridHeight := max(6, height-used)

	sessionCount := len(m.sessions)
	twoColumn := !m.compact && width >= 120 && sessionCount > 0
	var grid string
	if twoColumn {
		listWidth := width * 55 / 100
		previewWidth := width - listWidth
		list := m.renderInboxList(listWidth, gridHeight)
		preview := m.renderInboxPreview(previewWidth, gridHeight)
		grid = lipgloss.JoinHorizontal(lipgloss.Top, list, preview)
	} else {
		grid = m.renderInboxList(width, gridHeight)
	}
	return strings.Join([]string{toolbar, "", grid}, "\n")
}

// renderInboxFleetSummary is the compact fleet status shown opposite the
// toolbar actions. Loading and error states hijack it because their signal
// outranks the steady-state counts anyway.
func (m Model) renderInboxFleetSummary() string {
	if m.loading {
		return m.activity(first(m.loadingLabel, "Refreshing Sessions"))
	}
	if m.err != nil {
		return m.theme.danger.Render(trimOneLine(m.err.Error(), 60))
	}
	counts := summarizeFleet(m.sessions)
	badge := lipgloss.NewStyle().Background(m.theme.green).Foreground(m.theme.panel).
		Bold(true).Padding(0, 1).Render("CONNECTED")
	parts := []string{badge}
	if counts.needsAction > 0 {
		parts = append(parts, m.theme.warning.Render(fmt.Sprintf("%d needs input", counts.needsAction)))
	}
	if counts.running > 0 {
		parts = append(parts, m.theme.active.Render(fmt.Sprintf("%d running", counts.running)))
	}
	if counts.idle > 0 {
		parts = append(parts, m.theme.dim.Render(fmt.Sprintf("%d idle", counts.idle)))
	}
	if counts.other > 0 {
		parts = append(parts, m.theme.dim.Render(fmt.Sprintf("%d other", counts.other)))
	}
	return strings.Join(parts, m.theme.dim.Render("  ·  "))
}

// renderInboxToolbar prints the three top-of-page action buttons. Sitting on
// the cursor still highlights the button the same way it used to highlight a
// list row, so muscle memory (Enter on cursor 0 = New Session) is preserved.
func (m Model) renderInboxToolbar() string {
	labels := []string{"+ New", "/ Find", "↻ Refresh"}
	pills := make([]string, 0, 3)
	for index, label := range labels {
		selected := m.inboxCursor == index || index == 1 && (m.inboxList.SettingFilter() || m.inboxList.IsFiltered())
		pills = append(pills, choice(m.theme, label, selected, false))
	}
	return strings.Join(pills, "  ")
}

// renderInboxList is the durable-Session list column. Rows are compact but
// carry every fleet signal that matters at a glance: state pip, title, state
// text, activity time, sub-agent count, and the recently-attached badge.
func (m Model) renderInboxList(width, height int) string {
	if len(m.sessions) == 0 {
		body := m.theme.dim.Render("No durable Sessions yet. Press ") +
			m.theme.active.Render("n") + m.theme.dim.Render(" to start one.")
		return m.renderInboxPanel(width, height, body)
	}
	browser := m.inboxList
	browser.SetSize(max(1, width-4), max(1, height-2))
	browser.SetDelegate(sessionListDelegate{
		theme: m.theme, lastAttachedID: m.lastAttachedID,
		focused: m.inboxCursor >= 3, now: time.Now(),
	})
	return m.renderInboxPanel(width, height, browser.View())
}

// renderInboxPreview is the right-side detail card shown on wide terminals.
// It reveals everything the operator would need to decide "attach or not"
// without reading the compact left list row twice.
func (m Model) renderInboxPreview(width, height int) string {
	if m.inboxCursor < 3 {
		return m.renderToolbarHelpCard(width, height)
	}
	session, ok := m.selectedInboxSession()
	if !ok {
		if m.inboxList.SettingFilter() || m.inboxList.IsFiltered() {
			body := m.theme.title.Render("No matching Session") + "\n\n" +
				m.theme.dim.Render("Keep typing, or press Esc to clear the filter.")
			return m.renderInboxPanel(width, height, body)
		}
		return lipgloss.NewStyle().Width(width).Height(height).Render("")
	}
	inner := max(4, width-4)
	status := stateText(m.theme, session.Status)
	if since := humanizeSince(recency(session), time.Now()); since != "" {
		status += m.theme.dim.Render(" · " + since)
	}
	titleWidth := max(8, inner-lipgloss.Width(status)-2)
	title := m.theme.title.Render(truncate(first(session.Title, "Untitled Session"), titleWidth))
	header := joinSides(title, status, inner)
	rule := m.theme.dim.Render(strings.Repeat("─", inner))
	rows := []string{header, rule, "", m.previewSection("Agent")}
	agent := m.theme.title.Render(first(session.Agent.Name, "unknown"))
	if model := strings.TrimSpace(session.Agent.Model.ID); model != "" {
		agent += m.theme.dim.Render("  ·  " + truncate(model, max(4, inner-lipgloss.Width(agent)-5)))
	}
	rows = append(rows, agent)
	if env := strings.TrimSpace(session.EnvironmentID); env != "" {
		rows = append(rows, m.theme.dim.Render("Environment  ")+truncate(env, max(4, inner-13)))
	}

	roster := previewRosterNames(session)
	rows = append(rows, "", m.previewSection(fmt.Sprintf("Subagents · %d", len(roster))))
	if len(roster) == 0 {
		rows = append(rows, m.theme.dim.Render("Single-Agent Session"))
	} else {
		rows = append(rows, truncate(strings.Join(roster, "  ·  "), inner))
	}

	usage := []string{}
	if session.Usage.InputTokens > 0 {
		usage = append(usage, compactTokens(session.Usage.InputTokens)+" in")
	}
	if session.Usage.OutputTokens > 0 {
		usage = append(usage, compactTokens(session.Usage.OutputTokens)+" out")
	}
	if session.Stats.ActiveSeconds > 0 {
		usage = append(usage, fmt.Sprintf("%.1fs active", session.Stats.ActiveSeconds))
	}
	if len(usage) > 0 {
		rows = append(rows, "", m.previewSection("Usage"), truncate(strings.Join(usage, "  ·  "), inner))
	}

	rows = append(rows, "", m.previewSection("Session"), truncate(session.ID, inner))
	if session.ID != "" && session.ID == m.lastAttachedID {
		rows = append(rows, m.theme.active.Render("⤴ recently attached"))
	}
	rows = append(rows, "", m.theme.active.Render("enter")+m.theme.dim.Render(" attach   ·   ")+
		m.theme.active.Render("m")+m.theme.dim.Render(" manage"))
	body := strings.Join(rows, "\n")
	return m.renderInboxPanel(width, height, body)
}

// renderToolbarHelpCard shows a short explanation for the pill the operator's
// cursor is currently sitting on. It fills the right column when no Session
// is highlighted so the pane never renders as dead space.
func (m Model) renderToolbarHelpCard(width, height int) string {
	inner := max(4, width-4)
	title := "Getting started"
	body := "Move the cursor down to inspect a Session, or press Enter on a pill."
	switch m.inboxCursor {
	case 0:
		title, body = "Create a new Session", "Choose an Agent, an Environment, a title, and an optional first message. The Session is durable — you can detach and come back to it any time."
	case 1:
		title, body = "Find a Session", "Search every durable Session by title, Agent name, or ID. Useful when the fleet grows beyond the visible list."
	case 2:
		title, body = "Refresh from Cloud", "Re-fetch the Session list from Mango. Sessions keep running in the cloud after you detach, so their state may have moved since you last looked."
	}
	head := m.theme.title.Render(truncate(title, inner))
	rule := m.theme.dim.Render(strings.Repeat("─", inner))
	content := head + "\n" + rule + "\n" + m.theme.dim.Render(lipgloss.NewStyle().Width(inner).Render(body))
	return m.renderInboxPanel(width, height, content)
}

func (m Model) renderInboxPanel(width, height int, content string) string {
	background := lipgloss.NewStyle().Background(m.theme.panel)
	// Child styles end with a full SGR reset. Re-enter the panel background
	// after each reset so nested foreground colors do not punch terminal-colored
	// rectangles through the filled panel.
	probe := background.Render("x")
	if index := strings.IndexByte(probe, 'x'); index > 0 {
		prefix := probe[:index]
		content = strings.ReplaceAll(content, ansi.ResetStyle, ansi.ResetStyle+prefix)
	}
	// Both columns use the same edge-to-edge surface. Their padding creates
	// breathing room without splitting the browser into bordered cards.
	return background.Width(width).Height(height).Padding(1, 2).Render(content)
}

func (m Model) previewSection(label string) string {
	return m.theme.dim.Bold(true).Render(strings.ToUpper(label))
}

func previewRosterNames(session mango.Session) []string {
	if session.Agent.Multiagent == nil {
		return nil
	}
	names := []string{}
	seenIDs, seenNames := map[string]bool{}, map[string]bool{}
	for _, reference := range session.Agent.Multiagent.Agents {
		if reference.ID != "" && reference.ID == session.Agent.ID {
			continue
		}
		name := strings.TrimSpace(reference.Name)
		if name == "" {
			name = shortID(reference.ID)
		}
		if name == "" || seenIDs[reference.ID] || seenNames[name] {
			continue
		}
		if reference.ID != "" {
			seenIDs[reference.ID] = true
		}
		seenNames[name] = true
		names = append(names, name)
	}
	return names
}

func sessionStatePip(t theme, status string) string {
	switch status {
	case "running", "rescheduling":
		return t.active.Render("●")
	case "requires_action":
		return t.warning.Render("!")
	case "terminated", "failed", "error":
		return t.danger.Render("○")
	default:
		return t.dim.Render("○")
	}
}

func (m Model) renderConnect() string {
	return m.buildConnectLayout().view
}

func (m Model) renderDialog() string {
	width := m.dialogWidth()
	innerWidth := dialogInnerWidth(width)
	content := ""
	title := ""
	switch m.dialog {
	case dialogCommands:
		title = "Commands"
		rows := []string{}
		commands := m.filteredCommands()
		start, end := visibleRange(len(commands), m.dialogCursor, m.dialogListSize(9))
		for index := start; index < end; index++ {
			command := commands[index]
			marker := "  "
			style := lipgloss.NewStyle().Foreground(m.theme.text)
			if index == m.dialogCursor {
				marker, style = "› ", m.theme.active
			}
			rows = append(rows, marker+style.Render(command.title)+"\n    "+m.theme.dim.Render(command.help))
		}
		if len(rows) == 0 {
			rows = append(rows, m.theme.dim.Render("No matching commands."))
		}
		content = m.renderCreationSearch(m.dialogQuery, "Type to filter commands") + "\n\n" +
			strings.Join(rows, "\n") + "\n\n" + m.theme.dim.Render(m.dialogHint(
			"type filter  ↑↓ choose  enter run  esc", "type  ↑↓ choose  enter run  esc"))
	case dialogAgents:
		title = "Agents"
		threads := m.filteredThreads()
		start, end := visibleRange(len(threads), m.dialogCursor, m.dialogListSize(9))
		rows := make([]string, 0, end-start)
		for index := start; index < end; index++ {
			thread := threads[index]
			marker := "  "
			style := lipgloss.NewStyle().Foreground(m.theme.text)
			if index == m.dialogCursor {
				marker, style = "› ", m.theme.active
			}
			role := "specialist"
			if thread.Primary() {
				role = "coordinator"
			}
			unread := ""
			if count := m.unread[thread.ID]; count > 0 {
				unread = m.theme.warning.Render(fmt.Sprintf(" +%d", count))
			}
			nameWidth := max(8, innerWidth-ansi.StringWidth(marker)-4-ansi.StringWidth(unread))
			name := truncate(first(thread.Agent.Name, "Agent"), nameWidth)
			rows = append(rows, marker+style.Render(name)+unread+
				"\n    "+m.theme.dim.Render(role+" · ")+stateText(m.theme, thread.Status))
		}
		if len(rows) == 0 {
			rows = append(rows, m.theme.dim.Render("No matching Agents."))
		}
		content = m.renderCreationSearch(m.dialogQuery, "Type to filter Agents") + "\n\n" +
			strings.Join(rows, "\n") + "\n\n" + m.theme.dim.Render(m.dialogHint(
			"type filter  ↑↓ choose  enter view  esc", "type  ↑↓ choose  enter view  esc"))
	case dialogAction:
		title = fmt.Sprintf("Pending actions · %d", len(m.pending))
		if len(m.pending) == 0 {
			content = "Nothing is waiting."
			break
		}
		action := m.pending[clamp(m.actionCursor, 0, len(m.pending)-1)]
		thread := m.threadName(action.ThreadID)
		detail, omitted := boundedDialogText(action.Input, innerWidth, max(1, min(10, m.height-17)))
		header := trimOneLine(action.Name+" · "+thread, innerWidth)
		content = m.theme.warning.Render(header) + "\n\n" +
			m.theme.dim.Render(detail)
		if omitted > 0 {
			content += "\n" + m.theme.warning.Render(fmt.Sprintf("… %d more line(s) · expand the tool event in chat", omitted))
		}
		content += "\n\n"
		if action.Kind == mango.ActionConfirmation {
			content += choice(m.theme, "Allow once", m.actionChoice == 0, false) + "  " + choice(m.theme, "Deny", m.actionChoice == 1, true) +
				"\n\n" + m.theme.dim.Render(m.dialogHint(
				"←→ choose  enter confirm  a allow  d deny  esc close", "←→ choose  enter confirm  a allow  d deny"))
		} else {
			content += choice(m.theme, "Provide result", true, false) + "\n\n" + m.theme.dim.Render("enter continue  esc close")
		}
		if m.loading {
			content += "\n\n" + m.activity("Submitting decision")
		}
	case dialogResult:
		title = "Return tool result"
		state := m.theme.success.Render("success")
		if m.resultError {
			state = m.theme.danger.Render("error")
		}
		box := lipgloss.NewStyle().Width(innerWidth).Border(lipgloss.RoundedBorder()).BorderForeground(m.theme.accent).Padding(0, 1).Render(m.editor.View())
		content = "Result type: " + state + "\n\n" + box + "\n\n" + m.theme.dim.Render(m.dialogHint(
			"enter submit  ctrl+e success/error  esc back", "enter submit  ctrl+e type  esc back"))
		if m.loading {
			content += "\n\n" + m.activity("Submitting result")
		}
	case dialogInterrupt:
		title = "Interrupt Agent?"
		name := "current Agent"
		if m.threadCursor >= 0 && m.threadCursor < len(m.threads) {
			name = first(m.threads[m.threadCursor].Agent.Name, name)
		}
		name = truncate(name, max(8, innerWidth-24))
		content = "Cancel active work on " + m.theme.warning.Render(name) + "?\nThe durable Session and history stay intact.\n\n" +
			choice(m.theme, "Interrupt", m.dialogCursor == 0, true) + "  " + choice(m.theme, "Keep running", m.dialogCursor == 1, false) +
			"\n\n" + m.theme.dim.Render("←→ choose  enter confirm  esc close")
	case dialogNewSession:
		title, content = m.renderCreationDialog(width)
	case dialogSessions:
		title = "Sessions"
		filtered := m.filteredSessions()
		rows := make([]string, 0, min(len(filtered), 9))
		start, end := visibleRange(len(filtered), m.inboxCursor, m.dialogListSize(9))
		for index := start; index < end; index++ {
			session := filtered[index]
			marker, style := "  ", lipgloss.NewStyle().Foreground(m.theme.text)
			if index == m.inboxCursor {
				marker, style = "› ", m.theme.active
			}
			name := first(session.Title, session.Agent.Name, session.ID)
			rows = append(rows, marker+style.Render(truncate(name, width-18))+"  "+stateText(m.theme, session.Status)+
				"\n    "+m.theme.dim.Render(trimOneLine(first(session.Agent.Name, "Agent")+" · "+shortID(session.ID), innerWidth-4)))
		}
		if len(rows) == 0 {
			rows = append(rows, m.theme.dim.Render("No matching Sessions."))
		}
		content = m.renderCreationSearch(m.inboxFilter, "Type to filter Sessions") + "\n\n" +
			strings.Join(rows, "\n") + "\n\n" +
			m.theme.dim.Render(m.dialogHint(
				"type filter  ↑↓ choose  enter open  ctrl+e manage  ctrl+n new  esc", "type  ↑↓ choose  enter open  ctrl+e manage"))
		if m.loading {
			content += "\n\n" + m.activity("Opening Session")
		} else if m.err != nil {
			content += "\n\n" + m.theme.danger.Render(trimOneLine(m.err.Error(), width-8))
		}
	case dialogSessionActions, dialogRenameSession, dialogInterruptSession, dialogArchiveSession, dialogDeleteSession:
		title, content = m.renderSessionManagerDialog(innerWidth)
	case dialogHelp:
		title = "Keyboard help"
		helpLines := []string{
			m.theme.title.Render("Create and navigate"),
			"  enter        send or confirm",
			"  shift+enter  insert a newline",
			"  ctrl+n       create a Session",
			"  ctrl+s       search Sessions",
			"  m / ctrl+e   manage selected Session",
			"  ctrl+p       command palette",
			"  ctrl+g       switch Agent view",
			"",
			m.theme.title.Render("Inspect a conversation"),
			"  tab          move between editor and chat",
			"  ↑↓ / j k     scroll chat",
			"  shift+↑↓     select an event",
			"  enter/space  toggle details",
			"  ←→ / h l     collapse or expand",
			"",
			m.theme.dim.Render("f1 help  esc close  ctrl+c quit"),
		}
		if m.height < 24 {
			helpLines = []string{
				m.theme.title.Render("Mango shortcuts"),
				"  enter / shift+enter   send / newline",
				"  ctrl+n / ctrl+s       new / Sessions",
				"  ctrl+p / ctrl+g       commands / Agents",
				"  tab                   editor / chat",
				"  ↑↓ / shift+↑↓         scroll / select",
				"  enter / ←→            inspect event",
				m.theme.dim.Render("f1 help  esc close  ctrl+c quit"),
			}
		}
		content = strings.Join(helpLines, "\n")
	case dialogQuit:
		title = "Quit Mango?"
		content = lipgloss.JoinVertical(lipgloss.Center,
			"Managed Sessions and Agents keep running in the cloud.",
			"Leaving only detaches this terminal.",
			"",
			choice(m.theme, "Quit", m.dialogCursor == 0, true)+"  "+
				choice(m.theme, "Keep working", m.dialogCursor == 1, false),
			"",
			m.theme.dim.Render(m.dialogHint(
				"←→ choose  enter confirm  y/n decide  ctrl+c again quits  esc close",
				"←→ choose  enter confirm  ctrl+c again quits")),
		)
	}
	box := m.dialogTitle(title) + "\n" + m.dialogRule(innerWidth) + "\n\n" + content
	return lipgloss.NewStyle().Width(width).Padding(1, 2).Background(m.theme.panel).
		Border(lipgloss.RoundedBorder()).BorderForeground(m.theme.border).Render(box)
}

func (m Model) threadName(id string) string {
	for _, thread := range m.threads {
		if thread.ID == id {
			return first(thread.Agent.Name, shortID(id))
		}
	}
	return shortID(id)
}

func (m *Model) renderMarkdown(content string, width int, cache bool) string {
	width = max(20, width)
	key := markdownCacheKey{width: width, content: content}
	if cache {
		if rendered, ok := m.markdown.entries[key]; ok {
			return rendered
		}
	}
	renderer := m.markdown.renderers[width]
	if renderer == nil {
		var err error
		renderer, err = glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(width))
		if err != nil {
			return content
		}
		m.markdown.renderers[width] = renderer
	}
	rendered, err := renderer.Render(content)
	if err != nil {
		return content
	}
	rendered = strings.TrimSpace(rendered)
	if cache {
		if len(m.markdown.order) < markdownCacheLimit {
			m.markdown.order = append(m.markdown.order, key)
		} else {
			oldest := m.markdown.order[m.markdown.next]
			delete(m.markdown.entries, oldest)
			m.markdown.order[m.markdown.next] = key
			m.markdown.next = (m.markdown.next + 1) % markdownCacheLimit
		}
		m.markdown.entries[key] = rendered
	}
	return rendered
}

func stateText(t theme, status string) string {
	switch status {
	case "running", "rescheduling":
		return t.warning.Render(status)
	case "idle":
		return t.dim.Render("idle")
	case "requires_action":
		return t.warning.Render("needs input")
	case "terminated", "error", "failed":
		return t.danger.Render(status)
	default:
		return t.dim.Render(first(status, "unknown"))
	}
}

func choice(t theme, text string, selected, danger bool) string {
	style := lipgloss.NewStyle().Foreground(t.text).Background(t.soft).Padding(0, 1)
	if selected {
		style = style.Background(t.accent).Bold(true)
	}
	if danger && selected {
		style = style.Background(t.red)
	}
	return style.Render(text)
}

func joinSides(left, right string, width int) string {
	space := max(1, width-ansi.StringWidth(left)-ansi.StringWidth(right))
	return left + strings.Repeat(" ", space) + right
}

func first(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
func shortID(value string) string {
	width := ansi.StringWidth(value)
	if width <= 15 {
		return value
	}
	return ansi.Cut(value, 0, 9) + "…" + ansi.Cut(value, width-4, width)
}
func truncate(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(value) <= width {
		return value
	}
	return ansi.Truncate(value, width, "…")
}

func (m Model) dialogWidth() int {
	if m.dialog == dialogQuit {
		return min(56, max(40, m.width-8))
	}
	if m.dialog == dialogNewSession {
		// Keep the creation wizard as a focused center card with visible
		// breathing room around it, even on wide terminals.
		return min(66, max(52, m.width-12))
	}
	return min(70, max(40, m.width-8))
}

func dialogInnerWidth(width int) int {
	return max(1, width-6)
}

func (m Model) dialogListSize(limit int) int {
	return max(1, min(limit, (m.height-12)/2))
}

func (m Model) dialogHint(full, compact string) string {
	if dialogInnerWidth(m.dialogWidth()) < 60 {
		return compact
	}
	return full
}

func (m Model) creationListSize(limit int) int {
	return max(1, min(limit, (m.height-18)/2))
}

func (m Model) renderTooSmall() string {
	width, height := max(1, m.width), max(1, m.height)
	message := truncate(fmt.Sprintf("Mango needs at least %d×%d · resize this terminal", minTerminalWidth, minTerminalHeight), width)
	return lipgloss.NewStyle().Width(width).Height(height).Align(lipgloss.Center, lipgloss.Center).
		Render(m.theme.active.Render(message))
}

func boundedDialogText(value string, width, maxLines int) (string, int) {
	const maxSourceBytes = 64 << 10
	value = capUTF8(value, maxSourceBytes)
	wrapped := ansi.Hardwrap(value, max(1, width), true)
	lines := strings.Split(wrapped, "\n")
	if len(lines) <= maxLines {
		return wrapped, 0
	}
	return strings.Join(lines[:maxLines], "\n"), len(lines) - maxLines
}
func trimOneLine(value string, width int) string {
	return truncate(strings.Join(strings.Fields(value), " "), width)
}
func commas(value int64) string {
	text := fmt.Sprintf("%d", value)
	for index := len(text) - 3; index > 0; index -= 3 {
		text = text[:index] + "," + text[index:]
	}
	return text
}

func compactTokens(value int64) string {
	switch {
	case value >= 1_000_000:
		formatted := fmt.Sprintf("%.1fM", float64(value)/1_000_000)
		return strings.Replace(formatted, ".0M", "M", 1)
	case value >= 1_000:
		formatted := fmt.Sprintf("%.1fK", float64(value)/1_000)
		return strings.Replace(formatted, ".0K", "K", 1)
	default:
		return fmt.Sprintf("%d", value)
	}
}
