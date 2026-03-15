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
            DOCKER_SOCK="/tmp/docker.sock"
            export DOCKER_HOST="unix://$DOCKER_SOCK"
            if [ ! -S "$DOCKER_SOCK" ]; then
              echo "Docker daemon not running — starting dockerd..."
              sudo sh -c "dockerd --data-root /tmp/docker-data --host unix://$DOCKER_SOCK --storage-driver vfs &>/tmp/dockerd.log &"
              for i in $(seq 1 10); do
                [ -S "$DOCKER_SOCK" ] && break
                sleep 1
              done
              if [ ! -S "$DOCKER_SOCK" ]; then
                echo "Warning: Docker daemon did not start in time. Check /tmp/dockerd.log for details." >&2
              fi
            fi
            # Ensure socket is accessible by current user
            if [ -S "$DOCKER_SOCK" ]; then
              sudo chmod 666 "$DOCKER_SOCK"
            fi
          '';
        };
      });
}
