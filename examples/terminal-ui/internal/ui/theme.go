package ui

import (
	"image/color"

	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"
)

type theme struct {
	accent  color.Color
	green   color.Color
	blue    color.Color
	yellow  color.Color
	red     color.Color
	text    color.Color
	muted   color.Color
	border  color.Color
	panel   color.Color
	soft    color.Color
	title   lipgloss.Style
	dim     lipgloss.Style
	active  lipgloss.Style
	success lipgloss.Style
	warning lipgloss.Style
	danger  lipgloss.Style
}

func defaultTheme() theme {
	t := theme{
		accent: lipgloss.Color("#FF8A3D"), green: lipgloss.Color("#55D39A"), blue: lipgloss.Color("#6F8CFF"), yellow: lipgloss.Color("#E5C07B"), red: lipgloss.Color("#FF6B6B"),
		text: lipgloss.Color("#E7E9EE"), muted: lipgloss.Color("#7C8495"), border: lipgloss.Color("#333947"), panel: lipgloss.Color("#151820"), soft: lipgloss.Color("#242936"),
	}
	t.title = lipgloss.NewStyle().Foreground(t.text).Bold(true)
	t.dim = lipgloss.NewStyle().Foreground(t.muted)
	t.active = lipgloss.NewStyle().Foreground(t.accent).Bold(true)
	t.success = lipgloss.NewStyle().Foreground(t.green)
	t.warning = lipgloss.NewStyle().Foreground(t.yellow)
	t.danger = lipgloss.NewStyle().Foreground(t.red)
	return t
}

func (t theme) textareaStyles() textarea.Styles {
	styles := textarea.DefaultDarkStyles()
	styles.Focused.Base = lipgloss.NewStyle()
	styles.Focused.Text = lipgloss.NewStyle().Foreground(t.text)
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(t.accent)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(t.muted)
	styles.Blurred = styles.Focused
	return styles
}
