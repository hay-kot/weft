#!/usr/bin/env bash
# trap-int.sh <node> <seconds>: stands in for tofu or a backup that must
# clean up on SIGINT. On SIGINT it takes 2s to "release the lock", then
# exits 130.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

node="$1"
seconds="$2"

on_int() {
  echo "$node: got SIGINT, releasing lock"
  sleep 2
  echo "$node: lock released, exiting"
  exit 130
}
trap on_int INT

echo "$node: started, pid $$"
# Sleep in short steps so the trap runs promptly. bash defers traps until
# the foreground command returns.
steps=$(awk -v s="$seconds" 'BEGIN { printf "%d", s * 10 }')
for ((i = 0; i < steps; i++)); do sleep 0.1; done
finish "$node"
echo "$node: backup written (stub)"
