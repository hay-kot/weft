package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/report"
	"github.com/hay-kot/weft/internal/store"
)

const deployName = "deploy"

func app() *cli.Command {
	flags := &Flags{LogLevel: "info", NoColor: true}
	a := &cli.Command{Name: "weft"} //nolint:exhaustruct // a test app
	a = NewRunCmd(flags).Register(a)
	a = NewValidateCmd(flags).Register(a)
	a = NewGraphCmd(flags).Register(a)
	a = NewRunsCmd(flags).Register(a)
	a = NewStatusCmd(flags).Register(a)
	a = NewLogsCmd(flags).Register(a)
	return NewDocCmd(flags).Register(a)
}

// The doc is the reference agents trust, so it has to name everything
// they can see in weft's output.
func TestDocCoversCommandsStatesAndReasons(t *testing.T) {
	for _, c := range app().Commands {
		assert.Contains(t, agentDoc, "weft "+c.Name, "command %s", c.Name)
	}
	for _, s := range []events.State{
		events.Pending, events.Ready, events.Running, events.Waiting, events.Succeeded,
		events.Failed, events.Blocked, events.Canceled, events.Skipped, events.Excluded,
	} {
		assert.Contains(t, agentDoc, "`"+string(s)+"`", "state %s", s)
	}
	for _, r := range []events.Reason{
		events.ReasonExit, events.ReasonTimeout, events.ReasonCanceled, events.ReasonBlocked,
		events.ReasonStartError, events.ReasonSkipped, events.ReasonExcluded, events.ReasonAbandoned,
	} {
		assert.Contains(t, agentDoc, "`"+string(r)+"`", "reason %s", r)
	}
	for _, s := range []report.Status{report.Running, report.Succeeded, report.Failed, report.Canceled, report.Interrupted} {
		assert.Contains(t, agentDoc, "`"+string(s)+"`", "status %s", s)
	}
}

// project makes a workflow root with one failed run and changes into it.
func project(t *testing.T) *store.Run {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".weft"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".weft", "deploy.yml"),
		[]byte("nodes:\n  release: {run: x}\n  deploy: {run: y, needs: [release]}\n"), 0o600))
	t.Chdir(root)

	run, err := store.Create(root, deployName, time.Now())
	require.NoError(t, err)
	st := store.NewState(run, filepath.Join(root, ".weft", "deploy.yml"), time.Now())
	st.Nodes["release"] = store.NodeState{
		State: events.Succeeded, Attempt: 1, ExitCode: 0, Reason: events.ReasonExit, Error: "",
		Started: time.Now(), Finished: time.Now(), Log: "release.1.log", PGID: 0,
	}
	st.Nodes[deployName] = store.NodeState{
		State: events.Failed, Attempt: 2, ExitCode: 1, Reason: events.ReasonExit, Error: "exit code 1",
		Started: time.Now(), Finished: time.Now(), Log: "deploy.2.log", PGID: 0,
	}
	require.NoError(t, run.WriteState(st))
	for _, a := range []struct {
		node, text string
		n          int
	}{{"release", "released\n", 1}, {deployName, "first try\n", 1}, {deployName, "line 1\nline 2\nboom\n", 2}} {
		f, err := run.CreateAttemptLog(a.node, a.n)
		require.NoError(t, err)
		_, err = f.WriteString(a.text)
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
	return run
}

func runApp(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	a := app()
	a.Writer = &out
	err := a.Run(context.Background(), append([]string{"weft"}, args...))
	return out.String(), err
}

func TestLogs(t *testing.T) {
	run := project(t)

	out, err := runApp(t, "logs", deployName, deployName)
	require.NoError(t, err)
	assert.Equal(t, "line 1\nline 2\nboom\n", out, "the latest attempt by default")

	out, err = runApp(t, "logs", deployName, deployName, "--attempt", "1")
	require.NoError(t, err)
	assert.Equal(t, "first try\n", out)

	out, err = runApp(t, "logs", deployName, deployName, "--tail", "1", "--run", run.ID)
	require.NoError(t, err)
	assert.Equal(t, "boom\n", out)

	_, err = runApp(t, "logs", deployName, "nope")
	require.EqualError(t, err, `run `+run.ID+` has no node "nope"; its nodes are deploy, release`)
	_, err = runApp(t, "logs", deployName, deployName, "--attempt", "3")
	require.EqualError(t, err, "deploy has attempts 1 to 2 in run "+run.ID+", not 3")
}

func TestStatusAndRuns(t *testing.T) {
	run := project(t)

	out, err := runApp(t, "status", deployName)
	require.NoError(t, err)
	assert.Contains(t, out, "deploy run "+run.ID+": failed")
	assert.Contains(t, out, "  boom\n")
	assert.Contains(t, out, "weft run deploy --resume")

	out, err = runApp(t, "status", deployName, "--json", "--tail", "0")
	require.NoError(t, err)
	var r report.Run
	require.NoError(t, json.Unmarshal([]byte(out), &r))
	assert.Equal(t, report.Failed, r.Status)
	require.Len(t, r.Nodes, 2)
	assert.Nil(t, r.Nodes[1].LogTail, "--tail 0 leaves the tails out")

	out, err = runApp(t, "runs", deployName, "--json")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 1)
	var s report.Summary
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &s))
	assert.Equal(t, []string{deployName}, s.Unfinished)
}

func TestNoRunsYet(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".weft"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".weft", "deploy.yml"), []byte("nodes:\n  a: {run: x}\n"), 0o600))
	t.Chdir(root)

	_, err := runApp(t, "status", deployName)
	require.EqualError(t, err, "deploy has no runs yet; start one with 'weft run deploy'")
	out, err := runApp(t, "runs", deployName)
	require.NoError(t, err)
	assert.Equal(t, "deploy has no runs yet; start one with 'weft run deploy'\n", out)
	out, err = runApp(t, "runs", deployName, "--json")
	require.NoError(t, err)
	assert.Empty(t, out, "no runs is no lines, not an error")
}

func TestAttemptLogForANodeThatNeverRan(t *testing.T) {
	run, err := store.Create(t.TempDir(), deployName, time.Now())
	require.NoError(t, err)
	st := store.NewState(run, "", time.Now())
	st.Nodes["skip"] = store.NodeState{
		State: events.Excluded, Attempt: 0, ExitCode: 0, Reason: "", Error: "",
		Started: time.Time{}, Finished: time.Time{}, Log: "", PGID: 0,
	}
	st.Nodes["../up"] = st.Nodes["skip"]
	require.NoError(t, run.WriteState(st))

	_, err = attemptLog(run, "skip", 0)
	require.EqualError(t, err, "skip never ran in run "+run.ID+" (it is excluded)")
	_, err = attemptLog(run, "../up", 0)
	require.EqualError(t, err, "run "+run.ID+` has no node "../up"; its nodes are skip`)
}

func TestShellCompletionOffersWorkflowsAndNodes(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".weft"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".weft", "deploy.yml"), []byte("nodes:\n  build: {run: x}\n  ship: {run: y, needs: [build]}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".weft", "check.yml"), []byte("nodes:\n  lint: {run: x}\n"), 0o600))
	t.Chdir(root)

	complete := func(args ...string) string {
		t.Helper()
		a := app()
		a.EnableShellCompletion = true
		var out bytes.Buffer
		a.Writer = &out
		require.NoError(t, a.Run(context.Background(), append(append([]string{"weft"}, args...), "--generate-shell-completion")))
		return out.String()
	}
	assert.Equal(t, "check\ndeploy\n", complete("run"))
	assert.Equal(t, "check\ndeploy\n", complete("status", "dep"), "a name typed in part still lists the workflows")
	assert.Empty(t, complete("run", "deploy"), "the one argument is complete")
	assert.Equal(t, "check\ndeploy\n", complete("logs"))
	assert.Equal(t, "build\nship\n", complete("logs", "deploy"))
	assert.Empty(t, complete("logs", "deploy", "build"))
	assert.Contains(t, complete("run", "--u"), "--ui", "flags still complete")
	assert.Contains(t, complete("logs", "deploy", "--"), "--attempt")
}
