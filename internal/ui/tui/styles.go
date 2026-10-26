package tui

import (
	"charm.land/bubbles/v2/help"
	"charm.land/lipgloss/v2"

	"github.com/hay-kot/weft/internal/events"
)

// Styles holds every style the TUI draws with. Colors are the terminal's
// own 16-color palette, so they follow its theme on light and dark
// backgrounds alike.
type Styles struct {
	Title      lipgloss.Style
	Dim        lipgloss.Style
	Rule       lipgloss.Style
	Edge       lipgloss.Style
	EdgeActive lipgloss.Style
	// Selected marks the selected node's cell. Reverse video stays visible
	// without color.
	Selected lipgloss.Style
	Notice   lipgloss.Style
	Error    lipgloss.Style
	Help     help.Styles

	running   lipgloss.Style
	waiting   lipgloss.Style
	succeeded lipgloss.Style
	skipped   lipgloss.Style
	failed    lipgloss.Style
	stopped   lipgloss.Style
	idle      lipgloss.Style
}

// DefaultStyles returns the standard theme.
func DefaultStyles() Styles {
	base := lipgloss.NewStyle()
	dim := base.Faint(true)
	h := help.DefaultDarkStyles()
	h.ShortKey = base.Bold(true)
	h.FullKey = base.Bold(true)
	h.ShortDesc = dim
	h.FullDesc = dim
	h.ShortSeparator = dim
	h.FullSeparator = dim
	h.Ellipsis = dim
	return Styles{
		Title:      base.Bold(true),
		Dim:        dim,
		Rule:       dim,
		Edge:       dim,
		EdgeActive: base.Bold(true),
		Selected:   base.Reverse(true),
		Notice:     base.Foreground(lipgloss.Yellow),
		Error:      base.Foreground(lipgloss.Red),
		Help:       h,
		running:    base.Foreground(lipgloss.Blue),
		waiting:    base.Foreground(lipgloss.Magenta),
		succeeded:  base.Foreground(lipgloss.Green),
		skipped:    dim.Foreground(lipgloss.Green),
		failed:     base.Foreground(lipgloss.Red),
		stopped:    base.Foreground(lipgloss.Yellow),
		idle:       dim,
	}
}

// State returns the style for a node state's icon and status text.
func (s Styles) State(st events.State) lipgloss.Style {
	switch st {
	case events.Running:
		return s.running
	case events.Waiting:
		return s.waiting
	case events.Succeeded:
		return s.succeeded
	case events.Skipped:
		return s.skipped
	case events.Failed:
		return s.failed
	case events.Blocked, events.Canceled:
		return s.stopped
	case events.Pending, events.Ready:
		return s.idle
	default:
		return s.idle
	}
}
