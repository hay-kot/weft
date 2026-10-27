package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// logCapacity is how many lines the TUI keeps per node. The attempt log on
// disk has every line.
const logCapacity = 10_000

// logBytesCap bounds the bytes kept per node. The engine cuts lines at
// 64 KiB, so the line count alone could keep hundreds of MB of one node.
const logBytesCap = 16 << 20

// tabWidth is the tab stop interval used when expanding tabs.
const tabWidth = 8

// logLine is one line in a node's buffer.
type logLine struct {
	// Text is sanitized: printable text plus SGR sequences only.
	Text string
	// Notice marks a line weft wrote, such as a note about dropped output,
	// rather than the node.
	Notice bool
}

// logBuffer is a ring of the most recent lines of one node. Lines are
// addressed by absolute index: the n-th line ever appended has index n, so
// a scroll position stays on the same line while old lines fall off.
type logBuffer struct {
	lines    []logLine
	head     int // slot of the oldest line
	size     int
	total    int // lines ever appended
	bytes    int // bytes of text kept
	maxBytes int
}

func newLogBuffer(capacity, maxBytes int) *logBuffer {
	return &logBuffer{lines: make([]logLine, max(1, capacity)), head: 0, size: 0, total: 0, bytes: 0, maxBytes: maxBytes}
}

func (b *logBuffer) append(l logLine) {
	if b.size < len(b.lines) {
		b.lines[(b.head+b.size)%len(b.lines)] = l
		b.size++
	} else {
		b.bytes -= len(b.lines[b.head].Text)
		b.lines[b.head] = l
		b.head = (b.head + 1) % len(b.lines)
	}
	b.bytes += len(l.Text)
	b.total++
	for b.bytes > b.maxBytes && b.size > 1 {
		b.dropOldest()
	}
}

func (b *logBuffer) dropOldest() {
	b.bytes -= len(b.lines[b.head].Text)
	b.lines[b.head] = logLine{Text: "", Notice: false}
	b.head = (b.head + 1) % len(b.lines)
	b.size--
}

// first returns the absolute index of the oldest line kept.
func (b *logBuffer) first() int { return b.total - b.size }

// end returns the absolute index one past the newest line.
func (b *logBuffer) end() int { return b.total }

// at returns the line with absolute index i, which must be in [first, end).
func (b *logBuffer) at(i int) logLine {
	return b.lines[(b.head+i-b.first())%len(b.lines)]
}

// slice returns the lines with absolute indexes in [from, to), clamped to
// what the buffer holds.
func (b *logBuffer) slice(from, to int) []logLine {
	from, to = max(from, b.first()), min(to, b.end())
	if from >= to {
		return nil
	}
	out := make([]logLine, 0, to-from)
	for i := from; i < to; i++ {
		out = append(out, b.at(i))
	}
	return out
}

// sanitize turns one line of node output into something safe to draw in a
// cell grid. A bare carriage return redraws the line, so only the text
// after the last one is kept, which is enough for most progress bars.
// Color and style (SGR) sequences are kept; every other escape sequence and
// control character is dropped, since cursor movement or a screen clear
// inside a pane would corrupt the rest of the screen. Tabs expand to spaces.
func sanitize(line string) string {
	line = lastSegment(line)
	if plainASCII(line) {
		return line
	}
	line = strings.ToValidUTF8(line, "�")

	var b strings.Builder
	b.Grow(len(line))
	col := 0
	var state byte
	for len(line) > 0 {
		seq, width, n, next := ansi.DecodeSequence(line, state, nil)
		state = next
		line = line[n:]
		switch {
		case width > 0:
			b.WriteString(seq)
			col += width
		case seq == "\t":
			pad := tabWidth - col%tabWidth
			b.WriteString(strings.Repeat(" ", pad))
			col += pad
		case isSGR(seq), joinsText(seq):
			b.WriteString(seq)
		}
	}
	return b.String()
}

// joinsText reports whether a zero-width sequence is part of the text
// rather than a control: a combining mark, a joiner, or a variation
// selector. Dropping those would turn é into e and split emoji sequences.
// Other format characters, such as bidi overrides, stay out.
func joinsText(seq string) bool {
	r, _ := utf8.DecodeRuneInString(seq)
	switch {
	case r == utf8.RuneError:
		return false
	case unicode.Is(unicode.M, r):
		return true
	case r == '‌', r == '‍':
		return true
	case r >= '︀' && r <= '️', r >= 0xe0100 && r <= 0xe01ef:
		return true
	}
	return false
}

// lastSegment returns the text after the last carriage return that is
// followed by anything.
func lastSegment(line string) string {
	for {
		before, after, found := strings.Cut(line, "\r")
		if !found {
			return line
		}
		if strings.Trim(after, "\r") == "" {
			return before
		}
		line = after
	}
}

func plainASCII(s string) bool {
	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// isSGR reports whether seq is a CSI "m" sequence with only numeric
// parameters, such as "\x1b[1;31m".
func isSGR(seq string) bool {
	params, ok := strings.CutPrefix(seq, "\x1b[")
	if !ok {
		return false
	}
	params, ok = strings.CutSuffix(params, "m")
	if !ok {
		return false
	}
	for i := range len(params) {
		c := params[i]
		if (c < '0' || c > '9') && c != ';' && c != ':' {
			return false
		}
	}
	return true
}
