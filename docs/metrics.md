# Metrics and observability

## Prometheus endpoint

The watcher and worker each serve metrics in Prometheus text format on `/metrics`. Defaults are picked so the two binaries can run side-by-side on the same host without colliding:

| Binary  | Default metrics address | Override env var |
| ------- | ----------------------- | ---------------- |
| Worker  | `:9090`                 | `METRICS_ADDR`   |
| Watcher | `:9091`                 | `METRICS_ADDR`   |

Scrape config example:

```yaml
scrape_configs:
  - job_name: media-processor
    static_configs:
      - targets: ["worker:9090", "watcher:9091"]
```

### Scrape on shutdown

When a worker pod shuts down, the binary holds the `/metrics` endpoint open after activity drain to give Prometheus one final scrape covering end-of-lifecycle samples. The wait is bounded by `METRICS_SCRAPE_WAIT_TIMEOUT` (Go duration string, default `60s`); set it to `0s` to disable the gate. The endpoint then closes and the process exits.

For Kubernetes, ensure `terminationGracePeriodSeconds` on the worker pod is at least `METRICS_SCRAPE_WAIT_TIMEOUT` plus the longest activity drain you tolerate, otherwise the pod will be killed before the gate can wait for the scrape.

## Metric reference

### Worker — media workflow

| Metric                                       | Type      | Unit    | Description                                                                                              |
| -------------------------------------------- | --------- | ------- | -------------------------------------------------------------------------------------------------------- |
| `media_workflow_source_duration_seconds`     | histogram | seconds | Distribution of source media file durations as reported by probe (workload characterization).                                  |
| `media_workflow_source_file_size_bytes`      | histogram | bytes   | Distribution of source file sizes before transcoding.                                                                          |
| `media_workflow_destination_file_size_bytes` | histogram | bytes   | Distribution of output file sizes after transcoding. Compare against source for compression ratio.                             |
| `media_workflow_transcode_duration_seconds`  | histogram | seconds | Wall-clock time spent transcoding. Carries codec / hardware-acceleration / crop tags so runtime can be correlated with input characteristics. Not emitted when an existing output at the destination path is reused without re-encoding (the source/destination size histograms still fire so corpus shape stays accurate). |
| `media_workflow_audio_track_count`           | gauge     | tracks  | Audio track count from the most recent probe per label combination.                                                            |
| `media_workflow_subtitle_track_count`        | gauge     | tracks  | Subtitle track count from the most recent probe per label combination.                                                         |
| `media_workflow_invalid_files_total`         | counter   | —       | Files skipped because they could not be probed or contained no video stream.                                                   |
| `media_workflow_artwork_fetch_skipped_total` | counter   | —       | Transcode runs where artwork fetch was attempted but yielded no embeddable image.                                              |
| `media_workflow_metrics_errors_total`        | counter   | —       | Radarr/Sonarr `GetInfo` lookups that did not return a result while resolving high-cardinality tags — either a backend error (unreachable, auth failure, etc.) or the library could not parse the filename to a known item. |

End-to-end workflow latency, schedule-to-start latency, retry counts, and worker poll metrics are emitted by the Temporal SDK — see the [Temporal SDK metrics section](#worker--temporal-sdk) below. Per-activity execution latency is also available there, but only with SDK tags; the application-side `media_workflow_transcode_duration_seconds` above carries the media-domain tags needed to slice runtime by codec, hardware acceleration, and crop.

### Worker — Temporal SDK

The Temporal Go SDK emits its own set of counters and histograms onto the same `/metrics` endpoint. A few that are especially useful operationally:

| Metric                                                | Type      | Description                                                                  |
| ----------------------------------------------------- | --------- | ---------------------------------------------------------------------------- |
| `temporal_workflow_endtoend_latency_seconds`          | histogram | Wall-clock time from workflow start to close, tagged by `workflow_type`.     |
| `temporal_activity_execution_latency_seconds`         | histogram | Wall-clock time inside an activity, tagged by `activity_type` and `workflow_type`. Use `activity_type="Transcode"` for transcode duration. |
| `temporal_activity_schedule_to_start_latency_seconds` | histogram | Time between an activity being scheduled and a worker picking it up — a leading indicator of worker capacity. |
| `temporal_request_total`                              | counter   | gRPC requests issued by the SDK to the Temporal frontend.                    |
| `temporal_long_request_latency_seconds`               | histogram | Latency of long-poll RPCs.                                                   |

Refer to the [Temporal Go SDK metrics reference](https://docs.temporal.io/references/sdk-metrics) for the full catalogue, since the exact set of instruments depends on the SDK version in use.

### Watcher

| Metric                                      | Type      | Unit    | Description                                                       |
| ------------------------------------------- | --------- | ------- | ----------------------------------------------------------------- |
| `watcher_scans_total`                       | counter   | —       | Per-mapping directory scans completed (carries a `status` label). |
| `watcher_scan_duration_seconds`             | histogram | seconds | Wall-clock duration of each per-mapping directory walk.           |
| `watcher_last_successful_scan_unix_seconds` | gauge     | seconds | Unix timestamp of the most recent successful per-mapping scan.    |
| `watcher_files_discovered_total`            | counter   | —       | Files found during directory scans.                               |
| `watcher_dispatches_total`                  | counter   | —       | Workflow dispatches successfully submitted to Temporal.           |
| `watcher_dispatch_errors_total`             | counter   | —       | Workflow dispatch failures.                                       |

## Tags

### Worker — media workflow application tags

Worker application metrics carry the tags listed below. Per-metric coverage varies because some tags are only knowable later in the run (codec/container after probe; hardware-acceleration and crop only after transcode).

| Tag                     | Values                    | Description                                                                                                                                                                                                          |
| ----------------------- | ------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `media_type`            | `movie`, `show`           | Type of media being processed.                                                                                                                                                                                       |
| `mapping_name`          | _(configured watch name)_ | Name of the watch entry that triggered the job.                                                                                                                                                                      |
| `source_codec`          | e.g. `h264`, `hevc`       | Video codec of the source file.                                                                                                                                                                                      |
| `destination_codec`     | e.g. `hevc`, `copy`       | Video codec written to the output (`copy` when the source was remuxed without re-encode).                                                                                                                            |
| `source_container`      | e.g. `matroska,webm`      | Container format of the source file as reported by libavformat (comma-joined list).                                                                                                                                  |
| `destination_container` | `mkv`                     | Container format of the output file (always `mkv`).                                                                                                                                                                  |
| `hardware_accelerated`  | `true`, `false`           | `true` when at least one video stream was encoded with a hardware encoder (QSV, VAAPI). `false` when the encoder fell back to software (e.g. libx265), even if `MEDIA_HARDWARE_DEVICE_PATH` is set.                   |
| `crop_applied`          | `true`, `false`           | Whether a crop filter was applied during transcoding.                                                                                                                                                                |

| Metric                                       | Application tags                                                                                                                                          |
| -------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `media_workflow_source_duration_seconds`     | `media_type`, `mapping_name`                                                                                                                              |
| `media_workflow_audio_track_count`           | `media_type`, `mapping_name`                                                                                                                              |
| `media_workflow_subtitle_track_count`        | `media_type`, `mapping_name`                                                                                                                              |
| `media_workflow_invalid_files_total`         | `media_type`, `mapping_name`                                                                                                                              |
| `media_workflow_source_file_size_bytes`      | `media_type`, `mapping_name`, `source_codec`, `destination_codec`, `source_container`, `destination_container`, `hardware_accelerated`, `crop_applied`    |
| `media_workflow_destination_file_size_bytes` | same as `media_workflow_source_file_size_bytes`                                                                                                           |
| `media_workflow_transcode_duration_seconds`  | same as `media_workflow_source_file_size_bytes`                                                                                                           |
| `media_workflow_artwork_fetch_skipped_total` | _(none)_                                                                                                                                                  |
| `media_workflow_metrics_errors_total`        | _(none)_                                                                                                                                                  |

### Worker — Temporal SDK tags

Every metric the worker emits — including the application metrics above — additionally carries the SDK-injected tags below. Use them to filter per namespace, per task queue, or per workflow/activity type when you have multiple workers writing to the same Prometheus.

| Tag             | Description                                                                                  |
| --------------- | -------------------------------------------------------------------------------------------- |
| `namespace`     | The Temporal namespace the worker is connected to.                                           |
| `task_queue`    | The Temporal task queue the worker polls.                                                    |
| `activity_type` | Name of the activity that emitted the metric (present on activity-emitted metrics).          |
| `workflow_type` | Name of the workflow type (present on workflow-emitted SDK metrics).                         |

### Watcher tags

All watcher metrics carry `mapping_name`. `watcher_files_discovered_total`, `watcher_dispatches_total`, and `watcher_dispatch_errors_total` additionally carry `media_type`. `watcher_scans_total` carries a `status` label (`success` or `error`).

## High-cardinality labels

Set `METRICS_HIGH_CARDINALITY_LABELS=true` on the worker to attach per-item identification labels to the histograms that already carry the full transcode tag set (`media_workflow_source_file_size_bytes`, `media_workflow_destination_file_size_bytes`, `media_workflow_transcode_duration_seconds`):

| Label            | Applies to                | Description                          |
| ---------------- | ------------------------- | ------------------------------------ |
| `id`             | all                       | Library item ID from Radarr/Sonarr.  |
| `title`          | all                       | Title of the movie or episode.       |
| `year`           | all                       | Release year.                        |
| `series_title`   | shows (empty for movies)  | Series title.                        |
| `season_number`  | shows (empty for movies)  | Season number.                       |
| `episode_number` | shows (empty for movies)  | Episode number.                      |

When enabled, those three histograms carry the full set of six labels on every observation. For movie observations, the show-only labels (`series_title`, `season_number`, `episode_number`) are present with empty-string values so the label set stays consistent across observations.

These labels significantly increase the cardinality of your metrics. Enable them only if your metrics backend can handle the volume and you need per-item drill-down. If the Radarr/Sonarr metadata lookup required to populate them fails, the run proceeds without high-cardinality tags and `media_workflow_metrics_errors_total` is incremented.

## Logging

Both binaries write plain-text logs to stderr. Set `LOG_LEVEL` to one of `debug`, `info`, `warn`, or `error` (default: `info`).

Temporal Go SDK internal log lines are forwarded through the same handler, so `LOG_LEVEL` controls SDK verbosity as well as application verbosity. At `info` and above the SDK is generally quiet; setting `LOG_LEVEL=debug` surfaces SDK debug messages alongside application debug output.
