#!/usr/bin/env bash
set -euo pipefail

WORKSPACE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

export DEVCONTAINER_CACHE="${DEVCONTAINER_CACHE:-$HOME/.cache/devcontainer}"

append_once() {
  local file="$1"
  local line="$2"

  mkdir -p "$(dirname "$file")"
  touch "$file"

  grep -qxF "$line" "$file" 2>/dev/null || echo "$line" >> "$file"
}

mkdir -p "$DEVCONTAINER_CACHE/go-build" "$DEVCONTAINER_CACHE/go-mod"

append_once "$HOME/.bashrc" 'export GOCACHE="$DEVCONTAINER_CACHE/go-build"'
append_once "$HOME/.bashrc" 'export GOMODCACHE="$DEVCONTAINER_CACHE/go-mod"'