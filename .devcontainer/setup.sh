#!/bin/bash
set -euo pipefail

# Everything is derived from this script's own location, so the whole
# .devcontainer/ folder can be copied between projects unchanged.
WORKSPACE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROJECT_NAME="$(basename "$WORKSPACE_DIR")"

# append_once <file> <line> — keeps reruns/rebuilds from duplicating lines.
append_once() {
  grep -qxF "$2" "$1" 2>/dev/null || echo "$2" >> "$1"
}

append_once ~/.bashrc "export PS1='\\[\\e[1;32m\\]\\u@${PROJECT_NAME}\\[\\e[0m\\]:\\[\\e[1;34m\\]\\w\\[\\e[0m\\]\\$ '"
append_once ~/.bashrc 'export DIRENV_LOG_FORMAT=""'
append_once ~/.bashrc 'eval "$(direnv hook bash)"'

mkdir -p ~/.config/nix
append_once ~/.config/nix/nix.conf "warn-dirty = false"

# .envrc (`use flake`) is committed to the repo; recreate it only if a
# clean/checkout dropped it, then trust it so direnv activates the
# flake devShell in every new shell.
[ -f "$WORKSPACE_DIR/.envrc" ] || echo 'use flake' > "$WORKSPACE_DIR/.envrc"
direnv allow "$WORKSPACE_DIR"
