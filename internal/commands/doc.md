# weft: reference for agents

weft runs a project's tasks as a DAG. A workflow is a YAML file in
`.weft/<name>.yml`; each node runs a shell command once the nodes it needs
have succeeded. `<workflow>` in every command is a name (`deploy`) or a path
to a workflow file.

## Rules of thumb

- Run with `--ui=plain`. Without a terminal, weft picks plain mode on its
  own, but saying so keeps the output stable: one `<node> <mark> <text>`
  line per output line and state change.
- A workflow can deploy, release, or change infrastructure. Read it
  (`weft graph <workflow>`, or the YAML) before you run it, and ask first if
  it does anything you were not asked to do.
- An `interactive` node needs a terminal and fails without one. Leave it
  out with `--only`, or hand that step to the user.
- Never pass `--force` on your own. It goes ahead although the workflow file
  or git HEAD changed since the run being continued last started; that is how
  the wrong commit ships. Report the drift instead.
- After a run, read what happened with `weft status`. It reads the run
  directory, so it costs no rerun, and it works while a run is going.

## Commands

```text
weft validate [workflow] [--json]       check one workflow, or all in .weft/
weft graph <workflow> [--json]          print the nodes and their needs
weft run <workflow> [flags]             run it; exits 1 when the run fails
weft runs <workflow> [--json]           list runs, newest first
weft status <workflow> [--json]         one run, node by node, with log tails
weft logs <workflow> <node>             a node attempt's full output
weft doc                                this reference
```

`weft run` flags: `--ui=auto|tui|plain`, `--concurrency N` (`-j`),
`--resume`, `--from <node>`, `--only <a,b>`, `--force`, `--exit-on-end`.
`weft status` and `weft logs` take `--run <run-id>` (default: the latest
run) and `--tail N` (status: lines per node that did not succeed, default
20); `weft logs` also takes `--attempt N`.

## After a failure

```bash
weft status deploy               # which node failed, its error, its last log lines
weft logs deploy release         # the full output of the failed node
# fix the cause, then:
weft run deploy --ui=plain --resume   # skip what succeeded, rerun the rest
```

`weft status` ends with a `next:` list of the commands that fit the run.
`--resume` always continues the latest run, and never runs a node the latest
start left out with `--from` or `--only`. `--from <node>` reruns a node and
everything downstream; `--only a,b` runs exactly those nodes. Both take the
needs they leave out from the latest run, and fail when those did not let
the node start there (succeeded, or failed with `continue_on_error`). Nodes
downstream of the selection are `excluded`.

## JSON

- `weft status --json`: one object with `workflow`, `run_id`, `run_dir`,
  `status`, `pid` (while running), `created`, `updated`, `history`, `counts`
  (nodes per state), and `nodes`. Each node has `id`, `label`, `state`,
  `attempt`, `exit_code` (-1 when no process exited), and `duration_ms`;
  `reason`, `error`, `started`, `finished`, `log` (absolute path),
  `log_tail` (when it did not succeed), and `continue_on_error` appear
  when set. A `failed` node with `continue_on_error` does not fail the run.
- `weft runs --json`: JSON Lines, one run per line, the same fields without
  `nodes`, plus `unfinished` (IDs of nodes that did not succeed).
- `weft validate --json`: an array of `{workflow, path, valid, nodes,
  problems}`; `problems` appears when the file is invalid, and `workflow`
  is empty when the file could not be loaded.
- `weft graph --json`: `{workflow, orientation, levels, nodes, lines}`.

## Run status

| Status | Meaning |
| --- | --- |
| `running` | a weft process holds the run's lock |
| `succeeded` | every node succeeded, was skipped, or was excluded |
| `failed` | a node without `continue_on_error` failed, or a node was blocked |
| `canceled` | the run or a node was canceled, and nothing failed |
| `interrupted` | no weft holds the run, but nodes never finished: weft died. `--resume` carries it on |

## Node states and reasons

States: `pending`, `ready`, `waiting` (an interactive node waiting for the
terminal), `running`, `succeeded`, `failed`, `blocked` (a need did not
succeed), `canceled`, `skipped` (a resume kept an earlier success),
`excluded` (outside this start's `--from` or `--only`).

Reasons, on finished nodes: `exit`, `timeout`, `canceled`, `blocked`,
`start_error`, `skipped` (a resume kept an earlier result: a success, or a
failure with `continue_on_error`, which the node's `error` then says),
`excluded`, `abandoned` (the weft running the attempt died; recorded when
the run is resumed).

## Files

```text
.weft/runs/<workflow>/<run-id>/
  state.json               node states, attempts, log paths; history of starts
  lock                     held by the weft process using the run
  events.jsonl             every state change, in order
  weft.log                 weft's JSON log; more detail with --log-level=debug
  <node>.<n>.log           output of attempt n, stdout and stderr together
  <node>.<n>.json          attempt n: command, dir, times, exit code, signal, reason
```

weft keeps the newest 20 runs per workflow. Nodes get `WEFT_WORKFLOW`,
`WEFT_ROOT`, `WEFT_RUN_ID`, `WEFT_RUN_DIR`, `WEFT_NODE`, and `WEFT_ATTEMPT` in
their environment.

## Exit codes

`weft run` exits 0 when every node it ran succeeded and 1 otherwise,
including a cancel. Every command exits 1 on a usage error or a workflow it
can't load, with the reason on stderr. `weft status` and `weft runs` exit 0
whenever they could read the runs, whatever the runs' status.
