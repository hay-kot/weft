#!/usr/bin/env bash
# approve.sh: stands in for `tofu apply`, which prompts on the terminal.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

if [ -t 0 ]; then tty_state="a terminal"; else tty_state="not a terminal"; fi
echo "tofu-apply: stdin is $tty_state"
printf 'Apply these changes? Only "yes" is accepted: '
answer=""
read -r answer || true
finish tofu-apply
if [ "$answer" = "yes" ]; then
  echo "tofu-apply: applied (stub)"
else
  echo "tofu-apply: apply skipped"
fi
