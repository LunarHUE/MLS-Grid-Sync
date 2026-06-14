#!/usr/bin/env bash
set -euo pipefail

WORKSPACE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
PROJECT_NAME="$(basename "$WORKSPACE_DIR")"

append_once() {
  local file="$1"
  local line="$2"

  mkdir -p "$(dirname "$file")"
  touch "$file"

  grep -qxF "$line" "$file" 2>/dev/null || echo "$line" >> "$file"
}

append_once "$HOME/.bashrc" "export PS1='\\[\\e[1;32m\\]\\u@${PROJECT_NAME}\\[\\e[0m\\]:\\[\\e[1;34m\\]\\w\\[\\e[0m\\]\\$ '"
append_once "$HOME/.bashrc" 'export DIRENV_LOG_FORMAT=""'
append_once "$HOME/.bashrc" 'eval "$(direnv hook bash)"'
append_once "$HOME/.bashrc" 'export DEVCONTAINER_CACHE="$HOME/.cache/devcontainer"'

mkdir -p "$HOME/.config/nix"
append_once "$HOME/.config/nix/nix.conf" "experimental-features = nix-command flakes"
append_once "$HOME/.config/nix/nix.conf" "warn-dirty = false"

mkdir -p "$HOME/.cache/devcontainer"

[ -f "$WORKSPACE_DIR/.envrc" ] || echo 'use flake' > "$WORKSPACE_DIR/.envrc"
direnv allow "$WORKSPACE_DIR"

git config --global --get-all safe.directory | grep -qxF "$WORKSPACE_DIR" \
  || git config --global --add safe.directory "$WORKSPACE_DIR"
git lfs install --skip-repo

# GitHub auth / git identity bootstrap.
if [ -n "${GH_TOKEN:-}" ] || [ -n "${GITHUB_TOKEN:-}" ]; then
  echo "GitHub token detected from host environment; using token-based auth as primary."
elif [ -d "$HOME/.config/gh" ]; then
  echo "No GH_TOKEN/GITHUB_TOKEN detected; trying mounted GitHub CLI config."
else
  echo "No GitHub token or mounted GitHub CLI config detected."
fi

if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
  echo "GitHub CLI is authenticated; configuring git auth."

  if ! gh auth setup-git; then
    echo "warning: gh auth setup-git failed; continuing without GitHub git credential helper setup"
  fi

  git_name="$(gh api user --jq '.name // .login' 2>/dev/null || true)"
  git_email="$(gh api user --jq '.email // empty' 2>/dev/null || true)"

  if [ -z "$git_email" ]; then
    git_email="$(gh api user/emails --jq '.[] | select(.primary and .verified) | .email' 2>/dev/null || true)"
  fi

  if [ -z "$git_email" ]; then
    git_id="$(gh api user --jq '.id' 2>/dev/null || true)"
    git_login="$(gh api user --jq '.login' 2>/dev/null || true)"

    if [ -n "$git_id" ] && [ -n "$git_login" ]; then
      git_email="${git_id}+${git_login}@users.noreply.github.com"
    fi
  fi

  if [ -n "$git_name" ]; then
    git config --global user.name "$git_name"
    echo "Configured git user.name=$git_name"
  fi

  if [ -n "$git_email" ]; then
    git config --global user.email "$git_email"
    echo "Configured git user.email=$git_email"
  fi
else
  echo "GitHub CLI is not authenticated inside the container."
  echo "Set GH_TOKEN or GITHUB_TOKEN on the host, or mount a token-backed ~/.config/gh."
fi