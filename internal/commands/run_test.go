package commands

import (
	"io"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	_, _ = a.Write([]byte("after close"))
}

func TestAsyncWriterCloseTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, w := io.Pipe()
		a := newAsyncWriter(w, 4)
		_, _ = a.Write([]byte("never read\n"))

		start := time.Now()
		assert.False(t, a.Close(50*time.Millisecond, nil), "Close gives up after the timeout")
		assert.Equal(t, 50*time.Millisecond, time.Since(start))
		_ = r.Close()
	})
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
		synctest.Test(t, func(t *testing.T) {
			never := make(chan struct{})
			start := time.Now()
			assert.False(t, waitForOutput(never, func() int64 { return 7 }, 100*time.Millisecond, never))
			assert.Equal(t, 100*time.Millisecond, time.Since(start))
		})
	})

	t.Run("progressing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			never := make(chan struct{})
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
			start := time.Now()
			assert.True(t, waitForOutput(done, n.Load, 100*time.Millisecond, never))
			assert.Equal(t, 300*time.Millisecond, time.Since(start))
		})
	})

	t.Run("abort", func(t *testing.T) {
		abort := make(chan struct{})
		close(abort)
		assert.False(t, waitForOutput(never, func() int64 { return 0 }, time.Hour, abort))
	})
}

func TestPickUI(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	tests := []struct {
		name          string
		flag          string
		stdin, stdout bool
		vars          map[string]string
		want          string
		wantErr       bool
	}{
		{"auto in a terminal", uiAuto, true, true, nil, uiTUI, false},
		{"auto with stdout piped", uiAuto, true, false, nil, uiPlain, false},
		{"auto with stdin redirected", uiAuto, false, true, nil, uiPlain, false},
		{"auto in CI", uiAuto, true, true, map[string]string{"CI": "true"}, uiPlain, false},
		{"auto on a dumb terminal", uiAuto, true, true, map[string]string{"TERM": "dumb"}, uiPlain, false},
		{"plain in a terminal", uiPlain, true, true, nil, uiPlain, false},
		{"tui in CI with a terminal", uiTUI, true, true, map[string]string{"CI": "true"}, uiTUI, false},
		{"tui without a terminal", uiTUI, true, false, nil, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pickUI(tt.flag, tt.stdin, tt.stdout, env(tt.vars))
			if tt.wantErr {
				assert.ErrorContains(t, err, "--ui=tui needs a terminal")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNodeEnvColor(t *testing.T) {
	const path = "PATH=/bin"
	base := []string{path}
	tests := []struct {
		name    string
		base    []string
		tty     bool
		noColor bool
		want    []string
	}{
		{"tty forces color", base, true, false, []string{path, "CLICOLOR_FORCE=1", "FORCE_COLOR=1"}},
		{"no tty leaves color alone", base, false, false, []string{path}},
		{"--no-color on a tty", base, true, true, []string{path, "NO_COLOR=1"}},
		{"NO_COLOR already set", []string{path, "NO_COLOR=yes"}, true, true, []string{path, "NO_COLOR=yes"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, nodeEnv(tt.base, tt.tty, tt.noColor))
		})
	}
}
