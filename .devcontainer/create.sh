#!/usr/bin/env bash
set -euo pipefail

WORKSPACE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROJECT_NAME="$(basename "$WORKSPACE_DIR")"

append_once() {
  local file="$1"
  local line="$2"

  mkdir -p "$(dirname "$file")"
  touch "$file"

  grep -qxF "$line" "$file" 2>/dev/null || echo "$line" >> "$file"
}

append_once "$HOME/.bashrc" "export PS1='\\[\\e[1;32m\\]\\u@${PROJECT_NAME}\\[\\e[0m\\]:\\[\\e[1;34m\\]\\w\\[\\e[0m\\]\\$ '"
append_once "$HOME/.bashrc" 'eval "$(direnv hook bash)"'

# Configure nix (Enable flake and Nix-Command)
mkdir -p "$HOME/.config/nix"
append_once "$HOME/.config/nix/nix.conf" "experimental-features = nix-command flakes"
append_once "$HOME/.config/nix/nix.conf" "warn-dirty = false"

if [ ! -f "$WORKSPACE_DIR/.envrc" ]; then
  echo 'use flake' > "$WORKSPACE_DIR/.envrc" \
    || echo "warning: could not write $WORKSPACE_DIR/.envrc; skipping direnv setup" >&2
fi
if [ -f "$WORKSPACE_DIR/.envrc" ]; then
  direnv allow "$WORKSPACE_DIR" \
    || echo "warning: 'direnv allow' failed; env will load once the flake is fixed" >&2
fi

# Single-user Nix store is root-owned from the image build; hand it to the dev user.
# Runs against the mounted /nix volume, so it fixes the persistent volume too.
if [ ! -w /nix/var/nix/db/big-lock ]; then
  echo "Claiming /nix for $(id -un)..."
  sudo chown -R "$(id -u):$(id -g)" /nix \
    || echo "warning: could not chown /nix; nix will fail until this is fixed" >&2
fi

git config --global --get-all safe.directory | grep -qxF "$WORKSPACE_DIR" \
  || git config --global --add safe.directory "$WORKSPACE_DIR"
git lfs install --skip-repo