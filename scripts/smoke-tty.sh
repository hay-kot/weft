#!/usr/bin/env bash
# smoke-tty.sh <command...>: run a command and report whether it left the
# terminal's mode (raw mode, echo, and the rest of stty) as it found it.
# Prints "tty restored" or "tty changed" and "smoke-tty: exited", then
# waits $SMOKE_HOLD seconds (default 2) so the caller can read the pane's
# terminal modes while it is still alive, and exits with the command's
# status.
set -uo pipefail

before=$(stty -g)
"$@"
status=$?
if [ "$(stty -g)" = "$before" ]; then
  echo "tty restored"
else
  echo "tty changed"
  stty -a
fi
echo "smoke-tty: exited"
sleep "${SMOKE_HOLD:-2}"
exit "$status"
