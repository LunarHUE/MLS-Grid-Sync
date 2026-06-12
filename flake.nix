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

        # Toolchain shared by local dev and CI — keep the two shells in
        # sync by editing this list, not the shells.
        corePackages = with pkgs; [
          go
          gopls
          docker
          gcc
          postgresql
        ];

        # Interactive niceties that CI has no use for (and that aren't in
        # the public binary cache, so CI would have to build them).
        devOnlyPackages = with pkgs; [
          bashInteractive
          bash-completion
          nix-bash-completions
          development tool
          opencode
        ];
      in {
        devShells = {
          default = pkgs.mkShell {
            packages = corePackages ++ devOnlyPackages;

            # Export the path so the rcfile can find it
            BASH_COMPLETION_PATH = "${pkgs.bash-completion}/etc/profile.d/bash_completion.sh";

            # Keep shellHook minimal — don't set PS1 here, don't source completion here
            shellHook = ''
              echo "Nix devShell ready. Tools: $(go version 2>/dev/null)"
            '';
          };

          ci = pkgs.mkShell {
            packages = corePackages;
          };
        };
      });
}
