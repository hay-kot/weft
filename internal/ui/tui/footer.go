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

// footer shows an optional notice row above the key help.
type footer struct {
	help       help.Model
	showAll    bool
	notice     string
	noticeKind noticeKind
	width      int
	// maxRows caps the help; the full help falls back to the short one
	// when it does not fit.
	maxRows int
}

func newFooter(s Styles) footer {
	h := help.New()
	h.Styles = s.Help
	return footer{help: h, showAll: false, notice: "", noticeKind: noticeInfo, width: 0, maxRows: 0}
}

func (f footer) withWidth(w int) footer {
	f.width = w
	// One column of margin on each side.
	f.help.SetWidth(max(0, w-2))
	return f
}

func (f footer) withMaxRows(n int) footer {
	f.maxRows = n
	return f
}

func (f footer) withNotice(kind noticeKind, text string) footer {
	f.notice, f.noticeKind = text, kind
	return f
}

func (f footer) toggleHelp() footer {
	f.showAll = !f.showAll
	return f
}

// helpLines renders the key help for the given bindings.
func (f footer) helpLines(short []key.Binding, full [][]key.Binding) []string {
	if f.showAll && full != nil {
		lines := strings.Split(f.help.FullHelpView(full), "\n")
		if len(lines) < f.maxRows {
			return lines
		}
	}
	return []string{f.help.ShortHelpView(short)}
}

// height returns how many rows View draws.
func (f footer) height(short []key.Binding, full [][]key.Binding) int {
	n := len(f.helpLines(short, full))
	if f.notice != "" {
		n++
	}
	return n
}

// View draws the notice, if any, and the help.
func (f footer) View(short []key.Binding, full [][]key.Binding, s Styles) string {
	var rows []string
	if f.notice != "" {
		style := s.Dim
		if f.noticeKind == noticeWarn {
			style = s.Notice
		}
		rows = append(rows, ansi.Truncate(" "+style.Render(f.notice), f.width, "…"))
	}
	for _, l := range f.helpLines(short, full) {
		rows = append(rows, ansi.Truncate(" "+l, f.width, "…"))
	}
	return strings.Join(rows, "\n")
}
