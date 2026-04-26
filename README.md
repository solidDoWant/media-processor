# media-processor

media-processor sits transparently between a download client and Sonarr/Radarr. When a file lands in the watched download directory, it is automatically transcoded to H.265 in a Matroska container and the appropriate library service is notified to import the result.

## How it works

Two binaries run as separate processes and communicate through [Hatchet](https://hatchet.run), a distributed workflow engine:

- **The watcher** (`bin/watcher`) — scans configured filesystem paths on a configurable cron schedule and submits a media-processing job to Hatchet for each file found.
- **The worker** (`bin/worker`) — pulls jobs from Hatchet and processes them. It probes each file with FFmpeg, transcodes it, and notifies Radarr or Sonarr when the output is ready.

Hardware-accelerated encoding is supported via NVIDIA NVENC, Intel QSV (oneVPL), and VAAPI on Linux.

### The bind-mount arrangement

The key to making this transparent to Sonarr/Radarr is a single bind mount:

| Path                                                 | Visible to                       |
| ---------------------------------------------------- | -------------------------------- |
| `/downloads`                                         | Download client, watcher, worker |
| `/processed-output`                                  | watcher, worker                  |
| `/downloads` (bind-mounted from `/processed-output`) | Sonarr/Radarr                    |

Sonarr/Radarr's `/downloads` is bind-mounted from `/processed-output` on the host. This means Sonarr/Radarr never sees the raw download — it only sees files after they have been processed. The download client reports `/downloads/…` to Sonarr/Radarr; because `/processed-output` is mounted there, the path resolves to the transcoded output automatically.

### End-to-end flow

1. User requests media via Sonarr or Radarr.
2. Sonarr/Radarr sends the release to the download client.
3. The download client saves the completed file to `/downloads` and reports the path back to Sonarr/Radarr.
4. The watcher detects the new file and submits a Hatchet job.
5. The worker picks up the job: it probes the file, detects black-bar crop, and writes an MKV output to the directory specified by `output.path` in the watcher config (mirroring the input's subdirectory under that path when `watchedPath` is a parent of the input file). Non-H.264/H.265 video is re-encoded to H.265; H.264 or H.265 sources already in MKV are remuxed without re-encode unless a crop is being applied.
6. The worker calls the Radarr or Sonarr API to trigger a library rescan using the transcoded output file path. When `output.remotePath` is configured, the `output.path` prefix in that path is replaced by `output.remotePath` to produce the path as Sonarr/Radarr sees it (e.g., local `/processed-output/movies/film.mkv` becomes `/downloads/movies/film.mkv` when `output.remotePath` is `/downloads`).
7. Sonarr/Radarr scans the notified path, finds the processed file, and imports it into the library.

## Building

Requires Go 1.26 and FFmpeg 8 shared libraries. Use the Nix dev shell to get everything:

```sh
nix develop
make build
```

Binaries are written to `bin/watcher` and `bin/worker`.

## Container deployment

Container images for the watcher and worker can be built locally via `make build-images`. See [docs/container-deployment.md](docs/container-deployment.md) for volume mounts, environment variables, and hardware device passthrough.

## Configuration

### Watcher

The watcher is configured by a YAML file (passed via `--config`, defaults to `config.yaml`) and a small set of environment variables. See [docs/configuration.md](docs/configuration.md) for the full reference.

Minimal watcher config:

```yaml
watches:
  - name: movies
    watchedPath: /downloads/movies
    mediaType: movie
    output:
      path: /processed/movies
  - name: shows
    watchedPath: /downloads/tv
    mediaType: show
    output:
      path: /processed/tv
```

### Worker

The worker is configured entirely via environment variables. Required variables:

| Variable               | Description                                 |
| ---------------------- | ------------------------------------------- |
| `HATCHET_CLIENT_TOKEN` | Hatchet API token                           |
| `RADARR_URL`           | Radarr base URL (e.g. `http://radarr:7878`) |
| `RADARR_API_KEY`       | Radarr API key                              |
| `SONARR_URL`           | Sonarr base URL (e.g. `http://sonarr:8989`) |
| `SONARR_API_KEY`       | Sonarr API key                              |

See [docs/configuration.md](docs/configuration.md) for all variables including optional ones.

## Metrics and observability

Both binaries support Prometheus pull and OTLP push metrics. See [docs/metrics.md](docs/metrics.md).

## Kubernetes deployment

A Helm chart is available for deploying the watcher and worker to Kubernetes. See [docs/helm.md](docs/helm.md) for installation instructions, a full values reference, and a worked example.

For the full list of network connections made by each pod (required when writing `NetworkPolicy` resources), see [docs/network-policies.md](docs/network-policies.md).

## Hardware acceleration

NVIDIA NVENC, Intel QSV, and VAAPI are supported. See [docs/hardware-acceleration.md](docs/hardware-acceleration.md).

## Development

```sh
make fmt         # go fmt
make lint        # golangci-lint
make test        # unit tests
make test-integration   # starts local Hatchet containers, generates an API token, runs integration tests
make test-e2e    # end-to-end tests (requires Docker; first run downloads ~700 MB fixture)
```
