package commands

import (
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAsyncWriterNeverBlocks(t *testing.T) {
	r, w := io.Pipe()
	defer func() { _ = r.Close() }()
	a := newAsyncWriter(w, 4)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			_, _ = a.Write([]byte("notice\n"))
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		assert.FailNow(t, "Write blocked on a writer nobody reads")
	}
	assert.Positive(t, a.dropped.Load())

	abort := make(chan struct{})
	close(abort)
	assert.False(t, a.Close(time.Hour, abort), "Close gives up on abort")
	start := time.Now()
	assert.False(t, a.Close(50*time.Millisecond, nil), "Close gives up after the timeout")
	assert.Less(t, time.Since(start), time.Second)
	_, _ = a.Write([]byte("after close"))
}

func TestAsyncWriterFlushesOnClose(t *testing.T) {
	r, w := io.Pipe()
	a := newAsyncWriter(w, 16)
	_, _ = a.Write([]byte("one\n"))
	_, _ = a.Write([]byte("two\n"))

	got := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		got <- string(b)
	}()
	assert.True(t, a.Close(time.Second, nil))
	_ = w.Close()
	assert.Equal(t, "one\ntwo\n", <-got)
}

func TestWaitForOutput(t *testing.T) {
	never := make(chan struct{})

	t.Run("done", func(t *testing.T) {
		done := make(chan struct{})
		close(done)
		assert.True(t, waitForOutput(done, func() int64 { return 0 }, time.Second, never))
	})

	t.Run("stalled", func(t *testing.T) {
		start := time.Now()
		assert.False(t, waitForOutput(never, func() int64 { return 7 }, 100*time.Millisecond, never))
		assert.Less(t, time.Since(start), time.Second)
	})

	t.Run("progressing", func(t *testing.T) {
		var n atomic.Int64
		done := make(chan struct{})
		go func() {
			for range 30 {
				n.Add(1)
				time.Sleep(10 * time.Millisecond)
			}
			close(done)
		}()
		// 300ms of steady progress outlasts a 100ms stall window.
		assert.True(t, waitForOutput(done, n.Load, 100*time.Millisecond, never))
	})

	t.Run("abort", func(t *testing.T) {
		abort := make(chan struct{})
		close(abort)
		assert.False(t, waitForOutput(never, func() int64 { return 0 }, time.Hour, abort))
	})
}
