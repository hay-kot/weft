package tui

import "charm.land/bubbles/v2/key"

// KeyMap lists every key the TUI handles.
type KeyMap struct {
	Left     key.Binding
	Right    key.Binding
	Up       key.Binding
	Down     key.Binding
	Open     key.Binding
	Close    key.Binding
	Follow   key.Binding
	Layout   key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Top      key.Binding
	Bottom   key.Binding
	ScrollL  key.Binding
	ScrollR  key.Binding
	Cancel   key.Binding
	Quit     key.Binding
	Help     key.Binding
	Confirm  key.Binding
}

// DefaultKeyMap returns the standard bindings.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Left:     key.NewBinding(key.WithKeys("h", "left"), key.WithHelp("h/←", "left")),
		Right:    key.NewBinding(key.WithKeys("l", "right"), key.WithHelp("l/→", "right")),
		Up:       key.NewBinding(key.WithKeys("k", "up"), key.WithHelp("k/↑", "up")),
		Down:     key.NewBinding(key.WithKeys("j", "down"), key.WithHelp("j/↓", "down")),
		Open:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "log")),
		Close:    key.NewBinding(key.WithKeys("esc", "q", "enter"), key.WithHelp("esc/q", "back")),
		Follow:   key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "follow")),
		Layout:   key.NewBinding(key.WithKeys("v"), key.WithHelp("v", "layout")),
		PageUp:   key.NewBinding(key.WithKeys("pgup", "ctrl+u", "b"), key.WithHelp("pgup/ctrl+u", "page up")),
		PageDown: key.NewBinding(key.WithKeys("pgdown", "ctrl+d", "space"), key.WithHelp("pgdn/ctrl+d", "page down")),
		Top:      key.NewBinding(key.WithKeys("g", "home"), key.WithHelp("g", "top")),
		Bottom:   key.NewBinding(key.WithKeys("G", "end"), key.WithHelp("G", "bottom")),
		ScrollL:  key.NewBinding(key.WithKeys("h", "left"), key.WithHelp("h/l", "scroll sideways")),
		ScrollR:  key.NewBinding(key.WithKeys("l", "right")),
		Cancel:   key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "cancel")),
		Quit:     key.NewBinding(key.WithKeys("q"), key.WithHelp("q", "quit")),
		Help:     key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Confirm:  key.NewBinding(key.WithKeys("y", "Y")),
	}
}

// selectHelp stands in for the four selection keys in the short help.
var selectHelp = key.NewBinding(key.WithKeys("h", "j", "k", "l"), key.WithHelp("hjkl", "select"))

// mainHelp is the short help for the graph view.
func (k KeyMap) mainHelp(running bool) []key.Binding {
	cancel := k.Cancel
	cancel.SetEnabled(running)
	return []key.Binding{selectHelp, k.Open, cancel, k.Quit, k.Help, k.Follow, k.Layout}
}

// mainFullHelp is the expanded help for the graph view.
func (k KeyMap) mainFullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Left, k.Right, k.Up, k.Down},
		{k.Open, k.Layout, k.Follow, k.PageUp, k.PageDown},
		{k.Cancel, k.Quit, k.Help},
	}
}

// logHelp is the short help for the full-screen log.
func (k KeyMap) logHelp() []key.Binding {
	scroll := key.NewBinding(key.WithKeys("j", "k"), key.WithHelp("j/k", "scroll"))
	jump := key.NewBinding(key.WithKeys("g", "G"), key.WithHelp("g/G", "top/bottom"))
	return []key.Binding{k.Close, scroll, k.PageDown, jump, k.ScrollL, k.Follow}
}
