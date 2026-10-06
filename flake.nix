{
  description = "MLS-Grid-Sync";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";

    claude-code = {
      url = "github:sadjow/claude-code-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    codex-cli-nix = {
      url = "github:sadjow/codex-cli-nix";
      inputs.nixpkgs.follows = "nixpkgs";
      inputs.flake-utils.follows = "flake-utils";
    };
    headless-paper = {
      url = "git+https://github.com/LunarHUE/headless-paper.git?ref=main";
      inputs.nixpkgs.follows = "nixpkgs";
      inputs.flake-utils.follows = "flake-utils";
    };
    t3code = {
      url = "github:LunarHUE/t3code";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = { self, nixpkgs, flake-utils, claude-code, codex-cli-nix, headless-paper, t3code, ... }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs {
          inherit system;

          config.allowUnfree = true;

          overlays = [
            claude-code.overlays.default
            codex-cli-nix.overlays.default
          ];
        };

        # headless-paper is private, so only the dev shell may touch it.
        # Putting its overlay on the shared pkgs would make CI fetch it too.
        devPkgs = pkgs.extend headless-paper.overlays.default;

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
          pkgs.claude-code
          pkgs.codex
          opencode
          devPkgs.headless-paper
        ]
        ++ pkgs.lib.optionals (t3code.packages ? ${system}) [
          t3code.packages.${system}.t3
          t3code.packages.${system}.t3-devcontainer
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
