package runner_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/hay-kot/weft/internal/runner"
)

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

func spec(t *testing.T, script string, out *syncBuffer) runner.Spec {
	t.Helper()
	return runner.Spec{
		Node:        "test",
		Shell:       []string{"sh", "-c"},
		Script:      script,
		Dir:         t.TempDir(),
		Env:         os.Environ(),
		Timeout:     0,
		KillTimeout: 5 * time.Second,
		TermGrace:   0,
		Output:      out,
		Interactive: false,
		Kill:        nil,
		Log:         zerolog.Nop(),
	}
}

func newBuf() *syncBuffer { return &syncBuffer{mu: sync.Mutex{}, buf: bytes.Buffer{}} }

// waitFor polls out until it contains s or 5s pass. Scripts print a marker
// once their signal traps are installed, so tests signal only after that.
// It runs in helper goroutines, so it can't fail the test; on a missed
// marker it returns anyway and the test's own assertions report it.
func waitFor(out *syncBuffer, s string) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(out.String(), s) {
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExitCodes(t *testing.T) {
	tests := []struct {
		name    string
		script  string
		code    int
		success bool
	}{
		{"zero", "exit 0", 0, true},
		{"non-zero", "exit 7", 7, false},
		{"killed by signal", "kill -TERM $$", -1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := runner.Process{}.Run(t.Context(), spec(t, tt.script, newBuf()))
			assert.Equal(t, tt.code, res.ExitCode)
			assert.Equal(t, tt.success, res.Success())
			assert.Equal(t, runner.ReasonExit, res.Reason)
			require.NoError(t, res.Err)
		})
	}
}

func TestSignalNamedWhenKilled(t *testing.T) {
	res := runner.Process{}.Run(t.Context(), spec(t, "kill -TERM $$", newBuf()))
	assert.Equal(t, syscall.SIGTERM.String(), res.Signal)
}

func TestStdoutAndStderrShareOnePipeInOrder(t *testing.T) {
	out := newBuf()
	res := runner.Process{}.Run(t.Context(), spec(t, "echo one; echo two >&2; echo three", out))
	require.True(t, res.Success())
	assert.Equal(t, "one\ntwo\nthree\n", out.String())
}

func TestDirAndEnv(t *testing.T) {
	out := newBuf()
	s := spec(t, `pwd; echo "$WEFT_TEST_VAR"`, out)
	s.Env = append(s.Env, "WEFT_TEST_VAR=hello")
	res := runner.Process{}.Run(t.Context(), s)
	require.True(t, res.Success())
	dir, err := filepath.EvalSymlinks(s.Dir)
	require.NoError(t, err)
	assert.Equal(t, dir+"\nhello\n", out.String())
}

func TestStartError(t *testing.T) {
	s := spec(t, "true", newBuf())
	s.Shell = []string{"/nonexistent/shell"}
	res := runner.Process{}.Run(t.Context(), s)
	assert.Equal(t, runner.ReasonStartErr, res.Reason)
	assert.Equal(t, -1, res.ExitCode)
	require.Error(t, res.Err)
	assert.False(t, res.Success())
}

func TestTimeout(t *testing.T) {
	s := spec(t, "sleep 30", newBuf())
	s.Timeout = 100 * time.Millisecond
	start := time.Now()
	res := runner.Process{}.Run(t.Context(), s)
	assert.Equal(t, runner.ReasonTimeout, res.Reason)
	assert.False(t, res.Success())
	assert.Less(t, time.Since(start), 3*time.Second)
}

func TestCancelSendsSIGINTAndWaitsForTrap(t *testing.T) {
	out := newBuf()
	s := spec(t, `trap 'echo got-int; sleep 0.3; echo cleaned; exit 0' INT; echo ready; while :; do sleep 0.05; done`, out)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		waitFor(out, "ready")
		cancel()
	}()
	res := runner.Process{}.Run(ctx, s)

	assert.Equal(t, runner.ReasonCanceled, res.Reason)
	assert.Equal(t, 0, res.ExitCode, "the trap's own exit code is kept")
	assert.Contains(t, out.String(), "got-int\n")
	assert.Contains(t, out.String(), "cleaned\n", "the node gets its grace period")
}

func TestCancelEscalatesToTERMThenKILL(t *testing.T) {
	out := newBuf()
	s := spec(t, `trap 'echo int' INT; trap 'echo term' TERM; echo ready; while :; do sleep 0.05; done`, out)
	s.KillTimeout = 200 * time.Millisecond
	s.TermGrace = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		waitFor(out, "ready")
		cancel()
	}()
	start := time.Now()
	res := runner.Process{}.Run(ctx, s)

	assert.Equal(t, runner.ReasonCanceled, res.Reason)
	assert.Equal(t, syscall.SIGKILL.String(), res.Signal)
	assert.Contains(t, out.String(), "int\n")
	assert.Contains(t, out.String(), "term\n")
	assert.GreaterOrEqual(t, time.Since(start), 400*time.Millisecond)
}

func TestKillSkipsGrace(t *testing.T) {
	out := newBuf()
	s := spec(t, `trap 'echo int' INT; echo ready; while :; do sleep 0.05; done`, out)
	s.KillTimeout = time.Minute
	kill := make(chan struct{})
	s.Kill = kill
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		waitFor(out, "ready")
		cancel()
		waitFor(out, "int")
		close(kill)
	}()
	start := time.Now()
	res := runner.Process{}.Run(ctx, s)
	assert.Equal(t, syscall.SIGKILL.String(), res.Signal)
	assert.Less(t, time.Since(start), 10*time.Second)
}

func TestSIGINTReachesForegroundGrandchild(t *testing.T) {
	out := newBuf()
	inner := `trap 'echo grandchild-int; exit 0' INT; echo ready; while :; do sleep 0.05; done`
	s := spec(t, "sh -c \""+inner+"\"", out)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		waitFor(out, "ready")
		cancel()
	}()
	runner.Process{}.Run(ctx, s)
	assert.Contains(t, out.String(), "grandchild-int")
}

func TestCancelLeavesNoProcessesInTheGroup(t *testing.T) {
	out := newBuf()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	// Background jobs of a non-interactive shell ignore SIGINT, so this
	// sleep survives the signal ladder unless weft reaps the group.
	script := `sleep 30 & echo $! > ` + pidFile + `; trap 'exit 0' INT; echo ready; wait`
	s := spec(t, script, out)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		waitFor(out, "ready")
		cancel()
	}()
	runner.Process{}.Run(ctx, s)

	data, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	assert.Eventually(t, func() bool {
		return syscall.Kill(pid, 0) != nil
	}, 2*time.Second, 20*time.Millisecond, "background child %d still alive", pid)
}

func TestBackgroundChildHoldingPipeDoesNotHang(t *testing.T) {
	out := newBuf()
	start := time.Now()
	res := runner.Process{}.Run(t.Context(), spec(t, "sleep 30 & echo started", out))
	assert.True(t, res.Success())
	assert.Less(t, time.Since(start), 10*time.Second)
	assert.Contains(t, out.String(), "started")
}

// TestHelperWrapper is not a test. It is the wrapper process for
// TestGraceCoversChildOfExitingWrapper: like mise, it starts a child in its
// own process group and exits at once on SIGINT, while the child cleans up.
func TestHelperWrapper(t *testing.T) {
	marker := os.Getenv("WEFT_HELPER_MARKER")
	if marker == "" {
		t.Skip("helper process")
	}
	child := exec.CommandContext(t.Context(), "sh", "-c", `trap 'sleep 0.5; echo done > "$WEFT_HELPER_MARKER"; exit 0' INT; echo ready; while :; do sleep 0.05; done`)
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	<-sigs
	os.Exit(0)
}

func TestGraceCoversChildOfExitingWrapper(t *testing.T) {
	out := newBuf()
	marker := filepath.Join(t.TempDir(), "cleaned")
	s := spec(t, "", out)
	s.Shell = []string{os.Args[0], "-test.run=^TestHelperWrapper$", "-test.v=false"}
	s.Script = "-test.count=1"
	s.Env = append(s.Env, "WEFT_HELPER_MARKER="+marker)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		waitFor(out, "ready")
		cancel()
	}()
	res := runner.Process{}.Run(ctx, s)

	assert.Equal(t, runner.ReasonCanceled, res.Reason)
	data, err := os.ReadFile(marker)
	require.NoError(t, err, "the child was killed before its cleanup finished; output: %s", out.String())
	assert.Equal(t, "done\n", string(data))
}

func TestCanceledBeforeStartRunsNothing(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	res := runner.Process{}.Run(ctx, spec(t, "touch "+marker, newBuf()))
	assert.Equal(t, runner.ReasonCanceled, res.Reason)
	assert.NoFileExists(t, marker)
}

// TestHelperSession is not a test. It prints its PID and session ID for
// TestNodeRunsInItsOwnSession.
func TestHelperSession(t *testing.T) {
	if os.Getenv("WEFT_HELPER_SESSION") == "" {
		t.Skip("helper process")
	}
	sid, err := unix.Getsid(0)
	if err != nil {
		os.Exit(2)
	}
	_, _ = os.Stdout.WriteString("pid=" + strconv.Itoa(os.Getpid()) + " sid=" + strconv.Itoa(sid) + "\n")
	os.Exit(0)
}

func TestNodeRunsInItsOwnSession(t *testing.T) {
	out := newBuf()
	s := spec(t, "-test.count=1", out)
	s.Shell = []string{os.Args[0], "-test.run=^TestHelperSession$", "-test.v=false"}
	s.Env = append(s.Env, "WEFT_HELPER_SESSION=1")
	res := runner.Process{}.Run(t.Context(), s)
	require.True(t, res.Success(), "output: %s", out.String())

	var pid, sid int
	_, err := fmt.Sscanf(strings.TrimSpace(out.String()), "pid=%d sid=%d", &pid, &sid)
	require.NoError(t, err, "output: %s", out.String())
	assert.Equal(t, pid, sid, "a non-interactive node must lead its own session, with no controlling terminal")
}
