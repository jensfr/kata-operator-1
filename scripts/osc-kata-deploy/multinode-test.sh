#!/bin/bash
# Test selector changes and add/remove a real worker through its MachineSet.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/osc-kata-deploy/images.env
source "$SCRIPT_DIR/images.env"
exec python3 -B "$SCRIPT_DIR/multinode_test.py" "$@"
