package tui

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLogBufferKeepsNewestLines(t *testing.T) {
	b := newLogBuffer(3)
	assert.Equal(t, 0, b.first())
	assert.Equal(t, 0, b.end())
	assert.Nil(t, b.slice(0, 10))

	for i := range 5 {
		b.append(logLine{Text: strconv.Itoa(i), Notice: false})
	}
	assert.Equal(t, 2, b.first(), "the two oldest lines fell off")
	assert.Equal(t, 5, b.end())
	assert.Equal(t, "2", b.at(2).Text)
	assert.Equal(t, "4", b.at(4).Text)

	got := b.slice(0, 10)
	texts := make([]string, 0, len(got))
	for _, l := range got {
		texts = append(texts, l.Text)
	}
	assert.Equal(t, []string{"2", "3", "4"}, texts)
	assert.Len(t, b.slice(3, 4), 1)
	assert.Nil(t, b.slice(4, 3))
}

func TestSanitize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain ascii passes through", "go test ./...", "go test ./..."},
		{"empty", "", ""},
		{"progress bar keeps the last redraw", "10%\r50%\r100%", "100%"},
		{"trailing carriage return keeps the text", "done\r", "done"},
		{"leading carriage return", "\rready", "ready"},
		{"sgr color is kept", "\x1b[31mred\x1b[0m", "\x1b[31mred\x1b[0m"},
		{"extended sgr is kept", "\x1b[38;2;1;2;3mx\x1b[m", "\x1b[38;2;1;2;3mx\x1b[m"},
		{"cursor movement is dropped", "a\x1b[2Ab\x1b[Kc", "abc"},
		{"screen clear is dropped", "\x1b[2J\x1b[Hhello", "hello"},
		{"osc title is dropped", "\x1b]0;title\x07text", "text"},
		{"private mode is dropped", "\x1b[?25lhidden cursor", "hidden cursor"},
		{"bell and backspace are dropped", "a\x07b\x08c", "abc"},
		{"tabs expand to stops", "a\tb", "a       b"},
		{"tab stops count wide runes", "日本\tx", "日本    x"},
		{"tab after color counts only text", "\x1b[1mab\x1b[0m\tc", "\x1b[1mab\x1b[0m      c"},
		{"invalid utf-8 is replaced", "a\xffb", "a�b"},
		{"unicode passes through", "• building ✓", "• building ✓"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sanitize(tt.in))
		})
	}
}
