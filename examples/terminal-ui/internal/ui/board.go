package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/yanpgwang/mango/examples/terminal-ui/internal/feed"
	"github.com/yanpgwang/mango/examples/terminal-ui/internal/mango"
)

// renderAgentWorkspace is the durable Session's second surface: the main
// conversation remains visible on the left while every child Thread can be
// scanned, previewed, and opened here. Unlike the old passive sidebar, this
// rail owns focus and exposes pending actions in context.
func (m Model) renderAgentWorkspace(width, height int) string {
	inner := max(12, width-4)
	specialists := m.specialistThreads()
	ghosts := m.unspawnedRoster(specialists)
	roster := 0
	if m.session != nil {
		roster = subagentCount(*m.session)
	}

	count := fmt.Sprintf("%d live", len(specialists))
	if roster > 0 {
		count = fmt.Sprintf("%d live / %d roster", len(specialists), roster)
	}
	head := joinSides(m.theme.title.Render("Subagent workspace"), m.theme.dim.Render(count), inner)
	header := head + "\n" + m.theme.dim.Render(strings.Repeat("─", inner))
	agents := make([]string, 0, len(m.threads)+len(ghosts))

	for index, thread := range m.threads {
		agents = append(agents, m.renderAgentRailThread(thread, index, inner))
	}
	for _, ghost := range ghosts {
		agents = append(agents, m.renderGhostAgentRail(ghost, inner))
	}
	actions := ""
	if len(m.pending) > 0 {
		actions = m.renderRailActions(inner)
	}

	headerHeight := lipgloss.Height(header)
	actionHeight := lipgloss.Height(actions)
	agentHeight := max(1, height-headerHeight-actionHeight)
	body := header + "\n" + m.renderVisibleAgentBlocks(agents, agentHeight)
	if actions != "" {
		body += "\n" + actions
	}
	border := m.theme.border
	if m.focus == focusAgents {
		border = m.theme.accent
	}
	return lipgloss.NewStyle().Width(width).Height(height).Padding(0, 1).
		BorderLeft(true).BorderStyle(lipgloss.Border{Left: "│"}).BorderForeground(border).Render(body)
}

func (m Model) renderVisibleAgentBlocks(blocks []string, height int) string {
	if len(blocks) == 0 || height <= 0 {
		return ""
	}
	all := strings.Join(blocks, "\n\n")
	if lipgloss.Height(all) <= height {
		return all
	}
	target := 0
	if m.railCursor >= 0 && m.railCursor < len(m.threads) {
		target = m.railCursor
	}
	target = clamp(target, 0, len(blocks)-1)
	start, end := target, target+1
	window := func(first, last int) string {
		parts := make([]string, 0, last-first+2)
		if first > 0 {
			parts = append(parts, m.theme.dim.Render(fmt.Sprintf("… %d earlier", first)))
		}
		parts = append(parts, blocks[first:last]...)
		if last < len(blocks) {
			parts = append(parts, m.theme.dim.Render(fmt.Sprintf("… %d more", len(blocks)-last)))
		}
		return strings.Join(parts, "\n\n")
	}
	for {
		expanded := false
		if start > 0 && lipgloss.Height(window(start-1, end)) <= height {
			start--
			expanded = true
		}
		if end < len(blocks) && lipgloss.Height(window(start, end+1)) <= height {
			end++
			expanded = true
		}
		if !expanded {
			break
		}
	}
	return clipToHeight(window(start, end), height, m.theme)
}

func (m Model) renderAgentRailThread(thread mango.Thread, index, width int) string {
	selected := m.focus == focusAgents && m.railCursor == index
	viewing := m.currentThreadID() == thread.ID
	pip := sessionStatePip(m.theme, thread.Status)
	name := truncate(first(thread.Agent.Name, "Agent"), max(5, width-18))
	right := ""
	if thread.Primary() {
		right = m.theme.dim.Render("coordinator")
	} else if count := m.unread[thread.ID]; count > 0 {
		right = m.theme.warning.Render(fmt.Sprintf("+%d unread", count))
	}
	if viewing {
		right = m.theme.active.Render("‹ viewing")
	}
	head := pip + "  " + m.theme.title.Render(name)
	if right != "" {
		head = joinSides(head, right, width-2)
	}

	lines := []string{head}
	if thread.Primary() {
		lines = append(lines, m.theme.dim.Render("main conversation · user messages land here"))
	} else {
		task := first(m.delegationPreview(thread.ID), "waiting for a delegated task")
		lines = append(lines, m.theme.dim.Render("task  ")+trimOneLine(task, max(4, width-8)))
	}
	if activity, style := m.agentActivity(thread); activity != "" {
		lines = append(lines, style.Render(trimOneLine(activity, max(4, width-2))))
	}

	meta := []string{}
	if thread.Stats.ActiveSeconds > 0 {
		meta = append(meta, fmt.Sprintf("%.1fs", thread.Stats.ActiveSeconds))
	}
	tokens := thread.Usage.InputTokens + thread.Usage.OutputTokens
	if tokens > 0 {
		meta = append(meta, compactTokens(tokens)+" tok")
	}
	if len(meta) > 0 && !thread.Primary() {
		lines = append(lines, m.theme.dim.Render(strings.Join(meta, " · ")))
	}

	content := strings.Join(lines, "\n")
	style := lipgloss.NewStyle().Width(width).PaddingLeft(2)
	if selected {
		style = lipgloss.NewStyle().Width(width).PaddingLeft(1).Background(m.theme.soft).
			BorderLeft(true).BorderStyle(lipgloss.Border{Left: "▌"}).BorderForeground(m.theme.accent)
	}
	return style.Render(content)
}

func (m Model) agentActivity(thread mango.Thread) (string, lipgloss.Style) {
	for _, action := range m.pending {
		if action.ThreadID == thread.ID || (action.ThreadID == "" && thread.Primary()) {
			return "! " + action.Name + " needs your input", m.theme.warning
		}
	}
	if current := m.previews[thread.ID]; current != nil {
		switch {
		case strings.TrimSpace(current.content) != "":
			return "▐ streaming a reply…", m.theme.active
		case current.messageID != "":
			return "▐ preparing a reply…", m.theme.active
		case current.waiting || current.thinkingID != "":
			return "◐ thinking…", m.theme.active
		}
	}
	events := m.events[thread.ID]
	if thread.Primary() {
		filtered := make([]mango.Event, 0, len(events))
		for _, event := range events {
			target := stringAny(event["session_thread_id"])
			if target != "" && target != thread.ID {
				continue // child action cross-posted onto the primary ledger
			}
			filtered = append(filtered, event)
		}
		events = filtered
	}
	items := feed.Project(thread, events)
	if thread.Status == "running" || thread.Status == "rescheduling" {
		for index := len(items) - 1; index >= 0; index-- {
			item := items[index]
			switch item.Kind {
			case feed.Tool:
				if item.Status == "running" || item.Status == "waiting" || item.Status == "error" {
					style := m.theme.dim
					if item.Status == "waiting" {
						style = m.theme.warning
					} else if item.Status == "error" {
						style = m.theme.danger
					}
					return "✱ " + first(item.Title, "tool") + " · " + first(item.Status, "running"), style
				}
				return ansi.Strip(stateText(m.theme, thread.Status)), m.theme.active
			case feed.Failure:
				return first(item.Title, "failed"), m.theme.danger
			case feed.Report, feed.Agent:
				// A durable report can belong to the previous turn. The Thread's
				// live state is more useful until a new preview or tool arrives.
				return ansi.Strip(stateText(m.theme, thread.Status)), m.theme.active
			}
		}
		return ansi.Strip(stateText(m.theme, thread.Status)), m.theme.active
	}
	for index := len(items) - 1; index >= 0; index-- {
		item := items[index]
		switch item.Kind {
		case feed.Tool:
			style := m.theme.dim
			if item.Status == "waiting" {
				style = m.theme.warning
			} else if item.Status == "error" {
				style = m.theme.danger
			}
			return "✱ " + first(item.Title, "tool") + " · " + first(item.Status, "running"), style
		case feed.Report:
			return "↙ reported  " + first(firstLineOf(item.Body), item.Title), m.theme.success
		case feed.Agent:
			return "latest  " + first(firstLineOf(item.Body), "reply complete"), m.theme.dim
		case feed.Failure:
			return first(item.Title, "failed"), m.theme.danger
		}
	}
	return ansi.Strip(stateText(m.theme, thread.Status)), m.theme.dim
}

func (m Model) renderGhostAgentRail(reference mango.AgentReference, width int) string {
	name := first(reference.Name, shortID(reference.ID), "specialist")
	head := m.theme.dim.Render("◦  " + truncate(name, max(4, width-6)))
	body := m.theme.dim.Render("   available · not delegated yet")
	return head + "\n" + body
}

func (m Model) renderRailActions(width int) string {
	index := clamp(m.actionCursor, 0, len(m.pending)-1)
	if m.railCursor >= len(m.threads) {
		index = clamp(m.railCursor-len(m.threads), 0, len(m.pending)-1)
	}
	action := m.pending[index]
	selected := m.focus == focusAgents && m.railCursor == len(m.threads)+index
	marker := "  "
	if selected {
		marker = m.theme.warning.Render("▌ ")
	}
	title := "! " + action.Name + "  " + m.theme.dim.Render("on "+m.threadName(action.ThreadID))
	input := m.theme.dim.Render("  " + trimOneLine(action.Input, max(4, width-4)))
	sections := []string{
		m.theme.warning.Render(fmt.Sprintf("Needs your input · %d", len(m.pending))),
		m.theme.dim.Render(strings.Repeat("─", width)),
		marker + m.theme.warning.Render(title),
		marker + input,
	}
	return strings.Join(sections, "\n")
}

func (m Model) renderCompactAgentStrip(width int) string {
	parts := []string{m.theme.dim.Render("Agents")}
	for _, thread := range m.threads {
		name := first(thread.Agent.Name, "Agent")
		if thread.Primary() {
			name = "⌂ " + name
		} else {
			name = ansi.Strip(sessionStatePip(m.theme, thread.Status)) + " " + name
		}
		if count := m.unread[thread.ID]; count > 0 {
			name += fmt.Sprintf(" +%d", count)
		}
		parts = append(parts, name)
	}
	for _, ghost := range m.unspawnedRoster(m.specialistThreads()) {
		parts = append(parts, "◦ "+first(ghost.Name, shortID(ghost.ID), "available"))
	}
	if len(m.pending) > 0 {
		parts = append(parts, m.theme.warning.Render(fmt.Sprintf("! %d needs input", len(m.pending))))
	}
	line := strings.Join(parts, m.theme.dim.Render("  ·  "))
	return lipgloss.NewStyle().Width(width).Padding(0, 2).Render(truncate(line, max(1, width-4)))
}

// unspawnedRoster returns coordinator-configured specialists that do not yet
// own a Thread. They remain visible but deliberately non-interactive.
func (m Model) unspawnedRoster(live []mango.Thread) []mango.AgentReference {
	if m.session == nil || m.session.Agent.Multiagent == nil {
		return nil
	}
	liveIDs := map[string]struct{}{}
	liveNames := map[string]struct{}{}
	for _, thread := range live {
		if thread.Agent.ID != "" {
			liveIDs[thread.Agent.ID] = struct{}{}
		}
		if thread.Agent.Name != "" {
			liveNames[thread.Agent.Name] = struct{}{}
		}
	}
	out := make([]mango.AgentReference, 0, len(m.session.Agent.Multiagent.Agents))
	seenIDs := map[string]struct{}{}
	seenNames := map[string]struct{}{}
	for _, reference := range m.session.Agent.Multiagent.Agents {
		if reference.ID == m.session.Agent.ID {
			continue
		}
		if reference.ID != "" {
			if _, duplicate := seenIDs[reference.ID]; duplicate {
				continue
			}
		}
		if reference.Name != "" {
			if _, duplicate := seenNames[reference.Name]; duplicate {
				continue
			}
		}
		if _, ok := liveIDs[reference.ID]; ok {
			continue
		}
		if reference.Name != "" {
			if _, ok := liveNames[reference.Name]; ok {
				continue
			}
		}
		if reference.ID != "" {
			seenIDs[reference.ID] = struct{}{}
		}
		if reference.Name != "" {
			seenNames[reference.Name] = struct{}{}
		}
		out = append(out, reference)
	}
	return out
}

func (m Model) specialistThreads() []mango.Thread {
	out := make([]mango.Thread, 0, len(m.threads))
	for _, thread := range m.threads {
		if !thread.Primary() {
			out = append(out, thread)
		}
	}
	return out
}

func clipToHeight(content string, height int, t theme) string {
	if height <= 0 {
		return ""
	}
	lines := strings.Split(content, "\n")
	if len(lines) <= height {
		return content
	}
	kept := append([]string(nil), lines[:height-1]...)
	kept = append(kept, t.dim.Render("… more agents in ctrl+g"))
	return strings.Join(kept, "\n")
}

func firstLineOf(text string) string {
	text = strings.TrimSpace(text)
	if newline := strings.IndexByte(text, '\n'); newline >= 0 {
		return text[:newline]
	}
	return text
}
