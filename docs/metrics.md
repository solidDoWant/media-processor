# Metrics and observability

## Exporters

Both the watcher and the worker support two independent, opt-in metric exporters. Neither is enabled by default.

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

This follows the standard [OpenTelemetry environment variable convention](https://opentelemetry.io/docs/languages/sdk-configuration/otlp-exporter/). Point it at the gRPC port of your collector (typically 4317).

Both exporters can be active simultaneously.

### Choosing between the two

Use **Prometheus pull** if you already run a Prometheus server that scrapes targets directly. It is simpler to configure and requires no additional infrastructure beyond what Prometheus already provides.

Use **OTLP push** if you have an OpenTelemetry Collector (or another OTLP-compatible backend such as Grafana Alloy, Datadog Agent, or Honeycomb) already running. Push is also the better fit when the worker is run once per job rather than long-lived: metrics are flushed on shutdown, so each workflow run's observations are guaranteed to be delivered before the worker exits. A pull endpoint that is never scraped during a short-lived run would lose that data. The trade-off is that OTLP requires a running collector to receive and forward the metrics.

## Metric reference

### Media workflow metrics (worker)

Emitted by the worker during media workflow execution.

#### Histograms

| Metric                                       | Unit    | Description                                        |
| -------------------------------------------- | ------- | -------------------------------------------------- |
| `media_workflow_audio_track_count`           | tracks  | Number of audio tracks in the source file          |
| `media_workflow_subtitle_track_count`        | tracks  | Number of subtitle tracks in the source file       |
| `media_workflow_source_duration_seconds`     | seconds | Duration of the source media file                  |
| `media_workflow_source_file_size_bytes`      | bytes   | Size of the source file before transcoding         |
| `media_workflow_destination_file_size_bytes` | bytes   | Size of the output file after transcoding          |
| `media_workflow_transcode_duration_seconds`  | seconds | Wall-clock time spent transcoding                  |
| `media_workflow_total_duration_seconds`      | seconds | Wall-clock time for the full processing run       |

#### Counters

| Metric                                       | Description                                                                            |
| -------------------------------------------- | -------------------------------------------------------------------------------------- |
| `media_workflow_invalid_files_total`         | Files skipped because they could not be probed or contain no video stream              |
| `media_workflow_artwork_fetch_skipped_total` | Transcode runs where artwork fetch was attempted but yielded no embeddable image       |
| `media_workflow_metrics_errors_total`        | Errors encountered while collecting per-run metrics (e.g. Radarr/Sonarr API lookup failures) |

### Temporal SDK metrics (worker and watcher)

When `METRICS_ADDR` is set, the Temporal Go SDK's internal counters, gauges, and timers are bridged onto the same Prometheus registry that serves `/metrics` via [`go.temporal.io/sdk/contrib/tally`](https://pkg.go.dev/go.temporal.io/sdk/contrib/tally) and the [tally → Prometheus reporter](https://pkg.go.dev/github.com/uber-go/tally/v4/prometheus). The SDK's instrument names are exported with the conventional Prometheus suffixes (`_total` for counters, `_seconds` for timer histograms); common examples include:

| Metric                                                 | Kind      | Description                                                            |
| ------------------------------------------------------ | --------- | ---------------------------------------------------------------------- |
| `temporal_request_total`                               | counter   | gRPC requests issued by the SDK to the Temporal frontend.              |
| `temporal_long_request_latency_seconds`                | histogram | Latency of long-poll RPCs (e.g. workflow/activity task pollers).       |
| `temporal_activity_schedule_to_start_latency_seconds`  | histogram | Time between an activity being scheduled and a worker picking it up. |

Refer to the [Temporal Go SDK metrics reference](https://docs.temporal.io/references/sdk-metrics) for the full catalogue and descriptions, since the exact set of instruments depends on the SDK version in use.

### Watcher metrics

Emitted by the watcher on every scheduled directory scan.

| Metric                                      | Kind      | Unit    | Description                                                       |
| ------------------------------------------- | --------- | ------- | ----------------------------------------------------------------- |
| `watcher_scans_total`                       | counter   | —       | Per-mapping directory scans completed (carries a `status` label). |
| `watcher_scan_duration_seconds`             | histogram | seconds | Wall-clock duration of each per-mapping directory walk.           |
| `watcher_last_successful_scan_unix_seconds` | gauge     | seconds | Unix timestamp of the most recent successful per-mapping scan.    |
| `watcher_files_discovered_total`            | counter   | —       | Files found during directory scans.                               |
| `watcher_dispatches_total`                  | counter   | —       | Workflow dispatches successfully submitted to Temporal.           |
| `watcher_dispatch_errors_total`             | counter   | —       | Workflow dispatch failures.                                       |

## Labels

### Media workflow histograms

Every `media_workflow_*` histogram observation carries the full standard label set:

| Label                   | Values                    | Description                                                                                                                           |
| ----------------------- | ------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| `media_type`            | `movie`, `show`           | Type of media being processed.                                                                                                        |
| `mapping_name`          | _(configured watch name)_ | Name of the watch entry that triggered the job.                                                                                       |
| `source_codec`          | e.g. `h264`, `hevc`       | Video codec of the source file.                                                                                                       |
| `destination_codec`     | e.g. `hevc`, `copy`       | Video codec written to the output (`copy` when the source was remuxed without re-encode).                                             |
| `source_container`      | e.g. `matroska,webm`      | Container format of the source file as reported by libavformat (comma-joined list).                                                   |
| `destination_container` | `mkv`                     | Container format of the output file (always `mkv`).                                                                                   |
| `hardware_accelerated`  | `true`, `false`           | `true` when at least one video stream was encoded with a hardware encoder (NVENC, QSV, VAAPI) at runtime. `false` when the encoder fell back to software (e.g. libx265), even if `MEDIA_HARDWARE_DEVICE_PATH` is set. |
| `crop_applied`          | `true`, `false`           | Whether a crop filter was applied during transcoding.                                                                                 |

### Media workflow counters

Counters do **not** carry the full standard label set:

| Counter                                      | Labels                         |
| -------------------------------------------- | ------------------------------ |
| `media_workflow_invalid_files_total`         | `media_type`, `mapping_name`   |
| `media_workflow_artwork_fetch_skipped_total` | _(none)_                       |
| `media_workflow_metrics_errors_total`        | _(none)_                       |

### Watcher metrics

All watcher metrics carry `mapping_name`. `watcher_files_discovered_total`, `watcher_dispatches_total`, and `watcher_dispatch_errors_total` additionally carry `media_type`. `watcher_scans_total` carries a `status` label (`success` or `error`).

## High-cardinality labels

Set `METRICS_HIGH_CARDINALITY_LABELS=true` to attach per-item labels to every `media_workflow_*` histogram observation. These labels are **not** added to counters or watcher metrics.

| Label            | Applies to   | Description                         |
| ---------------- | ------------ | ----------------------------------- |
| `id`             | all          | Library item ID from Radarr/Sonarr. |
| `title`          | all          | Title of the movie or episode.      |
| `year`           | all          | Release year.                       |
| `series_title`   | shows only   | Series title.                       |
| `season_number`  | shows only   | Season number.                      |
| `episode_number` | shows only   | Episode number.                     |

These labels significantly increase the cardinality of your metrics. Enable them only if your metrics backend can handle the volume and you need per-item drill-down.

## Logging

Both binaries write plain-text logs to stderr. Set `LOG_LEVEL` to one of `debug`, `info`, `warn`, or `error` (default: `info`).

Temporal Go SDK internal log lines are forwarded through the same `slog` handler, so `LOG_LEVEL` controls SDK verbosity as well as application verbosity. At `info` and above the SDK is generally quiet; setting `LOG_LEVEL=debug` surfaces SDK debug messages alongside application debug output.
