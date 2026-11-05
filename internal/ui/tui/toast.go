package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// toastDuration is how long a toast stays up.
const toastDuration = 3 * time.Second

// toast is a short message drawn over the bottom-right corner of the
// screen. Unlike a footer notice it takes no rows, so the layout does not
// jump when it comes and goes.
type toast struct {
	text string
	kind noticeKind
	// id tells a toast's own expiry apart from that of one it replaced.
	id int
}

// toastExpiredMsg clears the toast with the same id.
type toastExpiredMsg struct{ id int }

// showToast puts text up as a toast and schedules its removal.
func (m Model) showToast(kind noticeKind, text string) (Model, tea.Cmd) {
	id := m.toast.id + 1
	m.toast = toast{text: text, kind: kind, id: id}
	return m, tea.Tick(toastDuration, func(time.Time) tea.Msg { return toastExpiredMsg{id: id} })
}

func (m Model) toastExpired(msg toastExpiredMsg) Model {
	if msg.id == m.toast.id {
		m.toast.text = ""
	}
	return m
}

// overlayToast draws the toast, if any, just above the footer's rule.
func (m Model) overlayToast(screen string) string {
	if m.toast.text == "" {
		return screen
	}
	style := m.styles.Toast
	if m.toast.kind == noticeWarn {
		style = m.styles.ToastWarn
	}
	// Border and padding take four columns; keep a column of margin too.
	room := m.width - 5
	if room < 8 {
		return screen
	}
	box := style.Render(ansi.Truncate(m.toast.text, room, "…"))
	bw, bh := lipgloss.Width(box), lipgloss.Height(box)
	y := m.height - m.footer.height() - 1 - bh
	if y < 0 {
		return screen
	}
	layer := lipgloss.NewLayer(box).X(max(0, m.width-bw-1)).Y(y).Z(1)
	return lipgloss.NewCompositor(lipgloss.NewLayer(screen), layer).Render()
}
