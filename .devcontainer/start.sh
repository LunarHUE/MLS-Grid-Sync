#!/usr/bin/env bash
set -euo pipefail

WORKSPACE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ -f "$WORKSPACE_DIR/.envrc" ]; then
  direnv allow "$WORKSPACE_DIR" >/dev/null 2>&1 || true
fi

# Remove the SSH-era PATH workarounds from homes created before they were dropped.
if [ -f "$HOME/.bashrc" ]; then
  sed -i '/^# BEGIN devcontainer SSH direnv$/,/^# END devcontainer SSH direnv$/d' "$HOME/.bashrc"
fi
if [ -f "$HOME/.profile" ]; then
  sed -i '/DEVCONTAINER_WORKSPACE_PATH/d' "$HOME/.profile"
fi

# Personal instructions and skills live on the per-user shared volume.
# Preserve a pre-existing local directory before replacing it with the link.
if [ -d /mnt/user-home/.agents ] && [ "$(readlink "$HOME/.agents" 2>/dev/null || true)" != /mnt/user-home/.agents ]; then
  if [ -e "$HOME/.agents" ] || [ -L "$HOME/.agents" ]; then
    mv "$HOME/.agents" "$HOME/.agents.before-shared-$(date +%Y%m%d-%H%M%S)"
  fi
  ln -s /mnt/user-home/.agents "$HOME/.agents"
fi

# The Coder template passes T3CODE_INSTANCE and T3CODE_RELAY_PRIVATE_URL as
# remoteEnv, which this lifecycle command inherits, and writes them to
# /etc/profile.d/99-t3code.sh for anything started outside the CLI.
if [ -z "${T3CODE_INSTANCE:-}" ] && [ -r /etc/profile.d/99-t3code.sh ]; then
  # shellcheck disable=SC1091
  . /etc/profile.d/99-t3code.sh
  export T3CODE_INSTANCE T3CODE_RELAY_PRIVATE_URL
fi
if [ -n "${T3CODE_INSTANCE:-}" ] && [ -n "${T3CODE_RELAY_PRIVATE_URL:-}" ]; then
  # Serve this workspace through T3 Connect once `t3 connect link --headless` has run.
  # The dev shell puts the agent tools on its PATH.
  PIDFILE=/tmp/t3-devcontainer.pid
  if ! { [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; }; then
    setsid nohup nix develop "$WORKSPACE_DIR" --command t3-devcontainer \
      >/tmp/t3-devcontainer.log 2>&1 </dev/null &
    echo $! >"$PIDFILE"
  fi
else
  echo "T3 Code: T3CODE_INSTANCE or T3CODE_RELAY_PRIVATE_URL is unset; not starting the server." >&2
fi
