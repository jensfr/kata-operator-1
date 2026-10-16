#!/bin/bash
# Capture a working baseline, then verify the same node after a reboot or OS update.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/osc-kata-deploy/images.env
source "$SCRIPT_DIR/images.env"
exec python3 "$SCRIPT_DIR/validation.py" lifecycle "$@"
