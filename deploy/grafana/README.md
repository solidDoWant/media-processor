# Grafana dashboard

`media-processor-dashboard.json` is a ready-to-import Grafana dashboard for the worker and watcher binaries. It visualises the Prometheus metrics documented in [`docs/metrics.md`](../../docs/metrics.md).

## What it shows

The dashboard is organised into rows:

- **Overview** — workflow completion rate, p95 end-to-end latency, aggregate compression ratio, hardware-acceleration rate, invalid-file rate, plus end-to-end and schedule-to-start latency trends.
- **Transcode performance** — transcode duration by acceleration, throughput, source-vs-destination file size, output codec mix, and per-mapping volume.
- **Transcode admission controller** — slots in flight, load utilization, admission blocked time, admission mode (probe vs static), and worker idle-exit countdown.
- **Workflow skips & errors** — invalid files, import skips (not-in-library / not-an-upgrade), artwork-fetch skips, metrics-lookup errors, and latest probe track counts.
- **Watcher** — time since last successful scan, scan rate by status, scan duration, and discovery/dispatch rates.
- **Worker fleet** — worker pods per enabled activity.

## Importing

1. In Grafana, go to **Dashboards → New → Import**.
2. Upload `media-processor-dashboard.json` (or paste its contents).
3. When prompted, select your Prometheus datasource. The dashboard exposes it as a `datasource` template variable, so it is not pinned to a specific datasource UID.

## Template variables

`namespace`, `task_queue`, `mapping_name`, and `media_type` are populated from label values and default to *All*. Use them to scope the view to a single Temporal namespace/task queue or a single watch mapping.

### Namespace label name (`namespace_label`)

The Temporal SDK emits a `namespace` label, but Prometheus renames a scraped label to `exported_namespace` when it collides with a target label of the same name (commonly the Kubernetes pod namespace applied during service discovery). To stay portable, every query references the namespace through a hidden `constant` variable (`namespace_label`) rather than a hard-coded label name.

The committed value is `exported_namespace`, matching the prod scrape. It is a hidden variable, not a user-facing dropdown. If your environment has no collision (the scraped label is preserved as-is), change the `namespace_label` constant's `query`/`current` value to `namespace`.

If you import the dashboard into multiple environments that disagree on the label name, render an environment-specific copy at publish time (e.g. `jq`/`envsubst` rewriting that one value) rather than editing the variable in each place — the grafana-operator does not substitute non-datasource inputs for raw-JSON (`url`/`json`/`configMapRef`) dashboard sources.

## Notes

- Temporal SDK panels (end-to-end latency, schedule-to-start latency) depend on the Temporal Go SDK Prometheus instruments. The exact instrument set varies by SDK version — see the [Temporal SDK metrics reference](https://docs.temporal.io/references/sdk-metrics).
- Admission-controller panels only show data for workers whose `WORKER_ACTIVITIES` set includes `transcode`. The idle-exit panel only shows data when `WORKER_IDLE_EXIT_AFTER` is set to a positive duration.
- The dashboard uses only the standard (non-high-cardinality) label set, so it works regardless of whether `METRICS_HIGH_CARDINALITY_LABELS` is enabled.
