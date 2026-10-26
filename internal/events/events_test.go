package events_test

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/events"
)

func collect(sub *events.Subscription) []events.Event {
	var got []events.Event
	for e := range sub.C {
		got = append(got, e)
	}
	return got
}

func TestBusFansOutInOrderAndCloses(t *testing.T) {
	bus := events.NewBus()
	a := bus.Subscribe(0)
	b := bus.Subscribe(0)

	first := events.NodeOutput{Node: "n", Line: "1"}
	second := events.RunFinished{Success: true, At: time.Time{}}
	bus.Publish(first)
	bus.Publish(second)
	bus.Close()
	bus.Close()
	bus.Publish(first)

	for _, sub := range []*events.Subscription{a, b} {
		assert.Equal(t, []events.Event{first, second}, collect(sub))
	}
	assert.Empty(t, collect(bus.Subscribe(0)), "subscribing after close returns a finished subscription")
}

func TestPublishNeverBlocksOnAStalledSubscriber(t *testing.T) {
	bus := events.NewBus()
	stalled := bus.Subscribe(1024)

	done := make(chan struct{})
	go func() {
		defer close(done)
		line := strings.Repeat("x", 100)
		for range 100_000 {
			bus.Publish(events.NodeOutput{Node: "n", Line: line})
		}
		bus.Publish(events.NodeChanged{
			Node: "n", State: events.Canceled, Attempt: 1, ExitCode: 0, Err: "", Reason: "",
			Started: time.Time{}, Finished: time.Time{}, At: time.Time{},
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "Publish blocked on a subscriber that never reads")
	}

	bus.Close()
	got := collect(stalled)
	var lines, dropped int
	var last events.Event
	for _, e := range got {
		switch e := e.(type) {
		case events.NodeOutput:
			lines++
		case events.OutputDropped:
			dropped += e.Lines
		case events.NodeChanged, events.RunStarted, events.RunFinished, events.Barrier:
		}
		last = e
	}
	// The budget covers the queue; the pump may hold one more line in hand.
	assert.LessOrEqual(t, lines*100, 1024+100, "queued output stays within the budget")
	assert.Equal(t, 100_000, lines+dropped, "every line is delivered or counted as dropped")
	changed, ok := last.(events.NodeChanged)
	require.True(t, ok, "state events are never dropped, got %T last", last)
	assert.Equal(t, events.Canceled, changed.State)
}

func TestDropReportPrecedesLaterOutput(t *testing.T) {
	bus := events.NewBus()
	sub := bus.Subscribe(10)
	bus.Publish(events.NodeOutput{Node: "n", Line: "0123456789"})
	bus.Publish(events.NodeOutput{Node: "n", Line: "dropped"})
	bus.Publish(events.NodeOutput{Node: "n", Line: "also dropped"})

	assert.Equal(t, events.NodeOutput{Node: "n", Line: "0123456789"}, <-sub.C)
	assert.Equal(t, events.OutputDropped{Node: "n", Lines: 2}, <-sub.C)

	bus.Publish(events.NodeOutput{Node: "n", Line: "after"})
	assert.Equal(t, events.NodeOutput{Node: "n", Line: "after"}, <-sub.C)
	bus.Close()
	assert.Empty(t, collect(sub))
}

func TestNegativeBudgetNeverDrops(t *testing.T) {
	bus := events.NewBus()
	sub := bus.Subscribe(-1)
	for range 1000 {
		bus.Publish(events.NodeOutput{Node: "n", Line: strings.Repeat("y", 1<<12)})
	}
	bus.Close()
	assert.Len(t, collect(sub), 1000)
}

func TestBarrier(t *testing.T) {
	bus := events.NewBus()
	sub := bus.Subscribe(0)
	bus.Publish(events.NodeOutput{Node: "n", Line: "before"})

	var seen []events.Event
	go func() {
		for e := range sub.C {
			if b, ok := e.(events.Barrier); ok {
				b.Done()
				continue
			}
			seen = append(seen, e)
		}
	}()
	require.True(t, sub.Barrier(time.Second))
	assert.Equal(t, []events.Event{events.NodeOutput{Node: "n", Line: "before"}}, seen,
		"the barrier comes after everything published before it")

	bus.Close()
}

func TestBarrierTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bus := events.NewBus()
		stalled := bus.Subscribe(0)
		start := time.Now()
		assert.False(t, stalled.Barrier(50*time.Millisecond), "an unread subscriber times out")
		assert.Equal(t, 50*time.Millisecond, time.Since(start))
		bus.Close()
		for range stalled.C {
		}
	})
}

func TestTerminalStates(t *testing.T) {
	for _, s := range []events.State{events.Succeeded, events.Failed, events.Blocked, events.Canceled, events.Skipped} {
		assert.True(t, s.Terminal(), s)
	}
	for _, s := range []events.State{events.Pending, events.Ready, events.Running, events.Waiting} {
		assert.False(t, s.Terminal(), s)
	}
}
