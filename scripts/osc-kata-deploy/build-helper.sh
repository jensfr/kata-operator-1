#!/bin/bash
# Stage only the helper recipe and public input locks.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
python3 "$REPO_ROOT/scripts/osc-artifacts/check-input-locks.py"
umask 077
mkdir -p "$REPO_ROOT/build/_output"
context="$(mktemp -d "$REPO_ROOT/build/_output/kata-helper-build.XXXXXX")"
cp "$SCRIPT_DIR/Containerfile.helper" "$SCRIPT_DIR/helper-input-locks.json" "$context/"
cp -r "$SCRIPT_DIR/locks" "$context/locks"
echo "Build context: $context"
podman build --layers --platform linux/amd64 "$@" -f "$context/Containerfile.helper" "$context"
