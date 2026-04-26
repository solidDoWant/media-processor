# media-processor Specification

## Tech Stack

| Component | Technology |
|-----------|------------|
| Language | Go 1.26 |
| Workflow orchestration | [Hatchet](https://hatchet.run) |
| Database | PostgreSQL |
| Directory scanning | `filepath.WalkDir` on a Hatchet cron schedule |
| Media processing | `github.com/asticode/go-astiav` (FFmpeg 8 Cgo bindings) |
| Static analysis | `golangci-lint` |
| Dev environment | Nix (flake.nix) |

## Binary Responsibilities

### `cmd/watcher`

Scans configured filesystem paths on a cron schedule and submits a Hatchet job for each file found. Uses `filepath.WalkDir` for recursive directory traversal rather than filesystem event notifications.

Responsibilities:
- Load and parse the watcher YAML config at startup
- On each cron tick, walk all configured watch directories and submit a Hatchet job per file found
- Apply ignore patterns to skip files and directory subtrees
- Reconnect and retry on transient failures

### `cmd/worker`

Registers workflow step handlers with Hatchet and processes jobs dispatched by the watcher. Performs the actual media processing by orchestrating calls into the library packages.

Responsibilities:
- Connect to Hatchet and register all workflow step handlers
- Invoke `pkg/ffprobe` to inspect incoming media files
- Invoke `pkg/ffmpeg` from `workflows/steps` to transcode or transform media
- Report step results and errors back to the workflow engine

## Library Package Contracts

### `pkg/ffmpeg`

Wraps `libavcodec`, `libavformat`, and related libraries via `github.com/asticode/go-astiav` for in-process media transcoding.

- Exposes a builder API: `NewTranscode(in, out).ToVideoCodec(...).ToAudioCodec(...).ToContainer(...).HardwareAccel(...).Build().Run(ctx)`
- Hardware encoder availability is determined in-process via `astiav.FindEncoderByName` — no subprocess probe
- No external binary required; CGO must be enabled and FFmpeg 8 shared libraries must be present at runtime

### `pkg/ffprobe`

Wraps `libavformat` and `libavcodec` via `github.com/asticode/go-astiav` for in-process media file inspection.

- Accepts a file path and context; returns structured `MediaInfo` (container format, duration, overall bitrate, streams)
- No external binary required; context cancellation is honoured via `astiav.IOInterrupter`

### `pkg/medialib`

Defines the shared media-library domain model used by the watcher, worker, and arr clients.

- Declares the `MediaType` enum (`movie`, `show`) plus the `Movie` and `Episode` types and their accessor interface
- Subpackages `pkg/medialib/radarr` and `pkg/medialib/sonarr` wrap the Radarr and Sonarr REST APIs (lookup, library import, path translation)
- Does not perform transcoding itself — workflow steps in `workflows/steps` invoke `pkg/ffmpeg`/`pkg/ffprobe` directly

### `pkg/webhook`

Provides an HTTP client for posting workflow-failure notifications to a configured outbound endpoint.

- `Client.NotifyFailure` POSTs a `FailureEvent` (workflow, file path, step, error) as JSON to `Client.URL`
- The payload shape is pluggable via `PayloadFunc`; `DefaultPayload` emits `{"workflow","file_path","step","error"}`
- A no-op when `Client.URL` is empty

## Sonarr/Radarr Integration Flow

This project is designed to sit transparently between a download client and Sonarr/Radarr, transcoding or transmuxing media before it is imported into the library.

### Directory layout

| Path | Visible to |
|------|------------|
| `/downloads` | Download client, watcher/worker |
| `/processed-output` | watcher/worker |
| `/downloads` (bind-mounted from `/processed-output`) | Sonarr/Radarr |

Sonarr/Radarr has `/processed-output` bind-mounted as its own `/downloads`. This means Sonarr/Radarr cannot see files in the real `/downloads` directory — it only sees files that have been placed in `/processed-output`. This is the mechanism that prevents premature import of unprocessed files.

### End-to-end flow

1. **User requests media.** The user requests a movie or TV episode via Sonarr or Radarr.
2. **Download initiated.** Sonarr/Radarr searches configured indexers, selects a release, and sends it to the configured download client.
3. **File lands in `/downloads`.** The download client saves the completed file to the real `/downloads` directory and reports the file path back to Sonarr/Radarr.
4. **Watcher detects the file.** The `cmd/watcher` process, which scans `/downloads` recursively on each Hatchet cron tick using `filepath.WalkDir`, discovers the new file and submits a media-processor workflow job to Hatchet.
5. **Workflow runs.** The `cmd/worker` process picks up the job. It probes the file with `pkg/ffprobe`, then transcodes or transmuxes it if required via `pkg/ffmpeg`/`pkg/medialib`, writing the output to `output.path` from the watcher config (mirroring the input's relative subdirectory under that path when `watchedPath` is a parent of the input file).
6. **Library import triggered.** The workflow's `notify` step calls `ArrLibrary.ImportByFilePath` with the transcoded output file path (`transcode.DestFilePath`). When `output.remotePath` is set, the `output.path` prefix is replaced by `output.remotePath` to produce the path as Sonarr/Radarr sees it (e.g., local `/processed/movies/sub/film.mkv` becomes `/media/movies/sub/film.mkv`). A `DownloadedMoviesScan` (Radarr) or `DownloadedEpisodesScan` (Sonarr) command is sent with that path, triggering the normal import pipeline.
7. **Sonarr/Radarr imports the file.** On receiving the refresh command, Sonarr/Radarr scans the path it was given (the `output.remotePath`-prefixed path, or the `output.path`-based path when `output.remotePath` is not set), finds the processed file, and imports it into the library.

### Why the bind-mount is necessary

The download client reports the file location to Sonarr/Radarr using the path as it sees it (`/downloads/…`). Without intervention, Sonarr/Radarr would attempt to import the original unprocessed file as soon as the download completes. By bind-mounting `/processed-output` over Sonarr/Radarr's `/downloads`, the processed output directory and the download client's report path are made to coincide — so Sonarr/Radarr will not find (and therefore not import) any file until the workflow has finished processing it and written the result to `/processed-output`.

## Configuration

| Config item | Mechanism | Rationale |
|-------------|-----------|-----------|
| Watcher path-to-workflow mappings | YAML file (path passed via `--config` flag) | Structured, multi-key data that is unwieldy as environment variables |
| All other runtime config (DB URL, Hatchet endpoint, secrets, log level) | Environment variables | Standard 12-factor practice; integrates cleanly with Kubernetes secrets |

No config file merging is performed. Exactly one YAML config file path is accepted; all other configuration comes from the process environment.

### Watcher config example

```yaml
cronSchedule: "*/5 * * * * *"
watches:
  - name: movies
    watchedPath: /media/incoming/movies
    mediaType: movie
    output:
      path: /media/processed/movies
    ignorePatterns:
      - \.!qB$
      - (^|/)_unpack(/|$)
  - name: shows
    watchedPath: /media/incoming/tv
    mediaType: show
    output:
      path: /media/processed/tv
  - name: archive
    watchedPath: /media/incoming/archive
    mediaType: movie
    output:
      path: /media/processed/archive
    preserveSource: true
    retainEmptyDirectories: true
```

`cronSchedule`, `ignorePatterns`, `preserveSource`, and `retainEmptyDirectories` are all optional. A minimal entry needs `name`, `watchedPath`, `mediaType`, and `output.path`. `ignorePatterns` accepts Go regular expressions; a matching file is silently skipped, a matching directory skips its entire subtree. When `preserveSource: true` is set on a watch entry, the source file is kept after successful processing; omitting it or setting it to `false` retains the default behaviour of deleting the source file. By default, after a source file is deleted (either because it is invalid media or after successful processing), any parent directories that become empty are removed bottom-up, stopping at the watch root. Set `retainEmptyDirectories: true` to disable this behaviour and leave empty directories in place.

### Environment variables

Variables marked **Required** cause the binary to exit immediately on startup when unset or empty.

#### Shared (`cmd/watcher` and `cmd/worker`)

| Variable | Type | Default | Required | Description |
|----------|------|---------|----------|-------------|
| `LOG_LEVEL` | string | `info` | Optional | Log verbosity: `debug`, `info`, `warn`, or `error`. An unrecognised value falls back to `info`. |
| `HATCHET_CLIENT_TOKEN` | string | — | **Required** | Hatchet API token used by the client SDK. |
| `HEALTH_ADDR` | string (TCP address) | `:8080` (worker) / `:8081` (watcher) | Optional | TCP address for the HTTP health server. Exposes `/healthz` (liveness) and `/readyz` (readiness). Always enabled; override to change the listen address. |
| `METRICS_ADDR` | string (TCP address) | `""` | Optional | TCP address on which to expose the Prometheus `/metrics` pull endpoint (e.g. `:9090`). Disabled when empty. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | string (URL) | `""` | Optional | OTLP gRPC endpoint for pushing metrics (e.g. `http://otel-collector:4317`). Disabled when empty. Follows the standard OpenTelemetry convention. |

#### `cmd/worker`

| Variable | Type | Default | Required | Description |
|----------|------|---------|----------|-------------|
| `RADARR_URL` | string (URL) | — | **Required** | Radarr base URL (e.g. `http://radarr:7878`). |
| `RADARR_API_KEY` | string | — | **Required** | Radarr API key. |
| `SONARR_URL` | string (URL) | — | **Required** | Sonarr base URL (e.g. `http://sonarr:8989`). |
| `SONARR_API_KEY` | string | — | **Required** | Sonarr API key. |
| `MEDIA_WEBHOOK_URL` | string (URL) | `""` | Optional | Webhook endpoint notified on workflow failure. No notification is sent when empty. |
| `MEDIA_HARDWARE_DEVICE_PATH` | string (path) | `""` | Optional | Hardware device path for hardware-accelerated transcoding (e.g. `/dev/dri/renderD128`). When empty, the software encoder is used. |
| `MEDIA_MIN_CROP_X` | integer | `10` | Optional | Minimum number of pixels that must be trimmed horizontally before a crop is applied. Set to `-1` to accept any detected crop. |
| `MEDIA_MIN_CROP_Y` | integer | `10` | Optional | Minimum number of pixels that must be trimmed vertically before a crop is applied. Set to `-1` to accept any detected crop. |
| `MEDIA_DETECT_CROP_TIMEOUT` | Go duration | `30m` | Optional | Hatchet execution timeout for the crop-detection step (e.g. `45m`, `1h`). |
| `MEDIA_TRANSCODE_TIMEOUT` | Go duration | `4h` | Optional | Hatchet execution timeout for the transcode step (e.g. `2h`, `8h`). |
| `MEDIA_H265_CRF` | integer (1–51) | _(encoder default)_ | Optional | H.265 constant-quality (CRF) value. When unset or empty the encoder default is used; values outside 1–51 cause a fatal startup error. |
| `METRICS_HIGH_CARDINALITY_LABELS` | `true` / `false` | `false` | Optional | When `true`, per-item labels (id, title, year, etc.) are attached to metric observations. |
