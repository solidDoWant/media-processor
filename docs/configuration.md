# Configuration

## Watcher config file

`cmd/watcher` reads a YAML configuration file at startup. Pass the path with `--config` (default: `config.yaml`).

### Full example

```yaml
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

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `cronSchedule` | string | `*/5 * * * * *` | 6-field cron expression controlling how often each watch directory is scanned. Defaults to every 5 seconds when omitted. |
| `watches[].name` | string | — | **Required.** Logical name for this watch entry; used as a label in metrics. |
| `watches[].path` | string | — | **Required.** Absolute path to watch. |
| `watches[].mediaType` | string | — | **Required.** `movie` or `show`. Determines which library service (Radarr or Sonarr) is notified. |
| `watches[].ignorePatterns` | []string | `[]` | Go regular expressions. A file whose path matches any pattern is silently skipped; a directory match skips the entire subtree. |
| `watches[].preserveSource` | bool | `false` | When `true`, the source file is kept after successful transcoding. |
| `watches[].retainEmptyDirectories` | bool | `false` | When `true`, parent directories that become empty after source deletion are left in place rather than being pruned bottom-up to the watch root. |

## Environment variables

Variables marked **Required** cause the binary to exit immediately if unset or empty.

### Shared (`cmd/watcher` and `cmd/worker`)

| Variable | Type | Default | Required | Description |
|----------|------|---------|----------|-------------|
| `LOG_LEVEL` | string | `info` | Optional | Log verbosity: `debug`, `info`, `warn`, or `error`. Unrecognised values fall back to `info`. |
| `HATCHET_CLIENT_TOKEN` | string | — | **Required** | Hatchet API token used by the client SDK. |
| `METRICS_ADDR` | string | `""` | Optional | TCP address for the Prometheus `/metrics` pull endpoint (e.g. `:9090`). Disabled when empty. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | string | `""` | Optional | OTLP gRPC endpoint for pushing metrics (e.g. `http://otel-collector:4317`). Disabled when empty. |

### `cmd/worker` only

| Variable | Type | Default | Required | Description |
|----------|------|---------|----------|-------------|
| `MEDIA_OUTPUT_DIR` | path | — | **Required** | Directory where transcoded output files are written. |
| `RADARR_URL` | URL | — | **Required** | Radarr base URL (e.g. `http://radarr:7878`). |
| `RADARR_API_KEY` | string | — | **Required** | Radarr API key. |
| `SONARR_URL` | URL | — | **Required** | Sonarr base URL (e.g. `http://sonarr:8989`). |
| `SONARR_API_KEY` | string | — | **Required** | Sonarr API key. |
| `RADARR_LOCAL_PATH_PREFIX` | path | `""` | Optional | Local-side prefix for Radarr path translation. When set, `RADARR_REMOTE_PATH_PREFIX` must also be set. |
| `RADARR_REMOTE_PATH_PREFIX` | path | `""` | Optional | Radarr-side prefix for path translation. Replaces `RADARR_LOCAL_PATH_PREFIX` in paths sent to Radarr. |
| `SONARR_LOCAL_PATH_PREFIX` | path | `""` | Optional | Local-side prefix for Sonarr path translation. When set, `SONARR_REMOTE_PATH_PREFIX` must also be set. |
| `SONARR_REMOTE_PATH_PREFIX` | path | `""` | Optional | Sonarr-side prefix for path translation. Replaces `SONARR_LOCAL_PATH_PREFIX` in paths sent to Sonarr. |
| `MEDIA_WEBHOOK_URL` | URL | `""` | Optional | Endpoint notified on workflow step failure. No notification is sent when empty. |
| `MEDIA_INPUT_ROOT` | path | `""` | Optional | Root of the watched input directories. When set, transcoded output is placed in a mirrored subdirectory under `MEDIA_OUTPUT_DIR`. **Must be set** for nested downloads to produce matching subdirectory structure in the output. |
| `MEDIA_HARDWARE_DEVICE_PATH` | path | `""` | Optional | Hardware device path passed to the encoder (e.g. `/dev/dri/renderD128`). Hardware acceleration is auto-detected regardless; this controls which specific device is opened. When empty, the library selects a device automatically. |
| `MEDIA_MIN_CROP_X` | integer | `10` | Optional | Minimum pixels to trim horizontally before a crop is applied. `-1` disables the threshold (any detected crop is accepted). |
| `MEDIA_MIN_CROP_Y` | integer | `10` | Optional | Minimum pixels to trim vertically before a crop is applied. `-1` disables the threshold. |
| `MEDIA_DETECT_CROP_TIMEOUT` | Go duration | `30m` | Optional | Hatchet execution timeout for the crop-detection step (e.g. `45m`, `1h`). |
| `MEDIA_TRANSCODE_TIMEOUT` | Go duration | `4h` | Optional | Hatchet execution timeout for the transcode step (e.g. `2h`, `8h`). |
| `MEDIA_H265_CRF` | integer | `0` | Optional | Constant-quality value for H.265 encoding (1–51). `0` uses the encoder's built-in default. For libx265 this is the CRF; for NVENC it is the CQ; for QSV/VAAPI it is the global_quality (ICQ). |
| `METRICS_HIGH_CARDINALITY_LABELS` | `true`/`false` | `false` | Optional | When `true`, per-item labels (id, title, year, season, episode) are attached to metric observations. |

## Path translation

If the worker and the arr services (Radarr/Sonarr) mount the processed-output directory at different paths, use the `*_LOCAL_PATH_PREFIX` / `*_REMOTE_PATH_PREFIX` pairs to translate paths before sending import commands.

Example: the worker writes to `/processed-output` but Sonarr sees that volume as `/downloads`:

```
SONARR_LOCAL_PATH_PREFIX=/processed-output
SONARR_REMOTE_PATH_PREFIX=/downloads
```

A path like `/processed-output/tv/show/ep.mkv` is sent to Sonarr as `/downloads/tv/show/ep.mkv`.
