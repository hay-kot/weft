package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Dialog sizing. A dialog is as wide as its content needs, at least
// dialogMinWidth, and always leaves a margin so the screen behind it shows
// it is an overlay. Text that wraps (a question, a long command) wraps at
// dialogTextWidth.
const (
	dialogMinWidth  = 46
	dialogTextWidth = 64
	dialogMaxHeight = 40
	dialogMargin    = 4
	// dialogKeyWidth aligns the descriptions in the help.
	dialogKeyWidth = 13
)

// dialogLine is one row of a dialog's body. A wrapped line flows onto as
// many rows as the dialog's width needs, with indent columns before every
// row after the first; any other line is cut to fit. A rule is a divider
// as wide as the dialog.
type dialogLine struct {
	text   string
	wrap   bool
	rule   bool
	indent int
}

// dialog is a box drawn over the screen: the key help, a yes/no question,
// or a node's details. The body scrolls when it is taller than the screen
// allows.
type dialog struct {
	title string
	body  []dialogLine
	// offset is the first body row shown.
	offset int
	// question dialogs have Yes and No buttons; yes is the selected one.
	question bool
	yes      bool
	yesLabel string
	noLabel  string
	// danger draws the border in the error color, for questions whose yes
	// stops work.
	danger bool
	// closeKeys close a dialog that is not a question.
	closeKeys key.Binding
	styles    Styles
	// want is the box width the content asks for, before the screen caps
	// it.
	want int
}

func newInfoDialog(title string, body []dialogLine, closeKeys key.Binding, s Styles) *dialog {
	d := &dialog{
		title: title, body: body, offset: 0,
		question: false, yes: false, yesLabel: "", noLabel: "", danger: false,
		closeKeys: closeKeys, styles: s, want: 0,
	}
	d.want = d.contentWidth()
	return d
}

// contentWidth is the box width that shows every unwrapped line and the
// title whole.
func (d *dialog) contentWidth() int {
	widest := ansi.StringWidth(d.title) + len(" (100%)")
	for _, l := range d.body {
		switch {
		case l.wrap:
			widest = max(widest, min(ansi.StringWidth(l.text), dialogTextWidth))
		case !l.rule:
			widest = max(widest, ansi.StringWidth(l.text))
		}
	}
	// border and padding
	return widest + 2 + 2*2
}

// newQuestion returns a yes/no dialog. No is selected at first, so an
// enter pressed out of habit does not cancel a deploy.
func newQuestion(title, text, yesLabel, noLabel string, danger bool, s Styles) *dialog {
	return &dialog{
		title: title, body: []dialogLine{{text: text, wrap: true, rule: false, indent: 0}}, offset: 0,
		question: true, yes: false, yesLabel: yesLabel, noLabel: noLabel, danger: danger,
		closeKeys: key.NewBinding(), styles: s, want: dialogTextWidth,
	}
}

// helpDialog lists the keys of the current view in titled sections.
func helpDialog(sections []helpSection, closeKeys key.Binding, s Styles) *dialog {
	var body []dialogLine
	for i, sec := range sections {
		if i > 0 {
			body = append(body, dialogLine{text: "", wrap: false, rule: false, indent: 0})
		}
		body = append(body,
			dialogLine{text: s.DialogSection.Render(sec.title), wrap: false, rule: false, indent: 0},
			dialogLine{text: "", wrap: false, rule: true, indent: 0})
		for _, e := range sec.entries {
			if !e.binding.Enabled() {
				continue
			}
			k := e.binding.Help().Key
			pad := strings.Repeat(" ", max(1, dialogKeyWidth-ansi.StringWidth(k)))
			body = append(body, dialogLine{text: s.DialogKey.Render(k) + pad + e.desc, wrap: false, rule: false, indent: 0})
		}
	}
	return newInfoDialog("Keyboard shortcuts", body, closeKeys, s)
}

// dialogResult is what a key did to a dialog.
type dialogResult int

const (
	// dialogKept: the dialog stays open.
	dialogKept dialogResult = iota
	// dialogClosed: an info dialog was closed.
	dialogClosed
	// dialogYes and dialogNo answer a question.
	dialogYes
	dialogNo
)

// update applies a key. width and height are the screen's, which decide
// how far the body can scroll.
func (d *dialog) update(k tea.KeyPressMsg, keys KeyMap, width, height int) dialogResult {
	if d.question {
		switch {
		case key.Matches(k, keys.Confirm):
			return dialogYes
		case key.Matches(k, keys.Decline):
			return dialogNo
		case key.Matches(k, keys.Choose):
			if d.yes {
				return dialogYes
			}
			return dialogNo
		case key.Matches(k, keys.Left), key.Matches(k, keys.Right), key.Matches(k, keys.NextButton):
			d.yes = !d.yes
			return dialogKept
		}
		// Other keys scroll: a question on a short screen must still be
		// readable to the end.
	} else if key.Matches(k, d.closeKeys) {
		return dialogClosed
	}
	rows := d.bodyRows(width)
	visible := d.layout(width, height).bodyHeight
	page := max(1, visible-1)
	switch {
	case key.Matches(k, keys.Up):
		d.offset--
	case key.Matches(k, keys.Down):
		d.offset++
	case key.Matches(k, keys.PageUp):
		d.offset -= page
	case key.Matches(k, keys.PageDown):
		d.offset += page
	case key.Matches(k, keys.Top):
		d.offset = 0
	case key.Matches(k, keys.Bottom):
		d.offset = len(rows)
	}
	d.offset = clamp(d.offset, 0, max(0, len(rows)-visible))
	return dialogKept
}

// dialogLayout is the dialog's size on a given screen.
type dialogLayout struct {
	// width is the whole box, border included; inner is its text width.
	width, inner int
	// padX and padY are the padding inside the border. A narrow or short
	// screen keeps the rows for content instead.
	padX, padY int
	bodyHeight int
}

func (d *dialog) boxWidth(screenWidth int) int {
	return min(max(d.want, dialogMinWidth), screenWidth-dialogMargin)
}

func (d *dialog) padX(screenWidth int) int {
	if d.boxWidth(screenWidth) >= 40 {
		return 2
	}
	return 1
}

// innerWidth is the text width inside the border and padding.
func (d *dialog) innerWidth(screenWidth int) int {
	return max(1, d.boxWidth(screenWidth)-2-2*d.padX(screenWidth))
}

func (d *dialog) layout(width, height int) dialogLayout {
	padY := 0
	if height >= 20 {
		padY = 1
	}
	// border, padding, title, divider, a blank row and the hint row
	chrome := 2 + 2*padY + 1 + 1 + 2
	if d.question {
		// the buttons and the blank row above them
		chrome += 2
	}
	rows := len(d.bodyRows(width))
	bodyHeight := max(1, min(rows, min(height-2, dialogMaxHeight)-chrome))
	return dialogLayout{
		width: d.boxWidth(width), inner: d.innerWidth(width),
		padX: d.padX(width), padY: padY, bodyHeight: bodyHeight,
	}
}

// bodyRows renders the body at the width the dialog has on this screen.
func (d *dialog) bodyRows(width int) []string {
	inner := d.innerWidth(width)
	var rows []string
	for _, l := range d.body {
		switch {
		case l.rule:
			rows = append(rows, d.styles.Rule.Render(strings.Repeat("─", inner)))
		case l.wrap:
			rows = append(rows, hangingWrap(l.text, inner, l.indent)...)
		default:
			rows = append(rows, ansi.Truncate(l.text, inner, "…"))
		}
	}
	return rows
}

func (d *dialog) hints(scrolls bool) string {
	s := d.styles
	var entries [][2]string
	switch {
	case d.question:
		entries = [][2]string{{"y", "yes"}, {"n/esc", "no"}, {"←/→", "select"}, {"enter", "choose"}}
	case scrolls:
		entries = [][2]string{{"j/k", "scroll"}, {d.closeKeys.Help().Key, "close"}}
	default:
		entries = [][2]string{{d.closeKeys.Help().Key, "close"}}
	}
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, s.Help.ShortKey.Render(e[0])+" "+s.Help.ShortDesc.Render(e[1]))
	}
	return strings.Join(parts, s.Help.ShortSeparator.Render(" • "))
}

// render draws the box, not yet placed on the screen.
func (d *dialog) render(width, height int) string {
	s := d.styles
	l := d.layout(width, height)
	rows := d.bodyRows(width)
	offset := clamp(d.offset, 0, max(0, len(rows)-l.bodyHeight))
	shown := rows[offset:min(len(rows), offset+l.bodyHeight)]
	scrolls := len(rows) > l.bodyHeight

	title := d.title
	if scrolls {
		pct := 100 * (offset + l.bodyHeight) / len(rows)
		title += s.Dim.Render(fmt.Sprintf(" (%d%%)", pct))
	}
	parts := []string{
		ansi.Truncate(s.DialogTitle.Render(title), l.inner, "…"),
		s.Rule.Render(strings.Repeat("─", l.inner)),
	}
	parts = append(parts, shown...)
	if d.question {
		yes, no := s.Button, s.ButtonSelected
		if d.yes {
			yes, no = s.ButtonSelected, s.Button
		}
		parts = append(parts, "", yes.Render(d.yesLabel)+"  "+no.Render(d.noLabel))
	}
	parts = append(parts, "", ansi.Truncate(d.hints(scrolls), l.inner, "…"))

	box := s.Dialog
	if d.danger {
		box = s.DialogDanger
	}
	return box.Padding(l.padY, l.padX).Width(l.width).Render(strings.Join(parts, "\n"))
}

// overlay centers the dialog over the rendered screen. The compositor keeps
// the screen's size, so the frame still has exactly one row per terminal
// row.
func (d *dialog) overlay(screen string, width, height int) string {
	box := d.render(width, height)
	bw, bh := lipgloss.Width(box), lipgloss.Height(box)
	layer := lipgloss.NewLayer(box).X(max(0, (width-bw)/2)).Y(max(0, (height-bh)/2)).Z(1)
	return lipgloss.NewCompositor(lipgloss.NewLayer(screen), layer).Render()
}

// hangingWrap wraps text to width, indenting every row after the first so
// a long value lines up under where it started. Every row fits in width:
// ansi.Wrap keeps the space it broke at, and a row wider than the box would
// wrap again when the box is rendered, making the dialog taller than the
// layout counted on.
func hangingWrap(text string, width, indent int) []string {
	if indent >= width {
		indent = 0
	}
	first := strings.Split(ansi.Wrap(text, width, ""), "\n")
	if indent == 0 || len(first) == 1 {
		return clampRows(first, width)
	}
	rows := []string{first[0]}
	rest := strings.TrimLeft(ansi.Cut(text, ansi.StringWidth(first[0]), ansi.StringWidth(text)), " ")
	pad := strings.Repeat(" ", indent)
	for r := range strings.SplitSeq(ansi.Wrap(rest, width-indent, ""), "\n") {
		rows = append(rows, pad+r)
	}
	return clampRows(rows, width)
}

func clampRows(rows []string, width int) []string {
	for i, r := range rows {
		if ansi.StringWidth(r) > width {
			rows[i] = ansi.Truncate(r, width, "")
		}
	}
	return rows
}

func clamp(v, lo, hi int) int {
	return max(lo, min(v, hi))
}
