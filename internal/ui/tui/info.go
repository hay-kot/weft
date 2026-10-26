package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/hay-kot/weft/internal/ui"
)

// infoLabelWidth aligns the values in the node info dialog.
const infoLabelWidth = 18

// nodeInfo is the dialog with everything weft knows about a node: what it
// runs and where, how it relates to the others, and how its latest attempt
// went.
func (m Model) nodeInfo(id string) *dialog {
	n := m.rs.wf.Node(id)
	st := m.rs.nodes[id]
	if n == nil || st == nil {
		return nil
	}
	var body []dialogLine
	add := func(label, value string) {
		if value == "" {
			return
		}
		pad := strings.Repeat(" ", max(1, infoLabelWidth-ansi.StringWidth(label)))
		body = append(body, dialogLine{text: m.styles.Dim.Render(label) + pad + value, wrap: true, rule: false, indent: infoLabelWidth})
	}
	section := func(title string) {
		if len(body) > 0 {
			body = append(body, dialogLine{text: "", wrap: false, rule: false, indent: 0})
		}
		body = append(body,
			dialogLine{text: m.styles.DialogSection.Render(title), wrap: false, rule: false, indent: 0},
			dialogLine{text: "", wrap: false, rule: true, indent: 0})
	}

	section("Node")
	if n.Label != "" {
		add("id", n.ID)
	}
	state := m.styles.State(st.State).Render(string(st.State))
	if st.Attempt > 1 {
		state += fmt.Sprintf(" (attempt %d)", st.Attempt)
	}
	add("state", state)
	add("needs", strings.Join(n.Needs, ", "))
	add("needed by", strings.Join(m.rs.wf.Dependents(id), ", "))

	section("Command")
	for i, line := range strings.Split(strings.TrimRight(n.Run, "\n"), "\n") {
		label := ""
		if i == 0 {
			label = "run"
		}
		pad := strings.Repeat(" ", infoLabelWidth-ansi.StringWidth(label))
		body = append(body, dialogLine{text: m.styles.Dim.Render(label) + pad + line, wrap: true, rule: false, indent: infoLabelWidth})
	}
	dir := n.Dir
	if dir == "" {
		dir = "."
	}
	add("dir", dir+" (from the workflow root)")
	add("shell", strings.Join(n.Shell, " "))
	if n.Interactive {
		add("interactive", "yes: runs alone, attached to the terminal")
	}
	if n.Timeout > 0 {
		add("timeout", n.Timeout.String())
	}
	add("kill_timeout", n.KillTimeout.String())
	if n.ContinueOnError {
		add("continue_on_error", "yes: a failure does not block downstream nodes")
	}

	if st.Attempt > 0 && !st.Started.IsZero() {
		section("Latest attempt")
		add("started", st.Started.Format("15:04:05"))
		if d, ok := nodeDuration(st, m.now); ok {
			add("duration", ui.FormatDuration(d))
		}
		if st.State.Terminal() && st.Reason != "" {
			add("exit code", strconv.Itoa(st.ExitCode))
			add("reason", string(st.Reason))
		}
		add("error", st.Err)
		if !n.Interactive {
			add("log", m.rs.run.AttemptLogPath(id, st.Attempt))
		}
	} else if st.Err != "" {
		section("Latest attempt")
		add("error", st.Err)
	}
	return newInfoDialog(n.DisplayName(), body, m.keys.infoClose(), m.styles)
}
