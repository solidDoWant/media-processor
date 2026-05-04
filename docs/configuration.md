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

| Field                              | Type     | Default | Description                                                                                                                                                                                                             |
| ---------------------------------- | -------- | ------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `scanInterval`                     | string   | `5s`    | Go duration string (e.g. `5s`, `1m30s`) controlling how often each watch directory is scanned. Defaults to `5s` when omitted.                                                                                           |
| `watches[].name`                   | string   | —       | **Required.** Logical name for this watch entry; used as a label in metrics.                                                                                                                                            |
| `watches[].watchedPath`            | string   | —       | **Required.** Path to the directory to watch. Relative paths are resolved against the watcher's working directory.                                                                                                      |
| `watches[].mediaType`              | string   | —       | **Required.** `movie` or `show`. Determines which library service (Radarr or Sonarr) is notified.                                                                                                                       |
| `watches[].output.path`            | string   | —       | **Required.** Directory where processed files for this watch entry are written.                                                                                                                                         |
| `watches[].output.remotePath`      | string   | `""`    | Path by which the output directory is known to the arr service (Radarr or Sonarr). Set this when the worker and the arr service mount the output volume at different paths. When empty, no path translation is applied. |
| `watches[].ignorePatterns`         | []string | `[]`    | Regular expressions in [RE2 syntax](https://github.com/google/re2/wiki/Syntax). A file whose path matches any pattern is silently skipped; a directory match skips the entire subtree.                                  |
| `watches[].preserveSource`         | bool     | `false` | When `true`, the source file is kept after successful transcoding.                                                                                                                                                      |
| `watches[].retainEmptyDirectories` | bool     | `false` | When `true`, parent directories that become empty after source deletion are left in place rather than being deleted up to the watch root.                                                                               |

## Temporal client configuration file

Both binaries build their `temporal.Client` via the [Temporal Go SDK envconfig package](https://pkg.go.dev/go.temporal.io/sdk/contrib/envconfig), which reads a TOML config file plus a set of `TEMPORAL_*` environment variables. Environment variables override file values.

The default file path is `$XDG_CONFIG_HOME/temporalio/temporal.toml` (e.g. `~/.config/temporalio/temporal.toml`); set `TEMPORAL_CONFIG_FILE` to point at a different path. `TEMPORAL_PROFILE` selects which profile to load from the file (default `default`). The Helm chart renders this file as a ConfigMap and points `TEMPORAL_CONFIG_FILE` at the mounted path.

### File format

```toml
[profile.default]
address = "temporal-frontend:7233"
namespace = "default"
api_key = "..."                      # prefer TEMPORAL_API_KEY from a Secret

[profile.default.tls]
client_cert_path           = "/etc/temporal-tls/mycerts/tls.crt"
client_key_path            = "/etc/temporal-tls/mycerts/tls.key"
server_ca_cert_path        = "/etc/temporal-tls/myca/ca.pem"
server_name                = "temporal.example.com"
disable_host_verification  = false

[profile.default.grpc_meta]
x-tenant-id = "media-processor"
```

Every field has both a TOML key (snake_case) and a `TEMPORAL_*` env var override, listed in the next section. The TLS table also accepts `client_cert_data`, `client_key_data`, and `server_ca_cert_data` keys for inline PEM bytes when mounting files is impractical; prefer the `*_path` form whenever possible.

`TEMPORAL_TASK_QUEUE` is **not** read by envconfig — it is consumed directly by the watcher and worker binaries as the workflow task queue (and the prefix used to derive activity task queues), and has no equivalent in the file.

### File-mounted API key (`file://` prefix)

The watcher and worker accept an extension to the standard `api_key` / `TEMPORAL_API_KEY` value: a `file:///absolute/path` URI tells the binary to read the API key from the named file on every Temporal RPC, rather than treating the value as the literal key. This is useful when an external rotator (Vault Agent, external-secrets refresh, etc.) updates the file in place — the new value is picked up automatically without a process restart.

Only the canonical empty-authority form with an absolute path is accepted (`file:///etc/temporal/my-api-key`). Relative paths and the single-slash `file:/path` form are rejected at startup. The file is also read once at startup so a misconfigured path fails immediately rather than at the first RPC.

Reads happen on every unary RPC, but the cost is negligible: Kubernetes Secret-mounted files (and Vault Agent–templated files) are tmpfs-backed and atomic, so each read is a few syscalls in RAM.

## Environment variables

Variables marked **Required** cause the binary to exit immediately if unset or empty. All `TEMPORAL_*` variables (except `TEMPORAL_TASK_QUEUE`) override the equivalent field in the loaded profile of the [Temporal client configuration file](#temporal-client-configuration-file).

### Shared (watcher and worker)

| Variable                                 | Type         | Default                                     | Required     | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| ---------------------------------------- | ------------ | ------------------------------------------- | ------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `LOG_LEVEL`                              | string       | `info`                                      | Optional     | Log verbosity: `debug`, `info`, `warn`, or `error`. Unrecognised values fall back to `info`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `TEMPORAL_CONFIG_FILE`                   | path         | `$XDG_CONFIG_HOME/temporalio/temporal.toml` | Optional     | Path to the Temporal client configuration file. The Helm chart sets this to `/etc/temporal/temporal.toml`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `TEMPORAL_PROFILE`                       | string       | `default`                                   | Optional     | Profile name to load from the configuration file.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `TEMPORAL_ADDRESS`                       | `host:port`  | `localhost:7233`                            | Optional     | Temporal frontend address (e.g. `temporal-frontend:7233`). Overrides `address` in the loaded profile. When neither this nor the file sets a value, the SDK defaults to `localhost:7233`; production deployments must set one of them so the startup health check can reach the frontend.                                                                                                                                                                                                                                                                                                                   |
| `TEMPORAL_NAMESPACE`                     | string       | `default`                                   | Optional     | Temporal namespace the workflows execute in. Overrides `namespace` in the loaded profile.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `TEMPORAL_API_KEY`                       | string       | `""`                                        | Optional     | API key for authenticated Temporal frontends (e.g. Temporal Cloud). Overrides `api_key` in the loaded profile. Sent as `Authorization: Bearer <key>` on every RPC. Accepts the `file:///absolute/path` extension — see [File-mounted API key](#file-mounted-api-key-file-prefix).                                                                                                                                                                                                                                                                                                                          |
| `TEMPORAL_TLS`                           | bool         | `false`                                     | Optional     | When `true`, enable TLS to the Temporal frontend even if the file's `[tls]` table is absent. Overrides the `disabled` field in the loaded profile's `[tls]` table.                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `TEMPORAL_TLS_CLIENT_CERT_PATH`          | path         | `""`                                        | Optional     | Path to the client certificate PEM file for mTLS. Overrides `tls.client_cert_path`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `TEMPORAL_TLS_CLIENT_CERT_DATA`          | string (PEM) | `""`                                        | Optional     | Inline client certificate PEM. Mutually exclusive with `TEMPORAL_TLS_CLIENT_CERT_PATH`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `TEMPORAL_TLS_CLIENT_KEY_PATH`           | path         | `""`                                        | Optional     | Path to the client private key PEM file for mTLS. Overrides `tls.client_key_path`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `TEMPORAL_TLS_CLIENT_KEY_DATA`           | string (PEM) | `""`                                        | Optional     | Inline client private key PEM. Mutually exclusive with `TEMPORAL_TLS_CLIENT_KEY_PATH`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `TEMPORAL_TLS_SERVER_CA_CERT_PATH`       | path         | `""`                                        | Optional     | Path to a CA certificate PEM file used to verify the server. Overrides `tls.server_ca_cert_path`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `TEMPORAL_TLS_SERVER_CA_CERT_DATA`       | string (PEM) | `""`                                        | Optional     | Inline CA certificate PEM. Mutually exclusive with `TEMPORAL_TLS_SERVER_CA_CERT_PATH`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `TEMPORAL_TLS_SERVER_NAME`               | string       | `""`                                        | Optional     | SNI / cert-verification hostname override. Overrides `tls.server_name`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `TEMPORAL_TLS_DISABLE_HOST_VERIFICATION` | bool         | `false`                                     | Optional     | Skip server certificate hostname verification. Development-only.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `TEMPORAL_GRPC_META_*`                   | string       | `""`                                        | Optional     | gRPC metadata headers added to every Temporal RPC. Suffix is the header name with `-` replaced by `_` and uppercased (e.g. `TEMPORAL_GRPC_META_X_TENANT_ID` sets `x-tenant-id`).                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `TEMPORAL_CODEC_ENDPOINT`                | URL          | `""`                                        | Optional     | Remote payload codec endpoint. Overrides `codec.endpoint`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `TEMPORAL_CODEC_AUTH`                    | string       | `""`                                        | Optional     | Authorization header value sent to the payload codec endpoint. Overrides `codec.auth`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `TEMPORAL_TASK_QUEUE`                    | string       | `media-processor`                           | Optional     | Workflow task queue: the watcher dispatches workflows here and the worker (when its `WORKER_ACTIVITIES` set includes `workflow`) polls it. The same value is also used as the prefix for activity task queues — each activity polls `{TEMPORAL_TASK_QUEUE}-{activity-token}` (see [Activity task queues](#activity-task-queues)). Read directly by the binaries (not via envconfig).                                                                                                                                                                                                                       |
| `HEALTH_ADDR`                            | string       | `:8080` (worker) / `:8081` (watcher)        | Optional     | TCP address for the HTTP health server. Exposes `/healthz` (liveness) and `/readyz` (readiness). Always enabled; override to change the listen address.                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `METRICS_ADDR`                           | string       | `:9090` (worker) / `:9091` (watcher)        | Optional     | TCP address for the Prometheus `/metrics` pull endpoint. Always exposed; override to change the listen address. The two binaries default to distinct ports so they can run side-by-side on the same host.                                                                                                                                                                                                                                                                                                                                                                                                  |
| `METRICS_SCRAPE_WAIT_TIMEOUT`            | duration     | `60s`                                       | Optional     | After drain (worker `Run` returns or watcher scan loop exits), the process holds the `/metrics` endpoint open until Prometheus collects one final scrape or this timeout elapses. Set this to at least 2x your Prometheus scrape interval so a missed first scrape still has time to retry before the deadline. Set to `0s` to disable the gate (the process exits immediately after drain without waiting for a scrape). On Kubernetes, this and `WORKER_STOP_TIMEOUT` must both fit within the pod's `terminationGracePeriodSeconds`; see [helm.md](helm.md#termination-and-drain) for the relationship. |

### Watcher only

| Variable                                     | Type | Default | Required | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| -------------------------------------------- | ---- | ------- | -------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `WATCHER_TEMPORAL_SEARCH_ATTRIBUTES_ENABLED` | bool | `true`  | Optional | When `true`, each dispatched workflow is started with the five custom Temporal search attributes listed in [Temporal search attributes](#temporal-search-attributes). The attributes must be pre-registered in the namespace (see below) before the watcher starts; if they are not registered, dispatch fails and the watcher logs the registration command. When `false`, search attributes are skipped entirely and Memo is still attached. Disable this when running against a Temporal server without advanced visibility (no Elasticsearch backend). Parsed via Go's `strconv.ParseBool`, which accepts `1`, `t`, `T`, `TRUE`, `true`, `True`, `0`, `f`, `F`, `FALSE`, `false`, `False`. |

### Worker only

| Variable                          | Type     | Default                              | Required     | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| --------------------------------- | -------- | ------------------------------------ | ------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `WORKER_ACTIVITIES`               | string   | `all`                                | Optional     | Comma-separated list of activity tokens this worker pod handles. See [Activity task queues](#activity-task-queues) for the full grammar (`all`, literal token, `!token`) and the set of known tokens. The worker exits at startup if the list resolves to an empty set or contains an unknown token (with or without the `!` prefix).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `RADARR_URL`                      | URL      | —                                    | **Required** | Radarr base URL (e.g. `http://radarr:7878`).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `RADARR_API_KEY`                  | string   | —                                    | **Required** | Radarr API key.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `SONARR_URL`                      | URL      | —                                    | **Required** | Sonarr base URL (e.g. `http://sonarr:8989`).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `SONARR_API_KEY`                  | string   | —                                    | **Required** | Sonarr API key.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `MEDIA_WEBHOOK_URL`               | URL      | `""`                                 | Optional     | Endpoint POSTed to when a media file fails to process. A single aggregated notification is sent per failed run, summarising every error encountered. No notification is sent when empty.                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `MEDIA_HARDWARE_DEVICE_PATH`      | path     | `""`                                 | Optional     | Hardware device path passed to the encoder (e.g. `/dev/dri/renderD128`). Hardware acceleration is auto-detected regardless; this controls which specific device is opened. When empty, the library selects a device automatically.                                                                                                                                                                                                                                                                                                                                                                                                              |
| `MEDIA_MIN_CROP_X`                | integer  | `10`                                 | Optional     | Minimum pixels to trim horizontally before a crop is applied. `-1` disables the threshold (any detected crop is accepted).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `MEDIA_MIN_CROP_Y`                | integer  | `10`                                 | Optional     | Minimum pixels to trim vertically before a crop is applied. `-1` disables the threshold.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `MEDIA_DETECT_CROP_TIMEOUT`       | duration | `30m`                                | Optional     | Maximum time allowed for crop detection before it is considered failed (e.g. `45m`, `1h`).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `MEDIA_TRANSCODE_TIMEOUT`         | duration | `4h`                                 | Optional     | Maximum time allowed for transcoding before it is considered failed (e.g. `2h`, `8h`).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `WORKER_STOP_TIMEOUT`             | duration | _(tracks `MEDIA_TRANSCODE_TIMEOUT`)_ | Optional     | On SIGTERM, the worker stops accepting new Temporal tasks and waits up to this long for in-flight activities to finish before cancelling them. When unset, defaults to the effective `MEDIA_TRANSCODE_TIMEOUT` (which itself defaults to `4h`), so an operator who raises the transcode timeout does not also have to raise this value to keep the drain ceiling above the longest expected activity. On Kubernetes, this and `METRICS_SCRAPE_WAIT_TIMEOUT` must both fit within the pod's `terminationGracePeriodSeconds`; see [helm.md](helm.md#termination-and-drain). The Helm chart sets a chart-level default of `30s` for this variable. |
| `MEDIA_PROGRESS_LOG_INTERVAL`     | duration | `30s`                                | Optional     | How often a progress log line is emitted during transcoding (e.g. `1m`, `5m`). Each line includes the estimated completion percentage, elapsed time, frames processed, and `fps` (computed over the last logging interval). The same interval also drives the transcode activity's Temporal `HeartbeatTimeout`, set to `2x` this value: each FFmpeg progress tick records a heartbeat, so a stuck encode is detected and failed within roughly twice the configured interval rather than waiting for `MEDIA_TRANSCODE_TIMEOUT` to elapse. Set to `0s` to disable progress logging; this also disables heartbeat-based stall detection.          |
| `MEDIA_H265_CRF`                  | integer  | _unset_                              | Optional     | Constant-quality value for H.265 encoding. Valid values are `1`–`51` (lower is higher quality); any other value (including `0`) causes the worker to exit at startup. When unset, the encoder's built-in default is used. See [hardware-acceleration.md](hardware-acceleration.md#quality-tuning) for how this value is applied per encoder.                                                                                                                                                                                                                                                                                                    |
| `METRICS_HIGH_CARDINALITY_LABELS` | bool     | `false`                              | Optional     | When true, per-item labels (title, year, episode, etc.) are attached to media workflow histogram observations. This adds fine-grained drill-down at the cost of creating a separate metric series per item, which can be expensive in your metrics backend. See [metrics.md](metrics.md#high-cardinality-labels) for the label set. Parsed via Go's `strconv.ParseBool`, which accepts `1`, `t`, `T`, `TRUE`, `true`, `True`, `0`, `f`, `F`, `FALSE`, `false`, `False`. Any other value (e.g. `yes`, `garbage`) causes the worker to exit at startup with a parse error.                                                                        |

## Activity task queues

Each Temporal activity in the media workflow polls a dedicated task queue, named by appending the activity token to `TEMPORAL_TASK_QUEUE` (default `media-processor`). The workflow itself runs on the prefix-only queue.

| Token            | Task queue                       | Method invoked          |
| ---------------- | -------------------------------- | ----------------------- |
| `workflow`       | `media-processor`                | `MediaWorkflow`         |
| `probe`          | `media-processor-probe`          | `Probe`                 |
| `detect-crop`    | `media-processor-detect-crop`    | `DetectCrop`            |
| `transcode`      | `media-processor-transcode`      | `Transcode`             |
| `notify`         | `media-processor-notify`         | `Notify`                |
| `cleanup`        | `media-processor-cleanup`        | `Cleanup`               |
| `notify-failure` | `media-processor-notify-failure` | `NotifyFailure`         |

`WORKER_ACTIVITIES` selects which of these tokens a worker pod handles. The value is a comma-separated list evaluated left-to-right against an initially empty set:

- `all` — sets the working set to every known token.
- `name` — adds that token to the set.
- `!name` — removes that token from the set.

The worker exits at startup if the resolved set is empty, or if any token (with or without `!`) is unknown.

Common configurations:

| Use case                              | `WORKER_ACTIVITIES`              |
| ------------------------------------- | -------------------------------- |
| Default all-in-one (workflow + every activity) | `all` (or unset)        |
| General worker, no transcode          | `all,!transcode`                 |
| Transcode-only autoscaling pool       | `transcode`                      |
| Workflow + transcode                  | `workflow,transcode`             |
| Probe and detect-crop only            | `probe,detect-crop`              |

Whitespace around tokens and empty entries are tolerated. The `workflow` token is opt-in: a pod that does not include it will not run the workflow function, leaving workflow execution to other pods. This is useful for autoscaling groups dedicated to a high-overhead activity (e.g. transcode) where you want admission control to apply only to that activity.

For each token in the resolved set, the worker starts one Temporal `Worker` polling the matching queue. The workflow registers itself on the prefix-only queue; each activity registers itself on its activity-specific queue. Workflow `ExecuteActivity` calls set `ActivityOptions.TaskQueue` so each activity routes to the correct pod regardless of which pod is executing the workflow body.

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

## Temporal search attributes

When `WATCHER_TEMPORAL_SEARCH_ATTRIBUTES_ENABLED` is `true` (the default), the watcher attaches five custom search attributes to every dispatched media workflow. These make workflows filterable in the Temporal Web UI and queryable via the visibility API using expressions like `MediaMappingName = "movies"` or `MediaFilePath STARTS_WITH "/downloads/"`.

Every dispatched workflow also receives a **Memo** with the same five fields. Memo is always attached regardless of the search-attribute setting, and its fields are visible in the Temporal Web UI's per-run detail pane without any namespace-level registration.

### Attribute names and types

| Attribute name     | Type      | Value                                             |
| ------------------ | --------- | ------------------------------------------------- |
| `MediaFilePath`    | `Keyword` | Absolute path of the input file                   |
| `MediaTitle`       | `Text`    | Basename of the input file (full-text searchable) |
| `MediaType`        | `Keyword` | `movie` or `show`                                 |
| `MediaMappingName` | `Keyword` | Name of the watch mapping that matched the file   |
| `MediaWatchRoot`   | `Keyword` | Absolute path of the watch root directory         |

Note: both `MediaFilePath` and `MediaTitle` reflect the original input path. When `output.remotePath` is set in the watcher config, the rewritten path is **not** used here.

### One-time registration

Custom search attributes are namespace-scoped and must be registered once before the watcher starts with `WATCHER_TEMPORAL_SEARCH_ATTRIBUTES_ENABLED=true`. Registration requires advanced visibility (an Elasticsearch-backed self-hosted Temporal server, or Temporal Cloud). Run:

```sh
temporal operator search-attribute create \
  --namespace <namespace> \
  --name MediaFilePath --type Keyword \
  --name MediaTitle --type Text \
  --name MediaType --type Keyword \
  --name MediaMappingName --type Keyword \
  --name MediaWatchRoot --type Keyword
```

Replace `<namespace>` with the Temporal namespace the watcher connects to (the value of `TEMPORAL_NAMESPACE`, default `default`).

If the attributes are not registered and the watcher starts with `WATCHER_TEMPORAL_SEARCH_ATTRIBUTES_ENABLED=true`, every workflow dispatch will fail. The watcher logs a clear error message including the full registration command and the attribute names. Set `WATCHER_TEMPORAL_SEARCH_ATTRIBUTES_ENABLED=false` to skip search attributes (e.g. when running against a basic-visibility Temporal server).
