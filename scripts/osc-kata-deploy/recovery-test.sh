#!/bin/bash
# Interrupt dispatcher and installer pods while preserving the selected node set.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/osc-kata-deploy/images.env
source "$SCRIPT_DIR/images.env"
exec python3 -B "$SCRIPT_DIR/recovery_test.py" "$@"
