# Configuration

## Watcher config file

The watcher reads a YAML configuration file at startup. Pass the path with `--config` (default: `config.yaml`).

### Schema

A JSON Schema for the watcher config is available [here](https://github.com/solidDoWant/media-processor/blob/master/schemas/watcher.schema.json).

Editors that support [yaml-language-server](https://github.com/redhat-developer/yaml-language-server) (VS Code with the YAML extension, Neovim via nvim-lspconfig, etc.) will pick up the schema automatically from the modeline comment shown in the example below, giving you inline validation and autocompletion.

### Full example

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/solidDoWant/media-processor/refs/heads/master/schemas/watcher.schema.json

# cronSchedule controls how often each watch directory is scanned.
# Accepts a 6-field cron expression (seconds-precision).
# Defaults to "*/5 * * * * *" (every 5 seconds) when omitted.
cronSchedule: "*/5 * * * * *"

watches:
  - name: movies
    path: /downloads/movies
    mediaType: movie          # "movie" or "show"
    ignorePatterns:
      - \.!qB$               # incomplete qBittorrent downloads
      - (^|/)_unpack(/|$)    # unpack-in-progress directories
    preserveSource: false     # delete source after processing (default)
    retainEmptyDirectories: false  # prune empty dirs after deletion (default)

  - name: shows
    path: /downloads/tv
    mediaType: show

  - name: archive
    path: /downloads/archive
    mediaType: movie
    preserveSource: true           # keep the original file
    retainEmptyDirectories: true   # leave empty directories in place
```

### Fields

| Field                              | Type     | Default         | Description                                                                                                                                                                            |
| ---------------------------------- | -------- | --------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `cronSchedule`                     | string   | `*/5 * * * * *` | 6-field cron expression controlling how often each watch directory is scanned. Defaults to every 5 seconds when omitted.                                                               |
| `watches[].name`                   | string   | —               | **Required.** Logical name for this watch entry; used as a label in metrics.                                                                                                           |
| `watches[].path`                   | string   | —               | **Required.** Path to watch. Relative paths are resolved against the watcher's working directory.                                                                                      |
| `watches[].mediaType`              | string   | —               | **Required.** `movie` or `show`. Determines which library service (Radarr or Sonarr) is notified.                                                                                      |
| `watches[].ignorePatterns`         | []string | `[]`            | Regular expressions in [RE2 syntax](https://github.com/google/re2/wiki/Syntax). A file whose path matches any pattern is silently skipped; a directory match skips the entire subtree. |
| `watches[].preserveSource`         | bool     | `false`         | When `true`, the source file is kept after successful transcoding.                                                                                                                     |
| `watches[].retainEmptyDirectories` | bool     | `false`         | When `true`, parent directories that become empty after source deletion are left in place rather than being deleted up to the watch root.                                              |

## Environment variables

Variables marked **Required** cause the binary to exit immediately if unset or empty.

### Shared (watcher and worker)

| Variable                      | Type   | Default                              | Required     | Description                                                                                                                                             |
| ----------------------------- | ------ | ------------------------------------ | ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `LOG_LEVEL`                   | string | `info`                               | Optional     | Log verbosity: `debug`, `info`, `warn`, or `error`. Unrecognised values fall back to `info`.                                                            |
| `HATCHET_CLIENT_TOKEN`        | string | —                                    | **Required** | Hatchet API token.                                                                                                                                      |
| `HEALTH_ADDR`                 | string | `:8080` (worker) / `:8081` (watcher) | Optional     | TCP address for the HTTP health server. Exposes `/healthz` (liveness) and `/readyz` (readiness). Always enabled; override to change the listen address. |
| `METRICS_ADDR`                | string | `""`                                 | Optional     | TCP address for the Prometheus `/metrics` pull endpoint (e.g. `:9090`). Disabled when empty.                                                            |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | string | `""`                                 | Optional     | OTLP gRPC endpoint for pushing metrics (e.g. `http://otel-collector:4317`). Disabled when empty.                                                        |

### Worker only

| Variable                          | Type           | Default | Required     | Description                                                                                                                                                                                                                                                                                                                                                                                                     |
| --------------------------------- | -------------- | ------- | ------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `MEDIA_OUTPUT_DIR`                | path           | —       | **Required** | Directory where transcoded output files are written.                                                                                                                                                                                                                                                                                                                                                            |
| `RADARR_URL`                      | URL            | —       | **Required** | Radarr base URL (e.g. `http://radarr:7878`).                                                                                                                                                                                                                                                                                                                                                                    |
| `RADARR_API_KEY`                  | string         | —       | **Required** | Radarr API key.                                                                                                                                                                                                                                                                                                                                                                                                 |
| `SONARR_URL`                      | URL            | —       | **Required** | Sonarr base URL (e.g. `http://sonarr:8989`).                                                                                                                                                                                                                                                                                                                                                                    |
| `SONARR_API_KEY`                  | string         | —       | **Required** | Sonarr API key.                                                                                                                                                                                                                                                                                                                                                                                                 |
| `RADARR_LOCAL_PATH_PREFIX`        | path           | `""`    | Optional     | Worker-side prefix for Radarr path translation. Set together with `RADARR_REMOTE_PATH_PREFIX`; setting one without the other will produce paths Radarr cannot resolve.                                                                                                                                                                                                                                          |
| `RADARR_REMOTE_PATH_PREFIX`       | path           | `""`    | Optional     | Radarr-side prefix for path translation. Replaces `RADARR_LOCAL_PATH_PREFIX` in paths sent to Radarr.                                                                                                                                                                                                                                                                                                           |
| `SONARR_LOCAL_PATH_PREFIX`        | path           | `""`    | Optional     | Worker-side prefix for Sonarr path translation. Set together with `SONARR_REMOTE_PATH_PREFIX`; setting one without the other will produce paths Sonarr cannot resolve.                                                                                                                                                                                                                                          |
| `SONARR_REMOTE_PATH_PREFIX`       | path           | `""`    | Optional     | Sonarr-side prefix for path translation. Replaces `SONARR_LOCAL_PATH_PREFIX` in paths sent to Sonarr.                                                                                                                                                                                                                                                                                                           |
| `MEDIA_WEBHOOK_URL`               | URL            | `""`    | Optional     | Endpoint POSTed to when a media file fails to process. A single aggregated notification is sent per failed run, summarising every error encountered. No notification is sent when empty.                                                                                                                                                                                                                        |
| `MEDIA_INPUT_ROOT`                | path           | `""`    | Optional     | Root of the watched input directories. When set, transcoded output is placed in a mirrored subdirectory under `MEDIA_OUTPUT_DIR`. **Must be set** for nested downloads to produce matching subdirectory structure in the output.                                                                                                                                                                                |
| `MEDIA_HARDWARE_DEVICE_PATH`      | path           | `""`    | Optional     | Hardware device path passed to the encoder (e.g. `/dev/dri/renderD128`). Hardware acceleration is auto-detected regardless; this controls which specific device is opened. When empty, the library selects a device automatically.                                                                                                                                                                              |
| `MEDIA_MIN_CROP_X`                | integer        | `10`    | Optional     | Minimum pixels to trim horizontally before a crop is applied. `-1` disables the threshold (any detected crop is accepted).                                                                                                                                                                                                                                                                                      |
| `MEDIA_MIN_CROP_Y`                | integer        | `10`    | Optional     | Minimum pixels to trim vertically before a crop is applied. `-1` disables the threshold.                                                                                                                                                                                                                                                                                                                        |
| `MEDIA_DETECT_CROP_TIMEOUT`       | duration       | `30m`   | Optional     | Maximum time allowed for crop detection before it is considered failed (e.g. `45m`, `1h`).                                                                                                                                                                                                                                                                                                                      |
| `MEDIA_TRANSCODE_TIMEOUT`         | duration       | `4h`    | Optional     | Maximum time allowed for transcoding before it is considered failed (e.g. `2h`, `8h`).                                                                                                                                                                                                                                                                                                                          |
| `MEDIA_PROGRESS_LOG_INTERVAL`     | duration       | `30s`   | Optional     | How often a progress log line is emitted during transcoding (e.g. `1m`, `5m`). Each line includes the estimated completion percentage, elapsed time, and frames processed. Set to `0` to disable progress logging.                                                                                                                                                                                              |
| `MEDIA_H265_CRF`                  | integer        | _unset_ | Optional     | Constant-quality value for H.265 encoding. Valid values are `1`–`51` (lower is higher quality); any other value (including `0`) causes the worker to exit at startup. When unset, the encoder's built-in default is used. See [hardware-acceleration.md](hardware-acceleration.md#quality-tuning) for how this value is applied per encoder.                                                                    |
| `METRICS_HIGH_CARDINALITY_LABELS` | `true`/`false` | `false` | Optional     | When set to exactly the string `true` (case-sensitive), per-item labels (title, year, episode, etc.) are attached to media workflow histogram observations. This adds fine-grained drill-down at the cost of creating a separate metric series per item, which can be expensive in your metrics backend. See [metrics.md](metrics.md#high-cardinality-labels) for the label set. Any other value disables them. |

## Path translation

The path the worker sends to Radarr/Sonarr is derived from the watcher's view of the source file (the path passed in by the watcher, with the extension swapped to `.mkv`) — not from `MEDIA_OUTPUT_DIR`. If the worker and the arr service mount that source volume at different paths, use the `*_LOCAL_PATH_PREFIX` / `*_REMOTE_PATH_PREFIX` pairs to rewrite the worker-side prefix to the arr-side prefix before the import command is sent.

Example: the watcher reads from `/mnt/downloads` (the worker's mount of the download volume) but Sonarr sees the same volume as `/downloads`:

```
SONARR_LOCAL_PATH_PREFIX=/mnt/downloads
SONARR_REMOTE_PATH_PREFIX=/downloads
```

A source path of `/mnt/downloads/tv/show/ep.mp4` produces an import path of `/mnt/downloads/tv/show/ep.mkv`, which is sent to Sonarr as `/downloads/tv/show/ep.mkv`.

When the watcher and the arr service already see the source volume at the same path (the typical bind-mount setup in the README), no translation is needed and these variables can be left unset.
