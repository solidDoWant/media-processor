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
            pkgs.go_1_26
            pkgs.golangci-lint
            pkgs.gnumake
            pkgs.docker
            pkgs.docker-compose
          ];
          shellHook = ''
            if [ -f .env.hatchet ]; then
              # shellcheck disable=SC1091
              source .env.hatchet
            fi

            # Start Docker daemon if not already running
            if ! docker info &>/dev/null 2>&1; then
              echo "Docker daemon not running — starting dockerd..."
              sudo mkdir -p /tmp/docker-nix
              sudo chown root:997 /tmp/docker-nix
              sudo chmod 775 /tmp/docker-nix
              sudo sh -c 'dockerd --data-root /tmp/docker-nix &>/tmp/docker-nix/dockerd.log &'
              # Wait up to 10 seconds for the daemon to become ready
              for i in $(seq 1 10); do
                if docker info &>/dev/null 2>&1; then
                  echo "Docker daemon started."
                  break
                fi
                sleep 1
              done
              if ! docker info &>/dev/null 2>&1; then
                echo "Warning: Docker daemon did not start in time. Check /tmp/docker-nix/dockerd.log for details." >&2
              fi
            fi
          '';
        };
      });
}
