# Container deployment

This guide covers running the watcher and worker as containers using the OCI images produced by `make build-images`. For the full environment-variable reference, see [configuration.md](configuration.md); for hardware-encoder details, see [hardware-acceleration.md](hardware-acceleration.md).

## Building the images

`make build-images` builds the watcher and worker OCI images with Nix and loads them into the local Docker daemon. It requires Nix and a running Docker daemon on the build host.

```sh
make build-images
```

After a successful build, both images are available locally:

- `watcher:latest` — also tagged under `ghcr.io/soliddowant/watcher`
- `worker:latest` — also tagged under `ghcr.io/soliddowant/worker`

The registry prefix and version used for the additional tags are defined at the top of the `Makefile` (`CONTAINER_REGISTRY`, `VERSION`). To push to a registry, override those and set `PUSH_ALL=true`:

```sh
make build-images CONTAINER_REGISTRY=ghcr.io/your-org VERSION=1.2.3 PUSH_ALL=true
```

Both images run as UID/GID `1000:1000` by default. If the host directories you bind-mount are owned by a different user, either override with `--user` at runtime or `chown` the host directories to match.

## Running the watcher

The watcher scans one or more download directories at a configurable interval and starts a Temporal workflow execution for each file it finds. It reads its YAML config from the path supplied via `--config`; the binary defaults `--config` to `config.yaml`, but the container image does not ship a config file at that path, so you typically mount one and/or pass `--config` explicitly.

### Required volume mounts

| Host path           | Container path      | Notes                                                                                                            |
| ------------------- | ------------------- | ---------------------------------------------------------------------------------------------------------------- |
| watcher config YAML | any path you choose | Mount read-only. Pass the container path via `--config`. Can also be supplied as a ConfigMap under Kubernetes.   |
| download root       | `/downloads`        | Same tree the download client writes to. Read-only access is sufficient — the watcher does not modify this tree. |

### Required environment variables

| Variable              | Description                                                                              |
| --------------------- | ---------------------------------------------------------------------------------------- |
| `TEMPORAL_ADDRESS`    | Temporal frontend `host:port` (for example `temporal-frontend:7233`).                    |
| `TEMPORAL_NAMESPACE`  | Temporal namespace the workflows execute in (for example `default`).                     |
| `TEMPORAL_TASK_QUEUE` | Task queue the watcher dispatches to. Must match the worker's `TEMPORAL_TASK_QUEUE`.     |

The watcher dials Temporal with `TEMPORAL_ADDRESS` and `TEMPORAL_NAMESPACE` and runs a `CheckHealth` request against the frontend before the scan loop starts. Of the three variables, only `TEMPORAL_TASK_QUEUE` is explicitly checked for non-emptiness at startup; an empty `TEMPORAL_ADDRESS` or `TEMPORAL_NAMESPACE` falls back to the Temporal Go SDK defaults (`localhost:7233` and `default`), which production deployments will need to override so the dial and health check succeed.

For the watcher, the health server (`/healthz` liveness, `/readyz` readiness) always runs on `:8081` by default; set `HEALTH_ADDR` to override the listen address. `METRICS_ADDR` (for example `:9090`) enables an optional Prometheus `/metrics` endpoint. See [configuration.md](configuration.md) for the full list of watcher and worker environment variables.

### Example invocation

```sh
docker run --rm \
  -v /srv/media/downloads:/downloads \
  -v /srv/media/watcher.yaml:/etc/watcher.yaml:ro \
  -e TEMPORAL_ADDRESS=temporal-frontend:7233 \
  -e TEMPORAL_NAMESPACE=default \
  -e TEMPORAL_TASK_QUEUE=media-processor \
  watcher:latest --config /etc/watcher.yaml
```

## Running the worker

The worker polls a Temporal task queue, transcodes each file, writes the output to the directory specified by `output.path` in the watcher config, and notifies Radarr or Sonarr.

### Required volume mounts

| Host path             | Container path      | Notes                                                                                                                                                                                                          |
| --------------------- | ------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| download root         | `/downloads`        | Same tree the watcher sees. Must be writable — the worker removes the source file after a successful transcode unless `preserveSource: true` is set in the watcher config.                                     |
| processed-output root | `/processed-output` | Where transcoded files are written. Must match the `output.path` values in the watcher config. See the [bind-mount arrangement](../README.md#the-bind-mount-arrangement) in the README for how this directory is exposed to Sonarr/Radarr. |

### Required environment variables

| Variable                       | Description                                                                          |
| ------------------------------ | ------------------------------------------------------------------------------------ |
| `TEMPORAL_ADDRESS`             | Temporal frontend `host:port` (for example `temporal-frontend:7233`).                |
| `TEMPORAL_NAMESPACE`           | Temporal namespace the workflows execute in (for example `default`).                 |
| `TEMPORAL_TASK_QUEUE`          | Task queue the worker polls. Must match the watcher's `TEMPORAL_TASK_QUEUE`.         |
| `RADARR_URL`, `RADARR_API_KEY` | Radarr base URL and API key.                                                         |
| `SONARR_URL`, `SONARR_API_KEY` | Sonarr base URL and API key.                                                         |

As with the watcher, the worker dials Temporal with `TEMPORAL_ADDRESS` and `TEMPORAL_NAMESPACE` and runs a `CheckHealth` request against the frontend before it starts polling. Only `TEMPORAL_TASK_QUEUE` is explicitly checked for non-emptiness at startup; an empty `TEMPORAL_ADDRESS` or `TEMPORAL_NAMESPACE` falls back to the Temporal Go SDK defaults (`localhost:7233` and `default`).

See [configuration.md](configuration.md) for the full list of worker environment variables, including crop-detection tuning, webhook notifications, and quality settings.

### Hardware device passthrough

For Intel QSV or VAAPI hardware encoding, pass the `/dev/dri` device tree into the container. The container process must also have permission to access the render node — typically this means running with a group that owns `/dev/dri/renderD*` on the host.

Docker flags:

```sh
--device /dev/dri:/dev/dri \
--group-add $(getent group render | cut -d: -f3)
```

The render-node GID varies between hosts. The command above resolves it from `/etc/group` at runtime; alternatively, pass the numeric GID directly (for example `--group-add 104`).

On Linux hosts with more than one rendering device, set `MEDIA_HARDWARE_DEVICE_PATH` to pick a specific one (e.g. `/dev/dri/renderD129`). When unset, the worker selects a device automatically. See [hardware-acceleration.md](hardware-acceleration.md) for backend selection and device-path semantics per backend.

The worker image already bundles the Intel iHD VA-API driver and the oneVPL GPU runtime, with `LIBVA_DRIVERS_PATH` and `ONEVPL_SEARCH_PATH` pre-set to point to them. Operators do not need to set these variables; override them only if you replace the bundled drivers with a mounted alternative.

### Required Linux capability

The Intel media stack (iHD, oneVPL) calls `set_mempolicy` for NUMA-aware memory placement, which requires the `SYS_NICE` capability. Docker drops this capability by default; add it explicitly:

```sh
--cap-add=SYS_NICE
```

### Example invocation

```sh
docker run --rm \
  --cap-add=SYS_NICE \
  --device /dev/dri:/dev/dri \
  --group-add $(getent group render | cut -d: -f3) \
  -v /srv/media/downloads:/downloads \
  -v /srv/media/processed-output:/processed-output \
  -e TEMPORAL_ADDRESS=temporal-frontend:7233 \
  -e TEMPORAL_NAMESPACE=default \
  -e TEMPORAL_TASK_QUEUE=media-processor \
  -e RADARR_URL=http://radarr:7878 \
  -e RADARR_API_KEY=... \
  -e SONARR_URL=http://sonarr:8989 \
  -e SONARR_API_KEY=... \
  worker:latest
```

## Example: Docker Compose

A minimal compose file that runs both services against an existing Temporal cluster. The render-node GID is supplied via the `RENDER_GID` environment variable because Docker resolves `group_add` names inside the container and the Nix-built images do not define a `render` group. Export it on the host before `docker compose up`:

```sh
export RENDER_GID=$(getent group render | cut -d: -f3)
```

```yaml
services:
  watcher:
    image: watcher:latest
    restart: on-failure
    volumes:
      - /srv/media/downloads:/downloads
      - /srv/media/watcher.yaml:/etc/watcher.yaml:ro
    environment:
      TEMPORAL_ADDRESS: temporal-frontend:7233
      TEMPORAL_NAMESPACE: default
      TEMPORAL_TASK_QUEUE: media-processor
      HEALTH_ADDR: ":9091"
      METRICS_ADDR: ":9090"
    command: ["--config", "/etc/watcher.yaml"]

  worker:
    image: worker:latest
    restart: on-failure
    cap_add:
      - SYS_NICE
    devices:
      - /dev/dri:/dev/dri
    group_add:
      - "${RENDER_GID}"
    volumes:
      - /srv/media/downloads:/downloads
      - /srv/media/processed-output:/processed-output
    environment:
      TEMPORAL_ADDRESS: temporal-frontend:7233
      TEMPORAL_NAMESPACE: default
      TEMPORAL_TASK_QUEUE: media-processor
      RADARR_URL: http://radarr:7878
      RADARR_API_KEY: "${RADARR_API_KEY}"
      SONARR_URL: http://sonarr:8989
      SONARR_API_KEY: "${SONARR_API_KEY}"
      HEALTH_ADDR: ":9091"
      METRICS_ADDR: ":9090"
```
