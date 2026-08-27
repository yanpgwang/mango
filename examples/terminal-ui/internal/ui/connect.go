package ui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/yanpgwang/mango/examples/terminal-ui/internal/endpoint"
)

type endpointAvailability int

const (
	endpointUnknown endpointAvailability = iota
	endpointChecking
	endpointReachable
	endpointUnreachable
)

type endpointChoice struct {
	url          string
	label        string
	source       string
	availability endpointAvailability
	skipProbe    bool
}

type connectState struct {
	cursor          int    // position in the grouped navigable order; len(navOrder) is the manual-entry row
	configured      string // url of the initially configured endpoint, marked in its group header
	editing         bool
	endpoints       []endpointChoice
	endpointInput   textinput.Model
	validationError string
}

type endpointProbeDone struct {
	url       string
	reachable bool
}

type endpointSaved struct{ err error }

func newConnectState(options Options, t theme) connectState {
	input := textinput.New()
	input.Prompt = "> "
	input.Placeholder = "https://mango.example.com"
	input.CharLimit = 2048
	input.SetVirtualCursor(false)
	styles := textinput.DefaultDarkStyles()
	styles.Focused.Prompt = t.active
	styles.Focused.Text = lipgloss.NewStyle().Foreground(t.text)
	styles.Focused.Placeholder = t.dim
	styles.Blurred = styles.Focused
	input.SetStyles(styles)
	input.Blur()

	state := connectState{endpointInput: input}
	for _, option := range options.Endpoints {
		state.addEndpoint(option)
	}
	sliceIndex := 0
	initial := strings.TrimSpace(options.Endpoint)
	if initial != "" {
		index := state.indexOf(initial)
		if index < 0 {
			state.endpoints = append([]endpointChoice{{url: initial, label: initial, source: "configured"}}, state.endpoints...)
			index = 0
		}
		sliceIndex = index
	}
	if len(state.endpoints) == 0 {
		state.endpoints = append(state.endpoints, endpointChoice{url: "Mango Cloud", label: "Mango Cloud", source: "configured", skipProbe: true})
	}
	sliceIndex = clamp(sliceIndex, 0, len(state.endpoints)-1)
	state.configured = state.endpoints[sliceIndex].url
	state.cursor = displayPos(state.navOrder(), sliceIndex)
	return state
}

func (c *connectState) addEndpoint(option EndpointOption) int {
	target := strings.TrimSpace(option.URL)
	if target == "" {
		target = strings.TrimSpace(option.Label)
	}
	if target == "" {
		return -1
	}
	if index := c.indexOf(target); index >= 0 {
		if c.endpoints[index].source == "" {
			c.endpoints[index].source = strings.TrimSpace(option.Source)
		}
		if option.Available {
			c.endpoints[index].availability = endpointReachable
		}
		return index
	}
	availability := endpointUnknown
	if option.Available {
		availability = endpointReachable
	} else if !option.SkipProbe {
		availability = endpointChecking
	}
	label := strings.TrimSpace(option.Label)
	if label == "" {
		label = target
	}
	c.endpoints = append(c.endpoints, endpointChoice{
		url: target, label: label, source: strings.TrimSpace(option.Source),
		availability: availability, skipProbe: option.SkipProbe,
	})
	return len(c.endpoints) - 1
}

func (c connectState) indexOf(target string) int {
	for index, option := range c.endpoints {
		if option.url == target {
			return index
		}
	}
	return -1
}

// Endpoints are shown grouped by origin. The buckets are fixed so the list
// order stays stable as endpoints are probed or added.
var endpointBuckets = []struct {
	key   string
	label string
}{
	{"configured", "Configured"},
	{"manual", "Manual"},
	{"demo", "Demo"},
}

func endpointBucket(option endpointChoice) string {
	switch {
	case strings.HasPrefix(option.url, "demo://"):
		return "demo"
	case option.source == "manual":
		return "manual"
	default:
		return "configured"
	}
}

// navOrder lists endpoint slice indices in the grouped display order. The
// cursor indexes into this order, so navigation and rendering agree on which
// row is highlighted.
func (c connectState) navOrder() []int {
	order := make([]int, 0, len(c.endpoints))
	for _, bucket := range endpointBuckets {
		for index, option := range c.endpoints {
			if endpointBucket(option) == bucket.key {
				order = append(order, index)
			}
		}
	}
	return order
}

func (c connectState) onManualRow() bool {
	return c.cursor >= len(c.navOrder())
}

func displayPos(order []int, sliceIndex int) int {
	for pos, index := range order {
		if index == sliceIndex {
			return pos
		}
	}
	return 0
}

func (c connectState) current() endpointChoice {
	order := c.navOrder()
	if len(order) == 0 {
		return endpointChoice{}
	}
	return c.endpoints[order[clamp(c.cursor, 0, len(order)-1)]]
}

func (m Model) endpointProbeCommands() []tea.Cmd {
	if m.options.ProbeEndpoint == nil {
		return nil
	}
	commands := make([]tea.Cmd, 0, len(m.connect.endpoints))
	for _, option := range m.connect.endpoints {
		if option.skipProbe || option.availability == endpointReachable || option.url == "" {
			continue
		}
		target := option.url
		probe := m.options.ProbeEndpoint
		ctx := m.ctx
		commands = append(commands, func() tea.Msg {
			return endpointProbeDone{url: target, reachable: probe(ctx, target)}
		})
	}
	return commands
}

func (m Model) probeEndpoint(target string) tea.Cmd {
	if m.options.ProbeEndpoint == nil || target == "" {
		return nil
	}
	probe := m.options.ProbeEndpoint
	ctx := m.ctx
	return func() tea.Msg {
		return endpointProbeDone{url: target, reachable: probe(ctx, target)}
	}
}

func (m Model) applyEndpointProbe(message endpointProbeDone) Model {
	for index := range m.connect.endpoints {
		if m.connect.endpoints[index].url != message.url {
			continue
		}
		if message.reachable {
			m.connect.endpoints[index].availability = endpointReachable
		} else {
			m.connect.endpoints[index].availability = endpointUnreachable
		}
	}
	return m
}

func (m Model) updateConnectKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.loading {
		return m, nil
	}
	if m.connect.editing {
		switch key.String() {
		case "esc":
			m.connect.editing = false
			m.connect.validationError = ""
			m.connect.endpointInput.Blur()
			return m, nil
		case "enter":
			target, err := normalizeEndpoint(m.connect.endpointInput.Value())
			if err != nil {
				m.connect.validationError = err.Error()
				return m, nil
			}
			index := m.connect.indexOf(target)
			if index < 0 {
				index = m.connect.addEndpoint(EndpointOption{URL: target, Source: "manual"})
			}
			m.connect.cursor = displayPos(m.connect.navOrder(), index)
			m.connect.editing = false
			m.connect.validationError = ""
			m.connect.endpointInput.Blur()
			return m, m.probeEndpoint(target)
		}
		var command tea.Cmd
		m.connect.endpointInput, command = m.connect.endpointInput.Update(key)
		m.connect.validationError = ""
		return m, command
	}

	count := len(m.connect.navOrder()) + 1 // final row is manual entry
	switch key.String() {
	case "up", "k":
		m.connect.cursor = wrap(m.connect.cursor-1, count)
		return m, nil
	case "down", "j":
		m.connect.cursor = wrap(m.connect.cursor+1, count)
		return m, nil
	case "e":
		return m.beginEndpointEdit()
	case "enter":
		if m.connect.onManualRow() {
			return m.beginEndpointEdit()
		}
		return m.connectSelectedEndpoint()
	case "ctrl+p":
		m.openCommands()
		return m, nil
	case "?":
		m.dialog = dialogHelp
		return m, nil
	}
	return m, nil
}

func (m Model) updateConnectPaste(message tea.PasteMsg) (tea.Model, tea.Cmd) {
	if !m.connect.editing || m.loading {
		return m, nil
	}
	var command tea.Cmd
	m.connect.endpointInput, command = m.connect.endpointInput.Update(message)
	m.connect.validationError = ""
	return m, command
}

func (m Model) beginEndpointEdit() (tea.Model, tea.Cmd) {
	m.connect.editing = true
	m.connect.validationError = ""
	current := m.connect.current().url
	if !m.connect.onManualRow() && (strings.HasPrefix(current, "http://") || strings.HasPrefix(current, "https://")) {
		m.connect.endpointInput.SetValue(current)
	} else {
		m.connect.endpointInput.Reset()
	}
	m.connect.endpointInput.CursorEnd()
	return m, m.connect.endpointInput.Focus()
}

func (m Model) connectSelectedEndpoint() (tea.Model, tea.Cmd) {
	selected := m.connect.current()
	if m.options.BackendForEndpoint != nil {
		backend, err := m.options.BackendForEndpoint(selected.url)
		if err != nil {
			m.err = err
			m.status = "invalid endpoint"
			return m, nil
		}
		m.backend = backend
	}
	m.options.Endpoint = selected.url
	m.loading, m.loadingLabel, m.err = true, "Connecting to "+first(selected.label, "Mango Cloud"), nil
	return m, m.loadInbox()
}

func (m Model) saveSelectedEndpoint() tea.Cmd {
	if m.options.SaveEndpoint == nil {
		return nil
	}
	target := m.connect.current().url
	if target == "" || strings.HasPrefix(target, "demo://") {
		return nil
	}
	save := m.options.SaveEndpoint
	return func() tea.Msg { return endpointSaved{err: save(target)} }
}

func normalizeEndpoint(value string) (string, error) {
	return endpoint.Normalize(value)
}
