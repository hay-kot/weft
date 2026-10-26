package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/engine"
	"github.com/hay-kot/weft/internal/events"
	"github.com/hay-kot/weft/internal/plan"
	"github.com/hay-kot/weft/internal/runner"
	"github.com/hay-kot/weft/internal/store"
	"github.com/hay-kot/weft/internal/workflow"
)

// copyFixture copies testdata/workflows into a temp dir so runs never
// write into the repository.
func copyFixture(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	src, err := filepath.Abs("../../testdata/workflows")
	require.NoError(t, err)
	require.NoError(t, os.CopyFS(filepath.Join(dst, "bin"), os.DirFS(filepath.Join(src, "bin"))))
	data, err := os.ReadFile(filepath.Join(src, "deploy.yml"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dst, "deploy.yml"), data, 0o600))
	return dst
}

func runFixture(t *testing.T, env ...string) (engine.Summary, *store.Run) {
	t.Helper()
	root := copyFixture(t)
	wf, err := workflow.Load(filepath.Join(root, "deploy.yml"), root)
	require.NoError(t, err)
	run, err := store.Create(root, wf.Name, time.Now())
	require.NoError(t, err)

	eng := engine.New(engine.Options{
		Workflow:    wf,
		Run:         run,
		Runner:      runner.Process{},
		Lender:      &fakeLender{err: nil, calls: 0},
		Bus:         events.NewBus(),
		Concurrency: 0,
		Env:         append(os.Environ(), env...),
		Plan:        nil,
		State:       nil,
		Reruns:      false,
		Log:         zerolog.Nop(),
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	return eng.Run(ctx), run
}

func TestFixtureRunsEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real processes for several seconds")
	}
	s, run := runFixture(t)
	assert.True(t, s.Success, "%v", s.States)

	tag, err := os.ReadFile(filepath.Join(run.Dir, "fixture", "release-tag"))
	require.NoError(t, err)
	deployLog, err := os.ReadFile(run.AttemptLogPath("deploy", 1))
	require.NoError(t, err)
	assert.Contains(t, string(deployLog), "rolling out "+string(tag[:len(tag)-1]))
}

func TestFixtureDeployFailureBlocksTofu(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real processes for several seconds")
	}
	s, _ := runFixture(t, "WEFT_FIXTURE_FAIL=deploy")
	assert.False(t, s.Success)
	assert.Equal(t, events.Succeeded, s.States["release"])
	assert.Equal(t, events.Failed, s.States["deploy"])
	assert.Equal(t, events.Blocked, s.States["tofu-apply"])
}

// With deploy failing, a resume of the same run reruns deploy, skips
// release, and deploys the tag the first attempt of release recorded.
func TestFixtureResumeRerunsDeployAndSkipsRelease(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real processes for several seconds")
	}
	root := copyFixture(t)
	wf, err := workflow.Load(filepath.Join(root, "deploy.yml"), root)
	require.NoError(t, err)
	run, err := store.Create(root, wf.Name, time.Now())
	require.NoError(t, err)

	runOnce := func(p *plan.Plan, state *store.State, env ...string) engine.Summary {
		eng := engine.New(engine.Options{
			Workflow: wf, Run: run, Runner: runner.Process{}, Lender: &fakeLender{err: nil, calls: 0},
			Bus: events.NewBus(), Concurrency: 0, Env: append(os.Environ(), env...),
			Plan: p, State: state, Reruns: false, Log: zerolog.Nop(),
		})
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		return eng.Run(ctx)
	}

	first := runOnce(nil, store.NewState(run, wf.Path, time.Now()), "WEFT_FIXTURE_FAIL=deploy")
	require.Equal(t, events.Failed, first.States["deploy"])
	tag, err := os.ReadFile(filepath.Join(run.Dir, "fixture", "release-tag"))
	require.NoError(t, err)

	prior, err := run.LoadState()
	require.NoError(t, err)
	p := plan.Resume(wf, prior)
	second := runOnce(&p, prior.Clone())
	assert.True(t, second.Success, "%v", second.States)
	assert.Equal(t, events.Skipped, second.States["release"])
	assert.Equal(t, events.Succeeded, second.States["deploy"])

	assert.NoFileExists(t, run.AttemptLogPath("release", 2), "release did not run again")
	again, err := os.ReadFile(filepath.Join(run.Dir, "fixture", "release-tag"))
	require.NoError(t, err)
	assert.Equal(t, string(tag), string(again))
	deployLog, err := os.ReadFile(run.AttemptLogPath("deploy", 2))
	require.NoError(t, err)
	assert.Contains(t, string(deployLog), "rolling out "+strings.TrimSpace(string(tag)))
	final, err := run.LoadState()
	require.NoError(t, err)
	assert.Equal(t, 2, final.Nodes["deploy"].Attempt)
	assert.Equal(t, events.Skipped, final.Nodes["release"].State)
}
