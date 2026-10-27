package tui

import (
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"github.com/charmbracelet/x/ansi"
)

// noticeKind sets how a footer notice is styled.
type noticeKind int

const (
	noticeInfo noticeKind = iota
	noticeWarn
)

// footer shows an optional notice row above the key help. The full key
// list is the help dialog's job, so the footer has one help row.
type footer struct {
	help       help.Model
	notice     string
	noticeKind noticeKind
	width      int
}

func newFooter(s Styles) footer {
	h := help.New()
	h.Styles = s.Help
	return footer{help: h, notice: "", noticeKind: noticeInfo, width: 0}
}

func (f footer) withWidth(w int) footer {
	f.width = w
	// One column of margin on each side.
	f.help.SetWidth(max(0, w-2))
	return f
}

func (f footer) withNotice(kind noticeKind, text string) footer {
	f.notice, f.noticeKind = text, kind
	return f
}

// height returns how many rows View draws.
func (f footer) height() int {
	if f.notice != "" {
		return 2
	}
	return 1
}

// View draws the notice, if any, and the help.
func (f footer) View(short []key.Binding, s Styles) string {
	var rows []string
	if f.notice != "" {
		style := s.Dim
		if f.noticeKind == noticeWarn {
			style = s.Notice
		}
		rows = append(rows, ansi.Truncate(" "+style.Render(f.notice), f.width, "…"))
	}
	rows = append(rows, ansi.Truncate(" "+f.help.ShortHelpView(short), f.width, "…"))
	return strings.Join(rows, "\n")
}
