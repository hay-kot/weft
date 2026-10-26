package workflow_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hay-kot/weft/internal/workflow"
)

func parseOK(t *testing.T, src string) *workflow.Workflow {
	t.Helper()
	wf, err := workflow.Parse([]byte(src))
	require.NoError(t, err)
	return wf
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func TestParseNodeOrderFollowsDeclaration(t *testing.T) {
	t.Parallel()

	wf := parseOK(t, `
nodes:
  zeta:
    run: echo z
  alpha:
    run: echo a
    needs: [zeta]
  mid:
    run: echo m
    needs: [alpha]
`)

	ids := make([]string, 0, len(wf.Nodes))
	for _, n := range wf.Nodes {
		ids = append(ids, n.ID)
	}
	assert.Equal(t, []string{"zeta", "alpha", "mid"}, ids)
}

func TestParseDefaults(t *testing.T) {
	t.Parallel()

	wf := parseOK(t, "nodes:\n  build:\n    run: make\n")

	assert.Equal(t, workflow.DefaultConcurrency, wf.Concurrency)

	node := wf.Node("build")
	require.NotNil(t, node)
	assert.Equal(t, workflow.DefaultShell, node.Shell)
	assert.Equal(t, workflow.DefaultKillTimeout, node.KillTimeout)
	assert.Zero(t, node.Timeout)
}

func TestParseShell(t *testing.T) {
	t.Parallel()

	wf := parseOK(t, `
shell: sh -c
nodes:
  inherits:
    run: echo one
  overrides:
    run: echo two
    shell: bash -c
`)

	inherits := wf.Node("inherits")
	require.NotNil(t, inherits)
	assert.Equal(t, []string{"sh", "-c"}, inherits.Shell)

	overrides := wf.Node("overrides")
	require.NotNil(t, overrides)
	assert.Equal(t, []string{"bash", "-c"}, overrides.Shell)
}

func TestParseDurations(t *testing.T) {
	t.Parallel()

	wf := parseOK(t, `
nodes:
  slow:
    run: sleep 100
    timeout: 90s
    kill_timeout: 5s
`)

	node := wf.Node("slow")
	require.NotNil(t, node)
	assert.Equal(t, 90*time.Second, node.Timeout)
	assert.Equal(t, 5*time.Second, node.KillTimeout)
}

func TestParseErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "unknown need",
			src: `
nodes:
  a:
    run: echo a
    needs: [ghost]
`,
			want: []string{`node "a": line 5: needs unknown node "ghost"`},
		},
		{
			name: "self need",
			src: `
nodes:
  a:
    run: echo a
    needs: [a]
`,
			want: []string{`node "a": line 5: needs itself`},
		},
		{
			name: "duplicate need",
			src: `
nodes:
  a:
    run: echo a
  b:
    run: echo b
    needs: [a, a]
`,
			want: []string{`node "b": line 7: lists "a" twice in needs`},
		},
		{
			name: "cycle",
			src: `
nodes:
  a:
    run: echo a
    needs: [b]
  b:
    run: echo b
    needs: [a]
`,
			want: []string{"cycle: a -> b -> a"},
		},
		{
			name: "bad ID",
			src: `
nodes:
  Bad_ID:
    run: echo bad
`,
			want: []string{`node ID "Bad_ID" must match`},
		},
		{
			name: "missing run",
			src: `
nodes:
  a:
    label: no command
`,
			want: []string{`node "a": line 3: run is required`},
		},
		{
			name: "unknown node field",
			src: `
nodes:
  a:
    run: echo a
    bogus: 1
`,
			want: []string{`node "a": line 5: unknown field "bogus"`},
		},
		{
			name: "unknown top-level field",
			src: `
other: 1
nodes:
  a:
    run: echo a
`,
			want: []string{`line 2: unknown field "other"; a workflow has concurrency, shell, and nodes`},
		},
		{
			name: "empty file",
			src:  "",
			want: []string{"the file is empty; a workflow needs a nodes: mapping with at least one node"},
		},
		{
			name: "no nodes",
			src:  "concurrency: 2\n",
			want: []string{"no nodes defined"},
		},
		{
			name: "bad duration",
			src: `
nodes:
  a:
    run: echo a
    timeout: soon
`,
			want: []string{`node "a": line 5: timeout must be a positive duration like 90s or 5m, got "soon"`},
		},
		{
			name: "absolute dir",
			src: `
nodes:
  a:
    run: echo a
    dir: /etc
`,
			want: []string{`node "a": line 5: dir must be relative`},
		},
		{
			name: "comment-only file",
			src:  "# nothing yet\n",
			want: []string{"the file is empty"},
		},
		{
			name: "top level is a list",
			src:  "- a\n",
			want: []string{"line 1: a workflow must be a mapping with a nodes: key"},
		},
		{
			name: "nodes left empty",
			src:  "nodes:\n",
			want: []string{"no nodes defined"},
		},
		{
			name: "concurrency not a number",
			src:  "concurrency: lots\nnodes:\n  a:\n    run: echo a\n",
			want: []string{`line 1: concurrency must be a whole number, got "lots"`},
		},
		{
			name: "field shapes",
			src: `
nodes:
  a:
    run: [echo, a]
    needs: b
    interactive: yes
    env: [A=1]
`,
			want: []string{
				`node "a": line 4: run must be a string`,
				`node "a": line 5: needs must be a list of node IDs, such as [build, test]`,
				`node "a": line 6: interactive must be true or false, got "yes"`,
				`node "a": line 7: env must be a mapping of names to values`,
			},
		},
		{
			name: "negative concurrency",
			src: `
concurrency: -1
nodes:
  a:
    run: echo a
`,
			want: []string{"line 2: concurrency must be positive, got -1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			wf, err := workflow.Parse([]byte(tc.src))
			require.Error(t, err)
			assert.Nil(t, wf)

			var verr *workflow.ValidationError
			require.ErrorAs(t, err, &verr)
			joined := strings.Join(verr.Problems, "\n")
			for _, want := range tc.want {
				assert.Contains(t, joined, want)
			}
			assert.NotContains(t, joined, "!!", "YAML tags are decoder internals")
			assert.NotContains(t, joined, "workflow.raw", "Go types are decoder internals")
		})
	}
}

func TestLevels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		src  string
		want map[string]int
	}{
		{
			name: "roots are level 0",
			src: `
nodes:
  a:
    run: echo a
  b:
    run: echo b
`,
			want: map[string]int{"a": 0, "b": 0},
		},
		{
			name: "fan-in takes max plus one",
			src: `
nodes:
  a:
    run: echo a
  b:
    run: echo b
  c:
    run: echo c
    needs: [a, b]
`,
			want: map[string]int{"a": 0, "b": 0, "c": 1},
		},
		{
			name: "level skip",
			src: `
nodes:
  a:
    run: echo a
  b:
    run: echo b
    needs: [a]
  c:
    run: echo c
    needs: [a, b]
`,
			want: map[string]int{"a": 0, "b": 1, "c": 2},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, parseOK(t, tc.src).Levels())
		})
	}
}

func TestDownstreamAndDependents(t *testing.T) {
	t.Parallel()

	wf := parseOK(t, `
nodes:
  a:
    run: echo a
  b:
    run: echo b
    needs: [a]
  d:
    run: echo d
  c:
    run: echo c
    needs: [b]
  e:
    run: echo e
    needs: [d]
  f:
    run: echo f
    needs: [a, c]
`)

	assert.Equal(t, []string{"a", "b", "c", "f"}, wf.Downstream("a"))
	assert.Equal(t, []string{"d", "e"}, wf.Downstream("d"))
	assert.Equal(t, []string{"f"}, wf.Downstream("f"))
	assert.Equal(t, []string{"b", "f"}, wf.Dependents("a"))
	assert.Equal(t, []string{"c"}, wf.Dependents("b"))
	assert.Empty(t, wf.Dependents("f"))
}

func TestFindRoot(t *testing.T) {
	t.Parallel()

	t.Run("walks up to the directory holding .weft", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(root, workflow.DirName), 0o700))
		nested := filepath.Join(root, "a", "b")
		require.NoError(t, os.MkdirAll(nested, 0o700))

		got, err := workflow.FindRoot(nested)
		require.NoError(t, err)
		assert.Equal(t, root, got)
	})

	t.Run("no .weft directory", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		got, err := workflow.FindRoot(dir)
		if err != nil {
			assert.ErrorContains(t, err, ".weft")
			return
		}
		// An ancestor of the temp dir may hold a .weft directory on this machine.
		assert.False(t, strings.HasPrefix(got, dir), "root %q should not be inside %q", got, dir)
	})
}

func TestResolve(t *testing.T) {
	t.Parallel()

	t.Run("name resolves to .weft/<name>.yml", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFile(t, filepath.Join(root, ".weft", "deploy.yml"), "nodes: {}\n")
		cwd := filepath.Join(root, "sub", "nested")
		require.NoError(t, os.MkdirAll(cwd, 0o700))

		path, gotRoot, err := workflow.Resolve(cwd, "deploy")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(root, ".weft", "deploy.yml"), path)
		assert.Equal(t, root, gotRoot)
	})

	t.Run("name falls back to .yaml", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFile(t, filepath.Join(root, ".weft", "ship.yaml"), "nodes: {}\n")

		path, gotRoot, err := workflow.Resolve(root, "ship")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(root, ".weft", "ship.yaml"), path)
		assert.Equal(t, root, gotRoot)
	})

	t.Run("unknown name errors", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(root, workflow.DirName), 0o700))

		_, _, err := workflow.Resolve(root, "missing")
		require.Error(t, err)
		require.ErrorContains(t, err, `"missing" not found`)
		assert.ErrorContains(t, err, "which holds no workflow files")
	})

	t.Run("unknown name lists the workflows there", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFile(t, filepath.Join(root, ".weft", "deploy.yml"), "nodes: {}\n")
		writeFile(t, filepath.Join(root, ".weft", "build.yaml"), "nodes: {}\n")

		_, _, err := workflow.Resolve(root, "deplyo")
		require.Error(t, err)
		assert.ErrorContains(t, err, `"deplyo" not found in `+filepath.Join(root, ".weft")+"; it has build, deploy")
	})

	t.Run("path argument uses its directory as root", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFile(t, filepath.Join(root, "sub", "flow.yml"), "nodes: {}\n")

		path, gotRoot, err := workflow.Resolve(root, "sub/flow.yml")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(root, "sub", "flow.yml"), path)
		assert.Equal(t, filepath.Join(root, "sub"), gotRoot)
	})

	t.Run("path inside .weft uses the parent of .weft as root", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		writeFile(t, filepath.Join(root, ".weft", "flow.yml"), "nodes: {}\n")

		path, gotRoot, err := workflow.Resolve(root, ".weft/flow.yml")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(root, ".weft", "flow.yml"), path)
		assert.Equal(t, root, gotRoot)
	})
}

func TestList(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dir := filepath.Join(root, workflow.DirName)
	writeFile(t, filepath.Join(dir, "deploy.yml"), "nodes: {}\n")
	writeFile(t, filepath.Join(dir, "build.yaml"), "nodes: {}\n")
	writeFile(t, filepath.Join(dir, "notes.txt"), "not a workflow\n")
	writeFile(t, filepath.Join(dir, "README.md"), "# readme\n")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "folder.yml"), 0o700))

	names, err := workflow.List(root)
	require.NoError(t, err)
	assert.Equal(t, []string{"build", "deploy"}, names)
}

func TestListMissingDir(t *testing.T) {
	t.Parallel()

	_, err := workflow.List(t.TempDir())
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestLoad(t *testing.T) {
	t.Parallel()

	t.Run("sets name, path, root, and hash", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		path := filepath.Join(root, workflow.DirName, "deploy.yml")
		src := "nodes:\n  build:\n    run: make\n"
		writeFile(t, path, src)

		wf, err := workflow.Load(path, root)
		require.NoError(t, err)

		assert.Equal(t, "deploy", wf.Name)
		assert.Equal(t, path, wf.Path)
		assert.Equal(t, root, wf.Root)

		sum := sha256.Sum256([]byte(src))
		assert.Equal(t, hex.EncodeToString(sum[:]), wf.Hash)
		assert.Len(t, wf.Hash, 64)
	})

	t.Run("validation error carries the file path", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		path := filepath.Join(root, workflow.DirName, "broken.yml")
		writeFile(t, path, "nodes:\n  Bad_ID:\n    run: echo\n")

		wf, err := workflow.Load(path, root)
		require.Error(t, err)
		assert.Nil(t, wf)

		var verr *workflow.ValidationError
		require.ErrorAs(t, err, &verr)
		assert.Equal(t, path, verr.Path)
	})
}
