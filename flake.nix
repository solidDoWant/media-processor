{
  description = "media-processor development environment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in {
        devShells.default = pkgs.mkShell {
          packages = [
            pkgs.go_1_25
            pkgs.golangci-lint
            pkgs.gnumake
          ];
          shellHook = ''
            if [ -f .env.hatchet ]; then
              # shellcheck disable=SC1091
              source .env.hatchet
            fi
          '';
        };
      });
}
