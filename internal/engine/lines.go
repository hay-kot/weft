package engine

import (
	"bytes"
	"sync"
	"unicode/utf8"
)

// MaxLineBytes caps one output line. Longer lines are split so a node that
// prints a huge minified blob can't grow memory without bound.
const MaxLineBytes = 64 * 1024

// lineWriter splits a byte stream into lines and hands each one to emit
// without its trailing newline. It is safe for concurrent use because
// os/exec may write from more than one goroutine.
type lineWriter struct {
	mu   sync.Mutex
	buf  []byte
	emit func(line string)
}

func newLineWriter(emit func(string)) *lineWriter {
	return &lineWriter{mu: sync.Mutex{}, buf: nil, emit: emit}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emitCapped(bytes.TrimSuffix(w.buf[:i], []byte("\r")))
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > MaxLineBytes {
		w.buf = w.emitFullChunks(w.buf)
	}
	if len(w.buf) == 0 {
		w.buf = nil
	}
	return len(p), nil
}

func (w *lineWriter) emitCapped(line []byte) {
	rest := w.emitFullChunks(line)
	w.emit(string(rest))
}

// emitFullChunks emits MaxLineBytes-sized pieces of b, cut on rune
// boundaries, while more than MaxLineBytes remain, and returns the rest.
func (w *lineWriter) emitFullChunks(b []byte) []byte {
	for len(b) > MaxLineBytes {
		cut := MaxLineBytes
		for cut > 0 && !utf8.RuneStart(b[cut]) {
			cut--
		}
		if cut == 0 {
			cut = MaxLineBytes
		}
		w.emit(string(b[:cut]))
		b = b[cut:]
	}
	return b
}

// Flush emits any trailing partial line.
func (w *lineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.emit(string(w.buf))
		w.buf = nil
	}
}
