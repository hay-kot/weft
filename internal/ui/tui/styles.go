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
	Notice     lipgloss.Style
	Error      lipgloss.Style
	Help       help.Styles

	// Dialog boxes and their parts.
	Dialog         lipgloss.Style
	DialogDanger   lipgloss.Style
	DialogTitle    lipgloss.Style
	DialogSection  lipgloss.Style
	DialogKey      lipgloss.Style
	Button         lipgloss.Style
	ButtonSelected lipgloss.Style

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
		Notice:     base.Foreground(lipgloss.Yellow),
		Error:      base.Foreground(lipgloss.Red),
		Help:       h,
		// Reverse video marks the selected button without needing color.
		Dialog:         base.Border(lipgloss.RoundedBorder()),
		DialogDanger:   base.Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Red),
		DialogTitle:    base.Bold(true),
		DialogSection:  dim.Italic(true),
		DialogKey:      base.Bold(true),
		Button:         dim.Padding(0, 1),
		ButtonSelected: base.Bold(true).Reverse(true).Padding(0, 1),
		running:        base.Foreground(lipgloss.Blue),
		waiting:        base.Foreground(lipgloss.Magenta),
		succeeded:      base.Foreground(lipgloss.Green),
		skipped:        dim.Foreground(lipgloss.Green),
		failed:         base.Foreground(lipgloss.Red),
		stopped:        base.Foreground(lipgloss.Yellow),
		idle:           dim,
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
	case events.Pending, events.Ready, events.Excluded:
		return s.idle
	default:
		return s.idle
	}
}

// selectedOn and selectedOff mark the selected node's label bold and
// underlined. Both stay visible without color and take no extra columns, so
// the layout does not move with the selection. They are written as one
// sequence around the whole label; see glyphClass.render.
const (
	selectedOn  = "\x1b[1;4m"
	selectedOff = "\x1b[0m"
)
