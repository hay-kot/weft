#!/usr/bin/env bash
# stub.sh <node> <seconds> [step...]: print each step, spread over <seconds>.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

node="$1"
seconds="$2"
shift 2
steps=("$@")
[ ${#steps[@]} -eq 0 ] && steps=("working")

pause=$(awk -v s="$seconds" -v n="${#steps[@]}" 'BEGIN { printf "%.2f", s / n }')
for step in "${steps[@]}"; do
  printf '\033[36m•\033[0m %s\n' "$step"
  sleep "$pause"
done
finish "$node"
echo "$node: done"
