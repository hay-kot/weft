package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/events"
)

func TestForwardBatchesWhileTheProgramIsBusy(t *testing.T) {
	in := make(chan events.Event)
	busy := make(chan struct{})
	var batches [][]events.Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		forward(in, func(b []events.Event) {
			if len(batches) == 0 {
				<-busy
			}
			batches = append(batches, b)
		})
	}()

	const n = 3000
	for i := range n {
		in <- events.NodeOutput{Node: "a", Line: string(rune('a' + i%26))}
		if i == maxBatch/2 {
			close(busy)
		}
	}
	close(in)
	<-done

	total := 0
	var lines []string
	for _, b := range batches {
		require.LessOrEqual(t, len(b), maxBatch)
		total += len(b)
		for _, e := range b {
			out, ok := e.(events.NodeOutput)
			require.True(t, ok)
			lines = append(lines, out.Line)
		}
	}
	assert.Equal(t, n, total, "no event is lost")
	require.Greater(t, len(batches), 1)
	// Everything published while the first send was blocked waits in the
	// second batch, however the first one was cut.
	assert.GreaterOrEqual(t, len(batches[0])+len(batches[1]), maxBatch/2+1, "events gather into a batch while send is busy")
	for i, l := range lines {
		require.Equal(t, string(rune('a'+i%26)), l, "order is kept at %d", i)
	}
}

func TestForwardReturnsOnClose(t *testing.T) {
	in := make(chan events.Event)
	close(in)
	forward(in, func([]events.Event) { t.Fatal("nothing to send") })
}
