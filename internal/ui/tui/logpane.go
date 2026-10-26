package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/ui"
)

// hScrollStep is how far one sideways scroll moves the full-screen log.
const hScrollStep = 8

// logPane shows the tail of the selected node's output under the graph.
type logPane struct {
	node   string
	scroll scroller
	width  int
	// height is the number of output rows, not counting the title.
	height int
}

func newLogPane(node string) logPane {
	return logPane{node: node, scroll: newScroller(), width: 0, height: 0}
}

// show switches the pane to node. A different node starts at its tail.
func (p logPane) show(node string) logPane {
	if node != p.node {
		p.node = node
		p.scroll = newScroller()
	}
	return p
}

func (p logPane) withSize(width, height int) logPane {
	p.width, p.height = max(0, width), max(0, height)
	return p
}

// Update handles the pane's scroll keys. It reports whether the key was
// one of them.
func (p logPane) Update(msg tea.Msg, keys KeyMap, buf *logBuffer) (logPane, bool) {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok || buf == nil {
		return p, false
	}
	first, end, half := buf.first(), buf.end(), max(1, p.height/2)
	switch {
	case key.Matches(k, keys.PageUp):
		p.scroll = p.scroll.scroll(-half, first, end, p.height)
	case key.Matches(k, keys.PageDown):
		p.scroll = p.scroll.scroll(half, first, end, p.height)
	case key.Matches(k, keys.Top):
		p.scroll = p.scroll.toTop(first, end, p.height)
	case key.Matches(k, keys.Bottom):
		p.scroll = p.scroll.toBottom()
	case key.Matches(k, keys.Follow):
		p.scroll = p.scroll.toggleFollow(first, end, p.height)
	default:
		return p, false
	}
	return p, true
}

// View draws the title row and exactly height output rows.
func (p logPane) View(rs *runState, s Styles, now time.Time) string {
	lines := []string{logTitle(rs, p.node, now, s, p.width, p.scroll.follow, false)}
	lines = append(lines, logBody(rs, p.node, p.scroll, 0, p.width, p.height, s)...)
	return strings.Join(lines, "\n")
}

// logView is the full-screen log of one node.
type logView struct {
	node   string
	scroll scroller
	// left is the first column shown, for lines wider than the screen.
	left  int
	width int
	// height is the number of output rows.
	height int
}

func newLogView(node string) logView {
	return logView{node: node, scroll: newScroller(), left: 0, width: 0, height: 0}
}

func (v logView) withSize(width, height int) logView {
	v.width, v.height = max(0, width), max(0, height)
	return v
}

// Update handles scrolling. It reports whether the key was handled; Close
// is left to the caller.
func (v logView) Update(msg tea.Msg, keys KeyMap, buf *logBuffer) (logView, bool) {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok || buf == nil {
		return v, false
	}
	first, end, page := buf.first(), buf.end(), max(1, v.height-1)
	switch {
	case key.Matches(k, keys.Up):
		v.scroll = v.scroll.scroll(-1, first, end, v.height)
	case key.Matches(k, keys.Down):
		v.scroll = v.scroll.scroll(1, first, end, v.height)
	case key.Matches(k, keys.PageUp):
		v.scroll = v.scroll.scroll(-page, first, end, v.height)
	case key.Matches(k, keys.PageDown):
		v.scroll = v.scroll.scroll(page, first, end, v.height)
	case key.Matches(k, keys.Top):
		v.scroll = v.scroll.toTop(first, end, v.height)
	case key.Matches(k, keys.Bottom):
		v.scroll = v.scroll.toBottom()
	case key.Matches(k, keys.Follow):
		v.scroll = v.scroll.toggleFollow(first, end, v.height)
	case key.Matches(k, keys.ScrollL):
		v.left = max(0, v.left-hScrollStep)
	case key.Matches(k, keys.ScrollR):
		v.left = min(v.left+hScrollStep, v.maxLeft(buf))
	default:
		return v, false
	}
	return v, true
}

// maxLeft is the furthest sideways scroll that still shows the end of the
// widest line in view.
func (v logView) maxLeft(buf *logBuffer) int {
	start, stop := v.scroll.window(buf.first(), buf.end(), v.height)
	widest := 0
	for _, l := range buf.slice(start, stop) {
		widest = max(widest, ansi.StringWidth(l.Text))
	}
	return max(0, widest-v.width)
}

// View draws the title row and exactly height output rows.
func (v logView) View(rs *runState, s Styles, now time.Time) string {
	lines := []string{logTitle(rs, v.node, now, s, v.width, v.scroll.follow, true)}
	lines = append(lines, logBody(rs, v.node, v.scroll, v.left, v.width, v.height, s)...)
	return strings.Join(lines, "\n")
}

// logTitle renders "› label  icon status  duration  command", with the
// follow state on the right. The full-screen view adds the attempt's log
// path, which is where the complete output is.
func logTitle(rs *runState, id string, now time.Time, s Styles, width int, follow, full bool) string {
	n := rs.wf.Node(id)
	st := rs.nodes[id]
	if n == nil || st == nil {
		return ""
	}
	parts := []string{
		s.Title.Render("› " + n.DisplayName()),
		s.State(st.State).Render(icon(st.State) + " " + statusText(st)),
	}
	if d, ok := nodeDuration(st, now); ok {
		parts = append(parts, ui.FormatDuration(d))
	}
	if st.Attempt > 1 {
		parts = append(parts, fmt.Sprintf("attempt %d", st.Attempt))
	}
	if full && !st.Started.IsZero() {
		parts = append(parts, s.Dim.Render("log "+rs.run.AttemptLogPath(id, st.Attempt)))
	} else if cmd := firstLine(n.Run); cmd != "" {
		parts = append(parts, s.Dim.Render(cmd))
	}
	left := " " + strings.Join(parts, "  ")

	right := ""
	if !follow {
		right = s.Notice.Render("paused · f to follow") + " "
	}
	return fit(left, right, width)
}

// statusText describes a node's state for the log title.
func statusText(st *nodeStatus) string {
	switch st.State {
	case events.Failed:
		if st.Err != "" {
			return "failed: " + st.Err
		}
		return "failed"
	case events.Blocked, events.Canceled:
		if st.Err != "" {
			return st.Err
		}
		return string(st.State)
	case events.Waiting:
		return "waiting for the terminal"
	case events.Pending, events.Ready, events.Running, events.Succeeded, events.Skipped, events.Excluded:
		return string(st.State)
	default:
		return string(st.State)
	}
}

// placeholder explains an empty log.
func placeholder(rs *runState, id string) string {
	n := rs.wf.Node(id)
	st := rs.nodes[id]
	if n == nil || st == nil {
		return ""
	}
	switch st.State {
	case events.Pending, events.Ready:
		var waiting []string
		for _, need := range n.Needs {
			if s := rs.state(need); s != events.Succeeded && s != events.Skipped {
				waiting = append(waiting, need)
			}
		}
		if len(waiting) > 0 {
			return "waiting on " + strings.Join(waiting, ", ")
		}
		return "waiting for a free slot"
	case events.Waiting:
		return "waiting for running nodes to finish, then it takes the terminal"
	case events.Running:
		if n.Interactive {
			return "has the terminal; its output goes there, not to this log"
		}
		return "no output yet"
	case events.Blocked, events.Canceled:
		if st.Started.IsZero() {
			return "did not run"
		}
		return "no output"
	case events.Skipped:
		if st.Err != "" {
			return fmt.Sprintf("skipped: %s; the output is in %s", st.Err, rs.run.AttemptLogPath(id, st.Attempt))
		}
		if st.Attempt == 0 {
			return "skipped: it succeeded in an earlier attempt"
		}
		return fmt.Sprintf("skipped: it succeeded in attempt %d, whose output is in %s", st.Attempt, rs.run.AttemptLogPath(id, st.Attempt))
	case events.Excluded:
		return "excluded: not selected for this run, so it does not run"
	case events.Succeeded, events.Failed:
		if n.Interactive {
			return "interactive: its output went to the terminal and was not captured"
		}
		return "no output"
	default:
		return ""
	}
}

// logBody renders exactly height rows of a node's log, starting at column
// left. Every row is cut to width and ends with an SGR reset, so a color a
// node never turned off can't bleed into the rest of the screen.
func logBody(rs *runState, id string, sc scroller, left, width, height int, s Styles) []string {
	rows := make([]string, 0, height)
	buf := rs.logs[id]
	if buf != nil && buf.end() > buf.first() {
		start, stop := sc.window(buf.first(), buf.end(), height)
		for _, l := range buf.slice(start, stop) {
			rows = append(rows, renderLogLine(l, left, width, s))
		}
	} else if height > 0 {
		rows = append(rows, ansi.Truncate(" "+s.Dim.Render(placeholder(rs, id)), width, "…"))
	}
	for len(rows) < height {
		rows = append(rows, "")
	}
	return rows
}

func renderLogLine(l logLine, left, width int, s Styles) string {
	room := width - 1
	if room <= 0 {
		return ""
	}
	text := l.Text
	if left > 0 {
		text = ansi.TruncateLeft(text, left, "")
	}
	if ansi.StringWidth(text) > room {
		text = ansi.Truncate(text, room, "…")
	}
	if l.Notice {
		return " " + s.Notice.Render(text)
	}
	if strings.ContainsRune(text, '\x1b') {
		text += ansi.ResetStyle
	}
	return " " + text
}

// fit lays out left and right on one row of width cells, truncating left
// to make room and dropping right when even that is not enough.
func fit(left, right string, width int) string {
	lw, rw := ansi.StringWidth(left), ansi.StringWidth(right)
	switch {
	case right == "":
		return ansi.Truncate(left, width, "…")
	case lw+rw+1 <= width:
		return left + strings.Repeat(" ", width-lw-rw) + right
	case rw+12 <= width:
		return ansi.Truncate(left, width-rw-1, "…") + " " + right
	default:
		return ansi.Truncate(left, width, "…")
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}
