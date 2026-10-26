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
directory unless a node sets `dir`. The file is plain YAML: aliases
(`*name`), merge keys (`<<`), and a second document are rejected, so a
workflow reads the same way to you and to weft.

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
| `shell` | Shell for every node, split on spaces with no quoting (default `bash -eo pipefail -c`) |
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
| `continue_on_error` | A failure does not block downstream nodes or fail the run |

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
  error. Ctrl-Z does nothing while it has the terminal: weft can't hand a
  suspended node to your shell, and a stopped one would leave the terminal
  stuck. The terminal's settings are put back when the node ends, even if
  it was killed inside a hidden-input prompt. Other nodes run without a
  controlling terminal, so a command that tries to prompt on `/dev/tty`
  (`sudo`, an SSH passphrase) fails instead of hanging or drawing over the
  screen. Mark such a node `interactive`.
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
 deploy  running  1:33  4/7 done                               run 20261025T191200.482-a3f9
 run dir /src/app/.weft/runs/deploy/20261025T191200.482-a3f9
───────────────────────────────────────────────────────────────────────────────────────────
               ┌─▸ ✓ go-check  0:02 ─┐
 ✓ guard 0:00 ─┼─▸ ✓ ui-check  0:02 ─┼─▸ ⠹ release 1:31 ───▸ ○ deploy ───▸ ○ tofu-apply
               └─▸ ✓ db-backup 0:01 ─┘
───────────────────────────────────────────────────────────────────────────────────────────
 › release  ● running  1:31  mise run release
 • building binaries  layout=dist
───────────────────────────────────────────────────────────────────────────────────────────
 hjkl select • enter log • ctrl+c cancel • q quit • ? help • r rerun • R rerun+downstream
```

Icons: `○` pending, a spinner for running, `◆` waiting for the terminal,
`✓` succeeded (dim when skipped by a resume), `✗` failed, `⊘` blocked or
canceled, `·` excluded by `--from` or `--only`.

The selected node's label is bold and underlined, and its edges are bold.
The selection follows the run (the node that just started, or the first
failure) until you move it. Keys:

| Key | Action |
| --- | --- |
| `h` `j` `k` `l`, arrows | Move the selection on screen |
| `enter` | Full-screen log of the selected node (`esc` or `q` goes back) |
| `pgup` `pgdn`, `ctrl+u` `ctrl+d` | Scroll the log; `g` and `G` jump to the top and bottom |
| `r` | Rerun the selected node, if its needs are satisfied |
| `R` | Rerun the selected node and everything downstream of it |
| `x` | Cancel the selected node: SIGINT and its `kill_timeout` if it runs, again to kill it. Nodes that need it are blocked; the rest of the run goes on |
| `f` | Pause or resume following the log's newest lines |
| `v` | Switch between the horizontal and vertical graph |
| `i` | Details of the selected node: needs, what needs it, command, dir, shell, timeouts, and its latest attempt with exit code and log path |
| `ctrl+c` | Cancel the run; again to kill the running nodes |
| `q` | Quit. During a run it asks first, and Yes cancels the run and quits once the nodes stop |
| `?` | A dialog with every key of the current view, in sections |

Help, node details, and questions open as dialogs over the screen. The
help and the details scroll with `j` `k` when they are taller than the
screen and close with `esc`. A question (quitting mid-run, starting or
rerunning after the workflow file or git HEAD changed) has Yes and No
buttons: `y` and `n` answer it, `←` `→` or `tab` and `enter` pick a button,
and No is selected first. Other keys do nothing until it is answered.
`ctrl+c` still cancels the run, and before the run starts it quits without
starting.

In the full-screen log, `j` `k` scroll by a line and `h` `l` scroll
sideways through long lines. The TUI keeps the last 10,000 lines of each
node; the node log on disk has all of them, and its path is in the
full-screen log's title.

`r` and `R` work while the run is going and after it ends. A rerun is a new
attempt of the node, in the same run directory, and its log in the TUI
shows the earlier attempt's output above an `── attempt N ──` line. After
the run ended, a rerun makes it active again, so `ctrl+c` cancels it and
`q` asks first. A node downstream of the rerun that has not started waits
for the new attempt, even if it was ready to go on the old one. `R` leaves
such a node alone and refuses while a downstream node is running.

When an interactive node runs, the TUI steps aside and the node gets the
terminal. The TUI comes back when the node exits. When the run ends, the
TUI stays open until you press `q` (or quits at once with `--exit-on-end`),
and weft exits with the run's status either way. weft's own log never goes
to the screen in the TUI; it is in the run's `weft.log`.

## Commands

```text
weft run <workflow>          run a workflow
weft validate [workflow]     check one workflow, or every file in .weft/
weft graph <workflow>        print the workflow's graph
weft runs <workflow>         list its runs, newest first
weft status <workflow>       how the latest run went, node by node
weft logs <workflow> <node>  a node's output from the latest run
weft doc                     a compact reference for agents and scripts
```

`<workflow>` is a name (`deploy`) or a path to a workflow file. For a file
outside a `.weft/` directory, the file's own directory is the workflow root,
and weft keeps that file's runs in a `.weft/runs/` there. `weft completion
bash|zsh|fish` prints a completion script; with it, the tab key completes
commands, flags, workflow names, and the node of `weft logs`.

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
| `--resume` | Carry on the latest run: skip nodes that succeeded, run the rest again; nodes the last start left out stay out |
| `--from <node>` | Run that node and everything downstream of it |
| `--only <a,b>` | Run only those nodes (comma-separated, or repeat the flag) |
| `--force` | With `--resume`, `--from`, or `--only`: go ahead even if the workflow file or git HEAD changed |

### Looking at a run

`weft status` reads a run directory and shows the run's status, each
node's state, attempt, duration, and error, and the last 20 lines of the log
of every node that did not succeed (`--tail` changes that). It ends with the
commands to run next. It works during a run, from another terminal:

```text
$ weft status deploy
deploy run 20261025T191200.482-a3f9: failed
started 2026-10-25 19:12:00, updated 2026-10-25 19:15:33
run dir /src/app/.weft/runs/deploy/20261025T191200.482-a3f9
progress: 5 succeeded · 1 failed · 1 blocked

guard       succeeded  0:00
...
deploy      failed     0:41  exit code 1
tofu-apply  blocked    blocked: deploy failed

deploy, attempt 1: failed
log /src/app/.weft/runs/deploy/20261025T191200.482-a3f9/nodes/deploy/1.log
  error: image tag v1.4.2 not found

next:
  weft logs deploy deploy --run 20261025T191200.482-a3f9    # full output of deploy
  weft run deploy --resume    # rerun what did not succeed; skip the rest
```

`weft runs` lists earlier runs with their status, and `weft logs <workflow>
<node>` prints a node's output (`--attempt`, `--tail`). `status` and `logs`
read the latest run unless `--run <run-id>` names another. A run is
`running` while a weft holds it, and `interrupted` when weft died before its
nodes finished; `--resume` carries that on.

### Agents and scripts

`weft doc` prints a Markdown reference for driving weft without a
terminal: safe defaults, every command, how to read a failed run, the JSON
shapes, states, reasons, and exit codes. It ships in the binary, so it
matches the weft installed. `weft status --json` prints one object with
every node and its log tail, `weft runs --json` prints JSON Lines, and
`validate` and `graph` take `--json` too. Errors go to stderr without color
codes unless stderr is a terminal, and name the command to run next where
there is one.

### Resuming a run

When a run fails late, fix the cause and resume it instead of starting
over:

```bash
weft run deploy            # deploy fails after release cut a tag
weft run deploy --resume   # release is skipped; deploy and tofu-apply run again
```

`--resume` carries on the latest run in the same run directory. Nodes that
succeeded are skipped, and every other node runs again as its next attempt
(`WEFT_ATTEMPT` goes up, and the logs of earlier attempts stay). That
includes a node the run shows as still running because weft itself was
killed. If that node's processes are still alive, weft refuses until you
stop them (it prints the process group), since a second copy of a release
must not start next to the first. Nodes the last start left out with
`--from` or `--only` stay `excluded`: a resume never runs a step you chose
not to run. When every node already succeeded or is excluded, there is
nothing to resume, and weft says so and exits 0.

`--from <node>` reruns a node and everything downstream of it. `--only
a,b` runs exactly the nodes you name. Needs they leave out are taken from
the latest run and must have let the node start there: succeeded, or failed
with `continue_on_error` (then shown as skipped with that note); otherwise
weft names the need and stops. Nodes downstream of the selection show as
`excluded`, since their earlier result was against the old run, and so do
the nodes a new run leaves out; excluded nodes don't fail a run. For
example, CI without a terminal can skip the interactive step, and someone
at a terminal can finish the run from it later:

```bash
weft run deploy --only guard,go-check,ui-check,release,deploy
weft run deploy --from tofu-apply
```

A resume (or `--from`/`--only` that takes results from the latest run)
first checks that the workflow file and git HEAD are what they were when
the run last started. Resuming a release after HEAD moved is how the wrong
commit ships. If either changed, the TUI asks before it starts, and plain
mode refuses unless you pass `--force`. Reruns from the TUI make the same
check and ask the same way.

Only one weft at a time can use a run directory. A second `--resume` while
the first is still going fails with the other weft's PID.

Global flags: `--log-level` (or `WEFT_LOG_LEVEL`) and `--no-color` (or
`NO_COLOR` set to anything). `--no-color` also turns color off for nodes,
unless a node's `env` sets `NO_COLOR` itself.

## Run directories and logs

Each run gets a sortable ID such as `20261025T191200.482-a3f9` and a directory
at `.weft/runs/<workflow>/<run-id>/`. The TUI shows both in its header, and
weft prints them to stderr when the run ends (and when it starts, in plain
mode). The newest 20 runs per workflow are kept; a run another weft is using is
never pruned. weft keeps `runs/` in `.weft/.gitignore`, writing the file on
first use and adding the line to one that lacks it, so command output stays
out of git.

```text
.weft/runs/deploy/20261025T191200.482-a3f9/
  weft.log                 weft's own JSON log; every line has run_id
  state.json               node states, attempts, and logs; history of starts
  lock                     held by the weft using the run
  events.jsonl             every state change, in order, with timestamps
  nodes/<node>/<n>.log     output of attempt n (stdout and stderr together)
  nodes/<node>/<n>.json    attempt n: command, dir, times, exit code, signal, reason
```

A resume appends to the same `weft.log` and `events.jsonl`. `state.json`
is rewritten after every state change, atomically and synced to disk, so a
weft that dies at any point leaves it at most one step behind. Its
`history` lists the first run, each resume, `--from`, `--only`, and TUI
rerun, with the git HEAD and workflow hash at that moment and whether the
user went ahead past a change. An attempt that weft lost track of gets an
`<n>.json` record with reason `abandoned` when the run is resumed.

Run with `--log-level=debug` to have `weft.log` explain each scheduling
decision and every signal sent. Run directories are mode 0700 and their
files 0600, because command output can hold secrets; a resume puts a file
back to 0600 if something widened it. `weft status` and `weft logs` read
only regular files inside the run directory, so a run directory that came
with a checkout can't point them anywhere else.

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
