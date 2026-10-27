#!/usr/bin/env bash
# deploy.sh: pretend to deploy the tag that release recorded.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

tag=$(cat "${WEFT_RUN_DIR:?}/fixture/release-tag")
echo "deploy: rolling out $tag (stub)"
sleep 0.5
finish deploy
echo "deploy: done"
