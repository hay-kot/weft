package tui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/rs/zerolog"

	"github.com/hay-kot/weft/internal/events"
)

// maxBatch caps how many events go to Update in one message, so a flood of
// output can't make one Update arbitrarily long.
const maxBatch = 1024

// barrierTimeout bounds how long an interactive node waits for the TUI to
// draw the events before it; a stuck TUI must not hold the node forever.
const barrierTimeout = 2 * time.Second

// ErrExited means the TUI stopped before it could lend the terminal.
var ErrExited = errors.New("the TUI exited, so the terminal can't be handed to the node")

// App runs the TUI program for one run.
type App struct {
	program *tea.Program
	sub     *events.Subscription
	log     zerolog.Logger
	exited  chan struct{}
}

// NewApp returns an App that renders sub's events. noColor drops every
// color, keeping only bold, faint, and reverse video.
func NewApp(opts Options, sub *events.Subscription, noColor bool) *App {
	log := opts.Log
	progOpts := []tea.ProgramOption{
		// weft handles SIGINT and SIGTERM itself: they cancel the run,
		// which must not depend on the TUI being responsive.
		tea.WithoutSignalHandler(),
		tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
			logMsg(log, msg)
			return msg
		}),
	}
	if noColor {
		progOpts = append(progOpts, tea.WithColorProfile(colorprofile.Ascii))
	}
	return &App{
		program: tea.NewProgram(New(opts), progOpts...),
		sub:     sub,
		log:     log,
		exited:  make(chan struct{}),
	}
}

// Run shows the TUI until the user quits, or until it fails. It returns
// the program's error, if any; the run itself may still be going.
func (a *App) Run() error {
	go a.forward()
	defer close(a.exited)
	_, err := a.program.Run()
	return err
}

func (a *App) forward() {
	forward(a.sub.C, func(batch []events.Event) {
		select {
		case <-a.exited:
			releaseBarriers(batch)
		default:
			a.program.Send(eventsMsg{events: batch})
		}
	})
}

// forward reads events from in and hands them to send in batches, in
// order. While send is busy (the program is still drawing the last batch),
// events gather into the next batch, so a flood of output costs one Update
// per batch rather than one per line. A full batch stops the reading, and
// the backlog waits in the subscription, where the bus drops output past
// its budget instead of growing memory or stalling the engine. forward
// returns once in closes and every event has been sent.
func forward(in <-chan events.Event, send func([]events.Event)) {
	batches := make(chan []events.Event)
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for b := range batches {
			send(b)
		}
	}()

	var pending []events.Event
	for in != nil || len(pending) > 0 {
		recv, out := in, batches
		if len(pending) >= maxBatch {
			recv = nil
		}
		if len(pending) == 0 {
			out = nil
		}
		select {
		case e, ok := <-recv:
			if !ok {
				in = nil
				continue
			}
			pending = append(pending, e)
		case out <- pending:
			pending = nil
		}
	}
	close(batches)
	<-sent
}

func releaseBarriers(batch []events.Event) {
	for _, e := range batch {
		if b, ok := e.(events.Barrier); ok {
			b.Done()
		}
	}
}

// Interrupted tells the TUI that a signal escalated the cancel to level.
// It never blocks.
func (a *App) Interrupted(level int, signal string) {
	go a.program.Send(interruptedMsg{level: level, signal: signal})
}

// Quit stops the TUI. It never blocks.
func (a *App) Quit() {
	go a.program.Quit()
}

// Lender returns the engine's TerminalLender for this TUI.
func (a *App) Lender() *Lender {
	return &Lender{app: a}
}

// Lender hands the terminal to interactive nodes by suspending the TUI.
type Lender struct {
	app *App
}

// Lend suspends the TUI, runs fn with the terminal restored to normal
// mode, and returns once the TUI has the terminal back. It returns an
// error, without calling fn, when the TUI can't give the terminal up.
func (l *Lender) Lend(fn func()) error {
	// Let the TUI draw the node as running before it disappears, so the
	// screen weft comes back to matches the one it left.
	l.app.sub.Barrier(barrierTimeout)

	reply := make(chan error, 1)
	select {
	case <-l.app.exited:
		return ErrExited
	default:
	}
	started, ran := make(chan struct{}), make(chan struct{})
	go l.app.program.Send(lendMsg{fn: fn, reply: reply, started: started, ran: ran})
	select {
	case err := <-reply:
		return err
	case <-l.app.exited:
		// The program can die after the node ran but before the reply is
		// sent (a failed terminal restore, a panic elsewhere). The node's
		// result stands then, so fn is waited for and no error reported.
		select {
		case <-started:
			<-ran
			return nil
		default:
			return ErrExited
		}
	}
}

// handoff is the tea.ExecCommand that runs an interactive node.
type handoff struct {
	fn           func()
	node         string
	out          io.Writer
	ran          bool
	started, did chan<- struct{}
}

func (h *handoff) SetStdin(io.Reader)    {}
func (h *handoff) SetStdout(w io.Writer) { h.out = w }
func (h *handoff) SetStderr(io.Writer)   {}

// Run runs the node. Bubble Tea has left the alternate screen and put the
// terminal back in normal mode by now; the node's process group takes the
// foreground itself.
func (h *handoff) Run() error {
	out := h.out
	if out == nil {
		out = os.Stdout
	}
	_, _ = fmt.Fprintf(out, "\nweft: %s has the terminal. weft comes back when it exits.\n\n", h.node)
	signalDone(h.started)
	h.fn()
	h.ran = true
	signalDone(h.did)
	return nil
}

func signalDone(ch chan<- struct{}) {
	if ch != nil {
		close(ch)
	}
}

func (m Model) lend(msg lendMsg) tea.Cmd {
	h := &handoff{fn: msg.fn, node: m.interactiveNode(), out: nil, ran: false, started: msg.started, did: msg.ran}
	return tea.Exec(h, func(err error) tea.Msg {
		return lendDoneMsg{reply: msg.reply, ran: h.ran, err: err}
	})
}

func lendDone(msg lendDoneMsg, log zerolog.Logger) tea.Cmd {
	return func() tea.Msg {
		switch {
		case !msg.ran:
			msg.reply <- fmt.Errorf("hand the terminal to the node: %w", msg.err)
		case msg.err != nil:
			log.Warn().Err(msg.err).Msg("restore the terminal after an interactive node")
			msg.reply <- nil
		default:
			msg.reply <- nil
		}
		return nil
	}
}

// logMsg records each message the program receives, so a stuck UI can be
// diagnosed from weft.log. Node output is never logged: it can hold
// secrets, and the attempt logs already have it.
func logMsg(log zerolog.Logger, msg tea.Msg) {
	switch msg := msg.(type) {
	case tickMsg:
		log.Trace().Str("msg", "tick").Msg("tui message")
	case eventsMsg:
		kinds := map[string]int{}
		for _, e := range msg.events {
			kinds[fmt.Sprintf("%T", e)]++
		}
		log.Debug().Str("msg", "events").Int("count", len(msg.events)).Interface("kinds", kinds).Msg("tui message")
	case tea.KeyPressMsg:
		log.Debug().Str("msg", "key").Str("key", msg.String()).Msg("tui message")
	case tea.WindowSizeMsg:
		log.Debug().Str("msg", "window_size").Int("width", msg.Width).Int("height", msg.Height).Msg("tui message")
	case interruptedMsg:
		log.Debug().Str("msg", "interrupted").Int("level", msg.level).Str("signal", msg.signal).Msg("tui message")
	case lendMsg:
		log.Debug().Str("msg", "lend").Msg("tui message")
	case lendDoneMsg:
		log.Debug().Str("msg", "lend_done").Bool("ran", msg.ran).AnErr("error", msg.err).Msg("tui message")
	default:
		log.Debug().Str("msg", fmt.Sprintf("%T", msg)).Msg("tui message")
	}
}
