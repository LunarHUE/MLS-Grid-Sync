#!/bin/bash

echo "export PS1='\[\e[1;32m\]\u@nix-dev\[\e[0m\]:\[\e[1;34m\]\w\[\e[0m\]\$ '" >> ~/.bashrc
echo 'export DIRENV_LOG_FORMAT=""' >> ~/.bashrc

mkdir -p ~/.config/nix
echo "warn-dirty = false" >> ~/.config/nix/nix.conf

echo 'eval "$(direnv hook bash)"' >> ~/.bashrc

echo 'use flake' > /workspaces/MLS-Grid-Sync/.envrc

direnv allow /workspaces/MLS-Grid-Sync
