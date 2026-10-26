package engine_test

import (
	"io"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/engine"
	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/runner"
	"github.com/hay-kot/weft/internal/store"
	"github.com/hay-kot/weft/internal/ui/plain"
	"github.com/hay-kot/weft/internal/workflow"
)

// TestCancelEscalatesOnTimeWithStdoutStalled wires the plain renderer to a
// pipe that nobody reads, as with `weft run | less` paused, and runs a node
// that floods output and ignores SIGINT. The cancel must still reach the
// SIGTERM step after kill_timeout, and the run must end promptly.
func TestCancelEscalatesOnTimeWithStdoutStalled(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real processes")
	}
	yml := `
nodes:
  flood:
    run: trap '' INT; echo started; while :; do echo "flood line padded to make the pipe fill quickly"; done
    kill_timeout: 300ms
  later:
    needs: [flood]
    run: echo never
`
	wf, err := workflow.Parse([]byte(yml))
	require.NoError(t, err)
	root := t.TempDir()
	wf.Root, wf.Name = root, "stall"
	run, err := store.Create(root, wf.Name, time.Now())
	require.NoError(t, err)

	stalledR, stalledW := io.Pipe()
	t.Cleanup(func() { _ = stalledR.Close() })

	bus := events.NewBus()
	renderer := plain.New(stalledW, wf, false)
	renderSub := bus.Subscribe(0)
	go renderer.Run(renderSub.C)

	eng := engine.New(engine.Options{
		Workflow:    wf,
		Run:         run,
		Runner:      runner.Process{},
		Lender:      plain.Lender{Sub: renderSub},
		Bus:         bus,
		Concurrency: 0,
		Env:         os.Environ(),
		Log:         zerolog.Nop(),
	})

	var canceledAt time.Time
	go func() {
		// Cancel once the flood has had time to fill the pipe and the
		// renderer's queue budget many times over.
		time.Sleep(time.Second)
		canceledAt = time.Now()
		eng.Cancel()
	}()

	done := make(chan engine.Summary, 1)
	go func() { done <- eng.Run(t.Context()) }()

	var s engine.Summary
	select {
	case s = <-done:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "run did not end: a stalled stdout blocked the engine")
	}
	elapsed := time.Since(canceledAt)

	assert.Equal(t, events.Canceled, s.States["flood"])
	assert.Equal(t, events.Canceled, s.States["later"])
	// kill_timeout is 300ms, then SIGTERM ends the loop. Allow for slow CI.
	assert.Less(t, elapsed, 3*time.Second, "cancel escalation was delayed")

	var rec store.AttemptRecord
	readJSON(t, run.AttemptRecordPath("flood", 1), &rec)
	assert.Equal(t, syscall.SIGTERM.String(), rec.Signal, "SIGINT was ignored, so SIGTERM ended it")

	info, err := os.Stat(run.AttemptLogPath("flood", 1))
	require.NoError(t, err)
	assert.Greater(t, info.Size(), int64(events.DefaultOutputBudget), "the node log holds output the terminal never got")
	bus.Close()
}
