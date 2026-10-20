#!/bin/bash
# Render or deploy the development KataDeploy operator without editing config/manager.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/osc-kata-deploy/images.env
source "$SCRIPT_DIR/images.env"
exec python3 "$SCRIPT_DIR/validation.py" deploy "$@"
