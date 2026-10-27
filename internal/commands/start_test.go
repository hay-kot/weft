package commands

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/store"
	"github.com/hay-kot/weft/internal/workflow"
)

const startYAML = `
nodes:
  build: {run: "true"}
  release: {run: "true", needs: [build]}
  deploy: {run: "true", needs: [release]}
  docs: {run: "true"}
`

func sel(resume bool, from string, only ...string) selection {
	return selection{resume: resume, from: from, only: only, force: false}
}

func loadStart(t *testing.T) *workflow.Workflow {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "deploy.yml")
	require.NoError(t, os.WriteFile(path, []byte(startYAML), 0o600))
	wf, err := workflow.Load(path, root)
	require.NoError(t, err)
	return wf
}

func nodeState(s events.State, attempt int) store.NodeState {
	return store.NodeState{
		State: s, Attempt: attempt, ExitCode: 0, Reason: "", Error: "",
		Started: time.Time{}, Finished: time.Time{}, Log: "", PGID: 0,
	}
}

// priorRun writes a finished earlier run in which deploy failed.
func priorRun(t *testing.T, wf *workflow.Workflow, head string) *store.Run {
	t.Helper()
	run, err := store.Create(wf.Root, wf.Name, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	st := store.NewState(run, wf.Path, time.Now().Add(-time.Hour))
	st.History = []store.Start{{
		At: st.Created, PID: 1, Mode: store.ModeRun, Nodes: nil,
		WorkflowHash: wf.Hash, GitHead: head, Forced: false,
	}}
	st.Nodes["build"] = nodeState(events.Succeeded, 1)
	st.Nodes["release"] = nodeState(events.Succeeded, 1)
	st.Nodes["deploy"] = nodeState(events.Failed, 1)
	st.Nodes["docs"] = nodeState(events.Succeeded, 1)
	require.NoError(t, run.WriteState(st))
	return run
}

func TestSelectionValidate(t *testing.T) {
	assert.NoError(t, sel(false, "").validate())
	assert.NoError(t, sel(true, "").validate())
	require.EqualError(t, sel(true, "deploy").validate(), "--resume, --from, and --only can't be combined; pick one")
	require.EqualError(t, sel(false, "deploy", "a").validate(), "--resume, --from, and --only can't be combined; pick one")
	forced := sel(false, "")
	forced.force = true
	require.EqualError(t, forced.validate(), "--force only applies with --resume, --from, or --only")
}

func TestPrepareFreshRun(t *testing.T) {
	wf := loadStart(t)
	prep, err := prepare(t.Context(), wf, sel(false, ""), time.Now())
	require.NoError(t, err)
	defer prep.lock.Release()

	assert.False(t, prep.plan.Continue)
	assert.Nil(t, prep.prior)
	require.Len(t, prep.state.History, 1)
	assert.Equal(t, store.ModeRun, prep.state.History[0].Mode)
	assert.Equal(t, wf.Hash, prep.state.History[0].WorkflowHash)
	assert.Equal(t, os.Getpid(), prep.state.History[0].PID)

	_, err = prep.run.Lock()
	require.Error(t, err, "the new run is locked")
}

func TestPrepareResume(t *testing.T) {
	wf := loadStart(t)
	run := priorRun(t, wf, "")

	prep, err := prepare(t.Context(), wf, sel(true, ""), time.Now())
	require.NoError(t, err)
	defer prep.lock.Release()

	assert.Equal(t, run.ID, prep.run.ID, "resume carries on in the same directory")
	assert.True(t, prep.plan.Continue)
	assert.Equal(t, []string{"deploy"}, prep.plan.Pending(wf))
	assert.Equal(t, events.Skipped, prep.plan.Nodes["release"].State)
	assert.Equal(t, 2, prep.plan.Nodes["deploy"].Attempt)
	assert.Empty(t, prep.drift)
	require.Len(t, prep.state.History, 2)
	assert.Equal(t, store.ModeResume, prep.state.Last().Mode)
	assert.False(t, prep.state.Last().Forced)
	assert.Equal(t, "--resume continues run "+run.ID+": running deploy; skipping 3 that succeeded",
		startNotice(wf, prep.plan, run.ID, sel(true, "")))
}

func TestPrepareResumeErrors(t *testing.T) {
	t.Run("no earlier run", func(t *testing.T) {
		wf := loadStart(t)
		_, err := prepare(t.Context(), wf, sel(true, ""), time.Now())
		require.EqualError(t, err, "--resume: workflow deploy has no earlier run to resume; start one with weft run deploy")
	})
	t.Run("no state.json", func(t *testing.T) {
		wf := loadStart(t)
		run, err := store.Create(wf.Root, wf.Name, time.Now())
		require.NoError(t, err)
		_, err = prepare(t.Context(), wf, sel(true, ""), time.Now())
		require.ErrorContains(t, err, "run "+run.ID+" has no state.json")
	})
	t.Run("nothing left", func(t *testing.T) {
		wf := loadStart(t)
		run := priorRun(t, wf, "")
		st, err := run.LoadState()
		require.NoError(t, err)
		st.Nodes["deploy"] = nodeState(events.Succeeded, 1)
		require.NoError(t, run.WriteState(st))
		_, err = prepare(t.Context(), wf, sel(true, ""), time.Now())
		require.ErrorIs(t, err, errNothingToRun)
	})
	t.Run("locked by another weft", func(t *testing.T) {
		wf := loadStart(t)
		run := priorRun(t, wf, "")
		lock, err := run.Lock()
		require.NoError(t, err)
		defer lock.Release()
		_, err = prepare(t.Context(), wf, sel(true, ""), time.Now())
		require.ErrorContains(t, err, "--resume: run "+run.ID+" is in use by another weft process")
	})
}

func TestPrepareDrift(t *testing.T) {
	wf := loadStart(t)
	run := priorRun(t, wf, "0123456789abcdef0123")
	prep, err := prepare(t.Context(), wf, sel(true, ""), time.Now())
	require.NoError(t, err)
	defer prep.lock.Release()

	head := gitHead(t.Context(), wf.Root)
	require.Empty(t, head, "the temp dir is not a git repository")
	assert.Equal(t, []string{"git HEAD moved from 0123456789ab to none"}, prep.drift)
	assert.True(t, prep.state.Last().Forced, "the start records that it went ahead anyway")
	assert.EqualError(t, driftError(sel(true, ""), run.ID, prep.drift),
		"--resume takes results from run "+run.ID+", but since it last started git HEAD moved from 0123456789ab to none. "+
			"Those results may not match what would run now. Pass --force to go ahead anyway, or start a new run")
}

func TestPrepareFromAndOnly(t *testing.T) {
	t.Run("from deploy continues the latest run", func(t *testing.T) {
		wf := loadStart(t)
		run := priorRun(t, wf, "")
		prep, err := prepare(t.Context(), wf, sel(false, "deploy"), time.Now())
		require.NoError(t, err)
		defer prep.lock.Release()
		assert.Equal(t, run.ID, prep.run.ID)
		assert.Equal(t, []string{"deploy"}, prep.plan.Pending(wf))
		assert.Equal(t, []string{"deploy"}, prep.state.Last().Nodes)
	})
	t.Run("only docs needs nothing earlier and starts a new run", func(t *testing.T) {
		wf := loadStart(t)
		run := priorRun(t, wf, "")
		prep, err := prepare(t.Context(), wf, sel(false, "", "docs"), time.Now())
		require.NoError(t, err)
		defer prep.lock.Release()
		assert.NotEqual(t, run.ID, prep.run.ID)
		assert.Nil(t, prep.prior)
		assert.Equal(t, events.Excluded, prep.plan.Nodes["deploy"].State)
		assert.Len(t, prep.state.History, 1)
		assert.Equal(t, "--only docs: running docs; leaving out build, release, deploy",
			startNotice(wf, prep.plan, prep.run.ID, sel(false, "", "docs")))
		_, err = run.Lock()
		require.NoError(t, err, "the latest run's lock was released")
	})
	t.Run("only deploy without an earlier run", func(t *testing.T) {
		wf := loadStart(t)
		_, err := prepare(t.Context(), wf, sel(false, "", "deploy"), time.Now())
		require.EqualError(t, err, "--only deploy: deploy needs release, which is not selected, and there is no earlier run to take its result from")
	})
	t.Run("from an unknown node", func(t *testing.T) {
		wf := loadStart(t)
		_, err := prepare(t.Context(), wf, sel(false, "deplyo"), time.Now())
		require.EqualError(t, err, `--from deplyo: workflow deploy has no node "deplyo" (nodes: build, release, deploy, docs)`)
	})
}

func TestPrepareLostAttempts(t *testing.T) {
	wf := loadStart(t)
	run := priorRun(t, wf, "")
	st, err := run.LoadState()
	require.NoError(t, err)

	// A process group that still exists: this test's own.
	alive := exec.CommandContext(context.WithoutCancel(t.Context()), "sleep", "30")
	alive.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, alive.Start())
	defer func() { _ = alive.Process.Kill(); _ = alive.Wait() }()

	deploy := nodeState(events.Running, 2)
	deploy.PGID = alive.Process.Pid
	deploy.Log = "nodes/deploy/2.log"
	st.Nodes["deploy"] = deploy
	require.NoError(t, run.WriteState(st))

	prep, err := prepare(t.Context(), wf, sel(true, ""), time.Now())
	require.NoError(t, err)
	defer prep.lock.Release()
	require.Len(t, prep.alive, 1)
	assert.Equal(t, lostAttempt{node: "deploy", attempt: 2, pgid: alive.Process.Pid}, prep.alive[0])
	require.ErrorContains(t, aliveError(sel(true, ""), run.ID, prep.alive), "deploy (attempt 2, process group")
	assert.Equal(t, 3, prep.plan.Nodes["deploy"].Attempt)

	require.NoError(t, prep.recordLost(wf))
	data, err := os.ReadFile(run.AttemptRecordPath("deploy", 2))
	require.NoError(t, err)
	assert.Contains(t, string(data), `"reason": "abandoned"`)
	assert.Contains(t, string(data), `"log": "nodes/deploy/2.log"`)

	_ = alive.Process.Kill()
	_ = alive.Wait()
	assert.False(t, groupAlive(alive.Process.Pid), "an empty group is gone")
}

func TestGitHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	git("init", "-q")
	assert.Empty(t, gitHead(t.Context(), dir), "a repository with no commits has no HEAD")
	git("commit", "-q", "--allow-empty", "-m", "x")
	assert.Len(t, gitHead(t.Context(), dir), 40)
}

func TestPrepareFreshRunWritesStateBeforeTheEngine(t *testing.T) {
	wf := loadStart(t)
	prep, err := prepare(t.Context(), wf, sel(false, ""), time.Now())
	require.NoError(t, err)
	defer prep.lock.Release()

	st, err := prep.run.LoadState()
	require.NoError(t, err, "a weft killed before the engine's first write leaves a readable run")
	assert.Len(t, st.History, 1)
	assert.Equal(t, events.Pending, st.Nodes["build"].State)
	assert.Equal(t, 1, st.Nodes["build"].Attempt)
	assert.Len(t, st.Nodes, len(wf.Nodes))
}

func TestGroupAliveRejectsInitAndEveryone(t *testing.T) {
	assert.False(t, groupAlive(0), "kill(0) would reach weft's own group")
	assert.False(t, groupAlive(1), "kill(-1) would reach every process")
	assert.True(t, groupAlive(syscall.Getpgrp()))
}
