package plain_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/ui/plain"
	"github.com/hay-kot/weft/internal/workflow"
)

func changed(node string, state events.State, errText string, dur time.Duration) events.NodeChanged {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	finished := time.Time{}
	if state.Terminal() {
		finished = start.Add(dur)
	}
	return events.NodeChanged{
		Node: node, State: state, Attempt: 1, ExitCode: 0, Err: errText, Reason: "",
		Started: start, Finished: finished, At: start,
	}
}

func TestRender(t *testing.T) {
	wf, err := workflow.Parse([]byte("nodes:\n  build: {run: x}\n  long-name: {run: y, label: Long Name}\n"))
	require.NoError(t, err)

	var out bytes.Buffer
	r := plain.New(&out, wf, false)
	for _, e := range []events.Event{
		events.RunStarted{Workflow: "w", RunID: "id", RunDir: "dir", At: time.Time{}},
		changed("build", events.Pending, "", 0),
		changed("build", events.Running, "", 0),
		events.NodeOutput{Node: "build", Line: "compiling"},
		events.NodeOutput{Node: "build", Line: "10%\r50%\r100%"},
		events.NodeOutput{Node: "build", Line: "done 100%\r"},
		changed("build", events.Failed, "exit code 2", 65*time.Second),
		changed("long-name", events.Waiting, "", 0),
		changed("long-name", events.Blocked, "blocked: build failed", 0),
		events.RunFinished{Success: false, At: time.Time{}},
	} {
		r.Render(e)
	}

	want := "" +
		"build     ▶ started\n" +
		"build     │ compiling\n" +
		"build     │ 100%\n" +
		"build     │ done 100%\n" +
		"build     ✗ failed: exit code 2 in 1:05\n" +
		"Long Name ◆ waiting for the terminal\n" +
		"Long Name ⊘ blocked: build failed\n"
	assert.Equal(t, want, out.String())
}

func TestRenderDropsAndBarrier(t *testing.T) {
	wf, err := workflow.Parse([]byte("nodes:\n  a: {run: x}\n"))
	require.NoError(t, err)
	var out bytes.Buffer
	r := plain.New(&out, wf, false)

	bus := events.NewBus()
	sub := bus.Subscribe(0)
	done := make(chan struct{})
	go func() { r.Run(sub.C); close(done) }()

	bus.Publish(events.OutputDropped{Node: "a", Lines: 12})
	assert.True(t, sub.Barrier(time.Second), "the renderer acknowledges barriers")
	assert.Equal(t, "a … 12 output lines not shown: stdout fell behind (the node log has them)\n", out.String())

	bus.Close()
	<-done
	assert.False(t, sub.Barrier(time.Second), "a finished subscription never reaches a barrier")
}
