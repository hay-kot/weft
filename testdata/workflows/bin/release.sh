#!/usr/bin/env bash
# release.sh: pretend to cut a release and record the tag for deploy.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

tag="v0.0.0-stub.$(date +%s)"
echo "• building binaries  layout=dist"
sleep 0.5
echo "• publishing  example.invalid/stub:$tag"
finish release
mkdir -p "${WEFT_RUN_DIR:?}/fixture"
echo "$tag" >"$WEFT_RUN_DIR/fixture/release-tag"
echo "release: tagged $tag"
