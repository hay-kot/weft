package events_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/hay-kot/weft/internal/events"
)

func TestBusFansOutInOrderAndCloses(t *testing.T) {
	bus := events.NewBus()
	a := bus.Subscribe(4)
	b := bus.Subscribe(4)

	first := events.NodeOutput{Node: "n", Line: "1"}
	second := events.RunFinished{Success: true, At: time.Time{}}
	bus.Publish(first)
	bus.Publish(second)
	bus.Close()
	bus.Close()
	bus.Publish(first)

	for _, ch := range []<-chan events.Event{a, b} {
		var got []events.Event
		for e := range ch {
			got = append(got, e)
		}
		assert.Equal(t, []events.Event{first, second}, got)
	}

	_, open := <-bus.Subscribe(1)
	assert.False(t, open, "subscribing after close returns a closed channel")
}

func TestTerminalStates(t *testing.T) {
	for _, s := range []events.State{events.Succeeded, events.Failed, events.Blocked, events.Canceled, events.Skipped} {
		assert.True(t, s.Terminal(), s)
	}
	for _, s := range []events.State{events.Pending, events.Ready, events.Running, events.Waiting} {
		assert.False(t, s.Terminal(), s)
	}
}
