package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
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

func (p phase) style(s Styles) lipgloss.Style {
	switch p {
	case phaseSucceeded:
		return s.succeeded
	case phaseFailed, phaseKilling:
		return s.failed
	case phaseCanceled, phaseCanceling:
		return s.stopped
	case phaseRunning:
		return s.running
	case phaseStarting:
		return s.idle
	default:
		return s.idle
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

// header is the row at the top with the run's status. The run directory
// is in the debug info that y copies, not on screen.
type header struct {
	width int
}

func (h header) withWidth(w int) header {
	h.width = w
	return h
}

// View draws exactly one row. The run ID is the first thing to go when
// the row is short.
func (h header) View(rs *runState, ph phase, now time.Time, s Styles) string {
	left := " " + strings.Join([]string{
		s.Title.Render(rs.wf.Name),
		ph.style(s).Render(ph.String()),
		ui.FormatDuration(rs.elapsed(now)),
		s.Dim.Render(progress(rs)),
	}, "  ")
	right := s.Dim.Render("run "+rs.run.ID) + " "
	if ansi.StringWidth(left)+ansi.StringWidth(right)+1 <= h.width {
		return fit(left, right, h.width)
	}
	return ansi.Truncate(left, h.width, "…")
}
