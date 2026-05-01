# Configuration

## Watcher config file

The watcher reads a YAML configuration file at startup. Pass the path with `--config` (default: `config.yaml`).

### Schema

A JSON Schema for the watcher config is available [here](https://github.com/solidDoWant/media-processor/blob/master/schemas/watcher.schema.json).

Editors that support [yaml-language-server](https://github.com/redhat-developer/yaml-language-server) (VS Code with the YAML extension, Neovim via nvim-lspconfig, etc.) will pick up the schema automatically from the modeline comment shown in the example below, giving you inline validation and autocompletion.

### Full example

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/solidDoWant/media-processor/refs/heads/master/schemas/watcher.schema.json

# scanInterval controls how often each watch directory is scanned.
# Accepts a Go duration string (e.g. "5s", "1m30s").
# Defaults to "5s" (every 5 seconds) when omitted.
scanInterval: 5s

watches:
  - name: movies
    watchedPath: /downloads/movies
    mediaType: movie          # "movie" or "show"
    output:
      path: /processed/movies
      remotePath: /media/movies   # path as seen by Radarr (omit if same as output.path)
    ignorePatterns:
      - \.!qB$               # incomplete qBittorrent downloads
      - (^|/)_unpack(/|$)    # unpack-in-progress directories
    preserveSource: false     # delete source after processing (default)
    retainEmptyDirectories: false  # prune empty dirs after deletion (default)

  - name: shows
    watchedPath: /downloads/tv
    mediaType: show
    output:
      path: /processed/tv

  - name: archive
    watchedPath: /downloads/archive
    mediaType: movie
    output:
      path: /processed/archive
    preserveSource: true           # keep the original file
    retainEmptyDirectories: true   # leave empty directories in place
```

### Fields

| Field                              | Type     | Default         | Description                                                                                                                                                                            |
| ---------------------------------- | -------- | --------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `scanInterval`                     | string   | `5s`            | Go duration string (e.g. `5s`, `1m30s`) controlling how often each watch directory is scanned. Defaults to `5s` when omitted.                                                          |
| `watches[].name`                   | string   | —               | **Required.** Logical name for this watch entry; used as a label in metrics.                                                                                                           |
| `watches[].watchedPath`            | string   | —               | **Required.** Path to the directory to watch. Relative paths are resolved against the watcher's working directory.                                                                     |
| `watches[].mediaType`              | string   | —               | **Required.** `movie` or `show`. Determines which library service (Radarr or Sonarr) is notified.                                                                                      |
| `watches[].output.path`            | string   | —               | **Required.** Directory where processed files for this watch entry are written.                                                                                                        |
| `watches[].output.remotePath`      | string   | `""`            | Path by which the output directory is known to the arr service (Radarr or Sonarr). Set this when the worker and the arr service mount the output volume at different paths. When empty, no path translation is applied. |
| `watches[].ignorePatterns`         | []string | `[]`            | Regular expressions in [RE2 syntax](https://github.com/google/re2/wiki/Syntax). A file whose path matches any pattern is silently skipped; a directory match skips the entire subtree. |
| `watches[].preserveSource`         | bool     | `false`         | When `true`, the source file is kept after successful transcoding.                                                                                                                     |
| `watches[].retainEmptyDirectories` | bool     | `false`         | When `true`, parent directories that become empty after source deletion are left in place rather than being deleted up to the watch root.                                              |

## Environment variables

Variables marked **Required** cause the binary to exit immediately if unset or empty.

### Shared (watcher and worker)

| Variable                      | Type           | Default                              | Required     | Description                                                                                                                                             |
| ----------------------------- | -------------- | ------------------------------------ | ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `LOG_LEVEL`                   | string         | `info`                               | Optional     | Log verbosity: `debug`, `info`, `warn`, or `error`. Unrecognised values fall back to `info`.                                                            |
| `TEMPORAL_ADDRESS`            | `host:port`    | —                                    | **Required** | Temporal frontend address dialed by `client.Dial` (e.g. `temporal-frontend:7233`).                                                                      |
| `TEMPORAL_NAMESPACE`          | string         | —                                    | **Required** | Temporal namespace the workflows execute in (e.g. `default`).                                                                                           |
| `TEMPORAL_TASK_QUEUE`         | string         | —                                    | **Required** | Task queue the worker polls and the watcher dispatches to. The watcher and worker exit at startup if this is empty.                                     |
| `HEALTH_ADDR`                 | string         | `:8080` (worker) / `:8081` (watcher) | Optional     | TCP address for the HTTP health server. Exposes `/healthz` (liveness) and `/readyz` (readiness). Always enabled; override to change the listen address. |
| `METRICS_ADDR`                | string         | `""`                                 | Optional     | TCP address for the Prometheus `/metrics` pull endpoint (e.g. `:9090`). Disabled when empty.                                                            |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | string         | `""`                                 | Optional     | OTLP gRPC endpoint for pushing metrics (e.g. `http://otel-collector:4317`). Disabled when empty.                                                        |

### Worker only

| Variable                          | Type           | Default | Required     | Description                                                                                                                                                                                                                                                                                                                                                                                                     |
| --------------------------------- | -------------- | ------- | ------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `RADARR_URL`                      | URL            | —       | **Required** | Radarr base URL (e.g. `http://radarr:7878`).                                                                                                                                                                                                                                                                                                                                                                    |
| `RADARR_API_KEY`                  | string         | —       | **Required** | Radarr API key.                                                                                                                                                                                                                                                                                                                                                                                                 |
| `SONARR_URL`                      | URL            | —       | **Required** | Sonarr base URL (e.g. `http://sonarr:8989`).                                                                                                                                                                                                                                                                                                                                                                    |
| `SONARR_API_KEY`                  | string         | —       | **Required** | Sonarr API key.                                                                                                                                                                                                                                                                                                                                                                                                 |
| `MEDIA_WEBHOOK_URL`               | URL            | `""`    | Optional     | Endpoint POSTed to when a media file fails to process. A single aggregated notification is sent per failed run, summarising every error encountered. No notification is sent when empty.                                                                                                                                                                                                                        |
| `MEDIA_HARDWARE_DEVICE_PATH`      | path           | `""`    | Optional     | Hardware device path passed to the encoder (e.g. `/dev/dri/renderD128`). Hardware acceleration is auto-detected regardless; this controls which specific device is opened. When empty, the library selects a device automatically.                                                                                                                                                                              |
| `MEDIA_MIN_CROP_X`                | integer        | `10`    | Optional     | Minimum pixels to trim horizontally before a crop is applied. `-1` disables the threshold (any detected crop is accepted).                                                                                                                                                                                                                                                                                      |
| `MEDIA_MIN_CROP_Y`                | integer        | `10`    | Optional     | Minimum pixels to trim vertically before a crop is applied. `-1` disables the threshold.                                                                                                                                                                                                                                                                                                                        |
| `MEDIA_DETECT_CROP_TIMEOUT`       | duration       | `30m`   | Optional     | Maximum time allowed for crop detection before it is considered failed (e.g. `45m`, `1h`).                                                                                                                                                                                                                                                                                                                      |
| `MEDIA_TRANSCODE_TIMEOUT`         | duration       | `4h`    | Optional     | Maximum time allowed for transcoding before it is considered failed (e.g. `2h`, `8h`).                                                                                                                                                                                                                                                                                                                          |
| `MEDIA_PROGRESS_LOG_INTERVAL`     | duration       | `30s`   | Optional     | How often a progress log line is emitted during transcoding (e.g. `1m`, `5m`). Each line includes the estimated completion percentage, elapsed time, frames processed, and `fps` (computed over the last logging interval). Set to `0` to disable progress logging.                                                                                                                                             |
| `MEDIA_H265_CRF`                  | integer        | _unset_ | Optional     | Constant-quality value for H.265 encoding. Valid values are `1`–`51` (lower is higher quality); any other value (including `0`) causes the worker to exit at startup. When unset, the encoder's built-in default is used. See [hardware-acceleration.md](hardware-acceleration.md#quality-tuning) for how this value is applied per encoder.                                                                    |
| `METRICS_HIGH_CARDINALITY_LABELS` | `true`/`false` | `false` | Optional     | When set to exactly the string `true` (case-sensitive), per-item labels (title, year, episode, etc.) are attached to media workflow histogram observations. This adds fine-grained drill-down at the cost of creating a separate metric series per item, which can be expensive in your metrics backend. See [metrics.md](metrics.md#high-cardinality-labels) for the label set. Any other value disables them. |

## Path translation

After transcoding, the worker notifies Radarr or Sonarr by sending the path of the output file. If the worker and the arr service mount the output volume at different paths, set `watches[].output.remotePath` in the watcher config to the path the arr service uses for that volume. The worker will substitute `output.remotePath` for `output.path` in the notification path before sending the import command.

Example: the worker writes to `/processed/radarr` but Radarr sees the same volume as `/media/radarr`:

```yaml
watches:
  - name: movies
    watchedPath: /downloads/movies
    mediaType: movie
    output:
      path: /processed/radarr
      remotePath: /media/radarr
```

An output file at `/processed/radarr/sub/Movie.mkv` is sent to Radarr as `/media/radarr/sub/Movie.mkv`.

When the worker and the arr service already see the output volume at the same path, omit `output.remotePath` — the local output path is used as-is.
