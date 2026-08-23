package ui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/yanpgwang/mango/examples/terminal-ui/internal/feed"
	"github.com/yanpgwang/mango/examples/terminal-ui/internal/mango"
)

type screen int
type focus int
type dialog int

const (
	screenConnect screen = iota
	screenInbox
	screenChat
)

const (
	focusEditor focus = iota
	focusChat
	focusAgents
)

const (
	dialogNone dialog = iota
	dialogCommands
	dialogAgents
	dialogAction
	dialogResult
	dialogInterrupt
	dialogNewSession
	dialogSessions
	dialogSessionActions
	dialogRenameSession
	dialogInterruptSession
	dialogArchiveSession
	dialogDeleteSession
	dialogHelp
	dialogQuit
)

type inboxLoaded struct {
	sessions []mango.Session
	err      error
}
type attachedLoaded struct {
	attachment mango.Attachment
	err        error
}
type summaryLoaded struct {
	summary mango.Summary
	err     error
}
type streamUpdate struct {
	source <-chan mango.StreamUpdate
	update mango.StreamUpdate
	open   bool
}
type operationDone struct {
	sessionID    string
	operationID  uint64
	label        string
	events       []mango.Event
	draft        string
	clearDraft   bool
	restoreDraft bool
	err          error
}
type reconnectTick time.Time

type preview struct {
	messageID  string
	thinkingID string
	content    string
	waiting    bool
	startedAt  time.Time
}

const (
	actionGraceQuietPeriod = 425 * time.Millisecond
	actionGraceMaxDelay    = 1500 * time.Millisecond
	actionReopenWindow     = 500 * time.Millisecond
	previewMaxBytes        = 1 << 20
	previewMaxAge          = 2 * time.Minute
)

type Model struct {
	backend mango.Backend
	ctx     context.Context
	cancel  context.CancelFunc

	width, height          int
	screen                 screen
	focus                  focus
	dialog                 dialog
	dialogCursor           int
	dialogQuery            string
	dialogGraceOpenedAt    time.Time
	dialogGraceLastInputAt time.Time
	quitReturnDialog       dialog
	lastActionClosedAt     time.Time
	pendingSignature       string
	pendingDismissed       bool

	sessions            []mango.Session
	inboxCursor         int
	inboxFilter         string
	inboxList           sessionBrowser
	lastAttachedID      string
	railCursor          int
	sessionAction       mango.Session
	sessionActionParent dialog
	sessionActionCursor int

	session      *mango.Session
	threads      []mango.Thread
	threadCursor int
	events       map[string][]mango.Event
	seen         map[string]map[string]bool
	previews     map[string]*preview
	unread       map[string]int
	pending      []mango.Action
	actionCursor int
	actionChoice int
	resultError  bool
	itemCursor   int
	expanded     map[string]bool
	fresh        map[string]int

	chat      viewport.Model
	chatLines int // cached wrapped line count; recomputed only on content/size change
	editor    textarea.Model
	filter    textinput.Model
	spinner   spinner.Model
	theme     theme
	connect   connectState
	compact   bool
	follow    bool
	motion    int

	loading         bool
	loadingLabel    string
	status          string
	err             error
	stream          <-chan mango.StreamUpdate
	streamCancel    context.CancelFunc
	operationCancel context.CancelFunc
	operationSeq    uint64
	activeOperation uint64
	reconnecting    bool
	reconnectTry    int
	directAttach    string
	creation        createState
	windowFocused   bool
	options         Options
	markdown        markdownState
}

func New(backend mango.Backend, directAttach string) Model {
	return NewWithOptions(backend, directAttach, Options{})
}

func NewWithOptions(backend mango.Backend, directAttach string, options Options) Model {
	t := defaultTheme()
	ctx, cancel := context.WithCancel(context.Background())
	directAttach = strings.TrimSpace(directAttach)
	loadingLabel := "Connecting to Mango"
	if directAttach != "" {
		loadingLabel = "Opening Session"
	}
	editor := textarea.New()
	editor.Placeholder = "Ask a managed Agent anything…"
	editor.ShowLineNumbers = false
	editor.DynamicHeight = true
	editor.MinHeight = 3
	editor.MaxHeight = 15
	editor.CharLimit = 64 << 10
	// Bubble Tea must own the real terminal cursor so IME candidate windows are
	// anchored to the textarea. A virtual cursor is only painted glyph data;
	// macOS input methods cannot discover its screen position.
	editor.SetVirtualCursor(false)
	editor.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "ctrl+j"))
	editor.SetStyles(t.textareaStyles())
	editor.Focus()
	filter := textinput.New()
	filter.Prompt = "> "
	filter.Placeholder = "Filter"
	filter.CharLimit = 256
	filter.SetVirtualCursor(false)
	filterStyles := textinput.DefaultDarkStyles()
	filterStyles.Focused.Prompt = t.active
	filterStyles.Focused.Text = lipgloss.NewStyle().Foreground(t.text)
	filterStyles.Focused.Placeholder = t.dim
	filterStyles.Blurred = filterStyles.Focused
	filter.SetStyles(filterStyles)
	filter.Blur()
	connect := newConnectState(options, t)
	inboxList := newSessionBrowser(t)

	chat := viewport.New()
	chat.SoftWrap = true
	chat.FillHeight = true
	chat.MouseWheelEnabled = true

	work := spinner.New(
		spinner.WithSpinner(spinner.Spinner{Frames: []string{"∙  ", "•• ", "•••", " ••", "  ∙", "   "}, FPS: 140 * time.Millisecond}),
		spinner.WithStyle(t.dim),
	)
	return Model{
		backend: backend, ctx: ctx, cancel: cancel, screen: screenConnect, focus: focusEditor,
		events: map[string][]mango.Event{}, seen: map[string]map[string]bool{},
		previews: map[string]*preview{}, unread: map[string]int{},
		expanded: map[string]bool{}, itemCursor: -1,
		fresh: map[string]int{}, markdown: newMarkdownState(),
		chat: chat, editor: editor, filter: filter, inboxList: inboxList, spinner: work, theme: t, connect: connect,
		loading: directAttach != "", loadingLabel: loadingLabel, follow: true,
		directAttach: directAttach, windowFocused: true, options: options,
	}
}

func (m Model) Init() tea.Cmd {
	if m.directAttach != "" {
		return tea.Batch(m.attach(m.directAttach), m.spinner.Tick)
	}
	commands := append([]tea.Cmd{m.spinner.Tick}, m.endpointProbeCommands()...)
	return tea.Batch(commands...)
}

func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		m.renderChat()
		return m, nil
	case spinner.TickMsg:
		var command tea.Cmd
		m.spinner, command = m.spinner.Update(msg)
		if !m.options.ReducedMotion {
			m.motion++
		}
		hasFresh := false
		for id, life := range m.fresh {
			life--
			if life <= 0 {
				delete(m.fresh, id)
				continue
			}
			m.fresh[id] = life
			hasFresh = true
		}
		for threadID, current := range m.previews {
			if !current.startedAt.IsZero() && time.Since(current.startedAt) >= previewMaxAge {
				delete(m.previews, threadID)
				m.status = "stream preview expired"
			}
		}
		if !m.options.ReducedMotion && m.screen == screenChat && (m.previews[m.currentThreadID()] != nil || hasFresh) {
			m.renderChat()
		}
		return m, command
	case tea.FocusMsg:
		m.windowFocused = true
		return m, nil
	case tea.BlurMsg:
		m.windowFocused = false
		return m, nil
	case endpointProbeDone:
		return m.applyEndpointProbe(msg), nil
	case endpointSaved:
		if msg.err != nil {
			m.status = "connected · endpoint not saved"
			m.err = fmt.Errorf("endpoint was not saved: %w", msg.err)
		}
		return m, nil
	case inboxLoaded:
		m.loading, m.err = false, msg.err
		if msg.err == nil {
			m.sessions = fleetOrder(msg.sessions)
			m.status, m.screen = "connected", screenInbox
			m.inboxCursor = clamp(m.inboxCursor, 0, max(2, len(m.sessions)+2))
			listCommand := m.syncInboxList()
			m.resizeInboxList()
			return m, tea.Batch(listCommand, m.saveSelectedEndpoint())
		} else {
			m.screen, m.status = screenConnect, "connection failed"
		}
		return m, nil
	case creationResourcesLoaded:
		if m.dialog != dialogNewSession {
			return m, nil
		}
		return m.resourcesLoaded(msg)
	case creationAgentCreated:
		if m.dialog != dialogNewSession {
			return m, nil
		}
		return m.agentCreated(msg)
	case creationEnvironmentCreated:
		if m.dialog != dialogNewSession {
			return m, nil
		}
		return m.environmentCreated(msg)
	case creationSessionCreated:
		if m.dialog != dialogNewSession {
			return m, nil
		}
		return m.sessionCreated(msg)
	case sessionMutationDone:
		return m.handleSessionMutation(msg)
	case attachedLoaded:
		m.loading, m.err = false, msg.err
		if msg.err != nil {
			if m.reconnecting {
				m.status = "reconnect failed"
				return m, m.reconnectAfter()
			}
			return m, nil
		}
		if m.session != nil && m.session.ID != msg.attachment.Session.ID {
			m.cancelOperation()
		}
		m.stopStream()
		m.session = &msg.attachment.Session
		m.threads = orderThreads(msg.attachment.Threads)
		m.threadCursor = primaryIndex(m.threads)
		m.events = msg.attachment.Events
		m.previews = map[string]*preview{}
		m.rebuildSeen()
		m.stream, m.streamCancel = msg.attachment.Updates, msg.attachment.Cancel
		m.refreshPending()
		m.screen, m.focus, m.status, m.dialog = screenChat, focusEditor, "attached", dialogNone
		m.railCursor = m.threadCursor
		m.editor.Placeholder = "Message coordinator…"
		m.follow = true
		if len(m.pending) > 0 {
			m.openActionDialog(true)
		} else {
			m.editor.Focus()
		}
		m.reconnecting, m.reconnectTry = false, 0
		m.loadingLabel = ""
		m.resize()
		m.renderChat()
		return m, m.waitStream(m.stream)
	case summaryLoaded:
		m.loading, m.err = false, msg.err
		if msg.err == nil && m.session != nil && msg.summary.Session.ID == m.session.ID {
			selected := m.currentThreadID()
			*m.session = msg.summary.Session
			ordered := orderThreads(msg.summary.Threads)
			if !sameRoster(m.threads, ordered) {
				m.loading, m.loadingLabel = true, "Syncing Agents"
				return m, m.attach(m.session.ID)
			}
			m.threads = ordered
			m.threadCursor = max(0, slices.IndexFunc(m.threads, func(t mango.Thread) bool { return t.ID == selected }))
			if m.railCursor < len(m.threads) {
				m.railCursor = m.threadCursor
			}
		}
		return m, nil
	case streamUpdate:
		if msg.source != m.stream {
			return m, nil
		}
		if !msg.open {
			m.stopStream()
			m.status = "stream disconnected"
			m.reconnecting = true
			return m, m.reconnectAfter()
		}
		if msg.update.Err != nil {
			m.err = fmt.Errorf("%s stream: %w", shortID(msg.update.ThreadID), msg.update.Err)
			return m, m.waitStream(msg.source)
		}
		notification := m.notificationFor(msg.update)
		rosterChanged := m.applyStream(msg.update)
		commands := []tea.Cmd{m.waitStream(msg.source), notification}
		if rosterChanged {
			commands = append(commands, m.loadSummary())
		}
		return m, tea.Batch(commands...)
	case operationDone:
		if m.session == nil || msg.sessionID != m.session.ID || msg.operationID != m.activeOperation {
			return m, nil
		}
		m.loading, m.err = false, msg.err
		m.loadingLabel = ""
		m.cancelOperation()
		m.editor.Placeholder = "Message coordinator…"
		if msg.err != nil {
			if current := m.previews[m.primaryThreadID()]; current != nil && current.messageID == "" {
				delete(m.previews, m.primaryThreadID())
				m.renderChat()
			}
			if msg.restoreDraft && strings.TrimSpace(msg.draft) != "" {
				current := strings.TrimSpace(m.editor.Value())
				m.editor.Reset()
				if current == "" {
					m.editor.SetValue(msg.draft)
				} else {
					m.editor.SetValue(msg.draft + "\n\n" + current)
				}
				m.resize()
			}
			m.status = "request failed"
			return m, nil
		}
		if msg.err == nil {
			for _, event := range msg.events {
				threadID := stringAny(event["session_thread_id"])
				if threadID == "" {
					threadID = m.primaryThreadID()
				}
				m.applyStream(mango.StreamUpdate{ThreadID: threadID, Frame: event})
			}
			m.status = msg.label
			m.closeActionDialog()
			m.dialog = dialogNone
			if msg.clearDraft && strings.TrimSpace(m.editor.Value()) == msg.draft {
				m.editor.Reset()
			}
			m.setFocus(focusEditor)
			m.refreshPending()
			if msg.label == "action resolved" && len(m.pending) > 0 {
				// The response arrives before the next durable idle boundary. Do
				// not reopen the just-resolved barrier while that boundary is stale.
				m.pendingDismissed = true
			}
			m.resize()
		}
		return m, nil
	case reconnectTick:
		if m.session != nil && m.stream == nil {
			m.loading, m.loadingLabel = true, "Reconnecting"
			return m, m.attach(m.session.ID)
		}
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" || msg.String() == "ctrl+q" {
			if m.dialog == dialogQuit {
				return m.confirmQuit()
			}
			m.openQuitDialog()
			return m, nil
		}
		if msg.String() == "f1" && m.dialog == dialogNone {
			m.dialog = dialogHelp
			m.editor.Blur()
			return m, nil
		}
		if m.dialog != dialogNone {
			return m.updateDialog(msg)
		}
		if m.screen == screenConnect {
			return m.updateConnect(msg)
		}
		if m.screen == screenInbox {
			return m.updateInbox(msg)
		}
		return m.updateChat(msg)
	case tea.PasteMsg:
		if m.screen == screenConnect {
			return m.updateConnectPaste(msg)
		}
		if m.screen == screenInbox && m.inboxList.SettingFilter() {
			return m, m.updateInboxList(msg)
		}
		return m, nil
	case tea.MouseWheelMsg:
		if m.screen != screenChat || m.dialog != dialogNone {
			return m, nil
		}
		mouse := msg.Mouse()
		headerHeight := lipgloss.Height(m.renderSessionHeader(m.width))
		if mouse.X < 0 || mouse.X >= m.workspaceMainWidth() ||
			mouse.Y < headerHeight || mouse.Y >= headerHeight+m.chat.Height() {
			return m, nil
		}
		var command tea.Cmd
		m.chat, command = m.chat.Update(msg)
		if mouse.Button == tea.MouseWheelUp {
			m.follow = false
		} else if m.chat.AtBottom() {
			m.follow = true
		}
		return m, command
	}
	if m.screen == screenInbox {
		return m, m.updateInboxList(message)
	}
	return m, nil
}

func (m Model) updateConnect(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	return m.updateConnectKey(key)
}

func (m Model) updateInbox(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.inboxList.SettingFilter() {
		return m, m.updateInboxList(key)
	}
	switch key.String() {
	case "left", "h":
		if m.inboxCursor < 3 {
			m.inboxCursor = wrap(m.inboxCursor-1, 3)
			return m, nil
		}
	case "right", "l":
		if m.inboxCursor < 3 {
			m.inboxCursor = wrap(m.inboxCursor+1, 3)
			return m, nil
		}
	case "up", "k":
		if m.inboxCursor >= 3 {
			if m.inboxList.Index() == 0 {
				// Leaving the top of the Session list snaps back onto the
				// toolbar. Landing on the first pill keeps the "left is where
				// I started" mental model consistent across sessions.
				m.inboxCursor = 0
				return m, nil
			}
		}
	case "down", "j":
		if m.inboxCursor < 3 {
			if len(m.inboxList.VisibleItems()) > 0 {
				m.inboxList.GoToStart()
				m.inboxCursor = 3
			}
			return m, nil
		}
	case "ctrl+s":
		m.openSessions()
		return m, nil
	case "ctrl+p":
		m.openCommands()
		m.editor.Blur()
		return m, nil
	case "?":
		m.dialog = dialogHelp
		return m, nil
	case "ctrl+n", "n":
		return m.startNewSession()
	case "r":
		if m.loading {
			return m, nil
		}
		m.loading, m.loadingLabel, m.err = true, "Refreshing Sessions", nil
		return m, m.loadInbox()
	case "m":
		if session, ok := m.selectedInboxSession(); ok {
			m.openSessionManager(session, dialogNone)
		}
		return m, nil
	case "enter":
		if m.loading {
			return m, nil
		}
		switch m.inboxCursor {
		case 0:
			return m.startNewSession()
		case 1:
			return m, m.beginInboxFilter()
		case 2:
			m.loading, m.loadingLabel, m.err = true, "Refreshing Sessions", nil
			return m, m.loadInbox()
		default:
			session, ok := m.selectedInboxSession()
			if !ok {
				return m, nil
			}
			m.loading, m.loadingLabel, m.err = true, "Opening Session", nil
			return m, m.attach(session.ID)
		}
	case "/":
		return m, m.beginInboxFilter()
	case "esc":
		if m.inboxList.IsFiltered() {
			return m, m.updateInboxList(key)
		}
		m.screen, m.status, m.err = screenConnect, "disconnected", nil
		return m, nil
	}
	if m.inboxCursor >= 3 {
		return m, m.updateInboxList(key)
	}
	return m, nil
}

func (m Model) updateChat(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "ctrl+n":
		return m.startNewSession()
	case "ctrl+s":
		m.openSessions()
		return m, nil
	case "ctrl+p":
		m.openCommands()
		return m, nil
	case "ctrl+g":
		m.dialog, m.dialogCursor, m.dialogQuery = dialogAgents, max(0, m.threadCursor), ""
		m.beginFilter("Filter Agents", "")
		return m, nil
	case "ctrl+x":
		if m.focus == focusAgents && m.railCursor >= 0 && m.railCursor < len(m.threads) {
			m.threadCursor = m.railCursor
			m.renderChat()
		}
		m.dialog, m.dialogCursor = dialogInterrupt, 0
		return m, nil
	case "tab", "shift+tab":
		m.cycleSessionFocus(key.String() == "shift+tab")
		return m, nil
	case "esc":
		if m.currentThreadID() != "" && !m.currentThreadPrimary() {
			m.selectCoordinator()
			m.setFocus(focusEditor)
			m.resize()
			m.renderChat()
			return m, nil
		}
		return m.returnToInbox()
	}

	if m.focus == focusEditor {
		if key.String() == "enter" && !m.loading && strings.TrimSpace(m.editor.Value()) != "" {
			text := strings.TrimSpace(m.editor.Value())
			m.editor.Reset()
			m.selectCoordinator()
			m.loading, m.loadingLabel, m.status = true, "Sending message", "sending"
			m.editor.Placeholder = "Working…"
			m.follow = true
			m.beginLiveTurn(m.primaryThreadID())
			ctx, operationID := m.beginOperation()
			return m, m.sendMessage(ctx, operationID, text)
		}
		if key.String() == "/" && strings.TrimSpace(m.editor.Value()) == "" {
			m.openCommands()
			return m, nil
		}
		var command tea.Cmd
		m.editor, command = m.editor.Update(key)
		m.resize()
		return m, command
	}

	if m.focus == focusAgents {
		count := m.railItemCount()
		if count == 0 {
			return m, nil
		}
		switch key.String() {
		case "up", "k":
			m.railCursor = wrap(m.railCursor-1, count)
		case "down", "j":
			m.railCursor = wrap(m.railCursor+1, count)
		case "space":
			return m.openRailSelection(false)
		case "enter", "z":
			return m.openRailSelection(true)
		case "x":
			if m.railCursor >= 0 && m.railCursor < len(m.threads) {
				m.threadCursor = m.railCursor
				m.renderChat()
				m.dialog, m.dialogCursor = dialogInterrupt, 0
			}
		}
		return m, nil
	}

	switch key.String() {
	case "shift+up":
		m.selectFeedItem(-1)
	case "shift+down":
		m.selectFeedItem(1)
	case "space", "enter":
		m.toggleFeedItem()
	case "up", "k":
		m.chat.ScrollUp(1)
		m.follow = false
	case "down", "j":
		m.chat.ScrollDown(1)
		m.follow = m.chat.AtBottom()
	case "pgup", "ctrl+u":
		m.chat.HalfPageUp()
		m.follow = false
	case "pgdown", "ctrl+d":
		m.chat.HalfPageDown()
		m.follow = m.chat.AtBottom()
	case "home", "g":
		m.chat.GotoTop()
		m.follow = false
	case "end", "G":
		m.chat.GotoBottom()
		m.follow = true
	}
	return m, nil
}

func (m *Model) cycleSessionFocus(reverse bool) {
	order := []focus{focusEditor, focusChat}
	if m.hasAgentRail() && m.railItemCount() > 0 {
		order = append(order, focusAgents)
	}
	index := slices.Index(order, m.focus)
	if index < 0 {
		index = 0
	}
	delta := 1
	if reverse {
		delta = -1
	}
	m.setFocus(order[wrap(index+delta, len(order))])
}

func (m Model) railItemCount() int {
	return len(m.threads) + len(m.pending)
}

// openRailSelection projects the selected right-rail item into the main
// conversation area. Threads are inspectable, while pending actions retain
// the existing guarded approval dialog and server-visible event ID.
func (m Model) openRailSelection(focusConversation bool) (tea.Model, tea.Cmd) {
	if m.railCursor < len(m.threads) {
		m.threadCursor = m.railCursor
		m.unread[m.currentThreadID()] = 0
		m.itemCursor = -1
		m.follow = true
		if focusConversation {
			m.setFocus(focusChat)
		}
		m.renderChat()
		return m, nil
	}
	pendingIndex := m.railCursor - len(m.threads)
	if pendingIndex < 0 || pendingIndex >= len(m.pending) {
		return m, nil
	}
	m.actionCursor = pendingIndex
	m.pendingDismissed = false
	m.openActionDialog(false)
	return m, nil
}

func (m Model) updateDialog(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.dialog == dialogQuit {
		switch key.String() {
		case "left", "right", "h", "l", "tab", "shift+tab":
			m.dialogCursor = 1 - m.dialogCursor
		case "y", "Y":
			return m.confirmQuit()
		case "n", "N", "esc":
			m.closeQuitDialog()
		case "enter", "space":
			if m.dialogCursor == 0 {
				return m.confirmQuit()
			}
			m.closeQuitDialog()
		}
		return m, nil
	}
	if m.dialog == dialogNewSession {
		return m.updateNewSession(key)
	}
	if m.sessionManagerDialog() {
		return m.updateSessionManagerDialog(key)
	}
	if m.dialog == dialogResult {
		if m.loading {
			return m, nil
		}
		switch key.String() {
		case "esc":
			m.dialog = dialogAction
			m.editor.Reset()
			m.resize()
			return m, nil
		case "ctrl+e":
			m.resultError = !m.resultError
			return m, nil
		case "enter":
			if strings.TrimSpace(m.editor.Value()) == "" {
				return m, nil
			}
			action := m.pending[clamp(m.actionCursor, 0, len(m.pending)-1)]
			response := mango.ActionResponse{Content: strings.TrimSpace(m.editor.Value()), IsError: m.resultError}
			m.loading, m.loadingLabel = true, "Submitting result"
			ctx, operationID := m.beginOperation()
			return m, m.resolveAction(ctx, operationID, action, response)
		}
		var command tea.Cmd
		m.editor, command = m.editor.Update(key)
		return m, command
	}
	if m.dialog == dialogAction && m.loading {
		return m, nil
	}
	if m.dialog == dialogAction && m.actionDialogInGrace() {
		m.dialogGraceLastInputAt = time.Now()
		return m, nil
	}
	if key.String() == "esc" {
		if m.dialog == dialogAction {
			m.pendingDismissed = true
			m.closeActionDialog()
		}
		m.dialog = dialogNone
		m.filter.Blur()
		m.setFocus(focusEditor)
		m.resize()
		return m, nil
	}
	switch m.dialog {
	case dialogCommands:
		commands := m.filteredCommands()
		switch key.String() {
		case "up":
			m.dialogCursor = wrap(m.dialogCursor-1, len(commands))
		case "down":
			m.dialogCursor = wrap(m.dialogCursor+1, len(commands))
		case "ctrl+u":
			m.dialogQuery, m.dialogCursor = "", 0
			m.filter.Reset()
		case "enter":
			if len(commands) > 0 {
				return m.runCommand(commands[clamp(m.dialogCursor, 0, len(commands)-1)].id)
			}
		default:
			var command tea.Cmd
			m.filter, command = m.filter.Update(key)
			m.dialogQuery = m.filter.Value()
			m.dialogCursor = 0
			return m, command
		}
	case dialogAgents:
		threads := m.filteredThreads()
		if len(m.threads) == 0 {
			m.dialog = dialogNone
			return m, nil
		}
		switch key.String() {
		case "up":
			m.dialogCursor = wrap(m.dialogCursor-1, len(threads))
		case "down":
			m.dialogCursor = wrap(m.dialogCursor+1, len(threads))
		case "ctrl+u":
			m.dialogQuery, m.dialogCursor = "", 0
			m.filter.Reset()
		case "enter":
			if len(threads) == 0 {
				break
			}
			selectedID := threads[clamp(m.dialogCursor, 0, len(threads)-1)].ID
			m.threadCursor = slices.IndexFunc(m.threads, func(thread mango.Thread) bool { return thread.ID == selectedID })
			m.railCursor = m.threadCursor
			m.unread[m.currentThreadID()] = 0
			m.itemCursor = -1
			m.follow = true
			m.dialog = dialogNone
			m.renderChat()
		default:
			var command tea.Cmd
			m.filter, command = m.filter.Update(key)
			m.dialogQuery = m.filter.Value()
			m.dialogCursor = 0
			return m, command
		}
	case dialogSessions:
		filtered := m.filteredSessions()
		switch key.String() {
		case "up":
			m.inboxCursor = wrap(m.inboxCursor-1, len(filtered))
		case "down":
			m.inboxCursor = wrap(m.inboxCursor+1, len(filtered))
		case "ctrl+n":
			return m.startNewSession()
		case "ctrl+e":
			if len(filtered) > 0 {
				m.openSessionManager(filtered[clamp(m.inboxCursor, 0, len(filtered)-1)], dialogSessions)
			}
		case "ctrl+u":
			m.inboxFilter, m.inboxCursor = "", 0
			m.filter.Reset()
		case "enter":
			if len(filtered) > 0 {
				m.loading, m.loadingLabel = true, "Opening Session"
				return m, m.attach(filtered[clamp(m.inboxCursor, 0, len(filtered)-1)].ID)
			}
		default:
			var command tea.Cmd
			m.filter, command = m.filter.Update(key)
			m.inboxFilter = m.filter.Value()
			m.inboxCursor = 0
			return m, command
		}
	case dialogAction:
		if len(m.pending) == 0 {
			m.dialog = dialogNone
			return m, nil
		}
		switch key.String() {
		case "up", "k":
			m.actionCursor = wrap(m.actionCursor-1, len(m.pending))
		case "down", "j":
			m.actionCursor = wrap(m.actionCursor+1, len(m.pending))
		case "left", "h", "shift+tab":
			m.actionChoice = wrap(m.actionChoice-1, 2)
		case "right", "l", "tab":
			m.actionChoice = wrap(m.actionChoice+1, 2)
		case "a":
			m.actionChoice = 0
			return m.submitActionChoice()
		case "d":
			m.actionChoice = 1
			return m.submitActionChoice()
		case "enter":
			return m.submitActionChoice()
		}
	case dialogInterrupt:
		switch key.String() {
		case "left", "h", "right", "l", "tab", "shift+tab":
			m.dialogCursor = 1 - m.dialogCursor
		case "enter":
			if m.dialogCursor == 1 {
				m.dialog = dialogNone
				return m, nil
			}
			threadID := m.currentThreadID()
			m.loading, m.loadingLabel, m.dialog = true, "Interrupting Agent", dialogNone
			ctx, operationID := m.beginOperation()
			return m, m.interrupt(ctx, operationID, threadID)
		}
	}
	return m, nil
}

type commandItem struct{ id, title, help string }

func (m Model) commands() []commandItem {
	if m.session == nil {
		return []commandItem{
			{"new_session", "New Session", "choose an Agent and Environment"},
			{"sessions", "Sessions", "open a durable Session"},
			{"help", "Keyboard help", "show every Mango shortcut"},
			{"quit", "Quit Mango", "leave managed work running"},
		}
	}
	commands := []commandItem{
		{"new_session", "New Session", "choose an Agent and Environment"},
		{"sessions", "Sessions", "open or switch durable Session"},
		{"manage_session", "Manage Session", "rename, interrupt, archive, or delete"},
		{"agents", "Switch Agent view", "open the Agent picker"},
		{"interrupt", "Interrupt current Agent", "cancel only the selected Thread"},
		{"interrupt_all", "Interrupt all Agents", "cancel the whole Session"},
		{"inbox", "Back to Sessions", "detach without stopping work"},
		{"help", "Keyboard help", "show every Mango shortcut"},
		{"quit", "Quit Mango", "leave managed work running"},
	}
	if len(m.pending) > 0 {
		commands = append([]commandItem{{"actions", "Review pending actions", fmt.Sprintf("%d waiting", len(m.pending))}}, commands...)
	}
	return commands
}

func (m Model) filteredCommands() []commandItem {
	commands := m.commands()
	query := strings.TrimSpace(m.dialogQuery)
	if query == "" {
		return commands
	}
	filtered := make([]commandItem, 0, len(commands))
	for _, command := range commands {
		if fuzzyContains(command.title+" "+command.help+" "+command.id, query) {
			filtered = append(filtered, command)
		}
	}
	return filtered
}

func (m Model) runCommand(id string) (tea.Model, tea.Cmd) {
	m.dialog = dialogNone
	switch id {
	case "new_session":
		return m.startNewSession()
	case "sessions":
		m.openSessions()
	case "manage_session":
		if m.session != nil {
			m.openSessionManager(*m.session, dialogNone)
		}
	case "actions":
		m.pendingDismissed = false
		m.openActionDialog(false)
	case "agents":
		m.dialog, m.dialogCursor, m.dialogQuery = dialogAgents, max(0, m.threadCursor), ""
		m.beginFilter("Filter Agents", "")
	case "interrupt":
		m.dialog, m.dialogCursor = dialogInterrupt, 0
	case "interrupt_all":
		m.loading, m.loadingLabel = true, "Interrupting Agents"
		ctx, operationID := m.beginOperation()
		return m, m.interrupt(ctx, operationID, "")
	case "inbox":
		return m.returnToInbox()
	case "help":
		m.dialog = dialogHelp
		m.editor.Blur()
	case "quit":
		m.openQuitDialog()
	}
	return m, nil
}

func (m *Model) openQuitDialog() {
	m.quitReturnDialog = m.dialog
	m.dialog = dialogQuit
	m.dialogCursor = 1 // Keep working is the safe default.
}

func (m *Model) closeQuitDialog() {
	m.dialog = m.quitReturnDialog
	m.quitReturnDialog = dialogNone
	if m.dialog != dialogNone {
		return
	}
	m.filter.Blur()
	if m.screen == screenChat {
		m.setFocus(m.focus)
	} else {
		m.editor.Blur()
	}
	m.resize()
}

func (m Model) confirmQuit() (tea.Model, tea.Cmd) {
	m.cancel()
	m.stopStream()
	return m, tea.Quit
}

func (m Model) returnToInbox() (tea.Model, tea.Cmd) {
	m.cancelOperation()
	m.stopStream()
	m.reconnecting, m.reconnectTry = false, 0
	if m.session != nil {
		m.lastAttachedID = m.session.ID
	}
	m.screen, m.loading, m.loadingLabel, m.session, m.err = screenInbox, true, "Refreshing Sessions", nil, nil
	m.editor.Reset()
	m.editor.Placeholder = landingPlaceholder(screenChat)
	m.editor.Blur()
	return m, m.loadInbox()
}

func (m Model) submitActionChoice() (tea.Model, tea.Cmd) {
	if len(m.pending) == 0 {
		m.dialog = dialogNone
		return m, nil
	}
	action := m.pending[clamp(m.actionCursor, 0, len(m.pending)-1)]
	if action.Kind != mango.ActionConfirmation {
		m.dialog, m.resultError = dialogResult, false
		m.editor.Reset()
		m.editor.Placeholder = "Return tool result…"
		m.editor.Focus()
		m.resize()
		return m, nil
	}
	response := mango.ActionResponse{Result: "allow"}
	if m.actionChoice == 1 {
		response.Result = "deny"
	}
	m.loading, m.loadingLabel = true, "Submitting action"
	ctx, operationID := m.beginOperation()
	return m, m.resolveAction(ctx, operationID, action, response)
}

func (m *Model) setFocus(next focus) {
	m.focus = next
	if next == focusEditor {
		m.editor.Focus()
	} else {
		m.editor.Blur()
		if next == focusChat && m.threadCursor >= 0 && m.threadCursor < len(m.threads) {
			items := feed.Project(m.threads[m.threadCursor], m.events[m.currentThreadID()])
			if len(items) > 0 {
				m.itemCursor = len(items) - 1
			}
		}
		m.renderChat()
	}
}

func (m *Model) selectThread(delta int) {
	if len(m.threads) == 0 {
		return
	}
	m.threadCursor = wrap(m.threadCursor+delta, len(m.threads))
	m.railCursor = m.threadCursor
	m.unread[m.currentThreadID()] = 0
	m.itemCursor = -1
	m.follow = true
	m.renderChat()
}

func (m *Model) selectFeedItem(delta int) {
	if m.threadCursor < 0 || m.threadCursor >= len(m.threads) {
		return
	}
	items := feed.Project(m.threads[m.threadCursor], m.events[m.currentThreadID()])
	if len(items) == 0 {
		return
	}
	if m.itemCursor < 0 {
		m.itemCursor = len(items) - 1
	} else {
		m.itemCursor = clamp(m.itemCursor+delta, 0, len(items)-1)
	}
	m.renderChat()
}

func (m *Model) toggleFeedItem() {
	if m.threadCursor < 0 || m.threadCursor >= len(m.threads) {
		return
	}
	items := feed.Project(m.threads[m.threadCursor], m.events[m.currentThreadID()])
	if len(items) == 0 {
		return
	}
	if m.itemCursor < 0 || m.itemCursor >= len(items) {
		m.itemCursor = len(items) - 1
	}
	id := items[m.itemCursor].ID
	if id != "" {
		m.expanded[id] = !m.expanded[id]
		m.renderChat()
	}
}

func (m *Model) setFeedItemExpanded(expanded bool) {
	if m.threadCursor < 0 || m.threadCursor >= len(m.threads) {
		return
	}
	items := feed.Project(m.threads[m.threadCursor], m.events[m.currentThreadID()])
	if len(items) == 0 {
		return
	}
	if m.itemCursor < 0 || m.itemCursor >= len(items) {
		m.itemCursor = len(items) - 1
	}
	id := items[m.itemCursor].ID
	if id != "" {
		m.expanded[id] = expanded
		m.renderChat()
	}
}

func (m Model) currentThreadID() string {
	if m.threadCursor < 0 || m.threadCursor >= len(m.threads) {
		return ""
	}
	return m.threads[m.threadCursor].ID
}

func (m Model) primaryThreadID() string {
	for _, thread := range m.threads {
		if thread.Primary() {
			return thread.ID
		}
	}
	return ""
}

func (m Model) currentThreadPrimary() bool {
	return m.threadCursor >= 0 && m.threadCursor < len(m.threads) && m.threads[m.threadCursor].Primary()
}

func (m *Model) selectCoordinator() {
	index := primaryIndex(m.threads)
	if index < 0 || index >= len(m.threads) {
		return
	}
	m.threadCursor = index
	m.railCursor = index
	m.unread[m.currentThreadID()] = 0
	m.itemCursor = -1
	m.follow = true
}

func (m Model) currentThreadRunning() bool {
	return m.threadCursor >= 0 && m.threadCursor < len(m.threads) && m.threads[m.threadCursor].Status == "running"
}

func (m Model) filteredSessions() []mango.Session {
	needle := strings.ToLower(strings.TrimSpace(m.inboxFilter))
	if needle == "" {
		return m.sessions
	}
	filtered := make([]mango.Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		text := strings.ToLower(session.Title + " " + session.ID + " " + session.Agent.Name + " " + session.Status)
		if strings.Contains(text, needle) {
			filtered = append(filtered, session)
		}
	}
	return filtered
}

func (m Model) filteredThreads() []mango.Thread {
	query := strings.TrimSpace(m.dialogQuery)
	if query == "" {
		return m.threads
	}
	filtered := make([]mango.Thread, 0, len(m.threads))
	for _, thread := range m.threads {
		if fuzzyContains(thread.Agent.Name+" "+thread.Agent.Model.ID+" "+thread.Status+" "+thread.ID, query) {
			filtered = append(filtered, thread)
		}
	}
	return filtered
}

func (m *Model) openSessions() {
	m.dialog = dialogSessions
	m.dialogCursor = 0
	m.inboxFilter = ""
	m.inboxCursor = 0
	m.editor.Blur()
	m.beginFilter("Filter Sessions", "")
}

func (m *Model) openCommands() {
	m.dialog = dialogCommands
	m.dialogCursor = 0
	m.dialogQuery = ""
	m.editor.Blur()
	m.beginFilter("Filter commands", "")
}

func (m *Model) beginFilter(placeholder, value string) {
	m.filter.Reset()
	m.filter.Placeholder = placeholder
	m.filter.SetValue(value)
	m.filter.SetWidth(max(12, dialogInnerWidth(m.dialogWidth())-3))
	m.filter.Focus()
}

func (m *Model) applyStream(update mango.StreamUpdate) bool {
	frame := update.Frame
	rosterChanged := false
	switch frame.Type() {
	case "event_start":
		event, _ := frame["event"].(map[string]any)
		id, typ := stringAny(event["id"]), stringAny(event["type"])
		if id != "" {
			current := m.ensurePreview(update.ThreadID)
			switch typ {
			case "agent.thinking":
				current.thinkingID = id
				current.waiting = true
			case "agent.message":
				current.messageID = id
				current.thinkingID = ""
				current.waiting = false
			}
		}
	case "event_delta":
		id := stringAny(frame["event_id"])
		if id == "" {
			return false
		}
		current := m.ensurePreview(update.ThreadID)
		// A reconnect can join between event_start and event_delta. Treat the
		// first delta as enough evidence to recover the live message instead of
		// silently dropping the rest of the streamed answer.
		if current.messageID == "" {
			current.messageID = id
			current.thinkingID = ""
			current.waiting = false
		}
		if id != current.messageID {
			return false
		}
		delta, _ := frame["delta"].(map[string]any)
		content, _ := delta["content"].(map[string]any)
		text := first(stringAny(content["text"]), stringAny(delta["text"]))
		current.content += capUTF8(text, previewMaxBytes-len(current.content))
	default:
		id := frame.ID()
		if id != "" && m.seen[update.ThreadID][id] {
			return false
		}
		if m.seen[update.ThreadID] == nil {
			m.seen[update.ThreadID] = map[string]bool{}
		}
		if id != "" {
			m.seen[update.ThreadID][id] = true
			if eventDeservesSpark(frame.Type()) {
				m.fresh[id] = 10
			}
		}
		m.events[update.ThreadID] = append(m.events[update.ThreadID], frame)
		if current := m.previews[update.ThreadID]; current != nil {
			switch frame.Type() {
			case "agent.thinking":
				if id == "" || id == current.thinkingID {
					current.thinkingID = ""
					if current.messageID == "" {
						current.waiting = false
					}
				}
			case "agent.message":
				if id == "" || current.messageID == "" || id == current.messageID {
					delete(m.previews, update.ThreadID)
				}
			case "span.model_request_end":
				// A failed or text-free provider turn has no authoritative
				// agent.message to retire its optimistic live state.
				if boolAny(frame["is_error"]) || current.messageID == "" {
					delete(m.previews, update.ThreadID)
				}
			case "session.status_idle":
				delete(m.previews, update.ThreadID)
			}
		}
		m.applyUsageEvent(update.ThreadID, frame)
		if update.ThreadID != m.currentThreadID() {
			m.unread[update.ThreadID]++
		}
		pendingChanged := frame.Type() == "session.status_idle" && update.ThreadID == m.primaryThreadID()
		if pendingChanged {
			m.refreshPending()
		}
		m.applyStatusEvent(update.ThreadID, frame)
		rosterChanged = frame.Type() == "session.thread_created" || frame.Type() == "session.updated"
		if pendingChanged && len(m.pending) == 0 && (m.dialog == dialogAction || m.dialog == dialogResult) {
			m.closeActionDialog()
			m.dialog = dialogNone
			m.setFocus(focusEditor)
			m.resize()
		} else if pendingChanged && len(m.pending) > 0 && m.dialog == dialogNone && !m.pendingDismissed {
			m.openActionDialog(true)
			m.resize()
		}
	}
	if update.ThreadID == m.currentThreadID() {
		m.renderChat()
	}
	return rosterChanged
}

func (m *Model) beginLiveTurn(threadID string) {
	if threadID == "" {
		return
	}
	m.previews[threadID] = &preview{waiting: true, startedAt: time.Now()}
	m.renderChat()
}

func (m *Model) ensurePreview(threadID string) *preview {
	current := m.previews[threadID]
	if current == nil {
		current = &preview{startedAt: time.Now()}
		m.previews[threadID] = current
	}
	if current.startedAt.IsZero() {
		current.startedAt = time.Now()
	}
	return current
}

func (m *Model) applyUsageEvent(threadID string, event mango.Event) {
	if event.Type() != "span.model_request_end" {
		return
	}
	usage, _ := event["model_usage"].(map[string]any)
	input := int64Any(usage["input_tokens"])
	output := int64Any(usage["output_tokens"])
	cacheRead := int64Any(usage["cache_read_input_tokens"])
	if input == 0 && output == 0 && cacheRead == 0 {
		return
	}
	for index := range m.threads {
		if m.threads[index].ID != threadID {
			continue
		}
		m.threads[index].Usage.InputTokens += input
		m.threads[index].Usage.OutputTokens += output
		m.threads[index].Usage.CacheReadInputTokens += cacheRead
		break
	}
	if m.session != nil {
		m.session.Usage.InputTokens += input
		m.session.Usage.OutputTokens += output
		m.session.Usage.CacheReadInputTokens += cacheRead
	}
}

func eventDeservesSpark(typ string) bool {
	switch typ {
	case "agent.message", "agent.thread_message_received", "agent.thread_message_sent",
		"agent.tool_use", "agent.tool_result", "session.thread_created":
		return true
	default:
		return false
	}
}

func (m Model) notificationFor(update mango.StreamUpdate) tea.Cmd {
	if m.windowFocused || update.Err != nil || update.Frame.ID() == "" {
		return nil
	}
	if m.seen[update.ThreadID] != nil && m.seen[update.ThreadID][update.Frame.ID()] {
		return nil
	}
	frame := update.Frame
	if frame.Type() != "session.status_idle" {
		return nil
	}
	stopReason, _ := frame["stop_reason"].(map[string]any)
	title := "Mango finished a turn"
	message := first(m.sessionTitle(), "Managed Agent Session")
	if stringAny(stopReason["type"]) == "requires_action" {
		title = "Mango needs you"
		message = "An Agent is waiting for an action"
	}
	return m.terminalNotify(title, message)
}

func (m Model) sessionTitle() string {
	if m.session == nil {
		return ""
	}
	return m.session.Title
}

func (m Model) terminalNotify(title, message string) tea.Cmd {
	clean := func(value string) string {
		value = strings.Map(func(r rune) rune {
			switch r {
			case '\a', '\x1b', ';':
				return ' '
			default:
				return r
			}
		}, value)
		return trimOneLine(value, 180)
	}
	switch m.options.Notifications {
	case NotificationBell:
		return tea.Raw("\a")
	case NotificationOSC777:
		return tea.Raw("\x1b]777;notify;" + clean(title) + ";" + clean(message) + "\a")
	default:
		return nil
	}
}

func (m *Model) refreshPending() {
	pending := feed.PendingActions(m.primaryThreadID(), m.events)
	parts := make([]string, 0, len(pending))
	for _, action := range pending {
		parts = append(parts, string(action.Kind)+":"+action.ID)
	}
	signature := strings.Join(parts, "|")
	if signature != m.pendingSignature {
		m.pendingDismissed = false
	}
	m.pending, m.pendingSignature = pending, signature
	m.actionCursor = clamp(m.actionCursor, 0, len(m.pending)-1)
	m.railCursor = clamp(m.railCursor, 0, max(0, m.railItemCount()-1))
}

func (m *Model) openActionDialog(async bool) {
	now := time.Now()
	m.dialog, m.actionCursor, m.actionChoice = dialogAction, clamp(m.actionCursor, 0, len(m.pending)-1), 0
	m.editor.Blur()
	if !async || (!m.lastActionClosedAt.IsZero() && now.Sub(m.lastActionClosedAt) < actionReopenWindow) {
		m.dialogGraceOpenedAt, m.dialogGraceLastInputAt = time.Time{}, time.Time{}
		return
	}
	m.dialogGraceOpenedAt, m.dialogGraceLastInputAt = now, now
}

func (m *Model) closeActionDialog() {
	if m.dialog == dialogAction || m.dialog == dialogResult {
		m.lastActionClosedAt = time.Now()
	}
	m.dialogGraceOpenedAt, m.dialogGraceLastInputAt = time.Time{}, time.Time{}
}

func (m Model) actionDialogInGrace() bool {
	if m.dialogGraceOpenedAt.IsZero() {
		return false
	}
	now := time.Now()
	return now.Sub(m.dialogGraceOpenedAt) < actionGraceMaxDelay &&
		now.Sub(m.dialogGraceLastInputAt) < actionGraceQuietPeriod
}

func (m *Model) applyStatusEvent(threadID string, event mango.Event) {
	status := ""
	switch event.Type() {
	case "session.thread_status_running":
		status = "running"
	case "session.thread_status_idle":
		status = "idle"
	case "session.thread_status_rescheduled":
		status = "rescheduling"
	case "session.thread_status_terminated":
		status = "terminated"
	}
	if status == "" {
		return
	}
	target := stringAny(event["session_thread_id"])
	if target == "" {
		target = threadID
	}
	for index := range m.threads {
		if m.threads[index].ID == target {
			m.threads[index].Status = status
		}
	}
}

func (m *Model) rebuildSeen() {
	m.seen = map[string]map[string]bool{}
	for threadID, events := range m.events {
		m.seen[threadID] = map[string]bool{}
		for _, event := range events {
			if event.ID() != "" {
				m.seen[threadID][event.ID()] = true
			}
		}
	}
}

func orderThreads(threads []mango.Thread) []mango.Thread {
	ordered := append([]mango.Thread(nil), threads...)
	slices.SortStableFunc(ordered, func(a, b mango.Thread) int {
		if a.Primary() {
			return -1
		}
		if b.Primary() {
			return 1
		}
		return strings.Compare(a.Agent.Name, b.Agent.Name)
	})
	return ordered
}

func primaryIndex(threads []mango.Thread) int {
	for index, thread := range threads {
		if thread.Primary() {
			return index
		}
	}
	return 0
}

func sameRoster(left, right []mango.Thread) bool {
	if len(left) != len(right) {
		return false
	}
	ids := make(map[string]struct{}, len(left))
	for _, thread := range left {
		ids[thread.ID] = struct{}{}
	}
	for _, thread := range right {
		if _, ok := ids[thread.ID]; !ok {
			return false
		}
	}
	return true
}

func (m Model) loadInbox() tea.Cmd {
	return func() tea.Msg { sessions, err := m.backend.ListSessions(m.ctx); return inboxLoaded{sessions, err} }
}

func (m Model) attach(sessionID string) tea.Cmd {
	return func() tea.Msg {
		attachment, err := m.backend.Attach(m.ctx, sessionID)
		return attachedLoaded{attachment, err}
	}
}

func (m Model) loadSummary() tea.Cmd {
	return func() tea.Msg {
		summary, err := m.backend.Refresh(m.ctx, m.session.ID)
		return summaryLoaded{summary, err}
	}
}

func (m Model) sendMessage(ctx context.Context, operationID uint64, text string) tea.Cmd {
	sessionID := m.session.ID
	return func() tea.Msg {
		events, err := m.backend.SendMessage(ctx, sessionID, text)
		return operationDone{
			sessionID: sessionID, operationID: operationID, label: "message sent", events: events,
			draft: text, restoreDraft: true, err: err,
		}
	}
}

func (m Model) interrupt(ctx context.Context, operationID uint64, threadID string) tea.Cmd {
	sessionID := m.session.ID
	return func() tea.Msg {
		events, err := m.backend.Interrupt(ctx, sessionID, threadID)
		return operationDone{sessionID: sessionID, operationID: operationID, label: "interrupt sent", events: events, err: err}
	}
}

func (m Model) resolveAction(ctx context.Context, operationID uint64, action mango.Action, response mango.ActionResponse) tea.Cmd {
	sessionID := m.session.ID
	return func() tea.Msg {
		events, err := m.backend.ResolveAction(ctx, sessionID, action, response)
		return operationDone{
			sessionID: sessionID, operationID: operationID, label: "action resolved", events: events,
			draft: response.Content, clearDraft: action.Kind != mango.ActionConfirmation, err: err,
		}
	}
}

func (m Model) waitStream(source <-chan mango.StreamUpdate) tea.Cmd {
	return func() tea.Msg { update, open := <-source; return streamUpdate{source, update, open} }
}

func (m *Model) reconnectAfter() tea.Cmd {
	delay := time.Second * time.Duration(1<<min(m.reconnectTry, 4))
	m.reconnectTry++
	return tea.Tick(delay, func(now time.Time) tea.Msg { return reconnectTick(now) })
}

func (m *Model) stopStream() {
	if m.streamCancel != nil {
		m.streamCancel()
	}
	m.streamCancel, m.stream = nil, nil
}

func (m *Model) beginOperation() (context.Context, uint64) {
	m.cancelOperation()
	ctx, cancel := context.WithCancel(m.ctx)
	m.operationCancel = cancel
	m.operationSeq++
	m.activeOperation = m.operationSeq
	return ctx, m.activeOperation
}

func (m *Model) cancelOperation() {
	if m.operationCancel != nil {
		m.operationCancel()
		m.operationCancel = nil
	}
	m.activeOperation = 0
}

func capUTF8(value string, remaining int) string {
	if remaining <= 0 {
		return ""
	}
	if len(value) <= remaining {
		return value
	}
	value = value[:remaining]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value
}

func stringAny(value any) string { text, _ := value.(string); return text }
func boolAny(value any) bool     { result, _ := value.(bool); return result }
func int64Any(value any) int64 {
	switch value := value.(type) {
	case int:
		return int64(value)
	case int64:
		return value
	case float64:
		return int64(value)
	case float32:
		return int64(value)
	default:
		return 0
	}
}
func landingPlaceholder(current screen) string {
	if current == screenInbox {
		return "Ask a managed Agent anything…"
	}
	return "Message coordinator…"
}
func wrap(value, length int) int {
	if length <= 0 {
		return 0
	}
	return (value%length + length) % length
}
func clamp(value, low, high int) int {
	if high < low {
		return low
	}
	return min(max(value, low), high)
}
