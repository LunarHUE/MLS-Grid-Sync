{
  description = "MLS-Grid-Sync";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils, ... }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs {
          inherit system;
          config.allowUnfree = true;
        };
      in {
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            bashInteractive
            bash-completion
            nix-bash-completions
            claude-code
            go
            gopls
            docker
            gcc
            postgresql
            opencode
          ];

          # Export the path so the rcfile can find it
          BASH_COMPLETION_PATH = "${pkgs.bash-completion}/etc/profile.d/bash_completion.sh";

          # Keep shellHook minimal — don't set PS1 here, don't source completion here
          shellHook = ''
            echo "Nix devShell ready. Tools: $(go version 2>/dev/null)"
          '';
        };
      });
}
