{
  description = "media-processor development environment";

  inputs = {
    # Pinned to the last nixpkgs-unstable commit before intel-gmmlib was bumped
    # to 22.10.0, which breaks iHD driver init on Intel Arc / Alchemist GPUs.
    # All packages (ffmpeg-full, intel-media-driver, vpl-gpu-rt) are pre-built
    # in the public binary cache at this revision — no local compilation needed.
    # To upgrade: verify intel-gmmlib-22.10.0 works first, then run nix flake update.
    # Test command for QSV support: `ffmpeg -f lavfi -i testsrc -c:v h264_qsv -t 1 -f null -`
    nixpkgs.url = "github:NixOS/nixpkgs/6214c690786d058d96962a791bf6d39c8c0276cf";
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
            pkgs.pkg-config
            pkgs.ffmpeg-full           # pre-built in cache with --enable-libvpl
            pkgs.intel-media-driver   # VA-API driver (iHD) for 8th-gen+ Intel iGPU
            pkgs.vpl-gpu-rt            # oneVPL GPU runtime (libmfx-gen); required by libvpl dispatcher
            pkgs.direnv
            pkgs.nix-direnv
          ];
          buildInputs = [
            pkgs.ffmpeg-full.dev
          ];
          # Point libva at the Nix-built iHD driver so QSV can open an MFX session.
          # iHD supports Broadwell and later; swap LIBVA_DRIVER_NAME=i965 for older hardware.
          LIBVA_DRIVERS_PATH = "${pkgs.intel-media-driver}/lib/dri";
          # Tell the oneVPL dispatcher where to find the Intel GPU implementation
          # (libmfx-gen.so). libvpl uses ONEVPL_SEARCH_PATH (not VPL_IMPL_SEARCH_PATH)
          # to scan for implementations; without this, MFXLoad finds no implementations
          # and MFXCreateSession fails with MFX_ERR_DEVICE_FAILED (-9).
          ONEVPL_SEARCH_PATH = "${pkgs.vpl-gpu-rt}/lib";
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
