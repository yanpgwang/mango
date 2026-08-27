package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

type connectLayout struct {
	view           string
	inputX, inputY int
}

// selectedRowStyle paints the highlighted endpoint as a full-width accent bar
// with dark text, matching the Crush-style picker the connect screen mirrors.
func (m Model) selectedRowStyle(width int) lipgloss.Style {
	return lipgloss.NewStyle().
		Width(width).
		Foreground(lipgloss.Color("#11150F")).
		Background(m.theme.accent)
}

// buildConnectLayout centers a borderless welcome + endpoint list in the whole
// terminal. It also reports where the manual-entry editor's caret lands so the
// real terminal cursor (and any IME window) can follow it.
func (m Model) buildConnectLayout() connectLayout {
	width, height := max(1, m.width), max(1, m.height)
	_, _, contentWidth, compact := connectDimensions(width, height)

	welcome := m.welcomeBlock(contentWidth, compact)
	body, inputX, inputY := m.renderConnectBody(contentWidth, compact)
	footer := m.theme.dim.Render(truncate(m.connectFooter(), contentWidth))

	parts := []string{welcome, "", body, "", footer}
	spacer := 1
	if lipgloss.Height(strings.Join(parts, "\n")) > height {
		welcome = m.welcomeBlock(contentWidth, true)
		parts = []string{welcome, body, footer}
		spacer = 0
	}
	inner := strings.Join(parts, "\n")
	innerWidth, innerHeight := lipgloss.Width(inner), lipgloss.Height(inner)
	innerLeft := max(0, (width-innerWidth)/2)
	innerTop := max(0, (height-innerHeight)/2)
	view := lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, inner)

	bodyTop := lipgloss.Height(welcome) + spacer
	bodyLeft := max(0, (innerWidth-lipgloss.Width(body))/2)
	return connectLayout{
		view:   view,
		inputX: innerLeft + bodyLeft + inputX,
		inputY: innerTop + bodyTop + inputY,
	}
}

// renderConnectBody returns the endpoint list (or the manual-entry editor) as a
// block exactly contentWidth wide, plus the editor caret offset inside it.
func (m Model) renderConnectBody(width int, compact bool) (string, int, int) {
	lines := []string{}

	inputX, inputY := 0, 0
	if m.connect.editing {
		lines = append(lines, m.theme.dim.Render("Endpoint · enter an address"))
		hpad := 2
		if compact {
			hpad = 1
		}
		cardInner := max(20, width-2-2*hpad)
		input := m.connect.endpointInput
		if input.Width() != cardInner {
			input.SetWidth(cardInner)
		}
		inputX, inputY = hpad, len(lines)
		lines = append(lines, strings.Repeat(" ", hpad)+input.View())
		if m.connect.validationError != "" {
			lines = append(lines, m.theme.danger.Render(trimOneLine(m.connect.validationError, width)))
		} else {
			lines = append(lines, m.theme.dim.Render("http:// and https:// are supported"))
		}
	} else {
		lines = append(lines, m.renderEndpointList(width)...)
	}

	if m.err != nil {
		lines = append(lines, m.theme.danger.Render(trimOneLine(m.err.Error(), width)))
	}
	if m.loading {
		lines = append(lines, lipgloss.PlaceHorizontal(width, lipgloss.Center,
			m.activity(first(m.loadingLabel, "Connecting to Mango Cloud"))))
	}
	return strings.Join(lines, "\n"), inputX, inputY
}

func (m Model) renderEndpointList(width int) []string {
	order := m.connect.navOrder()
	configuredBucket := ""
	if index := m.connect.indexOf(m.connect.configured); index >= 0 {
		configuredBucket = endpointBucket(m.connect.endpoints[index])
	}
	lines := make([]string, 0, len(order)+4)
	previous := ""
	for pos, index := range order {
		option := m.connect.endpoints[index]
		bucket := endpointBucket(option)
		if bucket != previous {
			if previous != "" {
				lines = append(lines, "")
			}
			lines = append(lines, m.endpointGroupHeader(bucket, bucket == configuredBucket, width))
			previous = bucket
		}
		lines = append(lines, m.endpointRow(option, pos == m.connect.cursor, width))
	}
	lines = append(lines, "")
	lines = append(lines, m.endpointManualRow(m.connect.onManualRow(), width))
	return lines
}

func (m Model) endpointGroupHeader(bucket string, marked bool, width int) string {
	left := m.theme.dim.Render(endpointBucketLabel(bucket)) + " "
	mark := ""
	if marked {
		mark = " " + m.theme.success.Render("✓")
	}
	fill := max(0, width-lipgloss.Width(left)-lipgloss.Width(mark))
	return left + m.theme.dim.Render(strings.Repeat("─", fill)) + mark
}

func (m Model) endpointRow(option endpointChoice, selected bool, width int) string {
	label := first(option.label, option.url)
	if selected {
		status := endpointStatusPlain(option)
		labelWidth := max(4, width-4-lipgloss.Width(status))
		left := "▌ " + truncate(label, labelWidth)
		gap := max(1, width-lipgloss.Width(left)-lipgloss.Width(status)-1)
		content := left + strings.Repeat(" ", gap) + status + " "
		return m.selectedRowStyle(width).Render(content)
	}
	status := m.endpointStatus(option)
	labelWidth := max(4, width-4-lipgloss.Width(status))
	left := "  " + lipgloss.NewStyle().Foreground(m.theme.text).Render(truncate(label, labelWidth))
	gap := max(1, width-lipgloss.Width(left)-lipgloss.Width(status)-1)
	return left + strings.Repeat(" ", gap) + status + " "
}

func (m Model) endpointManualRow(selected bool, width int) string {
	label := "Enter another endpoint…"
	if selected {
		return m.selectedRowStyle(width).Render("▌ " + label)
	}
	return "  " + m.theme.dim.Render(label)
}

func endpointBucketLabel(key string) string {
	for _, bucket := range endpointBuckets {
		if bucket.key == key {
			return bucket.label
		}
	}
	return key
}

// endpointStatusPlain is the uncolored status used inside the highlighted bar,
// where the accent background already carries the row's emphasis.
func endpointStatusPlain(option endpointChoice) string {
	switch option.availability {
	case endpointReachable:
		if option.skipProbe {
			return "● ready"
		}
		return "● reachable"
	case endpointChecking:
		return "◌ checking"
	case endpointUnreachable:
		return "○ not reachable"
	default:
		return first(option.source, "configured")
	}
}

func (m Model) endpointStatus(option endpointChoice) string {
	switch option.availability {
	case endpointReachable:
		if option.skipProbe {
			return m.theme.success.Render("● ready")
		}
		return m.theme.success.Render("● reachable")
	case endpointChecking:
		return m.theme.dim.Render("◌ checking")
	case endpointUnreachable:
		return m.theme.dim.Render("○ not reachable")
	default:
		return m.theme.dim.Render(first(option.source, "configured"))
	}
}

func connectDimensions(width, height int) (frameWidth, frameHeight, contentWidth int, compact bool) {
	frameWidth = min(112, max(50, width-8))
	frameHeight = min(38, max(16, height-4))
	compact = frameWidth < 76 || frameHeight < 27
	contentWidth = min(70, max(46, frameWidth-8))
	return
}

func connectCardMetrics(width int, compact bool) (innerWidth, horizontalPadding, verticalPadding int) {
	horizontalPadding, verticalPadding = 2, 1
	if compact {
		horizontalPadding, verticalPadding = 1, 0
	}
	innerWidth = max(20, width-2-2*horizontalPadding)
	return
}

func (m Model) connectFooter() string {
	if m.connect.editing {
		return "type endpoint · enter add · esc cancel"
	}
	return "↑/↓ choose · enter connect · e edit · ctrl+c quit"
}
