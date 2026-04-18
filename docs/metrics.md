# Metrics and observability

## Exporters

Both `cmd/watcher` and `cmd/worker` support two independent, opt-in metric exporters. Neither is enabled by default.

### Prometheus pull endpoint

Set `METRICS_ADDR` to a TCP address (e.g. `:9090`) to start an HTTP server that exposes metrics in Prometheus text format at `/metrics`.

```sh
METRICS_ADDR=:9090
```

Scrape config example:

```yaml
scrape_configs:
  - job_name: media-processor
    static_configs:
      - targets: ["worker:9090", "watcher:9090"]
```

### OTLP push (OpenTelemetry)

Set `OTEL_EXPORTER_OTLP_ENDPOINT` to an OTLP gRPC endpoint to push metrics periodically (default interval: 60 seconds).

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317
```

This follows the standard [OpenTelemetry environment variable convention](https://opentelemetry.io/docs/languages/sdk-configuration/otlp-exporter/). The exporter uses gRPC (not HTTP/protobuf), so point it at the gRPC port of your collector (typically 4317).

Both exporters can be active simultaneously.

### Choosing between the two

Use **Prometheus pull** if you already run a Prometheus server that scrapes targets directly. It is simpler to configure and requires no additional infrastructure beyond what Prometheus already provides. The downside is that Prometheus must be able to reach the worker's metrics port; this can be awkward in environments where processes are behind NAT or where scrape intervals add latency to alerting.

Use **OTLP push** if you have an OpenTelemetry Collector (or another OTLP-compatible backend such as Grafana Alloy, Datadog Agent, or Honeycomb) already running. Push works regardless of whether the worker is network-reachable from the backend, making it a better fit for ephemeral or firewalled environments. The trade-off is that it requires a running collector to receive and forward the metrics.

## Metric reference

All metrics are emitted by `cmd/worker` during media workflow execution. The meter name is `media_workflow`.

### Histograms

| Metric | Unit | Description |
|--------|------|-------------|
| `media_workflow_audio_track_count` | tracks | Number of audio tracks in the source file |
| `media_workflow_subtitle_track_count` | tracks | Number of subtitle tracks in the source file |
| `media_workflow_source_duration_seconds` | seconds | Duration of the source media file |
| `media_workflow_source_file_size_bytes` | bytes | Size of the source file before transcoding |
| `media_workflow_destination_file_size_bytes` | bytes | Size of the output file after transcoding |
| `media_workflow_transcode_duration_seconds` | seconds | Wall-clock time spent in the transcode step |
| `media_workflow_total_duration_seconds` | seconds | Wall-clock time from probe start to cleanup finish |

### Counters

| Metric | Description |
|--------|-------------|
| `media_workflow_invalid_files_total` | Files skipped because they are not valid media (no video stream) |
| `media_workflow_artwork_fetch_skipped_total` | Transcode runs where artwork fetch was attempted but yielded no embeddable image |
| `media_workflow_metrics_errors_total` | Errors encountered while collecting per-run metrics (e.g. library API lookup failures) |

## Standard labels

Every histogram observation and counter increment carries a common set of labels:

| Label | Values | Description |
|-------|--------|-------------|
| `media_type` | `movie`, `show` | Type of media being processed |
| `mapping_name` | _(configured watch name)_ | Name of the watch entry that triggered the job |
| `hardware_accelerated` | `true`, `false` | Whether a hardware encoder was used |
| `source_codec` | e.g. `h264`, `hevc` | Video codec of the source file |
| `container` | e.g. `matroska`, `mov` | Container format of the source file |

## High-cardinality labels

Set `METRICS_HIGH_CARDINALITY_LABELS=true` to attach per-item labels to every histogram observation. These labels are **not** added to counters.

| Label | Description |
|-------|-------------|
| `id` | Library item ID from Radarr/Sonarr |
| `title` | Title of the movie or series |
| `year` | Release year |
| `season` | Season number (TV episodes only) |
| `episode` | Episode number (TV episodes only) |

These labels significantly increase the cardinality of your metrics. Enable them only if your metrics backend can handle the volume and you need per-item drill-down.

## Logging

Both binaries use `log/slog` with a text handler. Set `LOG_LEVEL` to one of `debug`, `info`, `warn`, or `error` (default: `info`). Hatchet SDK logs are bridged through slog via zerolog so all output is consistently formatted.
