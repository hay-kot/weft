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
		"build     ✗ failed: exit code 2 in 1:05\n" +
		"Long Name ◆ waiting for the terminal\n" +
		"Long Name ⊘ blocked: build failed\n"
	assert.Equal(t, want, out.String())
}

func TestFormatDuration(t *testing.T) {
	tests := map[time.Duration]string{
		0:                       "0:00",
		1500 * time.Millisecond: "0:02",
		61 * time.Second:        "1:01",
		time.Hour + 2*time.Minute + 3*time.Second: "1:02:03",
	}
	for d, want := range tests {
		assert.Equal(t, want, plain.FormatDuration(d), d.String())
	}
}

func TestSyncAfterStop(t *testing.T) {
	wf, err := workflow.Parse([]byte("nodes:\n  a: {run: x}\n"))
	require.NoError(t, err)
	r := plain.New(&bytes.Buffer{}, wf, false)
	ch := make(chan events.Event)
	done := make(chan struct{})
	go func() { r.Run(ch); close(done) }()
	r.Sync()
	close(ch)
	<-done
	r.Sync() // must not block once the renderer has stopped
}
