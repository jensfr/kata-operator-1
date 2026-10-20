#!/bin/bash
# Install, run a Kata workload, check the uninstall guard, and verify host cleanup.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/osc-kata-deploy/images.env
source "$SCRIPT_DIR/images.env"
exec python3 "$SCRIPT_DIR/validation.py" e2e "$@"
