package commands

import (
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// asyncWriter writes to w from its own goroutine and never blocks the
// caller. When w falls behind (stderr is a paused pipe, say) and the queue
// is full, writes are dropped and counted. weft's notices and console log
// go through it so that writing them can never hold up a cancel.
type asyncWriter struct {
	w       io.Writer
	mu      sync.Mutex
	ch      chan []byte
	closed  bool
	done    chan struct{}
	dropped atomic.Int64
}

func newAsyncWriter(w io.Writer, queue int) *asyncWriter {
	a := &asyncWriter{
		w:       w,
		mu:      sync.Mutex{},
		ch:      make(chan []byte, queue),
		closed:  false,
		done:    make(chan struct{}),
		dropped: atomic.Int64{},
	}
	go func() {
		defer close(a.done)
		for b := range a.ch {
			_, _ = a.w.Write(b)
		}
	}()
	return a
}

// Write queues a copy of p. It always reports success.
func (a *asyncWriter) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		a.dropped.Add(1)
		return len(p), nil
	}
	select {
	case a.ch <- slices.Clone(p):
	default:
		a.dropped.Add(1)
	}
	return len(p), nil
}

// Close stops accepting writes and waits until the queue is written out,
// timeout passes, or abort closes. It reports whether everything queued was
// written.
func (a *asyncWriter) Close(timeout time.Duration, abort <-chan struct{}) bool {
	a.mu.Lock()
	if !a.closed {
		a.closed = true
		close(a.ch)
	}
	a.mu.Unlock()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-a.done:
		return true
	case <-abort:
		return false
	case <-t.C:
		return false
	}
}
