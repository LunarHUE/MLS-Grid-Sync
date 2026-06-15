#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

bash "$SCRIPT_DIR/base/start.sh"

if [ -f "$SCRIPT_DIR/project/start.sh" ]; then
  bash "$SCRIPT_DIR/project/start.sh"
fi