package ui

import (
	"fmt"
	"net/url"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/yanpgwang/mango/examples/terminal-ui/internal/mango"
)

type createStep int

const (
	createChooseAgent createStep = iota
	createAgentName
	createAgentModel
	createAgentSystem
	createAgentTools
	createAgentTopology
	createAgentRoster
	createAgentMCP
	createAgentMCPName
	createAgentMCPURL
	createChooseEnvironment
	createEnvironmentName
	createSessionTitle
	createInitialPrompt
	createConfirm
)

type mcpServerConfig struct {
	Name string
	URL  string
}

type createState struct {
	step              createStep
	agents            []mango.Agent
	environments      []mango.Environment
	agentCursor       int
	environmentCursor int
	agentQuery        string
	environmentQuery  string
	confirmChoice     int
	agent             mango.Agent
	environment       mango.Environment
	agentName         string
	agentModel        string
	agentSystem       string
	agentTools        bool
	agentTopology     int
	rosterCursor      int
	rosterSelected    map[string]bool
	mcpChoice         int
	mcpServers        []mcpServerConfig
	mcpName           string
	mcpURL            string
	environmentName   string
	title             string
	prompt            string
	quick             bool
	mutating          bool
}

type creationResourcesLoaded struct {
	agents       []mango.Agent
	environments []mango.Environment
	err          error
}

type creationAgentCreated struct {
	agent mango.Agent
	err   error
}

type creationEnvironmentCreated struct {
	environment mango.Environment
	err         error
}

type creationSessionCreated struct {
	session mango.Session
	err     error
}

func (m Model) startNewSession() (tea.Model, tea.Cmd) {
	m.creation = createState{step: createChooseAgent, agentTools: true, rosterSelected: map[string]bool{}}
	m.dialog = dialogNewSession
	m.loading, m.loadingLabel = true, "Loading Agents and Environments"
	m.err = nil
	m.editor.Blur()
	return m, m.loadCreationResources()
}

func (m Model) startSessionFromLanding(prompt string) (tea.Model, tea.Cmd) {
	prompt = strings.TrimSpace(prompt)
	m.creation = createState{
		step: createChooseAgent, prompt: prompt, title: titleFromPrompt(prompt), quick: true,
		agentTools: true, rosterSelected: map[string]bool{},
	}
	m.dialog = dialogNewSession
	m.loading, m.loadingLabel = true, "Loading Agents and Environments"
	m.err = nil
	m.editor.Reset()
	m.editor.Blur()
	return m, m.loadCreationResources()
}

func (m Model) resourcesLoaded(msg creationResourcesLoaded) (tea.Model, tea.Cmd) {
	m.loading, m.err = false, msg.err
	m.loadingLabel = ""
	if msg.err != nil {
		return m, nil
	}
	if m.creation.rosterSelected == nil {
		m.creation.rosterSelected = map[string]bool{}
	}
	for _, agent := range msg.agents {
		if agent.ArchivedAt == nil {
			m.creation.agents = append(m.creation.agents, agent)
		}
	}
	for _, environment := range msg.environments {
		if environment.ArchivedAt == nil {
			m.creation.environments = append(m.creation.environments, environment)
		}
	}
	m.creation.step = createChooseAgent
	m.creation.agentCursor = 0
	if len(m.creation.agents) > 0 {
		m.creation.agentCursor = 1
	}
	m.editor.Blur()
	m.beginFilter("Filter Agents", m.creation.agentQuery)
	return m, nil
}

func (m Model) agentCreated(msg creationAgentCreated) (tea.Model, tea.Cmd) {
	m.loading, m.creation.mutating, m.err = false, false, msg.err
	m.loadingLabel = ""
	if msg.err != nil {
		return m, nil
	}
	m.creation.agents = append(m.creation.agents, msg.agent)
	m.creation.agent = msg.agent
	m.creation.step = createChooseEnvironment
	m.creation.environmentCursor = 0
	if len(m.creation.environments) > 0 {
		m.creation.environmentCursor = 1
	}
	m.editor.Blur()
	m.beginFilter("Filter Environments", m.creation.environmentQuery)
	return m, nil
}

func (m Model) environmentCreated(msg creationEnvironmentCreated) (tea.Model, tea.Cmd) {
	m.loading, m.creation.mutating, m.err = false, false, msg.err
	m.loadingLabel = ""
	if msg.err != nil {
		return m, nil
	}
	m.creation.environments = append(m.creation.environments, msg.environment)
	m.creation.environment = msg.environment
	m.continueAfterEnvironment()
	return m, nil
}

func (m Model) sessionCreated(msg creationSessionCreated) (tea.Model, tea.Cmd) {
	m.loading, m.creation.mutating, m.err = false, false, msg.err
	m.loadingLabel = ""
	if msg.err != nil {
		return m, nil
	}
	m.dialog = dialogNone
	m.creation = createState{}
	m.editor.Reset()
	m.editor.Placeholder = "Ready!"
	m.editor.Focus()
	m.loading, m.loadingLabel = true, "Opening Session"
	m.status = "Session created"
	return m, m.attach(msg.session.ID)
}

func (m Model) updateNewSession(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.loading {
		if key.String() == "esc" && !m.creation.mutating {
			m.cancelCreation()
		}
		return m, nil
	}
	if key.String() == "esc" {
		m.backCreation()
		return m, nil
	}

	switch m.creation.step {
	case createChooseAgent:
		agents := m.filteredCreationAgents()
		choiceCount := len(agents) + 1
		switch key.String() {
		case "up":
			m.creation.agentCursor = wrap(m.creation.agentCursor-1, choiceCount)
		case "down":
			m.creation.agentCursor = wrap(m.creation.agentCursor+1, choiceCount)
		case "ctrl+a":
			m.beginCreationField(createAgentName, "Agent name", m.creation.agentName)
		case "enter":
			if m.creation.agentCursor == 0 {
				m.beginCreationField(createAgentName, "Agent name", m.creation.agentName)
				break
			}
			if len(agents) == 0 {
				break
			}
			m.creation.agent = agents[clamp(m.creation.agentCursor-1, 0, len(agents)-1)]
			m.creation.step = createChooseEnvironment
			m.creation.environmentCursor = 0
			if len(m.creation.environments) > 0 {
				m.creation.environmentCursor = 1
			}
			m.beginFilter("Filter Environments", m.creation.environmentQuery)
		case "backspace":
			var command tea.Cmd
			m.filter, command = m.filter.Update(key)
			m.creation.agentQuery = m.filter.Value()
			m.creation.agentCursor = firstResourceCursor(len(m.filteredCreationAgents()))
			return m, command
		default:
			if key.Key().Text != "" {
				var command tea.Cmd
				m.filter, command = m.filter.Update(key)
				m.creation.agentQuery = m.filter.Value()
				m.creation.agentCursor = firstResourceCursor(len(m.filteredCreationAgents()))
				return m, command
			}
		}
	case createChooseEnvironment:
		environments := m.filteredCreationEnvironments()
		choiceCount := len(environments) + 1
		switch key.String() {
		case "up":
			m.creation.environmentCursor = wrap(m.creation.environmentCursor-1, choiceCount)
		case "down":
			m.creation.environmentCursor = wrap(m.creation.environmentCursor+1, choiceCount)
		case "ctrl+e":
			m.beginCreationField(createEnvironmentName, "Environment name", first(m.creation.environmentName, "Mango cloud"))
		case "enter":
			if m.creation.environmentCursor == 0 {
				m.beginCreationField(createEnvironmentName, "Environment name", first(m.creation.environmentName, "Mango cloud"))
				break
			}
			if len(environments) == 0 {
				break
			}
			m.creation.environment = environments[clamp(m.creation.environmentCursor-1, 0, len(environments)-1)]
			m.continueAfterEnvironment()
		case "backspace":
			var command tea.Cmd
			m.filter, command = m.filter.Update(key)
			m.creation.environmentQuery = m.filter.Value()
			m.creation.environmentCursor = firstResourceCursor(len(m.filteredCreationEnvironments()))
			return m, command
		default:
			if key.Key().Text != "" {
				var command tea.Cmd
				m.filter, command = m.filter.Update(key)
				m.creation.environmentQuery = m.filter.Value()
				m.creation.environmentCursor = firstResourceCursor(len(m.filteredCreationEnvironments()))
				return m, command
			}
		}
	case createAgentName:
		if key.String() == "enter" {
			value := strings.TrimSpace(m.editor.Value())
			if value != "" {
				m.creation.agentName = value
				m.beginCreationField(createAgentModel, "Model ID", m.defaultAgentModel())
			}
			return m, nil
		}
		return m.updateCreationEditor(key)
	case createAgentModel:
		if key.String() == "enter" {
			value := strings.TrimSpace(m.editor.Value())
			if value != "" {
				m.creation.agentModel = value
				m.beginCreationField(createAgentSystem, "System prompt (optional)", m.creation.agentSystem)
			}
			return m, nil
		}
		return m.updateCreationEditor(key)
	case createAgentSystem:
		if key.String() == "enter" {
			m.creation.agentSystem = strings.TrimSpace(m.editor.Value())
			m.creation.step = createAgentTools
			m.editor.Blur()
			return m, nil
		}
		return m.updateCreationEditor(key)
	case createAgentTools:
		switch key.String() {
		case "left", "right", "h", "l", "up", "down", "tab", "shift+tab":
			m.creation.agentTools = !m.creation.agentTools
		case "enter":
			m.creation.step = createAgentTopology
		}
	case createAgentTopology:
		switch key.String() {
		case "left", "right", "h", "l", "up", "down", "tab", "shift+tab":
			m.creation.agentTopology = 1 - m.creation.agentTopology
			if len(m.creation.agents) == 0 {
				m.creation.agentTopology = 0
			}
		case "enter":
			if m.creation.agentTopology == 1 && len(m.creation.agents) > 0 {
				m.creation.step = createAgentRoster
				m.creation.rosterCursor = clamp(m.creation.rosterCursor, 0, len(m.creation.agents)-1)
			} else {
				m.creation.step = createAgentMCP
				m.creation.mcpChoice = 0
			}
		}
	case createAgentRoster:
		switch key.String() {
		case "up", "k":
			m.creation.rosterCursor = wrap(m.creation.rosterCursor-1, len(m.creation.agents))
		case "down", "j":
			m.creation.rosterCursor = wrap(m.creation.rosterCursor+1, len(m.creation.agents))
		case "space":
			if len(m.creation.agents) > 0 {
				id := m.creation.agents[clamp(m.creation.rosterCursor, 0, len(m.creation.agents)-1)].ID
				m.creation.rosterSelected[id] = !m.creation.rosterSelected[id]
			}
		case "enter":
			if m.creationRosterCount() > 0 {
				m.creation.step = createAgentMCP
				m.creation.mcpChoice = 0
			}
		}
	case createAgentMCP:
		switch key.String() {
		case "left", "right", "h", "l", "tab", "shift+tab":
			m.creation.mcpChoice = 1 - m.creation.mcpChoice
		case "a":
			m.beginCreationField(createAgentMCPName, "MCP server name", "")
		case "backspace", "d":
			if len(m.creation.mcpServers) > 0 {
				m.creation.mcpServers = m.creation.mcpServers[:len(m.creation.mcpServers)-1]
			}
		case "enter":
			if m.creation.mcpChoice == 1 {
				m.beginCreationField(createAgentMCPName, "MCP server name", "")
				return m, nil
			}
			m.loading, m.creation.mutating, m.loadingLabel, m.err = true, true, "Creating Agent", nil
			return m, m.createAgent()
		}
	case createAgentMCPName:
		if key.String() == "enter" {
			value := strings.TrimSpace(m.editor.Value())
			if value != "" && !m.creationMCPNameExists(value) {
				m.creation.mcpName = value
				m.beginCreationField(createAgentMCPURL, "https://example.com/mcp", "")
			}
			return m, nil
		}
		return m.updateCreationEditor(key)
	case createAgentMCPURL:
		if key.String() == "enter" {
			value := strings.TrimSpace(m.editor.Value())
			parsed, err := url.ParseRequestURI(value)
			if err == nil && parsed.Scheme == "https" && parsed.Host != "" {
				m.creation.mcpURL = value
				m.creation.mcpServers = append(m.creation.mcpServers, mcpServerConfig{Name: m.creation.mcpName, URL: value})
				m.creation.mcpName, m.creation.mcpURL = "", ""
				m.creation.step, m.creation.mcpChoice = createAgentMCP, 0
				m.editor.Blur()
				m.err = nil
			} else {
				m.err = fmt.Errorf("MCP URL must be a public https URL")
			}
			return m, nil
		}
		return m.updateCreationEditor(key)
	case createEnvironmentName:
		if key.String() == "enter" {
			value := strings.TrimSpace(m.editor.Value())
			if value != "" {
				m.creation.environmentName = value
				m.loading, m.creation.mutating, m.loadingLabel, m.err = true, true, "Creating Environment", nil
				return m, m.createEnvironment()
			}
			return m, nil
		}
		return m.updateCreationEditor(key)
	case createSessionTitle:
		if key.String() == "enter" {
			m.creation.title = strings.TrimSpace(m.editor.Value())
			if m.creation.title == "" {
				m.creation.title = first(m.creation.agent.Name, "New Session") + " Session"
			}
			m.beginCreationField(createInitialPrompt, "First message (optional)", m.creation.prompt)
			return m, nil
		}
		return m.updateCreationEditor(key)
	case createInitialPrompt:
		if key.String() == "enter" {
			m.creation.prompt = strings.TrimSpace(m.editor.Value())
			m.creation.step = createConfirm
			m.creation.confirmChoice = 0
			m.editor.Blur()
			return m, nil
		}
		return m.updateCreationEditor(key)
	case createConfirm:
		switch key.String() {
		case "left", "right", "h", "l", "tab", "shift+tab":
			m.creation.confirmChoice = 1 - m.creation.confirmChoice
		case "y":
			m.creation.confirmChoice = 0
			m.loading, m.creation.mutating, m.loadingLabel, m.err = true, true, "Creating Session", nil
			return m, m.createSession()
		case "n":
			m.creation.confirmChoice = 1
		case "enter":
			if m.creation.confirmChoice == 1 {
				m.beginCreationField(createInitialPrompt, "First message (optional)", m.creation.prompt)
				return m, nil
			}
			m.loading, m.creation.mutating, m.loadingLabel, m.err = true, true, "Creating Session", nil
			return m, m.createSession()
		}
	}
	return m, nil
}

func (m Model) updateCreationEditor(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	var command tea.Cmd
	m.editor, command = m.editor.Update(key)
	m.resize()
	return m, command
}

func (m *Model) beginCreationField(step createStep, placeholder, value string) {
	m.creation.step = step
	m.err = nil
	m.editor.Reset()
	m.editor.Placeholder = placeholder
	m.editor.SetValue(value)
	m.editor.Focus()
	m.filter.Blur()
	m.resize()
}

func (m *Model) beginSessionTitle() {
	value := m.creation.title
	if value == "" {
		value = first(m.creation.agent.Name, "New") + " Session"
	}
	m.beginCreationField(createSessionTitle, "Session title", value)
}

func (m *Model) continueAfterEnvironment() {
	if m.creation.quick {
		m.creation.step = createConfirm
		m.creation.confirmChoice = 0
		m.editor.Blur()
		return
	}
	m.beginSessionTitle()
}

func (m *Model) backCreation() {
	switch m.creation.step {
	case createChooseAgent, createAgentName:
		if m.creation.step == createAgentName {
			m.creation.agentName = strings.TrimSpace(m.editor.Value())
			m.creation.step = createChooseAgent
			m.editor.Blur()
			m.beginFilter("Filter Agents", m.creation.agentQuery)
			return
		}
		m.cancelCreation()
	case createAgentModel:
		m.creation.agentModel = strings.TrimSpace(m.editor.Value())
		m.beginCreationField(createAgentName, "Agent name", m.creation.agentName)
	case createAgentSystem:
		m.creation.agentSystem = strings.TrimSpace(m.editor.Value())
		m.beginCreationField(createAgentModel, "Model ID", m.creation.agentModel)
	case createAgentTools:
		m.beginCreationField(createAgentSystem, "System prompt (optional)", m.creation.agentSystem)
	case createAgentTopology:
		m.creation.step = createAgentTools
		m.editor.Blur()
	case createAgentRoster:
		m.creation.step = createAgentTopology
		m.editor.Blur()
	case createAgentMCP:
		if m.creation.agentTopology == 1 {
			m.creation.step = createAgentRoster
		} else {
			m.creation.step = createAgentTopology
		}
		m.editor.Blur()
	case createAgentMCPName:
		m.creation.mcpName = strings.TrimSpace(m.editor.Value())
		m.creation.step = createAgentMCP
		m.editor.Blur()
	case createAgentMCPURL:
		m.creation.mcpURL = strings.TrimSpace(m.editor.Value())
		m.beginCreationField(createAgentMCPName, "MCP server name", m.creation.mcpName)
	case createChooseEnvironment:
		m.creation.step = createChooseAgent
		m.editor.Blur()
		m.beginFilter("Filter Agents", m.creation.agentQuery)
	case createEnvironmentName:
		m.creation.environmentName = strings.TrimSpace(m.editor.Value())
		m.creation.step = createChooseEnvironment
		m.editor.Blur()
		m.beginFilter("Filter Environments", m.creation.environmentQuery)
	case createSessionTitle:
		m.creation.title = strings.TrimSpace(m.editor.Value())
		m.creation.step = createChooseEnvironment
		m.editor.Blur()
		m.beginFilter("Filter Environments", m.creation.environmentQuery)
	case createInitialPrompt:
		m.creation.prompt = strings.TrimSpace(m.editor.Value())
		if m.creation.quick {
			m.creation.step = createChooseEnvironment
			m.editor.Blur()
			m.beginFilter("Filter Environments", m.creation.environmentQuery)
		} else {
			m.beginSessionTitle()
		}
	case createConfirm:
		m.beginCreationField(createInitialPrompt, "First message (optional)", m.creation.prompt)
	}
}

func (m *Model) cancelCreation() {
	draft, restoreDraft := m.creation.prompt, m.creation.quick && m.screen == screenInbox
	m.dialog = dialogNone
	m.loading = false
	m.loadingLabel = ""
	m.creation = createState{}
	m.err = nil
	m.editor.Reset()
	if restoreDraft {
		m.editor.SetValue(draft)
	}
	m.editor.Placeholder = landingPlaceholder(m.screen)
	m.editor.Focus()
}

func (m Model) defaultAgentModel() string {
	if m.creation.agentModel != "" {
		return m.creation.agentModel
	}
	for _, agent := range m.creation.agents {
		if agent.Model.ID != "" {
			return agent.Model.ID
		}
	}
	return "offline-fake"
}

func (m Model) loadCreationResources() tea.Cmd {
	return func() tea.Msg {
		agents, err := m.backend.ListAgents(m.ctx)
		if err != nil {
			return creationResourcesLoaded{err: fmt.Errorf("list Agents: %w", err)}
		}
		environments, err := m.backend.ListEnvironments(m.ctx)
		if err != nil {
			return creationResourcesLoaded{err: fmt.Errorf("list Environments: %w", err)}
		}
		return creationResourcesLoaded{agents: agents, environments: environments}
	}
}

func (m Model) createAgent() tea.Cmd {
	input := mango.CreateAgentInput{Name: m.creation.agentName, Model: m.creation.agentModel, System: m.creation.agentSystem}
	if m.creation.agentTools {
		input.Tools = append(input.Tools, map[string]any{"type": "agent_toolset_20260401"})
	}
	for _, server := range m.creation.mcpServers {
		input.Tools = append(input.Tools, map[string]any{"type": "mcp_toolset", "mcp_server_name": server.Name})
		input.MCPServers = append(input.MCPServers, map[string]any{"type": "url", "name": server.Name, "url": server.URL})
	}
	if m.creation.agentTopology == 1 {
		ids := make([]string, 0, len(m.creation.rosterSelected))
		for _, agent := range m.creation.agents {
			if m.creation.rosterSelected[agent.ID] {
				ids = append(ids, agent.ID)
			}
		}
		input.Multiagent = &mango.MultiagentInput{Type: "coordinator", Agents: ids}
	}
	return func() tea.Msg {
		agent, err := m.backend.CreateAgent(m.ctx, input)
		return creationAgentCreated{agent: agent, err: err}
	}
}

func (m Model) creationRosterCount() int {
	count := 0
	for _, selected := range m.creation.rosterSelected {
		if selected {
			count++
		}
	}
	return count
}

func (m Model) creationMCPNameExists(name string) bool {
	for _, server := range m.creation.mcpServers {
		if strings.EqualFold(server.Name, name) {
			return true
		}
	}
	return false
}

func (m Model) createEnvironment() tea.Cmd {
	input := mango.CreateEnvironmentInput{Name: m.creation.environmentName}
	return func() tea.Msg {
		environment, err := m.backend.CreateEnvironment(m.ctx, input)
		return creationEnvironmentCreated{environment: environment, err: err}
	}
}

func (m Model) createSession() tea.Cmd {
	input := mango.CreateSessionInput{
		AgentID: m.creation.agent.ID, EnvironmentID: m.creation.environment.ID,
		Title: m.creation.title, InitialPrompt: m.creation.prompt,
	}
	return func() tea.Msg {
		session, err := m.backend.CreateSession(m.ctx, input)
		return creationSessionCreated{session: session, err: err}
	}
}

func (m Model) renderCreationDialog(width int) (string, string) {
	title := "New Session"
	if m.loading {
		content := m.activity(first(m.loadingLabel, "Talking to Mango"))
		if m.creation.mutating {
			content += "\n\n" + m.theme.dim.Render("Finishing the server-side change — cancellation is temporarily disabled.")
		}
		return title, content + m.creationError(width)
	}

	var content string
	switch m.creation.step {
	case createChooseAgent:
		rows := m.agentRows()
		content = m.theme.dim.Render("1 Agent  ›  2 Environment  ›  3 Session") + "\n\n" +
			m.theme.title.Render("Choose an Agent") + "\n" + m.renderCreationSearch(m.creation.agentQuery, "Type to filter Agents") +
			"\n\n" + strings.Join(rows, "\n") + "\n\n" +
			m.theme.dim.Render(m.dialogHint(
				"type filter  ↑↓ choose  enter select  esc back", "↑↓ choose  enter select  esc"))
	case createAgentTools:
		description := "Sandbox tools let this Agent inspect and change its cloud workspace."
		content = m.theme.dim.Render("Create Agent  ›  Runtime  ›  Team  ›  MCP") + "\n\n" +
			m.theme.title.Render("Built-in sandbox tools") + "\n" + m.theme.dim.Render(description) + "\n\n" +
			choice(m.theme, "Enabled", m.creation.agentTools, false) + "  " +
			choice(m.theme, "Disabled", !m.creation.agentTools, false) + "\n\n" +
			m.theme.dim.Render("←→ choose  enter continue  esc back")
	case createAgentTopology:
		coordinatorHelp := "A coordinator may delegate to durable child Agent Threads."
		if len(m.creation.agents) == 0 {
			coordinatorHelp = "Create a worker Agent first; a coordinator needs at least one roster member."
		}
		content = m.theme.dim.Render("Create Agent  ›  Runtime  ›  Team  ›  MCP") + "\n\n" +
			m.theme.title.Render("How should this Agent work?") + "\n" + m.theme.dim.Render(coordinatorHelp) + "\n\n" +
			choice(m.theme, "Solo", m.creation.agentTopology == 0, false) + "  " +
			choice(m.theme, "Coordinator", m.creation.agentTopology == 1, false) + "\n\n" +
			m.theme.dim.Render("←→ choose  enter continue  esc back")
	case createAgentRoster:
		rows := make([]string, 0, len(m.creation.agents))
		start, end := visibleRange(len(m.creation.agents), m.creation.rosterCursor, m.creationListSize(8))
		for index := start; index < end; index++ {
			agent := m.creation.agents[index]
			marker, style := "  ", lipgloss.NewStyle().Foreground(m.theme.text)
			if index == m.creation.rosterCursor {
				marker, style = "› ", m.theme.active
			}
			checked := "[ ]"
			if m.creation.rosterSelected[agent.ID] {
				checked = m.theme.success.Render("[✓]")
			}
			rows = append(rows, marker+checked+" "+style.Render(truncate(first(agent.Name, agent.ID), dialogInnerWidth(width)-8))+
				"\n      "+m.theme.dim.Render(first(agent.Model.ID, "unknown model")))
		}
		content = m.theme.dim.Render("Create Agent  ›  Runtime  ›  Team  ›  MCP") + "\n\n" +
			m.theme.title.Render(fmt.Sprintf("Choose child Agents · %d selected", m.creationRosterCount())) + "\n\n" +
			strings.Join(rows, "\n") + "\n\n" + m.theme.dim.Render("↑↓ choose  space toggle  enter continue  esc back")
	case createAgentMCP:
		servers := []string{}
		for _, server := range m.creation.mcpServers {
			servers = append(servers, m.theme.success.Render("✓ ")+server.Name+m.theme.dim.Render("  "+truncate(server.URL, max(12, dialogInnerWidth(width)-len(server.Name)-5))))
		}
		serverSummary := m.theme.dim.Render("No MCP servers configured.")
		if len(servers) > 0 {
			serverSummary = strings.Join(servers, "\n")
		}
		content = m.theme.dim.Render("Create Agent  ›  Runtime  ›  Team  ›  MCP") + "\n\n" +
			m.theme.title.Render("MCP servers") + "\n" + serverSummary + "\n\n" +
			choice(m.theme, "Create Agent", m.creation.mcpChoice == 0, false) + "  " +
			choice(m.theme, "Add server", m.creation.mcpChoice == 1, false) + "\n\n" +
			m.theme.dim.Render("←→ choose  enter confirm  a add  d remove last  esc back")
	case createChooseEnvironment:
		rows := m.environmentRows()
		content = m.selectionSummary() + "\n\n" + m.theme.title.Render("Choose an Environment") + "\n\n" +
			m.renderCreationSearch(m.creation.environmentQuery, "Type to filter Environments") + "\n\n" +
			strings.Join(rows, "\n") + "\n\n" +
			m.theme.dim.Render(m.dialogHint(
				"type filter  ↑↓ choose  enter select  esc back", "↑↓ choose  enter select  esc"))
	case createConfirm:
		prompt := m.theme.dim.Render("No first message — open an idle Session")
		if m.creation.prompt != "" {
			prompt = trimOneLine(m.creation.prompt, width-12)
		}
		content = m.selectionSummary() + "\n\n" +
			m.theme.title.Render(first(m.creation.title, "New Session")) + "\n" +
			m.theme.dim.Render("First message: ") + prompt + "\n\n" +
			choice(m.theme, "Create", m.creation.confirmChoice == 0, false) + "  " +
			choice(m.theme, "Not yet", m.creation.confirmChoice == 1, false) + "\n\n" +
			m.theme.dim.Render(m.dialogHint(
				"←→ choose  enter confirm  y create  esc back", "←→ choose  enter confirm  y create"))
	default:
		label, help := m.creationFieldCopy()
		content = m.creationEditorSummary(width) + "\n\n" + m.theme.title.Render(label) + "\n\n" +
			lipgloss.NewStyle().Width(dialogInnerWidth(width)).Border(lipgloss.RoundedBorder()).BorderForeground(m.theme.accent).Padding(0, 1).Render(m.editor.View()) +
			"\n\n" + m.theme.dim.Render(help)
	}
	return title, content + m.creationError(width)
}

func (m Model) creationEditorSummary(width int) string {
	if m.height >= 24 {
		return m.selectionSummary()
	}
	parts := make([]string, 0, 2)
	if m.creation.agent.ID != "" {
		parts = append(parts, first(m.creation.agent.Name, m.creation.agent.ID))
	}
	if m.creation.environment.ID != "" {
		parts = append(parts, first(m.creation.environment.Name, m.creation.environment.ID))
	}
	if len(parts) == 0 {
		return m.theme.dim.Render("Configure the new Session")
	}
	return m.theme.dim.Render(trimOneLine(strings.Join(parts, " · "), dialogInnerWidth(width)))
}

func (m Model) agentRows() []string {
	agents := m.filteredCreationAgents()
	start, end := visibleRange(len(agents)+1, m.creation.agentCursor, m.creationListSize(8))
	rows := make([]string, 0, end-start)
	for index := start; index < end; index++ {
		marker, style := "  ", lipgloss.NewStyle().Foreground(m.theme.text)
		if index == m.creation.agentCursor {
			marker, style = "› ", m.theme.active
		}
		if index == 0 {
			rows = append(rows, marker+style.Render("Create a new Agent")+"\n    "+m.theme.dim.Render("Configure model, tools, team, and MCP"))
			continue
		}
		agent := agents[index-1]
		innerWidth := dialogInnerWidth(m.dialogWidth())
		model := trimOneLine(first(agent.Model.ID, "model not set")+fmt.Sprintf(" · v%d", agent.Version), innerWidth-4)
		name := truncate(first(agent.Name, agent.ID), innerWidth-2)
		rows = append(rows, marker+style.Render(name)+"\n    "+m.theme.dim.Render(model))
	}
	return rows
}

func (m Model) environmentRows() []string {
	environments := m.filteredCreationEnvironments()
	start, end := visibleRange(len(environments)+1, m.creation.environmentCursor, m.creationListSize(8))
	rows := make([]string, 0, end-start)
	for index := start; index < end; index++ {
		marker, style := "  ", lipgloss.NewStyle().Foreground(m.theme.text)
		if index == m.creation.environmentCursor {
			marker, style = "› ", m.theme.active
		}
		if index == 0 {
			rows = append(rows, marker+style.Render("Create a cloud Environment")+"\n    "+m.theme.dim.Render("Add a new managed execution target"))
			continue
		}
		environment := environments[index-1]
		innerWidth := dialogInnerWidth(m.dialogWidth())
		name := truncate(first(environment.Name, environment.ID), innerWidth-2)
		detail := trimOneLine(first(environment.Config.Type, "cloud")+" · "+shortID(environment.ID), innerWidth-4)
		rows = append(rows, marker+style.Render(name)+"\n    "+m.theme.dim.Render(detail))
	}
	return rows
}

func (m Model) selectionSummary() string {
	innerWidth := dialogInnerWidth(m.dialogWidth())
	parts := []string{m.theme.dim.Render("1 Agent  ›  2 Environment  ›  3 Session")}
	if m.creation.agent.ID != "" {
		line := trimOneLine(first(m.creation.agent.Name, m.creation.agent.ID)+" · "+first(m.creation.agent.Model.ID, "unknown model"), innerWidth-2)
		parts = append(parts, m.theme.success.Render("✓ ")+line)
	}
	if m.creation.environment.ID != "" {
		line := trimOneLine(first(m.creation.environment.Name, m.creation.environment.ID)+" · cloud", innerWidth-2)
		parts = append(parts, m.theme.success.Render("✓ ")+line)
	}
	return strings.Join(parts, "\n")
}

func (m Model) creationFieldCopy() (string, string) {
	switch m.creation.step {
	case createAgentName:
		return "Create Agent · Name", "enter continue  esc back"
	case createAgentModel:
		return "Create Agent · Model", "enter continue  esc back"
	case createAgentSystem:
		return "Create Agent · System prompt", "enter continue  shift+enter newline  esc back"
	case createAgentMCPName:
		return "MCP server · Name", "enter continue  esc back"
	case createAgentMCPURL:
		return "MCP server · Streamable HTTP URL", "enter add server  esc back"
	case createEnvironmentName:
		return "Create cloud Environment · Name", "enter create Environment  esc back"
	case createSessionTitle:
		return "Session title", "enter continue  esc back"
	case createInitialPrompt:
		return "First message", "enter review  shift+enter newline  esc back"
	default:
		return "Configure", "enter continue  esc back"
	}
}

func (m Model) renderCreationSearch(query, placeholder string) string {
	width := max(1, dialogInnerWidth(m.dialogWidth())-3)
	if m.dialogUsesFilter() {
		return lipgloss.NewStyle().Width(width).Render(m.filter.View())
	}
	if query == "" {
		return m.theme.active.Render("> ") + m.theme.dim.Render(truncate(placeholder, width))
	}
	return m.theme.active.Render("> ") + truncate(query, width)
}

func (m Model) filteredCreationAgents() []mango.Agent {
	query := strings.TrimSpace(m.creation.agentQuery)
	if query == "" {
		return m.creation.agents
	}
	filtered := make([]mango.Agent, 0, len(m.creation.agents))
	for _, agent := range m.creation.agents {
		if fuzzyContains(agent.Name+" "+agent.Model.ID+" "+agent.ID, query) {
			filtered = append(filtered, agent)
		}
	}
	return filtered
}

func (m Model) filteredCreationEnvironments() []mango.Environment {
	query := strings.TrimSpace(m.creation.environmentQuery)
	if query == "" {
		return m.creation.environments
	}
	filtered := make([]mango.Environment, 0, len(m.creation.environments))
	for _, environment := range m.creation.environments {
		if fuzzyContains(environment.Name+" "+environment.Config.Type+" "+environment.ID, query) {
			filtered = append(filtered, environment)
		}
	}
	return filtered
}

func fuzzyContains(value, query string) bool {
	valueRunes := []rune(strings.ToLower(value))
	queryRunes := []rune(strings.ToLower(query))
	matched := 0
	for _, candidate := range valueRunes {
		if matched < len(queryRunes) && candidate == queryRunes[matched] {
			matched++
		}
	}
	return matched == len(queryRunes)
}

func removeLastRune(value string) string {
	runes := []rune(value)
	if len(runes) == 0 {
		return ""
	}
	return string(runes[:len(runes)-1])
}

func firstResourceCursor(filtered int) int {
	if filtered > 0 {
		return 1
	}
	return 0
}

func titleFromPrompt(prompt string) string {
	title := trimOneLine(prompt, 52)
	if title == "" {
		return "New Session"
	}
	return title
}

func (m Model) creationError(width int) string {
	if m.err == nil {
		return ""
	}
	return "\n\n" + m.theme.danger.Render(trimOneLine(m.err.Error(), width-8))
}

func visibleRange(length, cursor, size int) (int, int) {
	if length <= size {
		return 0, length
	}
	start := clamp(cursor-size/2, 0, length-size)
	return start, start + size
}
