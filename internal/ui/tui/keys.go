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
	Rerun    key.Binding
	RerunAll key.Binding
	// CancelNode cancels the selected node only; Cancel cancels the run.
	CancelNode key.Binding
	Follow     key.Binding
	Layout     key.Binding
	PageUp     key.Binding
	PageDown   key.Binding
	Top        key.Binding
	Bottom     key.Binding
	ScrollL    key.Binding
	ScrollR    key.Binding
	Cancel     key.Binding
	Quit       key.Binding
	Help       key.Binding
	// Info opens the selected node's details.
	Info key.Binding
	// Confirm, Decline, Choose, and NextButton answer a question dialog.
	Confirm    key.Binding
	Decline    key.Binding
	Choose     key.Binding
	NextButton key.Binding
}

// DefaultKeyMap returns the standard bindings.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Left:       key.NewBinding(key.WithKeys("h", "left"), key.WithHelp("h/←", "left")),
		Right:      key.NewBinding(key.WithKeys("l", "right"), key.WithHelp("l/→", "right")),
		Up:         key.NewBinding(key.WithKeys("k", "up"), key.WithHelp("k/↑", "up")),
		Down:       key.NewBinding(key.WithKeys("j", "down"), key.WithHelp("j/↓", "down")),
		Open:       key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "log")),
		Close:      key.NewBinding(key.WithKeys("esc", "q", "enter"), key.WithHelp("esc/q", "back")),
		Rerun:      key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "rerun")),
		RerunAll:   key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "rerun+downstream")),
		CancelNode: key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "cancel node")),
		Follow:     key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "follow")),
		Layout:     key.NewBinding(key.WithKeys("v"), key.WithHelp("v", "layout")),
		PageUp:     key.NewBinding(key.WithKeys("pgup", "ctrl+u", "b"), key.WithHelp("pgup/ctrl+u", "page up")),
		PageDown:   key.NewBinding(key.WithKeys("pgdown", "ctrl+d", "space"), key.WithHelp("pgdn/ctrl+d", "page down")),
		Top:        key.NewBinding(key.WithKeys("g", "home"), key.WithHelp("g", "top")),
		Bottom:     key.NewBinding(key.WithKeys("G", "end"), key.WithHelp("G", "bottom")),
		ScrollL:    key.NewBinding(key.WithKeys("h", "left"), key.WithHelp("h/l", "scroll sideways")),
		ScrollR:    key.NewBinding(key.WithKeys("l", "right")),
		Cancel:     key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "cancel")),
		Quit:       key.NewBinding(key.WithKeys("q"), key.WithHelp("q", "quit")),
		Help:       key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Info:       key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "info")),
		Confirm:    key.NewBinding(key.WithKeys("y", "Y"), key.WithHelp("y", "yes")),
		Decline:    key.NewBinding(key.WithKeys("n", "N", "esc"), key.WithHelp("n/esc", "no")),
		Choose:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "choose")),
		NextButton: key.NewBinding(key.WithKeys("tab", "shift+tab"), key.WithHelp("tab", "switch")),
	}
}

// selectHelp stands in for the four selection keys in the short help.
var selectHelp = key.NewBinding(key.WithKeys("h", "j", "k", "l"), key.WithHelp("hjkl", "select"))

// mainHelp is the short help for the graph view. The cancel keys show
// only while a pass runs.
func (k KeyMap) mainHelp(running bool) []key.Binding {
	cancel, cancelNode := k.Cancel, k.CancelNode
	cancel.SetEnabled(running)
	cancelNode.SetEnabled(running)
	return []key.Binding{selectHelp, k.Open, cancel, k.Quit, k.Help, k.Rerun, k.RerunAll, cancelNode, k.Info, k.Follow, k.Layout}
}

// logHelp is the short help for the full-screen log.
func (k KeyMap) logHelp() []key.Binding {
	scroll := key.NewBinding(key.WithKeys("j", "k"), key.WithHelp("j/k", "scroll"))
	jump := key.NewBinding(key.WithKeys("g", "G"), key.WithHelp("g/G", "top/bottom"))
	return []key.Binding{k.Close, scroll, k.PageDown, jump, k.ScrollL, k.Follow, k.Help}
}

// helpEntry is one row of the help dialog. The key comes from the binding,
// so the help can't name a key the TUI does not handle; the description
// has room to say more than the footer's.
type helpEntry struct {
	binding key.Binding
	desc    string
}

// helpSection is a titled group of help rows.
type helpSection struct {
	title   string
	entries []helpEntry
}

// graphHelp is the help for the graph view.
func (k KeyMap) graphHelp() []helpSection {
	return []helpSection{
		{"Graph", []helpEntry{
			{selectHelp, "select the node in that direction"},
			{k.Layout, "switch between horizontal and vertical"},
			{k.Info, "details of the selected node"},
		}},
		{"Log", []helpEntry{
			{k.Open, "open the selected node's log full screen"},
			{k.PageUp, "scroll the log pane up"},
			{k.PageDown, "scroll the log pane down"},
			{k.Top, "go to the start of the log"},
			{k.Bottom, "go to the end of the log"},
			{k.Follow, "follow new output, or stop following"},
		}},
		{"Run", []helpEntry{
			{k.Rerun, "rerun the selected node"},
			{k.RerunAll, "rerun it and everything downstream"},
			{k.CancelNode, "cancel the selected node; again kills it"},
			{k.Cancel, "cancel the run; again kills every node"},
			{k.Quit, "quit; asks first while the run is going"},
		}},
		k.generalHelp(),
	}
}

// logViewHelp is the help for the full-screen log.
func (k KeyMap) logViewHelp() []helpSection {
	scroll := key.NewBinding(key.WithKeys("j", "k"), key.WithHelp("j/k", "scroll"))
	return []helpSection{
		{"Log", []helpEntry{
			{scroll, "scroll one line"},
			{k.PageUp, "scroll up a page"},
			{k.PageDown, "scroll down a page"},
			{k.Top, "go to the start"},
			{k.Bottom, "go to the end"},
			{k.ScrollL, "scroll long lines sideways"},
			{k.Follow, "follow new output, or stop following"},
			{k.Close, "back to the graph"},
		}},
		{"Run", []helpEntry{
			{k.Cancel, "cancel the run; again kills every node"},
		}},
		k.generalHelp(),
	}
}

// helpClose closes the help dialog.
func (k KeyMap) helpClose() key.Binding {
	return key.NewBinding(key.WithKeys("esc", "?", "q"), key.WithHelp("esc/?", "close"))
}

// infoClose closes the node info dialog.
func (k KeyMap) infoClose() key.Binding {
	return key.NewBinding(key.WithKeys("esc", "i", "q", "enter"), key.WithHelp("esc/i", "close"))
}

func (k KeyMap) generalHelp() helpSection {
	return helpSection{"Help", []helpEntry{
		{k.Help, "show or hide this help"},
	}}
}
