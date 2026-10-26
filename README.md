# weft

Run task-runner tasks as a DAG with a live view in your terminal.

weft reads a workflow file of shell commands and the order they depend on.
It runs independent nodes in parallel, stops downstream work when something
fails, and gives terminal-hungry steps (like `tofu apply`) the real terminal.
Commands are opaque shell, so `mise run`, `task`, `make`, or plain scripts
all work.

Linux and macOS only.

## Installation

```bash
go install github.com/hay-kot/weft@latest
```

## Writing a workflow

Put one workflow per file in `.weft/<name>.yml` at the root of your project.
`weft run deploy` loads `.weft/deploy.yml` from the nearest directory above
the current one that holds a `.weft/` directory. Commands run from that
directory unless a node sets `dir`.

```yaml
# .weft/deploy.yml
concurrency: 4
nodes:
  guard:
    run: test "$(git branch --show-current)" = main
  go-check:
    needs: [guard]
    run: mise run go:check
  ui-check:
    needs: [guard]
    run: mise run ui:check
  release:
    needs: [go-check, ui-check]
    run: mise run release
  tofu-apply:
    needs: [release]
    interactive: true
    run: mise run tofu:apply
```

Top-level fields:

| Field | Meaning |
| --- | --- |
| `concurrency` | Most nodes running at once (default 4) |
| `shell` | Shell for every node, split on spaces (default `bash -eo pipefail -c`) |
| `nodes` | Map of node ID to node. IDs match `^[a-z0-9][a-z0-9-]*$` |

Node fields:

| Field | Meaning |
| --- | --- |
| `run` | Script passed to the shell. Required |
| `needs` | IDs of nodes that must succeed first |
| `label` | Display name (default: the ID) |
| `dir` | Working directory, relative to the workflow root |
| `env` | Extra environment variables |
| `shell` | Shell for this node |
| `interactive` | Needs the terminal: runs alone, attached to your terminal |
| `timeout` | Cancel the node after this long, for example `10m` |
| `kill_timeout` | Grace period after SIGINT on cancel (default `30s`) |
| `continue_on_error` | A failure does not block downstream nodes |

Every node also gets `WEFT_WORKFLOW`, `WEFT_ROOT`, `WEFT_RUN_ID`,
`WEFT_RUN_DIR`, `WEFT_NODE`, and `WEFT_ATTEMPT` in its environment.

### How a run behaves

- A node starts once all its `needs` succeeded (or failed with
  `continue_on_error`) and a concurrency slot is free.
- When a node fails, everything downstream of it is `blocked`. Independent
  branches keep going, and weft exits non-zero.
- An interactive node waits until nothing else is running, and nothing else
  starts while it runs. Its output goes straight to your terminal and is not
  captured. It needs a terminal on stdin; without one it fails with a clear
  error.
- Ctrl-C cancels the run. Running nodes get SIGINT, their `kill_timeout`, then
  SIGTERM and 5s more, then SIGKILL. Each node runs in its own process group,
  so `mise` and whatever it started get the signal together. A second Ctrl-C
  sends SIGKILL at once. Ctrl-C while an interactive node runs goes to that
  node, and weft then cancels the rest of the run.
- Output to a pipe never slows the run down. If stdout is a pager you
  paused, or a reader that stopped, weft keeps scheduling and canceling on
  time and skips lines the reader can't take (it says how many). The node
  logs on disk always have everything. weft also keeps going when stdout
  closes early, as with `weft run deploy | head`.

## Commands

```text
weft run <workflow>        run a workflow
weft validate [workflow]   check one workflow, or every file in .weft/
weft graph <workflow>      print the workflow's graph
```

`<workflow>` is a name (`deploy`) or a path to a workflow file.

`weft graph` prints the layered graph the run view uses:

```text
       ┌─▸ go-check ──┐
guard ─┼─▸ ui-check ──┼─▸ release ───▸ deploy ───▸ tofu-apply
       └─▸ db-backup ─┘
```

Levels run left to right. When that is wider than the terminal, they run
top to bottom instead. `--layout=horizontal|vertical` forces one, `--width`
sets the width to fit (default: the terminal's), and `--json` prints nodes,
levels, and the drawn lines. Where many nodes fan into many, edges share a
trunk, so the drawing can suggest an edge that isn't there; `needs` in the
file is the source of truth.

`weft run` flags:

| Flag | Meaning |
| --- | --- |
| `--ui=auto\|plain` | Output mode. `plain` prints prefixed lines |
| `--concurrency`, `-j` | Override the workflow's `concurrency` |

Global flags: `--log-level` (or `WEFT_LOG_LEVEL`), `--no-color` (or
`NO_COLOR`), `--json` (for `validate`).

## Run directories and logs

Each run gets a sortable ID such as `20261025T191200-a3f9` and a directory
at `.weft/runs/<workflow>/<run-id>/`. weft prints both when the run starts
and when it ends. The newest 20 runs per workflow are kept. On first use weft
writes `.weft/.gitignore` so runs stay out of git.

```text
.weft/runs/deploy/20261025T191200-a3f9/
  weft.log                 weft's own JSON log; every line has run_id
  events.jsonl             every state change, in order, with timestamps
  nodes/<node>/<n>.log     output of attempt n (stdout and stderr together)
  nodes/<node>/<n>.json    attempt n: command, dir, times, exit code, signal, reason
```

Run with `--log-level=debug` to have `weft.log` explain each scheduling
decision and every signal sent. Log files are created with mode 0600 because
command output can hold secrets.

## Development

```bash
mise run check   # tidy, lint, test
mise run smoke   # run the binary against the stub fixture in a private tmux server
```

`testdata/workflows/deploy.yml` is a stub with the shape of a real
release-and-deploy pipeline. Its commands are scripts under
`testdata/workflows/bin/` that only sleep and print. Set
`WEFT_FIXTURE_FAIL=deploy` to make a node fail, `WEFT_FIXTURE_HANG=db-backup`
to make one run until canceled, or `WEFT_FIXTURE_FLOOD=guard` to make one
print about 10 MB.
