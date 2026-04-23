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

        libav-minimal = pkgs.ffmpeg-headless.overrideAttrs (old:
          let
            # Packages whose corresponding FFmpeg feature flag is disabled below.
            # Filtering them out of buildInputs prevents the linker from pulling
            # them into the shared-library RPATH even when the configure flag is
            # set to --disable-*.
            removedBuildInputPnames = [
              "alsa-lib" "amf-headers" "libaom" "libass" "libbluray" "bzip2"
              "fontconfig" "freetype" "fribidi" "gmp-with-cxx" "gnutls"
              "harfbuzz" "xz" "lame" "openapv" "openjpeg" "libopenmpt"
              "libopus" "librist" "soxr" "speex" "srt" "libssh" "svt-av1"
              "libtheora" "v4l-utils" "vid.stab" "libvorbis" "libvpx"
              "libwebp" "libxml2" "xvidcore" "zimg" "zvbi"
            ];
          in {
          # Drop outputs that won't be populated: programs are disabled so no
          # binaries are installed, and documentation is disabled so no man
          # pages or HTML docs are installed. Nix fails if a declared output
          # path is never created.
          outputs = builtins.filter (o: o != "bin" && o != "doc" && o != "man") (old.outputs or [ "out" ]);
          buildInputs =
            (builtins.filter
              (dep: !(builtins.elem (dep.pname or dep.name or "") removedBuildInputPnames))
              (old.buildInputs or [ ]))
            ++ [ pkgs.libvpl ];
          # Append last so these override any earlier enable-* flags from the
          # upstream ffmpeg-headless expression.
          configureFlags = (old.configureFlags or [ ]) ++ [
            "--disable-programs"
            "--disable-manpages"
            "--disable-doc"
            "--enable-libvpl"

            # Software codec encoders not used by any workflow (hardware paths
            # cover H.264/H.265/VP9/AV1; built-in decoders handle the input side):
            "--disable-libsvtav1"
            "--disable-libaom"
            "--disable-libvpx"
            "--disable-libopus"
            "--disable-libvorbis"
            "--disable-libtheora"
            "--disable-libspeex"
            "--disable-libmp3lame"
            "--disable-libopenmpt"
            "--disable-libwebp"
            "--disable-libopenjpeg"
            "--disable-libxvid"
            "--disable-libvidstab"
            "--disable-libzvbi"
            "--disable-liboapv"

            # Text / subtitle / font rendering (drawtext filter, burn-in subs —
            # none of which are used):
            "--disable-libass"
            "--disable-libfreetype"
            "--disable-libfribidi"
            "--disable-libharfbuzz"
            "--disable-fontconfig"
            "--disable-libfontconfig"

            # Network protocols (local file I/O only; file:// is always built in):
            "--disable-network"
            "--disable-librist"
            "--disable-libsrt"
            "--disable-libssh"
            "--disable-gnutls"
            "--disable-gmp"
            "--disable-libxml2"

            # Compression formats beyond zlib (not needed for MP4/MKV):
            "--disable-bzlib"
            "--disable-lzma"

            # Miscellaneous unused features:
            "--disable-libzimg"   # zscale filter
            "--disable-libsoxr"   # optional swresample backend; built-in suffices
            "--disable-amf"       # AMD AMF (only QSV/NVENC/VAAPI implemented)
            "--disable-alsa"      # audio device I/O (server-side processing)
            "--disable-libv4l2"   # V4L2 camera input
            "--disable-v4l2-m2m"  # V4L2 memory-to-memory encoding
            "--disable-opencl"    # OpenCL compute (GPU done via VAAPI/QSV/NVENC)
            "--disable-libbluray" # Blu-ray container support
          ];
          # make check runs fate tests that invoke the ffmpeg CLI; skip since
          # we disabled programs.
          doCheck = false;
        });

        # Only hash Go source files, go.mod, and go.sum so that changes to
        # documentation, Nix files, etc. don't bust the build cache.
        goSrc = pkgs.lib.cleanSourceWith {
          src = ./.;
          filter = path: type:
            let baseName = builtins.baseNameOf path; in
            (type == "directory" && baseName != "e2e") ||
            (type != "directory" && (
              (pkgs.lib.hasSuffix ".go" path && !(pkgs.lib.hasSuffix "_test.go" path)) ||
              baseName == "go.mod" ||
              baseName == "go.sum"
            ));
        };

        watcherVendorHash = "sha256-xJuiqnbIaMTAQ4QquSmW7R/5XLxdeIL1wq90FO0Paa8=";
        workerVendorHash = "sha256-xJuiqnbIaMTAQ4QquSmW7R/5XLxdeIL1wq90FO0Paa8=";

        watcher-bin = pkgs.buildGoModule {
          name = "watcher";
          src = goSrc;
          vendorHash = watcherVendorHash;
          subPackages = [ "cmd/watcher" ];
          ldflags = [ "-s" "-w" ];
          env.CGO_ENABLED = "0";
        };

        worker-bin = pkgs.buildGoModule {
          name = "worker";
          src = goSrc;
          vendorHash = workerVendorHash;
          nativeBuildInputs = [ pkgs.pkg-config ];
          buildInputs = [ libav-minimal.dev ];
          subPackages = [ "cmd/worker" ];
          ldflags = [ "-s" "-w" ];
        };
      in
      {
        packages.watcher-bin = watcher-bin;
        packages.worker-bin = worker-bin;

        packages.watcher-image = pkgs.dockerTools.streamLayeredImage {
          name = "watcher";
          tag = "latest";
          contents = [ pkgs.cacert watcher-bin ];
          config = {
            Entrypoint = [ "/bin/watcher" ];
            User = "1000:1000";
            ExposedPorts = { "8081/tcp" = {}; };
          };
        };

        packages.worker-image = pkgs.dockerTools.streamLayeredImage {
          name = "worker";
          tag = "latest";
          contents = [ libav-minimal pkgs.cacert pkgs.intel-media-driver pkgs.vpl-gpu-rt worker-bin ];
          config = {
            Entrypoint = [ "/bin/worker" ];
            User = "1000:1000";
            ExposedPorts = { "8080/tcp" = {}; };
            Env = [
              "LIBVA_DRIVERS_PATH=${pkgs.intel-media-driver}/lib/dri"
              "ONEVPL_SEARCH_PATH=${pkgs.vpl-gpu-rt}/lib"
            ];
          };
        };

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
            pkgs.gh
            pkgs.dive
            pkgs.nix-prefetch
            pkgs.kubernetes-helm
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
