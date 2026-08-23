package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

func (m Model) brandWord(value string) string {
	return lipgloss.NewStyle().Foreground(m.theme.accent).Bold(true).Render(value)
}

// mangoWordmark is the quiet welcome scene used by the centered connect
// screen. The product name stays typographic; the illustration belongs to a
// small family of clouds that drift without changing the layout.
func (m Model) mangoWordmark() string {
	return m.welcomeBlock(58, false)
}

func (m Model) welcomeBlock(width int, compact bool) string {
	title := m.theme.title.Render("Welcome to ") + m.theme.active.Render("Mango")
	header := lipgloss.PlaceHorizontal(width, lipgloss.Center, title)
	subtitle := lipgloss.PlaceHorizontal(width, lipgloss.Center, m.theme.dim.Render("managed agents, one window"))
	spacer := ""
	if compact {
		spacer = m.theme.dim.Render(strings.Repeat("· ", max(0, width/2)))
	}
	return strings.Join([]string{header, subtitle, spacer, m.cloudScene(width, compact)}, "\n")
}

// cloudScene draws Mango's mascot: a chunky pixel cloud with a small face. It
// drifts sideways and blinks on a slow cycle, but always occupies the same
// width×height box so the surrounding layout never reflows. Motion is derived
// from the shared tick counter and can be disabled.
func (m Model) cloudScene(width int, compact bool) string {
	blink := !m.options.ReducedMotion && (m.motion/2)%20 == 19
	eye := "●"
	if blink {
		eye = "▬"
	}
	body := lipgloss.NewStyle().Foreground(lipgloss.Color("#C4CCD6"))
	face := lipgloss.NewStyle().Foreground(m.theme.accent)

	if compact {
		nub := body.Render("▟█ ") + face.Render(eye) + body.Render(" ") +
			face.Render("ᴗ") + body.Render(" ") + face.Render(eye) + body.Render(" █▙")
		return lipgloss.PlaceHorizontal(width, lipgloss.Center, nub)
	}

	const cloudWidth, height = 19, 4
	rows := [][]rune{
		centerRunes("▄▄▄▄▄▄▄", cloudWidth),
		centerRunes(strings.Repeat("█", 15), cloudWidth),
		centerRunes(strings.Repeat("█", 17), cloudWidth),
		centerRunes("▀"+strings.Repeat("█", 13)+"▀", cloudWidth),
	}
	// The face sits in the solid middle row so the eyes and mouth read as part
	// of the cloud rather than floating above it.
	rows[2][5], rows[2][9], rows[2][13] = []rune(eye)[0], []rune("ᴗ")[0], []rune(eye)[0]

	center := max(0, (width-cloudWidth)/2)
	if !m.options.ReducedMotion {
		center = max(0, center+cloudDrift(m.motion/3, 8, 2))
	}
	ramp := lipgloss.Blend1D(height, lipgloss.Color("#E4E8EE"), lipgloss.Color("#A9B2BE"))

	lines := make([]string, height)
	for row := 0; row < height; row++ {
		var builder strings.Builder
		rowStyle := lipgloss.NewStyle().Foreground(ramp[row])
		for column := 0; column < width; column++ {
			index := column - center
			if index < 0 || index >= len(rows[row]) {
				builder.WriteByte(' ')
				continue
			}
			character := rows[row][index]
			switch character {
			case '●', '▬', 'ᴗ':
				builder.WriteString(face.Render(string(character)))
			case ' ':
				builder.WriteByte(' ')
			default:
				builder.WriteString(rowStyle.Render(string(character)))
			}
		}
		lines[row] = builder.String()
	}
	return strings.Join(lines, "\n")
}

// centerRunes pads a rune-string with spaces on both sides to reach an exact
// rune width, keeping the pixel cloud's rows the same length regardless of the
// glyphs they contain.
func centerRunes(value string, width int) []rune {
	runes := []rune(value)
	if len(runes) >= width {
		return runes[:width]
	}
	left := (width - len(runes)) / 2
	out := make([]rune, width)
	for i := range out {
		out[i] = ' '
	}
	copy(out[left:], runes)
	return out
}

func cloudDrift(frame, period, span int) int {
	if period <= 0 || span <= 0 {
		return 0
	}
	phase := frame % period
	half := max(1, period/2)
	if phase > half {
		phase = period - phase
	}
	return phase*span/half - span/2
}

// brandLogo uses a compact text treatment in supporting views and a larger
// block wordmark on the central connection screen.
func (m Model) brandLogo(compact bool) string {
	if compact {
		return m.brandWord("MANGO")
	}
	return m.mangoWordmark()
}
