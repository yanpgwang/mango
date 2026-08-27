package ui

import (
	"fmt"
	"slices"
	"time"

	"github.com/yanpgwang/mango/examples/terminal-ui/internal/mango"
)

// fleetOrder reorders a Session slice so the ones that matter to the operator
// float to the top. This is a stable projection: same input always yields the
// same order, so the inbox cursor keeps pointing at the same row across
// re-renders.
func fleetOrder(sessions []mango.Session) []mango.Session {
	ordered := append([]mango.Session(nil), sessions...)
	slices.SortStableFunc(ordered, func(a, b mango.Session) int {
		if pa, pb := sessionStatePriority(a.Status), sessionStatePriority(b.Status); pa != pb {
			return pa - pb
		}
		return -recency(a).Compare(recency(b))
	})
	return ordered
}

// sessionStatePriority ranks Session states by "how likely does the operator
// need to look here." A lower number sorts higher.
func sessionStatePriority(status string) int {
	switch status {
	case "requires_action":
		return 0
	case "running":
		return 1
	case "rescheduling":
		return 2
	case "idle":
		return 3
	case "terminated", "failed", "error":
		return 5
	default:
		return 4
	}
}

func recency(s mango.Session) time.Time {
	if !s.UpdatedAt.IsZero() {
		return s.UpdatedAt
	}
	return s.CreatedAt
}

type fleetCounts struct {
	needsAction int
	running     int
	idle        int
	other       int
	inputTokens int64
}

func summarizeFleet(sessions []mango.Session) fleetCounts {
	var counts fleetCounts
	for _, session := range sessions {
		switch session.Status {
		case "requires_action":
			counts.needsAction++
		case "running", "rescheduling":
			counts.running++
		case "idle":
			counts.idle++
		default:
			counts.other++
		}
		counts.inputTokens += session.Usage.InputTokens + session.Usage.OutputTokens
	}
	return counts
}

// humanizeSince returns a short, monospace-friendly relative time. The inbox
// row only affords a handful of characters, so anything longer than "12d"
// truncates to a coarser bucket rather than adding words.
func humanizeSince(t time.Time, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	diff := now.Sub(t)
	if diff < 0 {
		diff = 0
	}
	switch {
	case diff < 45*time.Second:
		return "just now"
	case diff < time.Hour:
		return fmt.Sprintf("%dm ago", int(diff.Minutes()))
	case diff < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(diff.Hours()))
	case diff < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(diff.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
}

// subagentCount reports how many child agents this Session's coordinator can
// delegate to. Zero means a single-agent Session and is worth not printing.
func subagentCount(session mango.Session) int {
	if session.Agent.Multiagent == nil {
		return 0
	}
	seenIDs := map[string]struct{}{}
	seenNames := map[string]struct{}{}
	count := 0
	for _, reference := range session.Agent.Multiagent.Agents {
		if reference.ID != "" && reference.ID == session.Agent.ID {
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
		if reference.ID != "" {
			seenIDs[reference.ID] = struct{}{}
		}
		if reference.Name != "" {
			seenNames[reference.Name] = struct{}{}
		}
		count++
	}
	return count
}
