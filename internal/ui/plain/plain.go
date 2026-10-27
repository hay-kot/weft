// Package plain renders the engine's event stream as prefixed lines for
// output that is not a terminal, such as CI logs and pipes.
package plain

import (
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/ui"
	"github.com/hay-kot/weft/internal/workflow"
)

const (
	sgrReset = "\033[0m"
	sgrDim   = "\033[2m"
	sgrRed   = "\033[31m"
	sgrGreen = "\033[32m"
	sgrBlue  = "\033[34m"
	sgrMag   = "\033[35m"
	sgrYel   = "\033[33m"
)

// Renderer writes one line per output line and per notable state change.
type Renderer struct {
	out      *stickyWriter
	width    int
	color    bool
	names    map[string]string
	rendered atomic.Int64
}

// stickyWriter stops writing after the first error. Once stdout is a
// closed pipe (`weft run | head` after head exits), every later write would
// fail the same way, and the run itself must carry on regardless.
type stickyWriter struct {
	w   io.Writer
	err error
}

func (s *stickyWriter) Write(p []byte) (int, error) {
	if s.err == nil {
		_, s.err = s.w.Write(p)
	}
	return len(p), nil
}

// New returns a renderer for wf. color enables SGR styling of weft's own
// status lines; node output passes through unchanged either way.
func New(out io.Writer, wf *workflow.Workflow, color bool) *Renderer {
	names := make(map[string]string, len(wf.Nodes))
	width := 0
	for _, n := range wf.Nodes {
		names[n.ID] = n.DisplayName()
		width = max(width, len(n.DisplayName()))
	}
	return &Renderer{
		out:      &stickyWriter{w: out, err: nil},
		width:    width,
		color:    color,
		names:    names,
		rendered: atomic.Int64{},
	}
}

// Run renders events until ch closes.
func (r *Renderer) Run(ch <-chan events.Event) {
	for e := range ch {
		r.Render(e)
		r.rendered.Add(1)
	}
}

// Rendered returns how many events Run has finished rendering. It lets a
// caller tell a renderer that is slowly catching up from one stuck on a
// paused pipe.
func (r *Renderer) Rendered() int64 {
	return r.rendered.Load()
}

// Render writes the lines for one event.
func (r *Renderer) Render(e events.Event) {
	switch e := e.(type) {
	case events.NodeOutput:
		r.line(e.Node, "│", "", lastSegment(e.Line))
	case events.NodeChanged:
		r.state(e)
	case events.OutputDropped:
		r.line(e.Node, "…", sgrYel, fmt.Sprintf("%d output lines not shown: stdout fell behind (the node log has them)", e.Lines))
	case events.Barrier:
		e.Done()
	case events.RunStarted, events.RunFinished:
	}
}

func (r *Renderer) state(e events.NodeChanged) {
	dur := ""
	if !e.Started.IsZero() && !e.Finished.IsZero() && e.Reason != events.ReasonStartError {
		dur = " in " + ui.FormatDuration(e.Finished.Sub(e.Started))
	}
	attempt := ""
	if e.Attempt > 1 {
		attempt = fmt.Sprintf(" (attempt %d)", e.Attempt)
	}
	switch e.State {
	case events.Running:
		r.line(e.Node, "▶", sgrBlue, "started"+attempt)
	case events.Waiting:
		r.line(e.Node, "◆", sgrMag, "waiting for the terminal")
	case events.Succeeded:
		r.line(e.Node, "✓", sgrGreen, "succeeded"+dur)
	case events.Failed:
		r.line(e.Node, "✗", sgrRed, "failed: "+e.Err+dur)
	case events.Blocked:
		r.line(e.Node, "⊘", sgrYel, e.Err)
	case events.Canceled:
		r.line(e.Node, "⊘", sgrYel, e.Err+dur)
	case events.Skipped:
		why := "succeeded in an earlier attempt"
		if e.Err != "" {
			why = e.Err
		}
		r.line(e.Node, "✓", sgrDim, "skipped: "+why)
	case events.Excluded:
		r.line(e.Node, "·", sgrDim, "excluded: not selected for this run")
	case events.Pending, events.Ready:
	}
}

func (r *Renderer) line(node, mark, sgr, text string) {
	name := r.names[node]
	if name == "" {
		name = node
	}
	prefix := name + strings.Repeat(" ", r.width-len(name))
	if r.color && sgr != "" {
		_, _ = fmt.Fprintf(r.out, "%s%s %s%s %s%s\n", sgrDim, prefix, sgrReset+sgr, mark, text, sgrReset)
		return
	}
	if r.color {
		_, _ = fmt.Fprintf(r.out, "%s%s %s%s %s\n", sgrDim, prefix, mark, sgrReset, text)
		return
	}
	_, _ = fmt.Fprintf(r.out, "%s %s %s\n", prefix, mark, text)
}

// lastSegment keeps what a terminal would show after carriage returns
// redraw the line, so progress bars print once instead of as a smear. A
// trailing \r, as on a final progress frame without a newline, redraws
// nothing, so it does not blank the line.
func lastSegment(line string) string {
	line = strings.TrimRight(line, "\r")
	if _, after, found := strings.CutLast(line, "\r"); found {
		return after
	}
	return line
}
