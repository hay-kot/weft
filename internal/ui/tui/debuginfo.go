package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/hay-kot/weft/internal/ui"
)

// debugTailLines is how much of the selected node's output the debug info
// holds. The full log is on disk, and the report names its path.
const debugTailLines = 50

// errNoClipboard means no clipboard command is on the PATH, so the copy
// falls back to OSC 52 through the terminal.
var errNoClipboard = errors.New("no clipboard command found")

// copyDoneMsg reports how a copy of the debug info went.
type copyDoneMsg struct {
	node string
	text string
	err  error
}

// copyDebugInfo copies the run's debug info, centered on node, to the
// clipboard.
func (m Model) copyDebugInfo(node string) tea.Cmd {
	text := m.debugInfo(node)
	copyText := m.opts.CopyText
	if copyText == nil {
		copyText = systemCopy
	}
	return func() tea.Msg {
		return copyDoneMsg{node: node, text: text, err: copyText(text)}
	}
}

func (m Model) copyDone(msg copyDoneMsg) (Model, tea.Cmd) {
	what := "debug info"
	if msg.node != "" {
		what += " for " + m.displayName(msg.node)
	}
	switch {
	case errors.Is(msg.err, errNoClipboard):
		m, toast := m.showToast(noticeInfo, "sent "+what+" to the terminal clipboard (OSC 52)")
		return m, tea.Batch(tea.SetClipboard(msg.text), toast)
	case msg.err != nil:
		return m.showToast(noticeWarn, "copy "+what+": "+msg.err.Error())
	default:
		return m.showToast(noticeInfo, "copied "+what)
	}
}

// systemCopy writes text to the clipboard with the platform's clipboard
// command. OSC 52 is the fallback, not the default, because tmux and many
// terminals drop it unless configured to pass it through.
func systemCopy(text string) error {
	name, args := clipboardCommand()
	if name == "" {
		return errNoClipboard
	}
	cmd := exec.Command(name, args...) //nolint:gosec,noctx // name is one of the fixed clipboard commands above
	cmd.Stdin = strings.NewReader(text)
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%s: %s", name, msg)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func clipboardCommand() (string, []string) {
	candidates := [][]string{}
	switch {
	case runtime.GOOS == "darwin":
		candidates = append(candidates, []string{"pbcopy"})
	case os.Getenv("WAYLAND_DISPLAY") != "":
		candidates = append(candidates, []string{"wl-copy"})
	}
	if os.Getenv("DISPLAY") != "" {
		candidates = append(candidates,
			[]string{"xclip", "-selection", "clipboard"},
			[]string{"xsel", "--clipboard", "--input"})
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err == nil {
			return c[0], c[1:]
		}
	}
	return "", nil
}

// debugInfo is a plain-text report of the run and of node, meant to be
// pasted into a bug report or handed to an LLM: where the run's files
// are, how every node ended, and the tail of node's output.
func (m Model) debugInfo(node string) string {
	rs := m.rs
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	ph := runPhase(rs, m.cancelLevel)
	line("# weft run: %s", rs.wf.Name)
	line("")
	if rs.wf.Path != "" {
		line("workflow file: %s", rs.wf.Path)
	}
	line("run id: %s", rs.run.ID)
	line("run dir: %s", rs.run.Dir)
	line("weft log: %s", rs.run.LogPath())
	line("status: %s after %s (%s)", ph, ui.FormatDuration(rs.elapsed(m.now)), progress(rs))

	line("")
	line("## nodes")
	line("")
	nameWidth := 0
	for _, n := range rs.wf.Nodes {
		nameWidth = max(nameWidth, len(n.DisplayName()))
	}
	for _, n := range rs.wf.Nodes {
		st := rs.nodes[n.ID]
		if st == nil {
			continue
		}
		row := fmt.Sprintf("- %-*s  %s", nameWidth, n.DisplayName(), statusText(st))
		if d, ok := nodeDuration(st, m.now); ok {
			row += "  " + ui.FormatDuration(d)
		}
		if st.Attempt > 1 {
			row += fmt.Sprintf("  attempt %d", st.Attempt)
		}
		line("%s", row)
	}

	if n := rs.wf.Node(node); n != nil && rs.nodes[node] != nil {
		st := rs.nodes[node]
		line("")
		line("## selected node: %s", n.DisplayName())
		line("")
		if n.Label != "" {
			line("id: %s", n.ID)
		}
		line("state: %s", statusText(st))
		if st.State.Terminal() && st.Reason != "" {
			line("exit code: %s", strconv.Itoa(st.ExitCode))
			line("reason: %s", st.Reason)
		}
		line("attempt: %d", st.Attempt)
		if len(n.Needs) > 0 {
			line("needs: %s", strings.Join(n.Needs, ", "))
		}
		dir := n.Dir
		if dir == "" {
			dir = "."
		}
		line("dir: %s (from the workflow root)", dir)
		line("command:")
		line("```sh")
		line("%s", strings.TrimRight(n.Run, "\n"))
		line("```")
		if !st.Started.IsZero() && !n.Interactive {
			line("log: %s", rs.run.AttemptLogPath(node, st.Attempt))
		}
		line("")
		m.writeLogTail(&b, node)
	}
	return b.String()
}

func (m Model) writeLogTail(b *strings.Builder, node string) {
	buf := m.rs.logs[node]
	if buf == nil || buf.end() == buf.first() {
		fmt.Fprintf(b, "output: %s\n", placeholder(m.rs, node))
		return
	}
	start := max(buf.first(), buf.end()-debugTailLines)
	lines := buf.slice(start, buf.end())
	if start > 0 {
		fmt.Fprintf(b, "last %d lines of output:\n", len(lines))
	} else {
		fmt.Fprintf(b, "output:\n")
	}
	b.WriteString("```\n")
	for _, l := range lines {
		b.WriteString(ansi.Strip(l.Text))
		b.WriteByte('\n')
	}
	b.WriteString("```\n")
}
