#!/usr/bin/env bash
# smoke.sh: run the real weft binary against the stub fixture in a real
# terminal (a private tmux server) and fail on obvious breakage.
#
# Usage: scripts/smoke.sh [scenario...]   (default: all scenarios)
#
# Pane captures and run directories are kept under $SMOKE_OUT (default: a
# fresh temp dir) so a failure can be read afterwards.
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
fixture="$repo/testdata/workflows/deploy.yml"
socket=weft-smoke-$$
out=${SMOKE_OUT:-$(mktemp -d "${TMPDIR:-/tmp}/weft-smoke.XXXXXX")}
bin="$out/weft"
failures=0

tm() { tmux -L "$socket" -f /dev/null "$@"; }

cleanup() { tm kill-server 2>/dev/null || true; }
trap cleanup EXIT

log() { printf '\033[1m==> %s\033[0m\n' "$*"; }
fail() {
  printf '\033[31mFAIL\033[0m %s: %s\n' "$scenario" "$*"
  failures=$((failures + 1))
}

# start <width> <height> <command...>: run a command alone in a fresh pane.
# The pane stays after the command exits so its status can be read.
start() {
  local w=$1 h=$2
  shift 2
  tm kill-session -t smoke 2>/dev/null || true
  tm new-session -d -s smoke -x "$w" -y "$h" -c "$repo" "$*"
}

# When a pane's command exits under remain-on-exit, tmux scrolls its first
# line into history, so captures include history.
capture() { tm capture-pane -p -S - -t smoke; }

# wait_for <pattern> [seconds]: wait until the pane shows a fixed string.
wait_for() {
  local pattern=$1 timeout=${2:-15} i
  for ((i = 0; i < timeout * 10; i++)); do
    capture | grep -qF -- "$pattern" && return 0
    sleep 0.1
  done
  fail "timed out after ${timeout}s waiting for: $pattern"
  return 1
}

# wait_exit [seconds]: wait for the pane's command to exit; prints status.
wait_exit() {
  local timeout=${1:-30} i
  for ((i = 0; i < timeout * 10; i++)); do
    if [ "$(tm display-message -p -t smoke '#{pane_dead}')" = 1 ]; then
      tm display-message -p -t smoke '#{pane_dead_status}'
      return 0
    fi
    sleep 0.1
  done
  fail "command still running after ${timeout}s"
  echo "timeout"
}

save() { capture >"$out/$scenario.$1.txt"; }

expect() {
  local file="$out/$scenario.$1.txt" pattern=$2
  grep -qF -- "$pattern" "$file" || fail "$1 capture lacks: $pattern"
}

expect_not() {
  local file="$out/$scenario.$1.txt" pattern=$2
  if grep -qF -- "$pattern" "$file"; then fail "$1 capture has: $pattern"; fi
}

expect_status() {
  [ "$1" = "$2" ] || fail "exit status $1, want $2"
}

latest_run() { [ -d "$repo/testdata/workflows/.weft/runs/deploy" ] || return 0; find "$repo/testdata/workflows/.weft/runs/deploy" -mindepth 1 -maxdepth 1 -type d | sort | tail -1; }

weft() { printf '%q ' env "$@"; }

scenario_plain_success() {
  start 160 45 "$(weft "$bin" run --ui=plain "$fixture")"
  wait_for 'Only "yes" is accepted' || return
  save prompt
  tm send-keys -t smoke yes Enter
  local status
  status=$(wait_exit)
  save final
  expect_status "$status" 0
  expect prompt "tofu-apply: stdin is a terminal"
  expect final "tofu-apply: applied (stub)"
  expect final "succeeded (7 succeeded)"
  expect final "weft: run 20"
  expect final "weft: run dir "
  local run
  run=$(latest_run)
  for f in weft.log events.jsonl nodes/release/1.log nodes/release/1.json; do
    [ -f "$run/$f" ] || fail "run dir lacks $f"
  done
}

scenario_plain_fail() {
  start 160 45 "$(weft WEFT_FIXTURE_FAIL=deploy "$bin" run --ui=plain "$fixture")"
  local status
  status=$(wait_exit)
  save final
  expect_status "$status" 1
  expect final "deploy     ✗ failed: exit code 1"
  expect final "tofu-apply ⊘ blocked: deploy failed"
  expect final "release    ✓ succeeded"
}

scenario_plain_cancel() {
  start 160 45 "$(weft WEFT_FIXTURE_HANG=go-check,db-backup WEFT_LOG_LEVEL=debug "$bin" run --ui=plain "$fixture")"
  wait_for "db-backup: started" || return
  wait_for "ui-check   ✓ succeeded" || return
  tm send-keys -t smoke C-c
  local status
  status=$(wait_exit 20)
  save final
  expect_status "$status" 1
  expect final "db-backup: got SIGINT, releasing lock"
  expect final "db-backup: lock released, exiting"
  expect final "release    ⊘ canceled before start"
  grep -q '"signal":"interrupt".*signal sent to process group' "$(latest_run)/weft.log" ||
    fail "weft.log has no SIGINT sent to a process group"
}

scenario_plain_kill() {
  start 160 45 "$(weft WEFT_FIXTURE_HANG=db-backup "$bin" run --ui=plain "$fixture")"
  wait_for "db-backup: started" || return
  tm send-keys -t smoke C-c
  wait_for "db-backup: got SIGINT" || return
  tm send-keys -t smoke C-c
  local status
  status=$(wait_exit 10)
  save final
  expect_status "$status" 1
  expect final "weft: killing running nodes"
  expect final "db-backup  ⊘ canceled (killed)"
  expect_not final "db-backup: lock released"
}

scenario_plain_interactive_cancel() {
  start 160 45 "$(weft "$bin" run --ui=plain "$fixture")"
  wait_for 'Only "yes" is accepted' || return
  tm send-keys -t smoke C-c
  local status
  status=$(wait_exit 15)
  save final
  expect_status "$status" 1
  expect final "tofu-apply ⊘ canceled"
}

scenario_plain_no_tty() {
  local status=0
  "$bin" run --ui=plain "$fixture" </dev/null >"$out/$scenario.final.txt" 2>&1 || status=$?
  expect_status "$status" 1
  expect final "interactive node needs a terminal, but stdin is not a TTY"
  expect final "release    ✓ succeeded"
}

# Both stdout and stderr go into a pipe whose reader never reads, as with a
# paused pager. Ctrl-C must still cancel on time and weft must exit.
scenario_plain_stalled_output() {
  local exitfile="$out/$scenario.exit" before
  rm -f "$exitfile"
  before=$(latest_run)
  start 160 45 "{ trap '' INT; $(weft WEFT_FIXTURE_FLOOD=guard WEFT_FIXTURE_HANG=db-backup "$bin" run --ui=plain "$fixture") 2>&1; echo \$? > $exitfile; } | { trap '' INT; sleep 600; }"
  local run="" i
  for ((i = 0; i < 300; i++)); do
    run=$(latest_run)
    [ "$run" != "$before" ] && grep -q "db-backup: started" "$run/nodes/db-backup/1.log" 2>/dev/null && break
    sleep 0.1
  done
  grep -q "db-backup: started" "$run/nodes/db-backup/1.log" 2>/dev/null || {
    fail "db-backup never started behind the stalled pipe"
    return
  }
  local sent
  sent=$(date +%s)
  tm send-keys -t smoke C-c
  for ((i = 0; i < 150; i++)); do
    [ -s "$exitfile" ] && break
    sleep 0.1
  done
  [ -s "$exitfile" ] || {
    fail "weft did not exit within 15s of Ctrl-C with output stalled"
    return
  }
  local took=$(($(date +%s) - sent))
  expect_status "$(cat "$exitfile")" 1
  [ "$took" -le 10 ] || fail "exit took ${took}s after Ctrl-C"
  grep -q '"reason": "canceled"' "$run/nodes/db-backup/1.json" || fail "db-backup not recorded as canceled"
  grep -q '"exit_code": 130' "$run/nodes/db-backup/1.json" || fail "db-backup did not finish its SIGINT cleanup"
  [ "$(wc -l <"$run/nodes/guard/1.log")" -ge 200000 ] || fail "guard's node log lost output"
  echo "exited ${took}s after Ctrl-C" >"$out/$scenario.final.txt"
}

# stdout closes early (head exits after one line). weft must finish the run
# and exit with the run's own status, not die of SIGPIPE.
scenario_plain_closed_stdout() {
  local exitfile="$out/$scenario.exit"
  rm -f "$exitfile"
  start 160 45 "{ $(weft WEFT_FIXTURE_FAIL=deploy "$bin" run --ui=plain "$fixture") 2>/dev/null; echo \$? > $exitfile; } | head -n 1"
  local status
  status=$(wait_exit 30)
  save final
  expect_status "$status" 0
  [ -s "$exitfile" ] || {
    fail "no exit status written"
    return
  }
  expect_status "$(cat "$exitfile")" 1
  grep -q '"message":"run finished"' "$(latest_run)/weft.log" || fail "run did not finish after stdout closed"
}

# weft graph sizes itself to the real terminal: horizontal when it fits,
# vertical when it doesn't.
scenario_graph_wide() {
  start 160 45 "$(weft "$bin" graph "$fixture")"
  local status
  status=$(wait_exit 10)
  save final
  expect_status "$status" 0
  expect final "guard ─┼─▸ ui-check ──┼─▸ release ───▸ deploy ───▸ tofu-apply"
  expect final "┌─▸ go-check ──┐"
  expect final "└─▸ db-backup ─┘"
}

scenario_graph_narrow() {
  start 40 24 "$(weft "$bin" graph "$fixture")"
  local status
  status=$(wait_exit 10)
  save final
  expect_status "$status" 0
  expect final "           guard"
  expect final "go-check  ui-check  db-backup"
  expect final "▾"
  expect_not final "▸"
}

scenario_graph_forced_horizontal() {
  start 40 24 "$(weft "$bin" graph --layout horizontal "$fixture")"
  local status
  status=$(wait_exit 10)
  save final
  expect_status "$status" 0
  expect final "─┼─▸"
}

all=(plain_success plain_fail plain_cancel plain_kill plain_interactive_cancel plain_no_tty plain_stalled_output plain_closed_stdout graph_wide graph_narrow graph_forced_horizontal)

log "building weft into $out"
(cd "$repo" && go build -o "$bin" .)
tm new-session -d -s idle
tm set-option -g remain-on-exit on

for scenario in "${@:-${all[@]}}"; do
  log "$scenario"
  before=$failures
  "scenario_$scenario"
  [ "$failures" -eq "$before" ] && printf '\033[32mok\033[0m   %s\n' "$scenario"
done

log "captures in $out"
if [ "$failures" -gt 0 ]; then
  printf '\033[31m%d check(s) failed\033[0m\n' "$failures"
  exit 1
fi
