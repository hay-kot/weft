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
  error. Other nodes run without a controlling terminal, so a command that
  tries to prompt on `/dev/tty` (`sudo`, an SSH passphrase) fails instead of
  hanging or drawing over the screen. Mark such a node `interactive`.
- Ctrl-C cancels the run. Running nodes get SIGINT, their `kill_timeout`, then
  SIGTERM and 5s more, then SIGKILL. Each node runs in its own process group,
  so `mise` and whatever it started get the signal together. A second Ctrl-C
  sends SIGKILL at once. Ctrl-C while an interactive node runs goes to that
  node, and weft then cancels the rest of the run. SIGTERM and SIGHUP cancel
  the same way as Ctrl-C.
- Output to a pipe never slows the run down. If stdout is a pager you
  paused, or a reader that stopped, weft keeps scheduling and canceling on
  time and skips lines the reader can't take (it says how many). The node
  logs on disk always have everything. weft also keeps going when stdout
  closes early, as with `weft run deploy | head`.

## The run view

When stdin and stdout are terminals and `CI` is unset, `weft run` shows the
graph live, with the selected node's output under it:

```text
 deploy  running  1:33  4/7 done                                   run 20261025T191200-a3f9
 run dir /src/app/.weft/runs/deploy/20261025T191200-a3f9
───────────────────────────────────────────────────────────────────────────────────────────
               ┌─▸ ✓ go-check  0:02 ─┐
 ✓ guard 0:00 ─┼─▸ ✓ ui-check  0:02 ─┼─▸ ⠹ release 1:31 ───▸ ○ deploy ───▸ ○ tofu-apply
               └─▸ ✓ db-backup 0:01 ─┘
───────────────────────────────────────────────────────────────────────────────────────────
 › release  ● running  1:31  mise run release
 • building binaries  layout=dist
───────────────────────────────────────────────────────────────────────────────────────────
 hjkl select • enter log • ctrl+c cancel • q quit • ? help • f follow • v layout
```

Icons: `○` pending, a spinner for running, `◆` waiting for the terminal,
`✓` succeeded, `✗` failed, `⊘` blocked or canceled.

The selection follows the run (the node that just started, or the first
failure) until you move it. Keys:

| Key | Action |
| --- | --- |
| `h` `j` `k` `l`, arrows | Move the selection on screen |
| `enter` | Full-screen log of the selected node (`esc` or `q` goes back) |
| `pgup` `pgdn`, `ctrl+u` `ctrl+d` | Scroll the log; `g` and `G` jump to the top and bottom |
| `f` | Pause or resume following the log's newest lines |
| `v` | Switch between the horizontal and vertical graph |
| `ctrl+c` | Cancel the run; again to kill the running nodes |
| `q` | Quit. During a run it asks first, and `y` cancels the run and quits once the nodes stop |
| `?` | Show every key |

In the full-screen log, `j` `k` scroll by a line and `h` `l` scroll
sideways through long lines. The TUI keeps the last 10,000 lines of each
node; the node log on disk has all of them, and its path is in the
full-screen log's title.

When an interactive node runs, the TUI steps aside and the node gets the
terminal. The TUI comes back when the node exits. When the run ends, the
TUI stays open until you press `q` (or quits at once with `--exit-on-end`),
and weft exits with the run's status either way. weft's own log never goes
to the screen in the TUI; it is in the run's `weft.log`.

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
| `--ui=auto\|tui\|plain` | Output mode. `auto` picks `tui` when stdin and stdout are terminals, `CI` is unset, and `TERM` is not `dumb`; `plain` prints prefixed lines |
| `--exit-on-end` | Quit the TUI as soon as the run ends |
| `--concurrency`, `-j` | Override the workflow's `concurrency` |

Global flags: `--log-level` (or `WEFT_LOG_LEVEL`), `--no-color` (or
`NO_COLOR`), `--json` (for `validate`).

## Run directories and logs

Each run gets a sortable ID such as `20261025T191200-a3f9` and a directory
at `.weft/runs/<workflow>/<run-id>/`. The TUI shows both in its header, and
weft prints them to stderr when the run ends (and when it starts, in plain
mode). The newest 20 runs per workflow are kept. On first use weft
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
