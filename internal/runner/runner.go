// Package runner runs one workflow node as a child process in its own
// process group.
package runner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/hay-kot/weft/internal/term"
)

// TermGrace is how long a node gets after SIGTERM before SIGKILL.
const TermGrace = 5 * time.Second

// pipeDrainDelay bounds how long weft keeps reading output after a node's
// shell exits on its own. A background grandchild that inherited the pipe
// would otherwise hold the node open forever.
const pipeDrainDelay = 2 * time.Second

// groupPollInterval is how often weft checks whether a stopping node's
// process group is empty.
const groupPollInterval = 50 * time.Millisecond

// finalKillWait bounds how long weft waits for a process group to vanish
// after SIGKILL. Only a process stuck in the kernel outlives it.
const finalKillWait = 5 * time.Second

// Reason says why a node process ended.
type Reason string

// Reasons a node process ends.
const (
	ReasonExit     Reason = "exit"
	ReasonTimeout  Reason = "timeout"
	ReasonCanceled Reason = "canceled"
	ReasonStartErr Reason = "start_error"
)

// Spec describes one node attempt.
type Spec struct {
	// Node is the node ID, used only for logging.
	Node   string
	Shell  []string
	Script string
	// Dir is the absolute working directory.
	Dir string
	// Env is the complete child environment.
	Env         []string
	Timeout     time.Duration
	KillTimeout time.Duration
	// TermGrace overrides how long the node gets after SIGTERM. Zero means
	// the TermGrace constant.
	TermGrace time.Duration
	// Output receives combined stdout and stderr. Ignored when Interactive.
	Output io.Writer
	// Interactive attaches the child to weft's own stdin, stdout, and
	// stderr. When stdin is a terminal the child's process group becomes the
	// terminal's foreground group for the life of the child.
	Interactive bool
	// Kill, when closed, skips any remaining grace period and sends SIGKILL.
	Kill <-chan struct{}
	// Log receives debug lines about signals sent to the process group.
	Log zerolog.Logger
}

// Result is the outcome of one node attempt.
type Result struct {
	// ExitCode is the process exit code, or -1 when a signal ended it or it
	// never started.
	ExitCode int
	// Signal names the signal that ended the process, if any.
	Signal string
	Reason Reason
	// Err holds a start failure. It is nil for a process that ran.
	Err error
}

// Success reports whether the node exited 0 on its own.
func (r Result) Success() bool {
	return r.Reason == ReasonExit && r.ExitCode == 0
}

// Process runs nodes as real child processes.
type Process struct{}

// Run starts the node and waits for it. Canceling ctx starts graceful
// shutdown: SIGINT to the process group, SIGTERM after KillTimeout, then
// SIGKILL after TermGrace. A Timeout runs the same sequence. Each step
// waits for the whole group to exit, not only the shell, so a wrapper such
// as mise that exits at once does not cut short its child's cleanup.
func (Process) Run(ctx context.Context, spec Spec) Result {
	if len(spec.Shell) == 0 {
		return startFailure(errors.New("empty shell"))
	}
	if ctx.Err() != nil || isClosed(spec.Kill) {
		return Result{ExitCode: -1, Signal: "", Reason: ReasonCanceled, Err: nil}
	}
	args := append(slices.Clone(spec.Shell[1:]), spec.Script)
	// Not CommandContext: canceling means a SIGINT, SIGTERM, SIGKILL ladder
	// sent to the whole process group, not one SIGKILL to the shell.
	cmd := exec.Command(spec.Shell[0], args...) //nolint:gosec,noctx // running the workflow's command is the point
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env

	foreground := false
	var output *outputPipe
	if spec.Interactive {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if term.IsTerminal(os.Stdin) {
			foreground = true
			cmd.SysProcAttr.Foreground = true
			cmd.SysProcAttr.Ctty = int(os.Stdin.Fd())
		}
	} else {
		// A new session has no controlling terminal, so a node that opens
		// /dev/tty (a password prompt, a curses pager) gets an error instead
		// of drawing over the TUI or stopping on SIGTTIN in the background.
		// The session leader's process group ID is its PID, as with Setpgid.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		var err error
		output, err = newOutputPipe(spec.Output)
		if err != nil {
			return startFailure(err)
		}
		cmd.Stdout, cmd.Stderr = output.w, output.w
	}

	if err := cmd.Start(); err != nil {
		output.abort()
		return startFailure(err)
	}
	if foreground {
		defer reclaimForeground(os.Stdin)
	}
	output.started()

	p := &proc{
		pgid:   cmd.Process.Pid,
		waitCh: make(chan error, 1),
		exited: false,
		err:    nil,
		log:    spec.Log.With().Int("pgid", cmd.Process.Pid).Logger(),
	}
	p.log.Debug().Msg("process started")
	go func() { p.waitCh <- cmd.Wait() }()

	var timeout <-chan time.Time
	if spec.Timeout > 0 {
		t := time.NewTimer(spec.Timeout)
		defer t.Stop()
		timeout = t.C
	}

	reason := ReasonExit
	select {
	case err := <-p.waitCh:
		p.leaderExited(err)
		output.drain(pipeDrainDelay)
		return exitResult(p.err, reason)
	case <-ctx.Done():
		reason = ReasonCanceled
	case <-timeout:
		reason = ReasonTimeout
		p.log.Debug().Dur("timeout", spec.Timeout).Msg("node timed out")
	case <-spec.Kill:
		reason = ReasonCanceled
	}

	// The shell may have exited in the same instant as the cancel. If so,
	// report how it exited: a node that succeeded must not look canceled.
	select {
	case err := <-p.waitCh:
		p.leaderExited(err)
		output.drain(pipeDrainDelay)
		return exitResult(p.err, ReasonExit)
	default:
	}

	p.stop(spec)
	output.drain(pipeDrainDelay)
	return exitResult(p.err, reason)
}

// proc tracks a running node's process group.
type proc struct {
	pgid   int
	waitCh chan error
	exited bool
	err    error
	log    zerolog.Logger
}

func (p *proc) leaderExited(err error) {
	p.exited = true
	p.err = err
	p.waitCh = nil
}

// groupAlive reports whether any process is left in the group. Before the
// shell is reaped the group trivially exists, so this only asks the kernel
// afterwards. Once the group is empty its ID may be reused, so callers stop
// signaling it as soon as this returns false.
func (p *proc) groupAlive() bool {
	if !p.exited {
		return true
	}
	err := syscall.Kill(-p.pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// waitGroup waits until the group is empty, the grace period passes, or
// kill closes. It reports whether the group is gone and whether kill
// fired.
func (p *proc) waitGroup(grace time.Duration, kill <-chan struct{}) (gone, killed bool) {
	t := time.NewTimer(grace)
	defer t.Stop()
	tick := time.NewTicker(groupPollInterval)
	defer tick.Stop()
	for {
		if !p.groupAlive() {
			return true, false
		}
		select {
		case err := <-p.waitCh:
			p.leaderExited(err)
		case <-tick.C:
		case <-t.C:
			return false, false
		case <-kill:
			return false, true
		}
	}
}

// stop walks the SIGINT, SIGTERM, SIGKILL ladder until the group is empty.
func (p *proc) stop(spec Spec) {
	steps := []struct {
		sig   syscall.Signal
		grace time.Duration
	}{
		{syscall.SIGINT, spec.KillTimeout},
		{syscall.SIGTERM, cmp.Or(spec.TermGrace, TermGrace)},
	}
	kill := spec.Kill
	if isClosed(kill) {
		steps = nil
	}
	for _, step := range steps {
		p.signal(step.sig)
		gone, killed := p.waitGroup(step.grace, kill)
		if gone {
			return
		}
		if killed {
			break
		}
		p.log.Debug().Dur("grace", step.grace).Str("signal", step.sig.String()).Msg("grace period expired")
	}
	p.signal(syscall.SIGKILL)
	if !p.exited {
		p.leaderExited(<-p.waitCh)
	}
	if gone, _ := p.waitGroup(finalKillWait, nil); !gone {
		p.log.Warn().Msg("processes still in the group after SIGKILL")
	}
}

func (p *proc) signal(sig syscall.Signal) {
	if !p.groupAlive() {
		return
	}
	err := syscall.Kill(-p.pgid, sig)
	ev := p.log.Debug().Str("signal", sig.String())
	if err != nil {
		ev = ev.AnErr("signal_error", err)
	}
	ev.Msg("signal sent to process group")
}

func isClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// outputPipe carries a node's combined stdout and stderr. weft owns the
// pipe instead of handing os/exec a Writer so that Wait returns when the
// shell exits, and weft alone decides how long to keep reading: until the
// group is gone when stopping, or a short drain after a normal exit.
type outputPipe struct {
	r, w *os.File
	done chan struct{}
}

func newOutputPipe(dst io.Writer) (*outputPipe, error) {
	if dst == nil {
		dst = io.Discard
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	o := &outputPipe{r: r, w: w, done: make(chan struct{})}
	go func() {
		defer close(o.done)
		_, _ = io.Copy(dst, r)
	}()
	return o, nil
}

// started closes weft's copy of the write end; the child holds its own.
func (o *outputPipe) started() {
	if o != nil {
		_ = o.w.Close()
	}
}

func (o *outputPipe) abort() {
	if o != nil {
		_ = o.w.Close()
		<-o.done
		_ = o.r.Close()
	}
}

// drain waits for every writer to close the pipe, or for limit to pass,
// then stops reading.
func (o *outputPipe) drain(limit time.Duration) {
	if o == nil {
		return
	}
	t := time.NewTimer(limit)
	defer t.Stop()
	select {
	case <-o.done:
	case <-t.C:
	}
	_ = o.r.Close()
	<-o.done
}

func startFailure(err error) Result {
	return Result{ExitCode: -1, Signal: "", Reason: ReasonStartErr, Err: fmt.Errorf("start: %w", err)}
}

func exitResult(err error, reason Reason) Result {
	res := Result{ExitCode: 0, Signal: "", Reason: reason, Err: nil}
	if err == nil {
		return res
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		res.ExitCode = -1
		res.Err = err
		return res
	}
	res.ExitCode = exitErr.ExitCode()
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		res.Signal = ws.Signal().String()
	}
	return res
}
