// Package tui is weft's terminal UI: a live graph of the run, the selected
// node's log under it, and a full-screen log view.
//
// The Model is driven only by messages. Engine events arrive in batches
// (eventsMsg), time arrives as tickMsg, and every side effect, such as
// canceling the run or handing the terminal to an interactive node, runs
// as a tea.Cmd. That keeps Update testable with scripted events and no
// engine, processes, or timers.
package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rs/zerolog"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/layout"
	"github.com/hay-kot/weft/internal/store"
	"github.com/hay-kot/weft/internal/workflow"
)

// Options configures a Model.
type Options struct {
	Workflow *workflow.Workflow
	Run      *store.Run
	// ExitOnEnd quits as soon as the run finishes instead of waiting for q.
	ExitOnEnd bool
	// Interrupt escalates a cancel: the first call cancels the run, later
	// calls kill the running nodes. It returns how many times it has been
	// called, counting calls from signal handlers too.
	Interrupt func() int
	Log       zerolog.Logger
}

// Minimum terminal size for the full layout. Smaller terminals get a
// one-line status instead.
const (
	minWidth  = 30
	minHeight = 10
)

// mainChrome is the rows of the main view outside the graph, the log
// body, and the footer: two header rows, three rules, and the log title.
const mainChrome = 6

// tickInterval drives the spinner and the clocks while the run is active.
const tickInterval = spinnerInterval

// Messages the Model handles besides Bubble Tea's own.
type (
	// eventsMsg carries engine events in publish order.
	eventsMsg struct{ events []events.Event }
	// tickMsg advances the clock.
	tickMsg struct{ at time.Time }
	// interruptedMsg reports a cancel escalation, from a key or a signal.
	interruptedMsg struct {
		level  int
		signal string
	}
	// lendMsg asks for the terminal on behalf of an interactive node.
	lendMsg struct {
		fn    func()
		reply chan<- error
	}
	// lendDoneMsg reports that the terminal is back.
	lendDoneMsg struct {
		reply chan<- error
		ran   bool
		err   error
	}
)

// Model is the root Bubble Tea model.
type Model struct {
	opts   Options
	keys   KeyMap
	styles Styles
	rs     *runState
	now    time.Time
	width  int
	height int

	header header
	graph  graphPane
	pane   logPane
	view   logView
	footer footer

	viewOpen bool
	// auto makes the selection follow the run until the user moves it.
	auto         bool
	cancelLevel  int
	confirmQuit  bool
	quitAfterRun bool
}

// New returns a Model for one run.
func New(opts Options) Model {
	rs := newRunState(opts.Workflow, opts.Run, logCapacity)
	styles := DefaultStyles()
	g := newGraphPane(layout.FromWorkflow(opts.Workflow), rs.cells(time.Time{}))
	return Model{
		opts:         opts,
		keys:         DefaultKeyMap(),
		styles:       styles,
		rs:           rs,
		now:          time.Time{},
		width:        0,
		height:       0,
		header:       header{width: 0},
		graph:        g,
		pane:         newLogPane(g.selected),
		view:         newLogView(g.selected),
		footer:       newFooter(styles),
		viewOpen:     false,
		auto:         true,
		cancelLevel:  0,
		confirmQuit:  false,
		quitAfterRun: false,
	}
}

// Init starts the clock.
func (m Model) Init() tea.Cmd {
	return func() tea.Msg { return tickMsg{at: time.Now()} }
}

func tick() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg{at: t} })
}

// Update handles one message. Sizes are recomputed after every message,
// since any of them can change how many rows a component needs.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m, cmd := m.update(msg)
	return m.resized(), cmd
}

func (m Model) update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		m.now = msg.at
		m.graph = m.graph.withCells(m.rs.cells(m.now))
		if m.rs.done {
			return m, nil
		}
		return m, tick()
	case eventsMsg:
		return m.applyEvents(msg.events)
	case interruptedMsg:
		return m.interrupted(msg), nil
	case lendMsg:
		return m, m.lend(msg)
	case lendDoneMsg:
		return m, lendDone(msg, m.opts.Log)
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) applyEvents(evs []events.Event) (Model, tea.Cmd) {
	var cmds []tea.Cmd
	finished := false
	for _, e := range evs {
		switch e := e.(type) {
		case events.RunStarted:
			m.rs.started = e.At
		case events.NodeChanged:
			m = m.nodeChanged(e)
		case events.NodeOutput:
			if buf := m.rs.logs[e.Node]; buf != nil {
				buf.append(logLine{Text: sanitize(e.Line), Notice: false})
			}
		case events.OutputDropped:
			if buf := m.rs.logs[e.Node]; buf != nil {
				st := m.rs.nodes[e.Node]
				text := fmt.Sprintf("… %d lines not shown here because the TUI fell behind; %s has them",
					e.Lines, m.rs.run.AttemptLogPath(e.Node, st.Attempt))
				buf.append(logLine{Text: text, Notice: true})
			}
		case events.RunFinished:
			m.rs.done, m.rs.success, m.rs.finished = true, e.Success, e.At
			finished = true
		case events.Barrier:
			cmds = append(cmds, func() tea.Msg { e.Done(); return nil })
		}
	}
	if finished {
		m = m.runFinished()
		if m.opts.ExitOnEnd || m.quitAfterRun {
			cmds = append(cmds, tea.Quit)
		}
	}
	m.graph = m.graph.withCells(m.rs.cells(m.now))
	return m, tea.Batch(cmds...)
}

func (m Model) nodeChanged(e events.NodeChanged) Model {
	st, ok := m.rs.nodes[e.Node]
	if !ok {
		return m
	}
	st.State, st.Attempt, st.ExitCode = e.State, e.Attempt, e.ExitCode
	st.Err, st.Reason, st.Started, st.Finished = e.Err, e.Reason, e.Started, e.Finished
	if m.auto {
		m = m.selectNode(m.rs.autoSelect(m.graph.selected, e.Node))
	}
	return m
}

// runFinished points an untouched selection at the first failure, so the
// end screen shows what went wrong.
func (m Model) runFinished() Model {
	m.footer = m.footer.withNotice(noticeInfo, "")
	if !m.auto || m.rs.success || m.rs.state(m.graph.selected) == events.Failed {
		return m
	}
	for _, n := range m.rs.wf.Nodes {
		if m.rs.state(n.ID) == events.Failed {
			return m.selectNode(n.ID)
		}
	}
	return m
}

func (m Model) selectNode(id string) Model {
	m.graph = m.graph.selectNode(id)
	m.pane = m.pane.show(m.graph.selected)
	return m
}

func (m Model) interrupted(msg interruptedMsg) Model {
	m.cancelLevel = max(m.cancelLevel, msg.level)
	if m.rs.done {
		return m
	}
	from := ""
	if msg.signal != "" {
		from = " (" + msg.signal + ")"
	}
	switch {
	case m.cancelLevel >= 2:
		m.footer = m.footer.withNotice(noticeWarn, "killing running nodes"+from)
	case m.quitAfterRun:
		m.footer = m.footer.withNotice(noticeWarn, "canceling"+from+"; weft quits when the nodes stop. ctrl+c again kills them now")
	default:
		m.footer = m.footer.withNotice(noticeWarn, "canceling"+from+": running nodes got SIGINT and their grace period. ctrl+c again kills them now")
	}
	return m
}

func (m Model) interrupt() tea.Cmd {
	interrupt := m.opts.Interrupt
	return func() tea.Msg {
		if interrupt == nil {
			return nil
		}
		return interruptedMsg{level: interrupt(), signal: ""}
	}
}

func (m Model) handleKey(k tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.confirmQuit {
		m.confirmQuit = false
		m.footer = m.footer.withNotice(noticeInfo, "")
		if key.Matches(k, m.keys.Confirm) {
			m.quitAfterRun = true
			return m, m.interrupt()
		}
		if !key.Matches(k, m.keys.Cancel) {
			return m, nil
		}
	}

	if key.Matches(k, m.keys.Cancel) {
		if m.rs.done {
			return m, tea.Quit
		}
		return m, m.interrupt()
	}

	if m.viewOpen {
		if key.Matches(k, m.keys.Close) {
			m.viewOpen = false
			return m, nil
		}
		m.view, _ = m.view.Update(k, m.keys, m.rs.logs[m.view.node])
		return m, nil
	}

	switch {
	case key.Matches(k, m.keys.Quit):
		if m.rs.done {
			return m, tea.Quit
		}
		m.confirmQuit = true
		m.footer = m.footer.withNotice(noticeWarn, "The run is still going. Press y to cancel it and quit, any other key to keep it running.")
		return m, nil
	case key.Matches(k, m.keys.Help):
		m.footer = m.footer.toggleHelp()
		return m, nil
	case key.Matches(k, m.keys.Open):
		if m.graph.selected == "" {
			return m, nil
		}
		m.viewOpen = true
		m.view = newLogView(m.graph.selected)
		return m, nil
	}

	before := m.graph.selected
	var handled bool
	if m.graph, handled = m.graph.Update(k, m.keys); handled {
		if m.graph.selected != before {
			m.auto = false
			m.pane = m.pane.show(m.graph.selected)
		}
		return m, nil
	}
	m.pane, _ = m.pane.Update(k, m.keys, m.rs.logs[m.pane.node])
	return m, nil
}

// interactiveNode returns the interactive node that is running, which is
// the one asking for the terminal.
func (m Model) interactiveNode() string {
	for _, n := range m.rs.wf.Nodes {
		if n.Interactive && m.rs.state(n.ID) == events.Running {
			return n.DisplayName()
		}
	}
	return "an interactive node"
}

// footerHelp returns the help bindings for the current view.
func (m Model) footerHelp() ([]key.Binding, [][]key.Binding) {
	if m.viewOpen {
		return m.keys.logHelp(), nil
	}
	return m.keys.mainHelp(!m.rs.done), m.keys.mainFullHelp()
}

// resized recomputes every component's size from the terminal size.
func (m Model) resized() Model {
	w, h := m.width, m.height
	m.header = m.header.withWidth(w)
	// The full help gives way before the graph or the log would vanish.
	m.footer = m.footer.withWidth(w).withMaxRows(h - mainChrome - 2)
	short, full := m.footerHelp()
	footerH := m.footer.height(short, full)

	if m.viewOpen {
		// title, body, rule, footer
		m.view = m.view.withSize(w, h-2-footerH)
		return m
	}

	avail := max(0, h-mainChrome-footerH)
	logMin := max(3, avail/3)
	m.graph = m.graph.withSize(max(0, w-1), max(1, avail-logMin))
	graphRows := m.graph.rows()
	if len(m.rs.wf.Nodes) == 0 {
		graphRows = 1
	}
	m.pane = m.pane.withSize(w, avail-graphRows)
	return m
}

// View renders the screen.
func (m Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

func (m Model) render() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	ph := runPhase(m.rs, m.cancelLevel)
	if m.width < minWidth || m.height < minHeight {
		return ansi.Truncate(fmt.Sprintf("window too small · weft %s %s", m.rs.wf.Name, ph), m.width, "…")
	}

	rule := m.styles.Rule.Render(repeat("─", m.width))
	short, full := m.footerHelp()
	foot := m.footer.View(short, full, m.styles)
	if m.viewOpen {
		return join(m.view.View(m.rs, m.styles, m.now), rule, foot)
	}

	graph := m.graph.View(m.rs, m.styles, m.now)
	if len(m.rs.wf.Nodes) == 0 {
		graph = m.styles.Dim.Render("this workflow has no nodes")
	}
	return join(
		m.header.View(m.rs, ph, m.now, m.styles),
		rule,
		indent(graph),
		rule,
		m.pane.View(m.rs, m.styles, m.now),
		rule,
		foot,
	)
}

func repeat(s string, n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat(s, n)
}

// join stacks sections vertically. An empty section takes no rows.
func join(sections ...string) string {
	parts := make([]string, 0, len(sections))
	for _, s := range sections {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}

// indent puts a one-column margin before every row.
func indent(s string) string {
	if s == "" {
		return ""
	}
	return " " + strings.ReplaceAll(s, "\n", "\n ")
}
