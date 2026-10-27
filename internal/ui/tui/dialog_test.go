package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/events"
)

func TestHangingWrapRowsFitTheWidth(t *testing.T) {
	m, _ := newModel(t, 80, 24)
	for _, text := range []string{
		strings.Repeat("word ", 30),
		m.styles.Dim.Render("") + strings.Repeat(" ", 18) + strings.Repeat("x", 40),
		m.styles.Dim.Render("shell") + "             bash -eo pipefail -c and more words here",
	} {
		rows := hangingWrap(text, 22, 18)
		require.NotEmpty(t, rows)
		for _, r := range rows {
			assert.LessOrEqual(t, ansi.StringWidth(r), 22, "%q", r)
		}
	}
}

func TestQuestionDialogScrolls(t *testing.T) {
	m, _ := newModel(t, 30, 10)
	d := newQuestion("Quit while the run is going?", strings.Repeat("Running nodes get SIGINT and their grace period. ", 6), "Yes", "No", true, m.styles)
	assert.Equal(t, dialogKept, d.update(press("j"), m.keys, 30, 10))
	assert.Equal(t, 1, d.offset, "j scrolls a question's body")
	assert.Equal(t, dialogNo, d.update(press("n"), m.keys, 30, 10))
}

func TestSelectedLabelKeepsItsWidth(t *testing.T) {
	m, _ := newModel(t, 80, 24)
	for _, label := range []string{"👨\u200d👩\u200d👧 family", "e\u0301 done", "plain label"} {
		c := glyphClass{kind: glyphLabel, state: events.Succeeded, selected: true}
		assert.Equal(t, ansi.StringWidth(label), ansi.StringWidth(c.render(m.styles, label)), label)
		plain := glyphClass{kind: glyphLabel, state: events.Succeeded, selected: false}
		assert.Equal(t, ansi.StringWidth(label), ansi.StringWidth(plain.render(m.styles, label)), label)
	}
}
