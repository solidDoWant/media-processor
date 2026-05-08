# Helm chart

The `media-processor` Helm chart deploys the watcher and worker as separate Kubernetes Deployments. It uses the [bjw-s `common` library](https://bjw-s-labs.github.io/helm-charts) (app-template approach) as its base.

## Installation

The chart is published to GHCR as an OCI artifact:

```sh
helm install my-release oci://ghcr.io/soliddowant/charts/media-processor --version CHART_VERSION -f values.yaml
```

## Required values

`config.temporal.address` is required at `helm template` time — the chart fails with a clear error when it (or any of `config.temporal.namespace` / `config.temporal.taskQueue`) is empty. `namespace` and `taskQueue` have built-in defaults, so only `address` typically needs to be supplied. In addition, the pods will fail at runtime without the following values. You will need at a minimum:

- `config.temporal.address` — Temporal frontend host:port (e.g. `temporal-frontend.temporal.svc.cluster.local:7233`)
- `config.watcher.watches` — at least one watch entry
- `config.worker.radarr.url` + `config.worker.radarr.apiKey`
- `config.worker.sonarr.url` + `config.worker.sonarr.apiKey`
- `config.inputVolume` — volume definition for the media input directory
- `config.watcher.volumes` — one volume definition per distinct `output.volumeName` referenced by watch entries

## Values reference

### `config.temporal`

Temporal frontend connection settings. `address` is required; `namespace` and `taskQueue` have defaults but `helm template` still fails if they are explicitly set to an empty string.

Non-secret fields (`address`, `namespace`, `tls.*`, `grpcMeta`) are rendered into a `temporal.toml` ConfigMap consumed by the [Temporal Go SDK envconfig package](https://pkg.go.dev/go.temporal.io/sdk/contrib/envconfig). The file is mounted on both controllers at `/etc/temporal/temporal.toml` and pointed at via the `TEMPORAL_CONFIG_FILE` env var. Secret material — the API key and TLS cert/key/CA bytes — is delivered separately: the API key as a `valueFrom.secretKeyRef` env var, cert material as Secret-volume mounts. See [`docs/configuration.md`](configuration.md#temporal-client-configuration-file) for the file schema and per-field semantics.

| Field                       | Type   | Default             | Description                                                                                                      |
| --------------------------- | ------ | ------------------- | ---------------------------------------------------------------------------------------------------------------- |
| `config.temporal.address`   | string | `""`                | Temporal frontend host:port (no scheme). Written to `address` in the rendered `temporal.toml`                    |
| `config.temporal.namespace` | string | `"default"`         | Temporal namespace the workflows execute in. Written to `namespace` in the rendered `temporal.toml`              |
| `config.temporal.taskQueue` | string | `"media-processor"` | Task queue the worker polls and the watcher dispatches to. Sets `TEMPORAL_TASK_QUEUE` on both watcher and worker |

#### `config.temporal.apiKey`

API key for authenticated Temporal frontends (e.g. Temporal Cloud). Sets `TEMPORAL_API_KEY` on both watcher and worker. Setting both `value` and `secretKeyRef` is rejected; setting neither leaves the env var unset.

| Field                                      | Type   | Default | Description                            |
| ------------------------------------------ | ------ | ------- | -------------------------------------- |
| `config.temporal.apiKey.value`             | string | `""`    | Literal API key. Intended for dev only |
| `config.temporal.apiKey.secretKeyRef.name` | string | `""`    | Secret name holding the API key        |
| `config.temporal.apiKey.secretKeyRef.key`  | string | `""`    | Key within the Secret                  |

#### `config.temporal.tls`

Transport security to the Temporal frontend. The entire `[tls]` table is omitted from `temporal.toml` when `enabled: false` — the SDK falls back to plaintext gRPC. When `enabled: true`, the chart writes the resolved cert file paths into the TOML and mounts each referenced Secret read-only on both controllers under `/etc/temporal-tls/<secret-name>/`. Multiple references that share a Secret name share a single volume mount.

`clientCertificate` enables mTLS by pointing at a `kubernetes.io/tls` Secret — the standard k8s tls Secret format with `tls.crt` and `tls.key` keys, produced by `kubectl create secret tls` and cert-manager `Certificate` resources. `caCert` is a free-form `secretKeyRef` because CA bundles aren't standardised on a single layout; leave it empty to use the system trust roots. Any cert reference set while `enabled: false` causes `helm template` to fail, so a forgotten flag does not silently disable mTLS or CA verification.

| Field                                              | Type   | Default  | Description                                                                                                                      |
| -------------------------------------------------- | ------ | -------- | -------------------------------------------------------------------------------------------------------------------------------- |
| `config.temporal.tls.enabled`                      | bool   | `false`  | When true, render the `[tls]` table in `temporal.toml`                                                                           |
| `config.temporal.tls.serverName`                   | string | `""`     | Override SNI / cert-verification hostname. Useful when `address` is an IP or service DNS name that does not match the cert's SAN |
| `config.temporal.tls.disableHostVerification`      | bool   | `false`  | Skip server certificate hostname verification. Development-only; do not use against production frontends                         |
| `config.temporal.tls.clientCertificate.secretName` | string | `""`     | Name of a `kubernetes.io/tls` Secret containing the client cert and key for mTLS. Leave empty to skip mTLS                       |
| `config.temporal.tls.caCert.secretKeyRef.name`     | string | `""`     | Secret name holding a custom CA certificate. Leave empty to use the system trust roots                                           |
| `config.temporal.tls.caCert.secretKeyRef.key`      | string | `ca.crt` | Key inside the Secret. Defaults to the cert-manager / k8s convention; override only if the Secret uses a different key name      |

#### `config.temporal.grpcMeta`

Map of gRPC metadata headers added to every Temporal RPC. Keys are passed through verbatim — the Temporal SDK lowercases and hyphenates them on load (so e.g. `X-Tenant-ID` and `x-tenant-id` are equivalent). Rendered under `[profile.default.grpc_meta]` in `temporal.toml`.

```yaml
config:
  temporal:
    grpcMeta:
      X-Tenant-ID: media-processor
      X-Trace-Origin: helm
```

### `config.inputVolume`

A bjw-s persistence item describing the volume that holds the input media files. The chart mounts it at `/media/input` in both the watcher (read-only) and the worker (read-write). When empty (`{}`), no input volume is created.

Any bjw-s persistence item type is supported (`persistentVolumeClaim`, `hostPath`, `nfs`, `custom`, etc.). Do not set `globalMounts` or `advancedMounts` — the chart manages those.

| Field                        | Type   | Default | Description                                |
| ---------------------------- | ------ | ------- | ------------------------------------------ |
| `config.inputVolume.subPath` | string | `""`    | Optional subPath for the volume mount      |
| (all other fields)           | —      | —       | Passed through as a bjw-s persistence item |

Example:

```yaml
config:
  inputVolume:
    type: persistentVolumeClaim
    existingClaim: my-input-pvc
```

### `config.watcher`

| Field                                      | Type   | Default     | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| ------------------------------------------ | ------ | ----------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `config.watcher.configType`                | string | `ConfigMap` | Storage type for the watcher YAML config file. `ConfigMap` or `Secret`                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `config.watcher.scanInterval`              | string | `""`        | Duration between directory scans, as a Go duration string (e.g. `5s`, `1m30s`). When empty, the watcher uses the built-in default of `5s`. Written to `scanInterval` in the rendered watcher YAML config                                                                                                                                                                                                                                                                                   |
| `config.watcher.volumes`                   | map    | `{}`        | Map of volume names to bjw-s persistence items (see below). When empty, no output volumes are created                                                                                                                                                                                                                                                                                                                                                                                      |
| `config.watcher.watches`                   | list   | `[]`        | List of watch entries. Written to `watches` in the config file (see below)                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `config.watcher.logLevel`                  | string | `info`      | Sets `LOG_LEVEL` on the watcher container                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `config.watcher.metrics.enabled`           | bool   | `false`     | When true, the chart emits the watcher-metrics `Service` and its `ServiceMonitor`. The watcher binary always exposes `/metrics` on port 9091 regardless; this toggle only controls cluster-side scraping infrastructure (see [Metrics scraping](#metrics-scraping))                                                                                                                                                                                                                        |
| `config.watcher.metrics.scrapeWaitTimeout` | string | `""`        | Sets `METRICS_SCRAPE_WAIT_TIMEOUT` on the watcher container when non-empty. When empty, the binary default of `60s` applies. See [Termination and drain](#termination-and-drain) for the relationship with `terminationGracePeriodSeconds`                                                                                                                                                                                                                                                 |
| `config.watcher.searchAttributes.enabled`  | bool   | `true`      | When `true` (or unset), the chart leaves `WATCHER_TEMPORAL_SEARCH_ATTRIBUTES_ENABLED` unset and the watcher default (`true`) applies. When `false`, the chart sets `WATCHER_TEMPORAL_SEARCH_ATTRIBUTES_ENABLED=false` to disable custom search attributes. Requires the five custom search attributes to be pre-registered in the Temporal namespace when effectively enabled; see [Temporal search attributes](configuration.md#temporal-search-attributes) for the registration command. |

The watcher YAML config file is stored as a `ConfigMap` (or `Secret` when `configType: Secret`) and mounted read-only at `/etc/media-processor/`. The watcher container receives `--config /etc/media-processor/watcher.yaml`.

### `config.watcher.volumes`

A map of volume names to bjw-s persistence items. Keys become the bjw-s persistence key (and the Kubernetes volume name). Values are standard bjw-s persistence items (`type`, `existingClaim`, `server`/`path` for NFS, etc.) — do not set `globalMounts` or `advancedMounts`, the chart manages those. Volumes are mounted in the worker only. A volume is only mounted if at least one watch entry references it by `volumeName`; volumes with no references are ignored.

| Field                         | Type   | Default | Description                              |
| ----------------------------- | ------ | ------- | ---------------------------------------- |
| `config.watcher.volumes.NAME` | object | —       | bjw-s persistence item for volume `NAME` |

### `config.watcher.watches` — output fields

Each watch entry in `config.watcher.watches` may include the following fields in its `output` block. `output.volumeName`, `output.mountPath`, and `output.subPath` are Helm-only fields used to configure Kubernetes volume mounts; they are not written to the watcher YAML config. The chart injects `output.path` from `mountPath` so the worker receives the correct path at runtime. `output.remotePath` is not Helm-only and is preserved as `remotePath` in the watcher YAML config.

| Field               | Type   | Required | Description                                                                                    |
| ------------------- | ------ | -------- | ---------------------------------------------------------------------------------------------- |
| `output.volumeName` | string | yes      | Name of the volume from `config.watcher.volumes` to mount for this watch entry's output        |
| `output.mountPath`  | string | yes      | Container path where the volume is mounted; becomes `output.path` in the watcher YAML config   |
| `output.subPath`    | string | no       | Optional volume mount subPath; not written to the watcher YAML config                          |
| `output.remotePath` | string | no       | How the arr service (Radarr/Sonarr) sees this output path. Written to `remotePath` in the YAML |

Multiple watch entries may reference the same `volumeName` with different `mountPath`/`subPath` values; the chart creates one Kubernetes volume with multiple mounts rather than duplicating the underlying PVC or NFS share.

Example — one NFS share serving two watch entries at different sub-paths:

```yaml
config:
  watcher:
    volumes:
      media-output:
        type: nfs
        server: nas.example.com
        path: /volume1/media
    watches:
      - name: movies
        watchedPath: /media/input/movies
        mediaType: movie
        output:
          volumeName: media-output
          mountPath: /media/output/movies
          subPath: movies
          remotePath: /downloads/movies
      - name: shows
        watchedPath: /media/input/shows
        mediaType: show
        output:
          volumeName: media-output
          mountPath: /media/output/shows
          subPath: shows
          remotePath: /downloads/shows
```

### `config.worker`

| Field                                         | Type   | Default | Description                                                                                                                                                                                                                                                                                                                                                                                         |
| --------------------------------------------- | ------ | ------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `config.worker.logLevel`                      | string | `info`  | Sets `LOG_LEVEL` on every worker container                                                                                                                                                                                                                                                                                                                                                          |
| `config.worker.stopTimeout`                   | string | `30s`   | Sets `WORKER_STOP_TIMEOUT` on every worker container. The chart default of `30s` keeps the SIGTERM-to-SIGKILL window inside the default `terminationGracePeriodSeconds` of `120s`. Operators running long transcodes must raise this together with `metrics.scrapeWaitTimeout` and the worker controller's `pod.terminationGracePeriodSeconds`. See [Termination and drain](#termination-and-drain) |
| `config.worker.metrics.enabled`               | bool   | `false` | When true, the chart emits one `PodMonitor` per worker controller defined under `workers`. The worker binary always exposes `/metrics` on port 9090 regardless; this toggle only controls cluster-side scraping infrastructure (see [Metrics scraping](#metrics-scraping))                                                                                                                          |
| `config.worker.metrics.scrapeWaitTimeout`     | string | `""`    | Sets `METRICS_SCRAPE_WAIT_TIMEOUT` on every worker container when non-empty. When empty, the binary default of `60s` applies. See [Termination and drain](#termination-and-drain)                                                                                                                                                                                                                   |
| `config.worker.metrics.highCardinalityLabels` | bool   | `false` | When true, sets `METRICS_HIGH_CARDINALITY_LABELS=true` on every worker container. Enables labels with high cardinality (e.g. media titles) on worker metrics — useful for debugging but can multiply Prometheus series counts                                                                                                                                                                       |

### `config.worker.media.hardware`

| Field                                          | Type   | Default | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| ---------------------------------------------- | ------ | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `config.worker.media.hardware.devicePath`      | string | `""`    | Host path to the hardware encoding device (e.g. `/dev/dri/renderD128`). Sets `MEDIA_HARDWARE_DEVICE_PATH` when non-empty, which the worker uses verbatim. When empty, transcode-enabled workers auto-detect an Intel i915 render node from the pod's view of `/sys/class/drm/`. Auto-detection requires both the DRM sysfs view and the matching `/dev/dri/renderD*` character device to be visible in the pod (via `mountHostDevice: true` with a non-empty `devicePath`, a Kubernetes device plugin such as `intel.com/gpu`, DRA, or another explicit mount); when the device is not mounted the worker falls back to software-only mode |
| `config.worker.media.hardware.mountHostDevice` | bool   | `true`  | When true and `devicePath` is non-empty, creates a `hostPath` volume mounting the device at the same path inside the worker container. Set to `false` when using a Kubernetes device plugin (e.g. `intel.com/gpu`) or DRA                                                                                                                                                                                                                                                                                                                                                                                                                  |

### `config.worker.media.blackBarRemoval`

| Field                                                          | Type   | Default | Description                      |
| -------------------------------------------------------------- | ------ | ------- | -------------------------------- |
| `config.worker.media.blackBarRemoval.minimumPixels.horizontal` | int    | `10`    | Sets `MEDIA_MIN_CROP_X`          |
| `config.worker.media.blackBarRemoval.minimumPixels.vertical`   | int    | `10`    | Sets `MEDIA_MIN_CROP_Y`          |
| `config.worker.media.blackBarRemoval.detectTimeout`            | string | `30m`   | Sets `MEDIA_DETECT_CROP_TIMEOUT` |

### `config.worker.media.transcode`

| Field                                               | Type   | Default | Description                                                                                                                                                                                                                                                                                                                      |
| --------------------------------------------------- | ------ | ------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `config.worker.media.transcode.timeout`             | string | `4h`    | Sets `MEDIA_TRANSCODE_TIMEOUT`                                                                                                                                                                                                                                                                                                   |
| `config.worker.media.transcode.videoQuality`        | string | `""`    | H.265 CRF value (1–51, lower = better quality / larger file). Sets `MEDIA_H265_CRF` when non-empty                                                                                                                                                                                                                               |
| `config.worker.media.transcode.progressLogInterval` | string | `""`    | Sets `MEDIA_PROGRESS_LOG_INTERVAL` when non-empty. Controls transcode progress log cadence and also drives the activity's Temporal `HeartbeatTimeout` (set to `2x` this value), so a stuck encode is detected within roughly twice this interval. Set to `"0s"` to disable both. When empty, the binary default of `30s` applies |

### `config.worker.media.webhookUrl`

Sets `MEDIA_WEBHOOK_URL` on the worker when non-empty.

### `config.worker.radarr`

| Field                                           | Type   | Default | Description                                                 |
| ----------------------------------------------- | ------ | ------- | ----------------------------------------------------------- |
| `config.worker.radarr.url`                      | string | `""`    | Sets `RADARR_URL`                                           |
| `config.worker.radarr.apiKey.value`             | string | `""`    | Literal Radarr API key. Sets `RADARR_API_KEY`               |
| `config.worker.radarr.apiKey.secretKeyRef.name` | string | `""`    | Secret name for the API key (takes precedence over `value`) |
| `config.worker.radarr.apiKey.secretKeyRef.key`  | string | `""`    | Key within the Secret                                       |

### `config.worker.sonarr`

| Field                                           | Type   | Default | Description                                                 |
| ----------------------------------------------- | ------ | ------- | ----------------------------------------------------------- |
| `config.worker.sonarr.url`                      | string | `""`    | Sets `SONARR_URL`                                           |
| `config.worker.sonarr.apiKey.value`             | string | `""`    | Literal Sonarr API key. Sets `SONARR_API_KEY`               |
| `config.worker.sonarr.apiKey.secretKeyRef.name` | string | `""`    | Secret name for the API key (takes precedence over `value`) |
| `config.worker.sonarr.apiKey.secretKeyRef.key`  | string | `""`    | Key within the Secret                                       |

### `workers`

`workers` is a map of worker controller name → per-worker app config. Each entry produces one Kubernetes `Deployment` that polls a configurable subset of activity task queues. Default `workers: {}` produces a single controller named `main` with `activities: ["all"]`, behaviorally equivalent to the pre-multi-worker chart.

Per-worker app config (currently):

| Field                       | Type | Default | Description                                                                                                                                                                                                                                                                                                                     |
| --------------------------- | ---- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `workers.<name>.activities` | list | —       | List of `WORKER_ACTIVITIES` tokens. `all` expands to every known activity; `!name` removes one. Examples: `["all"]`, `["all", "!transcode"]`, `["transcode"]`. The chart joins the list with commas and sets `WORKER_ACTIVITIES` on the worker. See [configuration.md](configuration.md#activity-task-queues) for token grammar |

The container inside each worker controller is named `main` regardless of the worker key. Operator-supplied Kubernetes-level configuration (replicas, resources, node selectors, tolerations, pod spec overrides) for a given worker goes under `resources.controllers.<workerName>` using the upstream `app-template` controller schema and is deep-merged over the chart-generated defaults.

#### Default (single-controller) deployment

The chart's default values produce one Deployment named `<release>-media-processor-main` with `WORKER_ACTIVITIES=all`. No `workers` block is required:

```yaml
config:
  temporal:
    address: "temporal-frontend.temporal.svc.cluster.local:7233"
  # ... rest of config ...
# workers omitted — chart implicitly defines workers.main = { activities: ["all"] }
```

To raise replicas or override container resources for the default worker, add a `resources.controllers.main` block:

```yaml
resources:
  controllers:
    main:
      replicas: 3
      containers:
        main:
          resources:
            requests:
              cpu: 500m
              memory: 1Gi
```

#### Split-controller deployment

Defining at least one entry under `workers` fully replaces the implicit `main` default — operators control the worker set entirely. This example separates a transcode-only worker (with GPU resource limits and a node selector) from a general worker that handles everything else:

```yaml
workers:
  general:
    activities: ["all", "!transcode"]
  transcode:
    activities: ["transcode"]

resources:
  controllers:
    general:
      replicas: 2
    transcode:
      replicas: 4
      pod:
        nodeSelector:
          intel.feature.node.kubernetes.io/gpu: "true"
      containers:
        main:
          resources:
            limits:
              gpu.intel.com/i915: "1"
```

The chart emits two `Deployment`s (`<release>-media-processor-general` and `<release>-media-processor-transcode`) and, when `config.worker.metrics.enabled` is true, one `PodMonitor` per worker (`<release>-media-processor-<workerName>`). Each worker's `Deployment` carries its own `WORKER_ACTIVITIES` env var and inherits the shared `config.worker.*` settings.

To keep `main` alongside additional workers, include it explicitly:

```yaml
workers:
  main:
    activities: ["all"]
  transcode:
    activities: ["transcode"]
```

### `resources`

Arbitrary bjw-s app-template resources. The chart deep-merges this with its generated resources — values here take precedence over chart defaults. You can use this to override controllers, add services, tune persistence, add RBAC, etc.

The top-level keys follow the [bjw-s common library schema](https://bjw-s-labs.github.io/helm-charts/docs/common-library/common-library-storage): `controllers`, `persistence`, `service`, `serviceMonitor`, `podMonitor`, `configMaps`, `secrets`, `rbac`, `serviceAccount`, `rawResources`, etc.

Worker controllers (one per `workers.<name>` entry) are generated by the chart template — there are no static worker controller defaults under `resources.controllers` in `values.yaml`. The chart-generated defaults include the worker image, named metrics port `9090`, `/healthz` and `/readyz` probes on `8080`, hardened `securityContext`, a `PodDisruptionBudget` enabled when replicas > 1, and `pod.terminationGracePeriodSeconds: 120`. Override per-worker by adding `resources.controllers.<workerName>` with the standard bjw-s controller schema.

Default image repositories:

| Field                                                                 | Default                                                                                               |
| --------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| `resources.controllers.watcher.containers.watcher.image.repository`   | `ghcr.io/soliddowant/media-processor/watcher`                                                         |
| `resources.controllers.watcher.containers.watcher.image.tag`          | `""` (defaults to `Chart.AppVersion`)                                                                 |
| `resources.controllers.<workerName>.containers.main.image.repository` | `ghcr.io/soliddowant/media-processor/worker` (chart-generated default; override per worker if needed) |
| `resources.controllers.<workerName>.containers.main.image.tag`        | `Chart.AppVersion` (chart-generated default; override per worker if needed)                           |

## Metrics scraping

The watcher and worker binaries always expose `/metrics` (worker on `:9090`, watcher on `:9091` — defaults chosen so the two can run side-by-side on the same host). The `config.{watcher,worker}.metrics.enabled` toggles control whether the chart creates the cluster-side scraping infrastructure on top — a Prometheus-operator monitor (and, for the watcher, the `Service` it scrapes through). Leave both off when running without prometheus-operator CRDs installed; the binaries still serve `/metrics` for `kubectl port-forward` or any other in-cluster client.

When a toggle is true, the chart emits a monitor for the matching controller. The watcher and each worker use different monitor types because their drain behavior differs.

| Controller          | Monitor type     | Resource name                            | Why                                                                                                                                                                                                                                                                                                                                      |
| ------------------- | ---------------- | ---------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| watcher             | `ServiceMonitor` | `{release}-media-processor-watcher`      | The scan loop exits within milliseconds of SIGTERM. The pod is removed from the `Endpoints` object on termination, but the metrics scrape-wait gate completes well before that matters                                                                                                                                                   |
| each `<workerName>` | `PodMonitor`     | `{release}-media-processor-<workerName>` | Worker activity drain can run for the full `WORKER_STOP_TIMEOUT`. A `ServiceMonitor` would lose the terminating pod from `Endpoints` immediately and the final scrape would be missed, breaking the [scrape-on-shutdown gate](configuration.md#shared-watcher-and-worker). `PodMonitor` watches pods directly and is unaffected by drain |

`config.worker.metrics.enabled` toggles the metrics infrastructure for **every** worker controller — it is shared, not per-worker. When true, the chart emits one `PodMonitor` (`{release}-media-processor-<workerName>`) for each entry in `workers`. No per-worker `Service` is created — the `PodMonitor` selects pods directly. The watcher's `Service` (`{release}-media-processor-watcher-metrics`) backs its `ServiceMonitor`.

The `PodMonitor`s are emitted via the bjw-s `podMonitor` map (added in `common` library 5.0). Each `PodMonitor`'s selector matches `app.kubernetes.io/name`, `app.kubernetes.io/instance`, and `app.kubernetes.io/controller: <workerName>` — the labels the bjw-s app-template applies to every controller's pods, including pods produced by `type: scaledjob` workers. Each worker container exposes the metrics port under the name `metrics` so the `PodMonitor` can reference it by port name. The `jobLabel` defaults to `app.kubernetes.io/name`, so the `job` Prometheus label on scraped worker and watcher metrics is the chart name (`media-processor`); operators upgrading from an earlier chart that scraped under `release-media-processor-watcher` / `release-media-processor-<workerName>` will need to update dashboards and alerts that pinned the old `job` value.

## Termination and drain

The chart sets the following defaults to make the SIGTERM → drain → final-scrape → exit sequence fit inside the Kubernetes pod grace period:

| Knob                                                                   | Default | Notes                                                                                                                                                                                                                                                                 |
| ---------------------------------------------------------------------- | ------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `resources.controllers.watcher.pod.terminationGracePeriodSeconds`      | `120`   | Covers the watcher's near-instant scan-loop teardown plus the binary's `60s` `METRICS_SCRAPE_WAIT_TIMEOUT` plus a buffer                                                                                                                                              |
| `resources.controllers.<workerName>.pod.terminationGracePeriodSeconds` | `120`   | Chart-generated default for every worker controller; sized for the chart's `WORKER_STOP_TIMEOUT=30s` + binary's `METRICS_SCRAPE_WAIT_TIMEOUT=60s` + 30s buffer. Override per worker by setting `resources.controllers.<workerName>.pod.terminationGracePeriodSeconds` |
| `config.worker.stopTimeout`                                            | `30s`   | Sets `WORKER_STOP_TIMEOUT` on every worker. Overrides the binary's much longer fallback (which tracks `MEDIA_TRANSCODE_TIMEOUT`) so a fresh-out-of-the-box install drains within its 120s grace period                                                                |
| `config.watcher.metrics.scrapeWaitTimeout`                             | `""`    | Empty leaves the binary default of `60s`. Sets `METRICS_SCRAPE_WAIT_TIMEOUT` on the watcher                                                                                                                                                                           |
| `config.worker.metrics.scrapeWaitTimeout`                              | `""`    | Empty leaves the binary default of `60s`. Sets `METRICS_SCRAPE_WAIT_TIMEOUT` on every worker                                                                                                                                                                          |

The required relationship is:

```
terminationGracePeriodSeconds >= WORKER_STOP_TIMEOUT + METRICS_SCRAPE_WAIT_TIMEOUT + buffer
```

For the worker chart defaults this works out to `120 >= 30 + 60 + 30` (30s buffer for SIGKILL margin). For the watcher there is no `WORKER_STOP_TIMEOUT` and the scan loop teardown is sub-second, so `120 >= 0 + 60 + 60` holds with room to spare.

**Operators running long transcodes must raise all three together.** The chart's 120s grace will SIGKILL an in-flight activity on shutdown unless `config.worker.stopTimeout`, `config.worker.metrics.scrapeWaitTimeout`, and the worker controller's `pod.terminationGracePeriodSeconds` are all increased to fit the longest expected activity. `config.worker.*` is shared across every worker controller; the per-worker `terminationGracePeriodSeconds` override goes under `resources.controllers.<workerName>.pod`. For the default single-controller layout the worker name is `main`:

```yaml
config:
  worker:
    # Allow up to 6h for an in-flight transcode to drain on SIGTERM.
    stopTimeout: "6h"
    metrics:
      enabled: true
      scrapeWaitTimeout: "60s"

resources:
  controllers:
    main:
      pod:
        # 6h drain + 60s scrape wait + 60s buffer.
        terminationGracePeriodSeconds: 21720
```

In a split-controller setup, only the worker(s) running the long activity need the raised grace period:

```yaml
workers:
  general:
    activities: ["all", "!transcode"]
  transcode:
    activities: ["transcode"]

resources:
  controllers:
    transcode:
      pod:
        terminationGracePeriodSeconds: 21720
```

### Autoscaling (KEDA ScaledJob)

The chart can render selected worker controllers as KEDA `ScaledJob` resources that scale 0..N on Temporal task-queue backlog. Set `resources.controllers.<name>.type: scaledjob` to opt a worker in. The full operator runbook (cluster prerequisites, the `keda.*` and `job.*` knob tables, the `PodDisruptionBudget` default difference, the `WORKER_IDLE_EXIT_AFTER` chart defaults, the worked split-deployment example, and the trade-offs) lives in [docs/autoscaling.md](autoscaling.md).

Chart values added for ScaledJob controllers — see [autoscaling.md](autoscaling.md) for full descriptions:

| Field                                 | Default              | Description                                                                                                                                                                                                                                                  |
| ------------------------------------- | -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `resources.controllers.<name>.type`   | `deployment`         | Set to `scaledjob` to render the controller as a `keda.sh/v1alpha1` `ScaledJob` instead of a Deployment. Only valid for entries that match a `workers.<name>` key                                                                                            |
| `resources.controllers.<name>.keda.*` | _unset_              | KEDA-level fields lifted onto the `ScaledJob` spec: `maxReplicaCount`, `pollingInterval`, `successfulJobsHistoryLimit`, `failedJobsHistoryLimit`, `scalingStrategy`, `targetQueueSize` (default `5`), `activationTargetQueueSize` (default `0`)              |
| `resources.controllers.<name>.job.*`  | _unset_              | Job-template fields lifted onto `jobTargetRef.spec`: `parallelism`, `completions`, `activeDeadlineSeconds`, `backoffLimit`, `ttlSecondsAfterFinished`                                                                                                        |
| `config.temporal.keda.apiKey`         | `{}`                 | Temporal API key used by KEDA's scaler. Required when at least one `type: scaledjob` worker is defined; the chart fails at template time otherwise. Configured separately from `config.temporal.apiKey` so KEDA can be granted a least-privileged credential |
| `config.temporal.keda.tls.*`          | `{ enabled: false }` | KEDA's transport security to the Temporal frontend, configured independently from `config.temporal.tls.*`. Mirrors the same shape (`enabled`, `serverName`, `disableHostVerification`, `clientCertificate.secretName`, `caCert.secretKeyRef`)                |

Chart-driven defaults for `type: scaledjob` controllers:

- `WORKER_IDLE_EXIT_AFTER` is set automatically (`5m` for activity-only workers, `15m` for workers whose activities resolve to include the `workflow` token). Override per-controller via `containers.main.env.WORKER_IDLE_EXIT_AFTER`.
- `podDisruptionBudget.maxUnavailable: 0` is enabled by default (vs. the `enabled when replicas > 1, minAvailable: 1` default for Deployment-typed workers). Opt out per-controller via `podDisruptionBudget: null` or `podDisruptionBudget.enabled: false`.

Operators running long transcodes on ScaledJob workers must raise `resources.controllers.<name>.pod.terminationGracePeriodSeconds` together with `config.worker.stopTimeout` and `config.worker.metrics.scrapeWaitTimeout`, exactly as for Deployment workers; see [Termination and drain](#termination-and-drain).

Cluster prerequisites: KEDA &gt;= 2.16 and Temporal server &gt;= 1.24 must be installed cluster-side before enabling `type: scaledjob`. The chart only renders the `ScaledJob` and `TriggerAuthentication` resources; KEDA's CRDs and operator are not bundled.

Count-based HPA is *not* recommended as an alternative: `status.replicas` includes terminating pods, so a scale-out triggered while one pod is still draining may not create a new replica.

## Hard-coded internals

These values are intentionally not configurable in `values.yaml`:

| Setting               | Value                       |
| --------------------- | --------------------------- |
| Input mount path      | `/media/input`              |
| Watcher config mount  | `/etc/media-processor/`     |
| Temporal config mount | `/etc/temporal/`            |
| Temporal TLS root     | `/etc/temporal-tls/<name>/` |
| Watcher health port   | `8081`                      |
| Worker health port    | `8080`                      |
| Watcher metrics port  | `9091`                      |
| Worker metrics port   | `9090`                      |
| Liveness probe path   | `/healthz`                  |
| Readiness probe path  | `/readyz`                   |

## Using Secrets for credentials

Instead of putting API keys directly in `values.yaml`, reference a pre-existing Secret:

```yaml
config:
  worker:
    radarr:
      apiKey:
        secretKeyRef:
          name: media-processor-secrets
          key: radarr-api-key
    sonarr:
      apiKey:
        secretKeyRef:
          name: media-processor-secrets
          key: sonarr-api-key
```

Create the Secret before installing the chart:

```sh
kubectl create secret generic media-processor-secrets \
  --from-literal=radarr-api-key=YOUR_KEY \
  --from-literal=sonarr-api-key=YOUR_KEY
```

Alternatively, store the watcher YAML config (which may include watch paths) as a Secret by setting `config.watcher.configType: Secret`.

## Worked example: persistence, arr path translation, and hardware acceleration

This example uses a PVC for input and NFS for output, configures arr path translation for the bind-mount arrangement described in the [README](../README.md), and enables Intel QSV hardware acceleration.

```yaml
config:
  temporal:
    address: "temporal-frontend.temporal.svc.cluster.local:7233"
    namespace: "default"
    taskQueue: "media-processor"

  inputVolume:
    type: persistentVolumeClaim
    existingClaim: downloads-pvc

  watcher:
    scanInterval: "30s"
    volumes:
      processed-output:
        type: nfs
        server: nas.example.com
        path: /volume1/processed
    watches:
      - name: movies
        watchedPath: /media/input/movies
        mediaType: movie
        output:
          volumeName: processed-output
          mountPath: /media/output/movies
          subPath: movies
          # Radarr's /downloads/movies is bind-mounted from the NFS share on the host,
          # so Radarr sees /downloads/movies where the worker writes /media/output/movies.
          remotePath: /downloads/movies
      - name: shows
        watchedPath: /media/input/shows
        mediaType: show
        output:
          volumeName: processed-output
          mountPath: /media/output/shows
          subPath: shows
          remotePath: /downloads/shows

  worker:
    media:
      hardware:
        # Intel QSV via /dev/dri/renderD128 (bare-metal or VM node)
        devicePath: "/dev/dri/renderD128"
        mountHostDevice: true

    radarr:
      url: "http://radarr.media.svc.cluster.local:7878"
      apiKey:
        secretKeyRef:
          name: media-processor-secrets
          key: radarr-api-key

    sonarr:
      url: "http://sonarr.media.svc.cluster.local:8989"
      apiKey:
        secretKeyRef:
          name: media-processor-secrets
          key: sonarr-api-key

# Add a Service for the worker health port so a load balancer or
# external health check can reach it. The default single-controller layout
# names its worker "main".
resources:
  service:
    worker-health:
      controller: main
      ports:
        health:
          port: 8080
          protocol: HTTP
```

### Using a device plugin instead of hostPath

When using a Kubernetes device plugin (e.g. `intel.com/gpu`) or DRA to expose the hardware device, set `mountHostDevice: false` and configure device access via the `resources` passthrough. The default single-controller layout names its worker `main` and the container inside it is also named `main`:

```yaml
config:
  worker:
    media:
      hardware:
        devicePath: "/dev/dri/renderD128"
        mountHostDevice: false

resources:
  controllers:
    main:
      containers:
        main:
          resources:
            limits:
              gpu.intel.com/i915: "1"
```
