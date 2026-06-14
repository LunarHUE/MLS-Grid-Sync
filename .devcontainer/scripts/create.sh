#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

bash "$SCRIPT_DIR/base/create.sh"

if [ -f "$SCRIPT_DIR/project/create.sh" ]; then
  bash "$SCRIPT_DIR/project/create.sh"
fi