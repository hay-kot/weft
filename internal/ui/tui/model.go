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
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rs/zerolog"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/layout"
	"github.com/hay-kot/weft/internal/plan"
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
	// calls kill the running nodes. It returns the escalation level,
	// counting calls from signal handlers too, or 0 when no pass of the run
	// is active.
	Interrupt func() int
	// Plan, when set, is where nodes start before the first events arrive.
	Plan *plan.Plan
	// Confirm, when set, is a question shown before the run starts. y calls
	// Start; any other key quits without starting.
	Confirm string
	// Start starts the run once Confirm is answered with y.
	Start func()
	// Rerun reruns a node, with everything downstream of it when
	// downstream is set. Unless force is set it may return a
	// *plan.DriftError, and the TUI asks before calling it again with force.
	Rerun func(id string, downstream, force bool) error
	// CancelNode cancels one node and leaves the rest of the run going. It
	// returns what it did; plan.Cancel describes the cases.
	CancelNode func(id string) (plan.CancelAction, error)
	// Notice is shown in the footer at the start, for example to say that
	// the run resumes an earlier one.
	Notice string
	Log    zerolog.Logger
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
	// rerunDoneMsg reports how a rerun request went.
	rerunDoneMsg struct {
		node       string
		downstream bool
		err        error
	}
	// cancelNodeDoneMsg reports how a node cancel request went.
	cancelNodeDoneMsg struct {
		node   string
		action plan.CancelAction
		err    error
	}
)

// askKind is what a footer question is about.
type askKind int

const (
	askNone askKind = iota
	// askQuit: q during a run; y cancels it and quits once nodes stop.
	askQuit
	// askStart: Options.Confirm before the run starts.
	askStart
	// askRerun: the workflow file or HEAD changed; y reruns anyway.
	askRerun
)

// question is the footer question waiting for an answer.
type question struct {
	kind       askKind
	node       string
	downstream bool
}

var noQuestion = question{kind: askNone, node: "", downstream: false}

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
	ask          question
	quitAfterRun bool
	// stopping is the node whose cancel the footer is showing. When that
	// node ends with the notice still up, the notice says how it ended.
	stopping       string
	stoppingNotice string
	// ticking is set while a tick is scheduled, so a pass that starts
	// after the clock stopped restarts it exactly once.
	ticking bool
}

// New returns a Model for one run.
func New(opts Options) Model {
	rs := newRunState(opts.Workflow, opts.Run, opts.Plan, logCapacity)
	styles := DefaultStyles()
	g := newGraphPane(layout.FromWorkflow(opts.Workflow), rs.cells(time.Time{}))
	f := newFooter(styles).withNotice(noticeInfo, opts.Notice)
	ask := noQuestion
	if opts.Confirm != "" {
		ask = question{kind: askStart, node: "", downstream: false}
		f = f.withNotice(noticeWarn, opts.Confirm)
	}
	return Model{
		opts:           opts,
		keys:           DefaultKeyMap(),
		styles:         styles,
		rs:             rs,
		now:            time.Time{},
		width:          0,
		height:         0,
		header:         header{width: 0},
		graph:          g,
		pane:           newLogPane(g.selected),
		view:           newLogView(g.selected),
		footer:         f,
		viewOpen:       false,
		auto:           true,
		cancelLevel:    0,
		ask:            ask,
		quitAfterRun:   false,
		stopping:       "",
		stoppingNotice: "",
		ticking:        true,
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
			m.ticking = false
			return m, nil
		}
		return m, tick()
	case eventsMsg:
		return m.applyEvents(msg.events)
	case interruptedMsg:
		return m.interrupted(msg)
	case rerunDoneMsg:
		return m.rerunDone(msg), nil
	case cancelNodeDoneMsg:
		return m.cancelNodeDone(msg), nil
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
	for _, e := range evs {
		switch e := e.(type) {
		case events.RunStarted:
			m.rs.started = e.At
			if m.rs.done {
				m = m.passStarted()
				if !m.ticking {
					m.ticking = true
					cmds = append(cmds, tick())
				}
			}
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
			m = m.runFinished()
			if m.opts.ExitOnEnd || m.quitAfterRun {
				cmds = append(cmds, tea.Quit)
			}
		case events.Barrier:
			cmds = append(cmds, func() tea.Msg { e.Done(); return nil })
		}
	}
	m.graph = m.graph.withCells(m.rs.cells(m.now))
	return m, tea.Batch(cmds...)
}

// passStarted makes a finished run active again: a rerun after the end
// started another pass.
func (m Model) passStarted() Model {
	m.rs.done, m.rs.success, m.rs.finished = false, false, time.Time{}
	m.cancelLevel = 0
	return m
}

func (m Model) nodeChanged(e events.NodeChanged) Model {
	st, ok := m.rs.nodes[e.Node]
	if !ok {
		return m
	}
	if buf := m.rs.logs[e.Node]; e.State == events.Running && buf != nil && buf.end() > buf.first() {
		buf.append(logLine{Text: fmt.Sprintf("── attempt %d ──", e.Attempt), Notice: true})
	}
	st.State, st.Attempt, st.ExitCode = e.State, e.Attempt, e.ExitCode
	st.Err, st.Reason, st.Started, st.Finished = e.Err, e.Reason, e.Started, e.Finished
	if e.Node == m.stopping && e.State.Terminal() {
		if m.footer.notice == m.stoppingNotice {
			m.footer = m.footer.withNotice(noticeInfo, m.displayName(e.Node)+" "+e.Err)
		}
		m.stopping, m.stoppingNotice = "", ""
	}
	if m.auto {
		m = m.selectNode(m.rs.autoSelect(m.graph.selected, e.Node))
	}
	return m
}

// runFinished points an untouched selection at the first failure, so the
// end screen shows what went wrong.
func (m Model) runFinished() Model {
	switch m.ask.kind {
	case askQuit:
		// q quits outright now, so the question is moot.
		m.ask = noQuestion
		m.footer = m.footer.withNotice(noticeInfo, "")
	case askNone:
		m.footer = m.footer.withNotice(noticeInfo, "")
	case askStart, askRerun:
	}
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

func (m Model) interrupted(msg interruptedMsg) (Model, tea.Cmd) {
	if m.ask.kind == askStart {
		// A signal before the user answered: don't start at all.
		m.ask = noQuestion
		return m, tea.Quit
	}
	m.cancelLevel = max(m.cancelLevel, msg.level)
	if m.rs.done || msg.level == 0 {
		return m, nil
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
	return m, nil
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
	if m.ask.kind != askNone {
		var cmd tea.Cmd
		var handled bool
		if m, cmd, handled = m.answer(k); handled {
			return m, cmd
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
	case key.Matches(k, m.keys.Rerun), key.Matches(k, m.keys.RerunAll):
		return m.requestRerun(m.graph.selected, key.Matches(k, m.keys.RerunAll))
	case key.Matches(k, m.keys.CancelNode):
		return m.requestCancelNode(m.graph.selected)
	case key.Matches(k, m.keys.Quit):
		if m.rs.done {
			return m, tea.Quit
		}
		m.ask = question{kind: askQuit, node: "", downstream: false}
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

// answer handles a key while a question waits. It reports whether the key
// is used up; a ctrl+c that only dismisses a question still cancels.
func (m Model) answer(k tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	ask := m.ask
	m.ask = noQuestion
	yes := key.Matches(k, m.keys.Confirm)
	switch ask.kind {
	case askStart:
		if !yes {
			return m, tea.Quit, true
		}
		m.footer = m.footer.withNotice(noticeInfo, m.opts.Notice)
		start := m.opts.Start
		return m, func() tea.Msg {
			if start != nil {
				start()
			}
			return nil
		}, true
	case askQuit:
		m.footer = m.footer.withNotice(noticeInfo, "")
		if yes {
			m.quitAfterRun = true
			return m, m.interrupt(), true
		}
	case askRerun:
		if yes {
			m.footer = m.footer.withNotice(noticeInfo, "rerunning "+m.displayName(ask.node))
			return m, m.rerun(ask.node, ask.downstream, true), true
		}
		m.footer = m.footer.withNotice(noticeInfo, "rerun of "+m.displayName(ask.node)+" skipped")
	case askNone:
	}
	return m, nil, !key.Matches(k, m.keys.Cancel)
}

// requestRerun checks that id may rerun before asking for it, so an
// ineligible node gets its reason at once. The engine checks again.
func (m Model) requestRerun(id string, downstream bool) (Model, tea.Cmd) {
	if id == "" || m.opts.Rerun == nil {
		return m, nil
	}
	if m.cancelLevel > 0 && !m.rs.done {
		m.footer = m.footer.withNotice(noticeWarn, "the run is being canceled; rerun once it has stopped")
		return m, nil
	}
	if _, err := plan.Rerun(m.rs.wf, m.rs.states(), id, downstream); err != nil {
		m.footer = m.footer.withNotice(noticeWarn, err.Error())
		return m, nil
	}
	return m, m.rerun(id, downstream, false)
}

func (m Model) rerun(id string, downstream, force bool) tea.Cmd {
	fn := m.opts.Rerun
	return func() tea.Msg {
		return rerunDoneMsg{node: id, downstream: downstream, err: fn(id, downstream, force)}
	}
}

func (m Model) rerunDone(msg rerunDoneMsg) Model {
	name := m.displayName(msg.node)
	if drift, ok := errors.AsType[*plan.DriftError](msg.err); ok {
		m.ask = question{kind: askRerun, node: msg.node, downstream: msg.downstream}
		m.footer = m.footer.withNotice(noticeWarn, "Since this run last started, "+strings.Join(drift.Reasons, " and ")+". y reruns "+name+" anyway, any other key skips it.")
		return m
	}
	switch {
	case msg.err != nil:
		m.footer = m.footer.withNotice(noticeWarn, msg.err.Error())
	case msg.downstream:
		m.footer = m.footer.withNotice(noticeInfo, "rerunning "+name+" and everything downstream of it")
	default:
		m.footer = m.footer.withNotice(noticeInfo, "rerunning "+name)
	}
	return m
}

// requestCancelNode checks that id may be canceled before asking for it,
// so a finished node gets its reason at once. The engine checks again,
// and decides whether a running node is canceled or killed.
func (m Model) requestCancelNode(id string) (Model, tea.Cmd) {
	if id == "" || m.opts.CancelNode == nil {
		return m, nil
	}
	if _, err := plan.Cancel(m.rs.wf, m.rs.states(), id, false); err != nil {
		m.footer = m.footer.withNotice(noticeWarn, err.Error())
		return m, nil
	}
	fn := m.opts.CancelNode
	return m, func() tea.Msg {
		action, err := fn(id)
		return cancelNodeDoneMsg{node: id, action: action, err: err}
	}
}

func (m Model) cancelNodeDone(msg cancelNodeDoneMsg) Model {
	name := m.displayName(msg.node)
	if msg.err != nil {
		m.footer = m.footer.withNotice(noticeWarn, msg.err.Error())
		return m
	}
	m.stopping = ""
	switch msg.action {
	case plan.CancelUnstarted:
		m.footer = m.footer.withNotice(noticeWarn, "canceled "+name+" before it started")
	case plan.CancelRunning:
		grace := ""
		if n := m.rs.wf.Node(msg.node); n != nil {
			grace = " (" + n.KillTimeout.String() + ")"
		}
		m.footer = m.footer.withNotice(noticeWarn, "canceling "+name+": it got SIGINT and has its kill_timeout"+grace+" to stop. x again kills it now")
		m.stopping, m.stoppingNotice = msg.node, m.footer.notice
	case plan.KillRunning:
		m.footer = m.footer.withNotice(noticeWarn, "killing "+name)
		m.stopping, m.stoppingNotice = msg.node, m.footer.notice
	}
	return m
}

func (m Model) displayName(id string) string {
	if n := m.rs.wf.Node(id); n != nil {
		return n.DisplayName()
	}
	return id
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
	// ctrl+c before the start confirmation quits without starting, so it
	// is not a cancel there.
	running := !m.rs.done && m.ask.kind != askStart
	return m.keys.mainHelp(running), m.keys.mainFullHelp()
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
