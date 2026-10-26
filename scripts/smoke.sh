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

# The TUI draws on the alternate screen, so its captures are the visible
# screen only, with no history.
screen() { tm capture-pane -p -t smoke; }

# wait_screen <pattern> [seconds]: like wait_for, on the visible screen.
wait_screen() {
  local pattern=$1 timeout=${2:-15} i
  for ((i = 0; i < timeout * 10; i++)); do
    screen | grep -qF -- "$pattern" && return 0
    sleep 0.1
  done
  fail "timed out after ${timeout}s waiting for: $pattern"
  screen >"$out/$scenario.timeout.txt"
  return 1
}

save_screen() { screen >"$out/$scenario.$1.txt"; }

# check_frame <name> <width> <height>: the saved screen has the TUI's
# layout and nothing drawn over it: the header on top, a rule under it,
# the key help on the last row, and no log line from weft itself.
check_frame() {
  local file="$out/$scenario.$1.txt" w=$2 h=$3
  local rows
  rows=$(wc -l <"$file" | tr -d ' ')
  [ "$rows" -eq "$h" ] || fail "$1: $rows rows, want $h"
  sed -n 1p "$file" | grep -q '^ deploy  ' || fail "$1: row 1 is not the header: $(sed -n 1p "$file")"
  sed -n 2p "$file" | grep -q '^ run dir ' || fail "$1: row 2 is not the run dir: $(sed -n 2p "$file")"
  [ "$(sed -n 3p "$file" | tr -d ' ')" = "$(printf '%*s' "$w" '' | sed 's/ /─/g')" ] || fail "$1: row 3 is not a full-width rule"
  sed -n "${h}p" "$file" | grep -qE 'help|back|quit' || fail "$1: last row is not the key help: $(sed -n "${h}p" "$file")"
  if grep -qE '(DBG|INF|WRN|ERR) |"level":' "$file"; then fail "$1: a log line is drawn on the screen"; fi
}

# check_restored: after weft exits the terminal is back to normal: no
# alternate screen, the cursor visible, no mouse or keypad modes, and the
# same stty settings as before. scripts/smoke-tty.sh compares stty and
# holds the pane open, since a dead pane reports no cursor.
check_restored() {
  wait_for "smoke-tty: exited" 10 || return
  local flags
  flags=$(tm display-message -p -t smoke '#{alternate_on} #{cursor_flag} #{keypad_flag} #{keypad_cursor_flag} #{mouse_any_flag}')
  [ "$flags" = "0 1 0 0 0" ] || fail "terminal modes after exit: alternate cursor keypad keypad_cursor mouse = $flags"
  capture | grep -qF "tty restored" || fail "stty settings changed: $(capture | grep -A3 'tty changed')"
}

# quit_and_check <status>: quit with q, then check the exit status and the
# terminal state, and save the final pane.
quit_and_check() {
  tm send-keys -t smoke q
  check_restored
  local status
  status=$(wait_exit 10)
  save final
  expect_status "$status" "$1"
}

# tui <env...> -- <args...>: the command line for weft in TUI mode,
# wrapped so the pane records whether the tty mode survived.
tui() {
  local envs=()
  while [ "$1" != "--" ]; do
    envs+=("$1")
    shift
  done
  shift
  weft ${envs[@]+"${envs[@]}"} "$repo/scripts/smoke-tty.sh" "$bin" run "$@" "$fixture"
}

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

scenario_tui_success() {
  start 160 45 "$(tui WEFT_LOG_LEVEL=debug --)"
  wait_screen "deploy  running" || return
  wait_screen "go-check" || return
  save_screen running
  check_frame running 160 45
  expect running "─┼─▸"
  wait_for 'Only "yes" is accepted' || return
  save handoff
  expect handoff "weft: tofu-apply has the terminal"
  tm send-keys -t smoke yes Enter
  wait_screen "deploy  succeeded" || return
  save_screen finished
  check_frame finished 160 45
  expect finished "7 succeeded"
  expect finished "run 20"
  expect finished "interactive: its output went to the terminal"
  quit_and_check 0
  expect final "tofu-apply: applied (stub)"
  expect final "weft: run 20"
  expect final "succeeded (7 succeeded)"
  expect final "weft: run dir "
  grep -q '"msg":"key","key":"q"' "$(latest_run)/weft.log" || fail "weft.log lacks the TUI's received messages"
}

# 80x24 is too narrow for the horizontal graph, so it falls back to
# vertical. deploy fails, tofu-apply is blocked, and the end screen shows
# the failure.
scenario_tui_narrow_fail() {
  start 80 24 "$(tui WEFT_FIXTURE_FAIL=deploy --)"
  wait_screen "deploy  failed" 20 || return
  save_screen finished
  check_frame finished 80 24
  expect finished "▾"
  expect_not finished "─┼─▸"
  expect finished "› deploy  ✗ failed: exit code 1"
  expect finished "deploy: failing on request (WEFT_FIXTURE_FAIL)"
  expect finished "5 succeeded, 1 failed, 1 blocked"
  quit_and_check 1
  expect final "failed (1 blocked, 1 failed, 5 succeeded)"
}

scenario_tui_resize() {
  start 160 45 "$(tui WEFT_FIXTURE_HANG=db-backup --)"
  wait_screen "db-backup: started" || return
  save_screen wide
  check_frame wide 160 45
  tm resize-window -t smoke -x 80 -y 24
  wait_screen "▾" 5 || return
  save_screen narrow
  check_frame narrow 80 24
  tm resize-window -t smoke -x 120 -y 30
  wait_screen "─┼─▸" 5 || return
  save_screen medium
  check_frame medium 120 30
  tm resize-window -t smoke -x 20 -y 6
  wait_screen "window too small" 5 || return
  tm resize-window -t smoke -x 100 -y 30
  wait_screen "─┼─▸" 5 || return
  save_screen back
  check_frame back 100 30
  tm send-keys -t smoke C-c
  wait_screen "deploy  canceled" 20 || return
  quit_and_check 1
}

# Ctrl-C in the TUI cancels gracefully: db-backup traps SIGINT and gets its
# grace period to release its lock. The TUI stays up to show the result.
scenario_tui_cancel() {
  start 160 45 "$(tui WEFT_FIXTURE_HANG=go-check,db-backup WEFT_LOG_LEVEL=debug --)"
  wait_screen "ui-check" || return
  local run i
  for ((i = 0; i < 150; i++)); do
    run=$(latest_run)
    grep -q "hanging until canceled" "$run/nodes/go-check/1.log" 2>/dev/null &&
      grep -q "db-backup: started" "$run/nodes/db-backup/1.log" 2>/dev/null && break
    sleep 0.1
  done
  tm send-keys -t smoke C-c
  wait_screen "deploy  canceling" 5 || return
  save_screen canceling
  check_frame canceling 160 45
  expect canceling "ctrl+c again kills them now"
  wait_screen "deploy  canceled" 20 || return
  save_screen finished
  check_frame finished 160 45
  quit_and_check 1
  grep -q "db-backup: lock released, exiting" "$run/nodes/db-backup/1.log" || fail "db-backup did not get its grace period"
  grep -q '"state": "canceled"' "$run/nodes/release/1.json" || fail "release not recorded as canceled"
  grep -q '"signal":"interrupt".*signal sent to process group' "$run/weft.log" || fail "weft.log has no SIGINT sent to a process group"
}

scenario_tui_kill() {
  start 160 45 "$(tui WEFT_FIXTURE_HANG=db-backup --)"
  wait_screen "db-backup" || return
  local run i
  for ((i = 0; i < 150; i++)); do
    run=$(latest_run)
    grep -q "db-backup: started" "$run/nodes/db-backup/1.log" 2>/dev/null && break
    sleep 0.1
  done
  tm send-keys -t smoke C-c
  wait_screen "deploy  canceling" 5 || return
  tm send-keys -t smoke C-c
  wait_screen "deploy  canceled" 10 || return
  save_screen finished
  check_frame finished 160 45
  quit_and_check 1
  if grep -q "lock released" "$run/nodes/db-backup/1.log"; then fail "db-backup finished its cleanup despite the kill"; fi
}

# q during a run asks first. Any other key keeps the run going; y cancels
# it and weft quits once the nodes stop.
scenario_tui_quit_confirm() {
  start 160 45 "$(tui WEFT_FIXTURE_HANG=db-backup --)"
  wait_screen "db-backup" || return
  tm send-keys -t smoke q
  wait_screen "Press y to cancel it and quit" 5 || return
  save_screen confirm
  check_frame confirm 160 45
  tm send-keys -t smoke n
  sleep 0.5
  save_screen kept
  expect kept "deploy  running"
  expect_not kept "Press y"
  tm send-keys -t smoke q
  wait_screen "Press y to cancel it and quit" 5 || return
  tm send-keys -t smoke y
  check_restored
  local status
  status=$(wait_exit 20)
  save final
  expect_status "$status" 1
  expect final "weft: run 20"
  expect final "canceled"
}

scenario_tui_exit_on_end() {
  start 160 45 "$(tui WEFT_FIXTURE_FAIL=release -- --exit-on-end)"
  check_restored
  local status
  status=$(wait_exit 20)
  save final
  expect_status "$status" 1
  expect final "failed (2 blocked, 1 failed, 4 succeeded)"
}

# Ctrl-C at the interactive node's prompt reaches only the node, which
# owns the terminal; the run cancels and the TUI comes back to show it.
scenario_tui_interactive_cancel() {
  start 160 45 "$(tui --)"
  wait_for 'Only "yes" is accepted' 20 || return
  tm send-keys -t smoke C-c
  wait_screen "deploy  canceled" 10 || return
  save_screen finished
  check_frame finished 160 45
  expect finished "⊘ tofu-apply"
  quit_and_check 1
}

# guard prints about 10 MB. The TUI keeps up or drops lines with a notice,
# stays responsive, and the node log on disk keeps every line.
scenario_tui_flood() {
  start 160 45 "$(tui WEFT_FIXTURE_FLOOD=guard WEFT_FIXTURE_FAIL=release --)"
  wait_screen "deploy  failed" 15 || return
  save_screen finished
  check_frame finished 160 45
  tm send-keys -t smoke h h h
  wait_screen "› guard" 5 || return
  tm send-keys -t smoke Enter
  wait_screen "esc/q back" 5 || return
  save_screen log
  expect log "guard: done"
  tm send-keys -t smoke Escape
  # A q right behind an escape parses as alt+q.
  sleep 0.3
  quit_and_check 1
  [ "$(wc -l <"$(latest_run)/nodes/guard/1.log")" -ge 200000 ] || fail "guard's node log lost output"
}

# Without a terminal, --ui=tui refuses before creating a run.
scenario_tui_no_tty() {
  local status=0
  "$bin" run --ui=tui "$fixture" </dev/null >"$out/$scenario.final.txt" 2>&1 || status=$?
  expect_status "$status" 1
  expect final "--ui=tui needs a terminal on both stdin and stdout"
}

all=(plain_success plain_fail plain_cancel plain_kill plain_interactive_cancel plain_no_tty plain_stalled_output plain_closed_stdout graph_wide graph_narrow graph_forced_horizontal
  tui_success tui_narrow_fail tui_resize tui_cancel tui_kill tui_quit_confirm tui_exit_on_end
  tui_interactive_cancel tui_flood tui_no_tty)

log "building weft into $out"
(cd "$repo" && go build -o "$bin" .)
tm new-session -d -s idle
tm set-option -g remain-on-exit on

for scenario in "${@:-${all[@]}}"; do
  log "$scenario"
  before=$failures
  "scenario_$scenario" || true
  [ "$failures" -eq "$before" ] && printf '\033[32mok\033[0m   %s\n' "$scenario"
done

log "captures in $out"
if [ "$failures" -gt 0 ]; then
  printf '\033[31m%d check(s) failed\033[0m\n' "$failures"
  exit 1
fi
