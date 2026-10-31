package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/engine"
	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/runner"
	"github.com/hay-kot/weft/internal/store"
	"github.com/hay-kot/weft/internal/workflow"
)

const deployYAML = `
concurrency: 4
nodes:
  guard:
    run: guard
  go-check:
    needs: [guard]
    run: go-check
  ui-check:
    needs: [guard]
    run: ui-check
  db-backup:
    needs: [guard]
    run: db-backup
  release:
    needs: [go-check, ui-check, db-backup]
    run: release
  deploy:
    needs: [release]
    run: deploy
  tofu-apply:
    needs: [deploy]
    interactive: true
    run: tofu-apply
`

// behavior scripts one node in the fake runner.
type behavior struct {
	exit int
	// hang keeps the node running until it is canceled or killed.
	hang   bool
	signal string
	output string
	delay  time.Duration
	// trapINT makes a hanging node ignore graceful cancel, like a process
	// that traps SIGINT and keeps going. Only Kill ends it.
	trapINT bool
	// gate, when set, holds the node until it closes.
	gate chan struct{}
}

type fakeRunner struct {
	mu        sync.Mutex
	behaviors map[string]behavior
	running   int
	peak      int
	started   []string
	ended     []string
	active    map[string]bool
	// overlaps records, per node, the nodes running when it started.
	overlaps map[string][]string
	killed   []string
	onStart  func(node string)
	// perAttempt overrides behaviors for one attempt of a node.
	perAttempt map[string]map[int]behavior
}

func newFake(b map[string]behavior) *fakeRunner {
	return &fakeRunner{
		mu:         sync.Mutex{},
		behaviors:  b,
		running:    0,
		peak:       0,
		started:    nil,
		ended:      nil,
		active:     map[string]bool{},
		overlaps:   map[string][]string{},
		killed:     nil,
		onStart:    nil,
		perAttempt: map[string]map[int]behavior{},
	}
}

// attemptOf reads WEFT_ATTEMPT from a spec's environment.
func attemptOf(spec runner.Spec) int {
	for _, kv := range spec.Env {
		if v, ok := strings.CutPrefix(kv, "WEFT_ATTEMPT="); ok {
			n, _ := strconv.Atoi(v)
			return n
		}
	}
	return 0
}

func (f *fakeRunner) Run(ctx context.Context, spec runner.Spec) runner.Result {
	f.mu.Lock()
	b := f.behaviors[spec.Node]
	if pa, ok := f.perAttempt[spec.Node][attemptOf(spec)]; ok {
		b = pa
	}
	var others []string
	for id := range f.active {
		others = append(others, id)
	}
	slices.Sort(others)
	f.overlaps[spec.Node] = others
	f.active[spec.Node] = true
	f.running++
	f.peak = max(f.peak, f.running)
	f.started = append(f.started, spec.Node)
	onStart := f.onStart
	f.mu.Unlock()

	if onStart != nil {
		onStart(spec.Node)
	}
	defer func() {
		f.mu.Lock()
		f.running--
		delete(f.active, spec.Node)
		f.ended = append(f.ended, spec.Node)
		f.mu.Unlock()
	}()

	if b.output != "" && spec.Output != nil {
		_, _ = spec.Output.Write([]byte(b.output))
	}
	if b.delay > 0 {
		time.Sleep(b.delay)
	}
	if b.gate != nil {
		<-b.gate
	}
	if b.hang {
		done := ctx.Done()
		if b.trapINT {
			done = nil
		}
		select {
		case <-done:
			return result(runner.ReasonCanceled, 130, "")
		case <-spec.Kill:
			f.mu.Lock()
			f.killed = append(f.killed, spec.Node)
			f.mu.Unlock()
			return result(runner.ReasonCanceled, -1, "killed")
		}
	}
	return result(runner.ReasonExit, b.exit, b.signal)
}

func result(reason runner.Reason, code int, sig string) runner.Result {
	return runner.Result{ExitCode: code, Signal: sig, Reason: reason, Err: nil}
}

type fakeLender struct {
	err   error
	calls int
}

func (l *fakeLender) Lend(fn func()) error {
	l.calls++
	if l.err != nil {
		return l.err
	}
	fn()
	return nil
}

type harness struct {
	wf       *workflow.Workflow
	closeBus func()
	eng      *engine.Engine
	fake     *fakeRunner
	lender   *fakeLender
	run      *store.Run
	logBuf   *syncBuffer
	mu       sync.Mutex
	events   []events.Event
	done     chan struct{}
}

// snapshot returns the events collected so far.
func (h *harness) snapshot() []events.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.events)
}

// waitEvent waits until an event satisfies match and returns how many
// events had arrived by then.
func (h *harness) waitEvent(t *testing.T, match func(events.Event) bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		return slices.ContainsFunc(h.snapshot(), match)
	}, 5*time.Second, 5*time.Millisecond)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newHarness(t *testing.T, yml string, b map[string]behavior, concurrency int) *harness {
	t.Helper()
	return newHarnessWith(t, yml, b, func(o *engine.Options) { o.Concurrency = concurrency })
}

// newHarnessWith builds a harness whose engine options mod may change.
func newHarnessWith(t *testing.T, yml string, b map[string]behavior, mod func(*engine.Options)) *harness {
	t.Helper()
	wf, err := workflow.Parse([]byte(yml))
	require.NoError(t, err)
	root := t.TempDir()
	wf.Root = root
	wf.Name = "test"

	run, err := store.Create(root, wf.Name, time.Now())
	require.NoError(t, err)

	h := &harness{
		wf:       wf,
		closeBus: nil,
		eng:      nil,
		fake:     newFake(b),
		lender:   &fakeLender{err: nil, calls: 0},
		run:      run,
		logBuf:   &syncBuffer{mu: sync.Mutex{}, buf: bytes.Buffer{}},
		mu:       sync.Mutex{},
		events:   nil,
		done:     make(chan struct{}),
	}
	bus := events.NewBus()
	sub := bus.Subscribe(-1)
	go func() {
		defer close(h.done)
		for e := range sub.C {
			h.mu.Lock()
			h.events = append(h.events, e)
			h.mu.Unlock()
		}
	}()
	t.Cleanup(bus.Close)

	log := zerolog.New(h.logBuf).Level(zerolog.DebugLevel).With().Str("run_id", run.ID).Logger()
	opts := engine.Options{
		Workflow:    wf,
		Run:         run,
		Runner:      h.fake,
		Lender:      h.lender,
		Bus:         bus,
		Concurrency: 0,
		Env:         nil,
		Plan:        nil,
		State:       nil,
		Reruns:      false,
		Log:         log,
	}
	mod(&opts)
	h.eng = engine.New(opts)
	h.closeBus = bus.Close
	return h
}

// runToEnd runs the engine and waits for every event to be collected.
func (h *harness) runToEnd(t *testing.T) engine.Summary {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	s := h.eng.Run(ctx)
	require.NoError(t, ctx.Err(), "run did not finish in time")
	h.closeBus()
	<-h.done
	return s
}

func (h *harness) transitions(node string) []events.State {
	var out []events.State
	for _, e := range h.events {
		if nc, ok := e.(events.NodeChanged); ok && nc.Node == node {
			out = append(out, nc.State)
		}
	}
	return out
}

func indexOf(s []string, v string) int { return slices.Index(s, v) }

func TestRunSucceedsInDependencyOrder(t *testing.T) {
	h := newHarness(t, deployYAML, map[string]behavior{
		"go-check": {exit: 0, hang: false, signal: "", output: "", delay: 20 * time.Millisecond},
		"ui-check": {exit: 0, hang: false, signal: "", output: "", delay: 20 * time.Millisecond},
	}, 0)
	s := h.runToEnd(t)

	assert.True(t, s.Success)
	for id, st := range s.States {
		assert.Equal(t, events.Succeeded, st, id)
	}
	started := h.fake.started
	require.Len(t, started, 7)
	assert.Equal(t, "guard", started[0])
	for _, check := range []string{"go-check", "ui-check", "db-backup"} {
		assert.Less(t, indexOf(started, check), indexOf(started, "release"), check)
		assert.Less(t, indexOf(h.fake.ended, check), indexOf(started, "release"), check)
	}
	assert.Equal(t, []string{"release", "deploy", "tofu-apply"}, started[4:])
	assert.Equal(t, 3, h.fake.peak, "the three checks run in parallel")

	assert.Equal(t,
		[]events.State{events.Pending, events.Ready, events.Running, events.Succeeded},
		h.transitions("release"))

	_, first := h.events[0].(events.RunStarted)
	assert.True(t, first, "first event is RunStarted")
	last, ok := h.events[len(h.events)-1].(events.RunFinished)
	require.True(t, ok, "last event is RunFinished")
	assert.True(t, last.Success)
}

func TestConcurrencyLimit(t *testing.T) {
	yml := `
nodes:
  a: {run: a}
  b: {run: b}
  c: {run: c}
  d: {run: d}
  e: {run: e}
`
	slow := behavior{exit: 0, hang: false, signal: "", output: "", delay: 30 * time.Millisecond}
	h := newHarness(t, yml, map[string]behavior{"a": slow, "b": slow, "c": slow, "d": slow, "e": slow}, 2)
	s := h.runToEnd(t)

	assert.True(t, s.Success)
	assert.Equal(t, 2, h.fake.peak)
	assert.Equal(t, []string{"a", "b", "c", "d", "e"}, sortedCopy(h.fake.started))
	assert.Contains(t, h.logBuf.String(), "node ready but no free slot")
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

func TestFailureBlocksDownstreamAndIndependentBranchesFinish(t *testing.T) {
	yml := `
nodes:
  a: {run: a}
  b: {run: b, needs: [a]}
  c: {run: c, needs: [b]}
  x: {run: x}
  y: {run: y, needs: [x]}
`
	h := newHarness(t, yml, map[string]behavior{
		"a": {exit: 3, hang: false, signal: "", output: "", delay: 0},
		"x": {exit: 0, hang: false, signal: "", output: "", delay: 20 * time.Millisecond},
	}, 0)
	s := h.runToEnd(t)

	assert.False(t, s.Success)
	assert.Equal(t, map[string]events.State{
		"a": events.Failed,
		"b": events.Blocked,
		"c": events.Blocked,
		"x": events.Succeeded,
		"y": events.Succeeded,
	}, s.States)
	assert.NotContains(t, h.fake.started, "b")

	var blockedErr, failedErr string
	for _, e := range h.events {
		if nc, ok := e.(events.NodeChanged); ok {
			switch {
			case nc.Node == "c" && nc.State == events.Blocked:
				blockedErr = nc.Err
			case nc.Node == "a" && nc.State == events.Failed:
				failedErr = nc.Err
				assert.Equal(t, 3, nc.ExitCode)
			}
		}
	}
	assert.Equal(t, "blocked: b blocked", blockedErr)
	assert.Equal(t, "exit code 3", failedErr)

	var rec store.AttemptRecord
	readJSON(t, h.run.AttemptRecordPath("b", 1), &rec)
	assert.Equal(t, events.ReasonBlocked, rec.Reason)
	assert.Equal(t, "blocked", rec.State)
	assert.Empty(t, rec.Log)
}

func TestContinueOnError(t *testing.T) {
	yml := `
nodes:
  lint:
    run: lint
    continue_on_error: true
  build:
    run: build
    needs: [lint]
`
	h := newHarness(t, yml, map[string]behavior{
		"lint": {exit: 1, hang: false, signal: "", output: "", delay: 0},
	}, 0)
	s := h.runToEnd(t)

	assert.True(t, s.Success)
	assert.Equal(t, events.Failed, s.States["lint"])
	assert.Equal(t, events.Succeeded, s.States["build"])
}

func TestBlockingIgnoresDeclarationOrder(t *testing.T) {
	yml := `
nodes:
  c: {run: c, needs: [b]}
  b: {run: b, needs: [a]}
  a: {run: a}
`
	h := newHarness(t, yml, map[string]behavior{
		"a": {exit: 1, hang: false, signal: "", output: "", delay: 0},
	}, 0)
	s := h.runToEnd(t)
	assert.Equal(t, events.Blocked, s.States["b"])
	assert.Equal(t, events.Blocked, s.States["c"])
}

func TestInteractiveNodeRunsAlone(t *testing.T) {
	yml := `
nodes:
  slow: {run: slow}
  fast: {run: fast}
  prompt:
    run: prompt
    interactive: true
    needs: [fast]
  after: {run: after, needs: [fast]}
`
	h := newHarness(t, yml, map[string]behavior{
		"slow": {exit: 0, hang: false, signal: "", output: "", delay: 50 * time.Millisecond},
	}, 0)
	s := h.runToEnd(t)

	assert.True(t, s.Success)
	assert.Equal(t, 1, h.lender.calls)
	assert.Empty(t, h.fake.overlaps["prompt"], "nothing else runs while the interactive node runs")
	assert.Less(t, indexOf(h.fake.ended, "slow"), indexOf(h.fake.started, "prompt"))
	assert.Greater(t, indexOf(h.fake.started, "after"), indexOf(h.fake.ended, "prompt"),
		"a ready node does not start ahead of a waiting interactive node")
	assert.Equal(t,
		[]events.State{events.Pending, events.Ready, events.Waiting, events.Running, events.Succeeded},
		h.transitions("prompt"))

	logData, err := readFile(h.run.AttemptLogPath("prompt", 1))
	require.NoError(t, err)
	assert.Contains(t, logData, "output went to the terminal")
	assert.Contains(t, logData, "exit code 0")
}

func TestInteractiveWithoutTerminalFails(t *testing.T) {
	yml := `
nodes:
  prompt: {run: prompt, interactive: true}
  after: {run: after, needs: [prompt]}
`
	h := newHarness(t, yml, nil, 0)
	h.lender.err = errors.New("no tty")
	s := h.runToEnd(t)

	assert.False(t, s.Success)
	assert.Equal(t, events.Failed, s.States["prompt"])
	assert.Equal(t, events.Blocked, s.States["after"])
	assert.Empty(t, h.fake.started)

	var rec store.AttemptRecord
	readJSON(t, h.run.AttemptRecordPath("prompt", 1), &rec)
	assert.Equal(t, events.ReasonStartError, rec.Reason)
	assert.Contains(t, rec.Error, "no tty")
}

func TestInteractiveInterruptCancelsRun(t *testing.T) {
	yml := `
nodes:
  prompt: {run: prompt, interactive: true}
  other: {run: other, needs: [prompt]}
  later: {run: later, needs: [gate]}
  gate: {run: gate, needs: [prompt]}
  free: {run: free, needs: [prompt], continue_on_error: true}
`
	h := newHarness(t, yml, map[string]behavior{
		"prompt": {exit: -1, hang: false, signal: "interrupt", output: "", delay: 0},
	}, 0)
	s := h.runToEnd(t)

	assert.Equal(t, events.Canceled, s.States["prompt"])
	for _, id := range []string{"other", "later", "gate", "free"} {
		assert.Contains(t, []events.State{events.Canceled, events.Blocked}, s.States[id], id)
	}
	assert.Equal(t, []string{"prompt"}, h.fake.started)
}

func TestCancelGivesRunningNodesSIGINTAndCancelsTheRest(t *testing.T) {
	yml := `
nodes:
  a: {run: a}
  b: {run: b}
  c: {run: c, needs: [a]}
`
	h := newHarness(t, yml, map[string]behavior{
		"a": {exit: 0, hang: true, signal: "", output: "", delay: 0},
		"b": {exit: 0, hang: true, signal: "", output: "", delay: 0},
	}, 0)
	var once sync.WaitGroup
	once.Add(2)
	h.fake.onStart = func(string) { once.Done() }
	go func() {
		once.Wait()
		h.eng.Cancel()
		h.eng.Cancel()
	}()
	s := h.runToEnd(t)

	assert.False(t, s.Success)
	assert.Equal(t, map[string]events.State{
		"a": events.Canceled, "b": events.Canceled, "c": events.Canceled,
	}, s.States)
	assert.Empty(t, h.fake.killed)
	assert.NotContains(t, h.fake.started, "c")
	assert.Contains(t, h.logBuf.String(), "node canceled before start")
}

func TestKillSkipsGracePeriod(t *testing.T) {
	yml := `
nodes:
  a: {run: a}
`
	h := newHarness(t, yml, map[string]behavior{
		"a": {exit: 0, hang: true, signal: "", output: "", delay: 0, trapINT: true},
	}, 0)
	h.fake.onStart = func(string) {
		go func() {
			h.eng.Cancel()
			h.eng.Kill()
		}()
	}
	s := h.runToEnd(t)
	assert.Equal(t, events.Canceled, s.States["a"])
	assert.Equal(t, []string{"a"}, h.fake.killed)
}

func TestContextCancelBeforeRunStartsNothing(t *testing.T) {
	h := newHarness(t, "nodes:\n  a: {run: a}\n", nil, 0)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s := h.eng.Run(ctx)
	assert.Equal(t, events.Canceled, s.States["a"])
	assert.Empty(t, h.fake.started)
}

func TestOutputIsLoggedAndPublishedAsLines(t *testing.T) {
	long := strings.Repeat("x", engine.MaxLineBytes+10)
	h := newHarness(t, "nodes:\n  a: {run: a}\n", map[string]behavior{
		"a": {exit: 0, hang: false, signal: "", output: "one\r\ntwo\n" + long + "\npartial", delay: 0},
	}, 0)
	h.runToEnd(t)

	var lines []string
	for _, e := range h.events {
		if out, ok := e.(events.NodeOutput); ok {
			assert.Equal(t, "a", out.Node)
			lines = append(lines, out.Line)
		}
	}
	require.Len(t, lines, 5)
	assert.Equal(t, []string{"one", "two"}, lines[:2])
	assert.Len(t, lines[2], engine.MaxLineBytes)
	assert.Equal(t, "xxxxxxxxxx", lines[3])
	assert.Equal(t, "partial", lines[4])

	data, err := readFile(h.run.AttemptLogPath("a", 1))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(data, "one\r\ntwo\n"), "the log file keeps raw bytes")
}

func TestNodeEnvironment(t *testing.T) {
	yml := `
nodes:
  a:
    run: a
    dir: sub
    env: {FOO: bar}
`
	h := newHarness(t, yml, nil, 0)
	var spec runner.Spec
	h.eng = engine.New(engine.Options{
		Workflow: h.wf,
		Run:      h.run,
		Runner: runnerFunc(func(_ context.Context, s runner.Spec) runner.Result {
			spec = s
			return result(runner.ReasonExit, 0, "")
		}),
		Lender:      h.lender,
		Bus:         nil,
		Concurrency: 0,
		Env:         []string{"BASE=1"},
		Plan:        nil,
		State:       nil,
		Reruns:      false,
		Log:         zerolog.Nop(),
	})
	h.eng.Run(t.Context())

	assert.Contains(t, spec.Env, "BASE=1")
	assert.Contains(t, spec.Env, "FOO=bar")
	assert.Contains(t, spec.Env, "WEFT_NODE=a")
	assert.Contains(t, spec.Env, "WEFT_ATTEMPT=1")
	assert.Contains(t, spec.Env, "WEFT_RUN_ID="+h.run.ID)
	assert.Contains(t, spec.Env, "WEFT_RUN_DIR="+h.run.Dir)
	assert.True(t, strings.HasSuffix(spec.Dir, "/sub"), spec.Dir)
	assert.Equal(t, workflow.DefaultShell, spec.Shell)
}

type runnerFunc func(context.Context, runner.Spec) runner.Result

func (f runnerFunc) Run(ctx context.Context, s runner.Spec) runner.Result { return f(ctx, s) }

func TestLogLinesCarryRunNodeAndAttempt(t *testing.T) {
	h := newHarness(t, deployYAML, nil, 0)
	h.runToEnd(t)

	nodeLines := 0
	for line := range strings.SplitSeq(strings.TrimSpace(h.logBuf.String()), "\n") {
		var entry map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &entry), line)
		assert.Equal(t, h.run.ID, entry["run_id"], line)
		if msg := entry["message"]; msg == "node started" || msg == "node finished" {
			nodeLines++
			assert.NotEmpty(t, entry["node"], line)
			assert.EqualValues(t, 1, entry["attempt"], line)
		}
	}
	assert.Equal(t, 14, nodeLines)
}

func TestAttemptRecords(t *testing.T) {
	h := newHarness(t, deployYAML, map[string]behavior{
		"deploy": {exit: 2, hang: false, signal: "", output: "boom\n", delay: 0},
	}, 0)
	h.runToEnd(t)

	var rec store.AttemptRecord
	readJSON(t, h.run.AttemptRecordPath("deploy", 1), &rec)
	assert.Equal(t, "deploy", rec.Node)
	assert.Equal(t, 1, rec.Attempt)
	assert.Equal(t, "deploy", rec.Command)
	assert.Equal(t, "failed", rec.State)
	assert.Equal(t, events.ReasonExit, rec.Reason)
	assert.Equal(t, 2, rec.ExitCode)
	assert.Equal(t, "deploy.1.log", rec.Log)
	assert.False(t, rec.Started.IsZero())
	assert.False(t, rec.Finished.Before(rec.Started))

	readJSON(t, h.run.AttemptRecordPath("tofu-apply", 1), &rec)
	assert.Equal(t, events.ReasonBlocked, rec.Reason)
}

func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	return string(data), err
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, v), "%s: %s", path, data)
}
