package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/ui"
)

// phase is the run's overall status as the header shows it.
type phase int

const (
	phaseStarting phase = iota
	phaseRunning
	phaseCanceling
	phaseKilling
	phaseSucceeded
	phaseFailed
	phaseCanceled
)

func (p phase) String() string {
	switch p {
	case phaseStarting:
		return "starting"
	case phaseRunning:
		return "running"
	case phaseCanceling:
		return "canceling"
	case phaseKilling:
		return "killing"
	case phaseSucceeded:
		return "succeeded"
	case phaseFailed:
		return "failed"
	case phaseCanceled:
		return "canceled"
	default:
		return "unknown"
	}
}

// runPhase derives the phase from the run state and how many times the
// user asked to cancel.
func runPhase(rs *runState, cancelLevel int) phase {
	switch {
	case rs.done && rs.success:
		return phaseSucceeded
	case rs.done && rs.counts()[events.Canceled] > 0:
		return phaseCanceled
	case rs.done:
		return phaseFailed
	case cancelLevel >= 2:
		return phaseKilling
	case cancelLevel == 1:
		return phaseCanceling
	case rs.started.IsZero():
		return phaseStarting
	default:
		return phaseRunning
	}
}

func (p phase) style(s Styles) func(...string) string {
	switch p {
	case phaseSucceeded:
		return s.succeeded.Render
	case phaseFailed, phaseKilling:
		return s.failed.Render
	case phaseCanceled, phaseCanceling:
		return s.stopped.Render
	case phaseRunning:
		return s.running.Render
	case phaseStarting:
		return s.idle.Render
	default:
		return s.idle.Render
	}
}

// summaryOrder lists states in the order the finished header counts them.
var summaryOrder = []events.State{events.Succeeded, events.Skipped, events.Failed, events.Blocked, events.Canceled, events.Excluded}

// progress returns "3/7 done" while running and the per-state counts once
// the run is over.
func progress(rs *runState) string {
	if !rs.done {
		return fmt.Sprintf("%d/%d done", rs.finishedCount(), len(rs.wf.Nodes))
	}
	counts := rs.counts()
	var parts []string
	for _, st := range summaryOrder {
		if n := counts[st]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, st))
		}
	}
	return strings.Join(parts, ", ")
}

// header is the two rows at the top: the run's status, and the run
// directory where its logs are.
type header struct {
	width int
}

func (h header) withWidth(w int) header {
	h.width = w
	return h
}

// View draws exactly two rows.
func (h header) View(rs *runState, ph phase, now time.Time, s Styles) string {
	left := " " + strings.Join([]string{
		s.Title.Render(rs.wf.Name),
		ph.style(s)(ph.String()),
		ui.FormatDuration(rs.elapsed(now)),
		s.Dim.Render(progress(rs)),
	}, "  ")
	// The run ID is also the last part of the run dir below, so it is the
	// first thing to go when the row is short.
	right := s.Dim.Render("run "+rs.run.ID) + " "
	top := ansi.Truncate(left, h.width, "…")
	if ansi.StringWidth(left)+ansi.StringWidth(right)+1 <= h.width {
		top = fit(left, right, h.width)
	}

	// A long run dir loses its start, never its label or the run ID.
	const label = " run dir "
	dir := rs.run.Dir
	if room := h.width - len(label); ansi.StringWidth(dir) > room && room > 1 {
		dir = ansi.TruncateLeft(dir, ansi.StringWidth(dir)-room+1, "…")
	}
	bottom := ansi.Truncate(s.Dim.Render(label+dir), h.width, "")
	return top + "\n" + bottom
}
