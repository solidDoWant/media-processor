# Helm chart

The `media-processor` Helm chart deploys the watcher and worker as separate Kubernetes Deployments. It uses the [bjw-s `common` library](https://bjw-s-labs.github.io/helm-charts) (app-template approach) as its base.

## Installation

The chart is published to GHCR as an OCI artifact:

```sh
helm install my-release oci://ghcr.io/soliddowant/charts/media-processor --version CHART_VERSION -f values.yaml
```

## Required values

The chart has no required values at `helm template` time — the manifests render without error with all defaults. However, the pods will fail to start at runtime without the following values. You will need at a minimum:

- `config.hatchet.token` — Hatchet API token
- `config.watcher.watches` — at least one watch entry
- `config.worker.radarr.url` + `config.worker.radarr.apiKey`
- `config.worker.sonarr.url` + `config.worker.sonarr.apiKey`
- `config.inputVolume` — volume definition for the media input directory
- `config.watcher.volumes` — one volume definition per distinct output path referenced by watch entries

## Values reference

### `config.hatchet.token`

The Hatchet API token for both watcher and worker. Sets `HATCHET_CLIENT_TOKEN` on both containers.

| Field                                        | Type   | Default | Description                                                |
| -------------------------------------------- | ------ | ------- | ---------------------------------------------------------- |
| `config.hatchet.token.value`                 | string | `""`    | Literal token value                                        |
| `config.hatchet.token.secretKeyRef.name`     | string | `""`    | Secret name (takes precedence over `value` when non-empty) |
| `config.hatchet.token.secretKeyRef.key`      | string | `""`    | Key within the Secret                                      |

### `config.inputVolume`

A bjw-s persistence item describing the volume that holds the input media files. The chart mounts it at `/media/input` in both the watcher (read-only) and the worker (read-write). When empty (`{}`), no input volume is created. The chart does not inject `MEDIA_INPUT_ROOT` or `MEDIA_OUTPUT_DIR`, so configure any path-related environment variables your containers need explicitly.

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

### `config.metrics`

Shared observability settings applied to both watcher and worker.

| Field                                  | Type   | Default | Description                                                              |
| -------------------------------------- | ------ | ------- | ------------------------------------------------------------------------ |
| `config.metrics.otel.endpoint`         | string | `""`    | Sets `OTEL_EXPORTER_OTLP_ENDPOINT` on both containers when non-empty     |
| `config.metrics.highCardinalityLabels` | bool   | `false` | Sets `METRICS_HIGH_CARDINALITY_LABELS=true` on both containers when true |

### `config.watcher`

| Field                            | Type   | Default     | Description                                                                                                                 |
| -------------------------------- | ------ | ----------- | --------------------------------------------------------------------------------------------------------------------------- |
| `config.watcher.configType`      | string | `ConfigMap` | Storage type for the watcher YAML config file. `ConfigMap` or `Secret`                                                      |
| `config.watcher.schedule`        | string | `""`        | 6-field Hatchet cron expression for the scan schedule (e.g. `*/30 * * * * *`). When empty, the watcher uses the built-in default (`*/5 * * * * *`, every 5 seconds). Written to `cronSchedule` in the config file |
| `config.watcher.volumes`         | map    | `{}`        | Map of volume names to bjw-s persistence items (see below). When empty, no output volumes are created                      |
| `config.watcher.watches`         | list   | `[]`        | List of watch entries. Written to `watches` in the config file (see below)                                                  |
| `config.watcher.logLevel`        | string | `info`      | Sets `LOG_LEVEL` on the watcher container                                                                                   |
| `config.watcher.metrics.enabled` | bool   | `false`     | When true, sets `METRICS_ADDR=:9090` on the watcher container                                                               |

The watcher YAML config file is stored as a `ConfigMap` (or `Secret` when `configType: Secret`) and mounted read-only at `/etc/media-processor/`. The watcher container receives `--config /etc/media-processor/watcher.yaml`.

### `config.watcher.volumes`

A map of volume names to bjw-s persistence items. Keys become the bjw-s persistence key (and the Kubernetes volume name). Values are standard bjw-s persistence items (`type`, `existingClaim`, `server`/`path` for NFS, etc.) — do not set `globalMounts` or `advancedMounts`, the chart manages those. Volumes are mounted in the worker only. A volume is only mounted if at least one watch entry references it by `volumeName`; volumes with no references are ignored.

| Field                         | Type   | Default | Description                                |
| ----------------------------- | ------ | ------- | ------------------------------------------ |
| `config.watcher.volumes.NAME` | object | —       | bjw-s persistence item for volume `NAME`   |

### `config.watcher.watches` — output fields

Each watch entry in `config.watcher.watches` may include the following Helm-only fields in its `output` block. These fields are stripped before the watcher YAML config is written; the chart injects `output.path` from `mountPath` so the worker receives the correct path at runtime.

| Field                            | Type   | Required | Description                                                                                     |
| -------------------------------- | ------ | -------- | ----------------------------------------------------------------------------------------------- |
| `output.volumeName`              | string | yes      | Name of the volume from `config.watcher.volumes` to mount for this watch entry's output         |
| `output.mountPath`               | string | yes      | Container path where the volume is mounted; becomes `output.path` in the watcher YAML config    |
| `output.subPath`                 | string | no       | Optional volume mount subPath; not written to the watcher YAML config                           |
| `output.remotePath`              | string | no       | How the arr service (Radarr/Sonarr) sees this output path. Written to `remotePath` in the YAML |

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

| Field                           | Type   | Default | Description                                                  |
| ------------------------------- | ------ | ------- | ------------------------------------------------------------ |
| `config.worker.logLevel`        | string | `info`  | Sets `LOG_LEVEL` on the worker container                     |
| `config.worker.metrics.enabled` | bool   | `false` | When true, sets `METRICS_ADDR=:9090` on the worker container |

### `config.worker.media.hardware`

| Field                                          | Type   | Default | Description                                                                                                                                                                                                               |
| ---------------------------------------------- | ------ | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `config.worker.media.hardware.devicePath`      | string | `""`    | Host path to the hardware encoding device (e.g. `/dev/dri/renderD128`). Sets `MEDIA_HARDWARE_DEVICE_PATH` when non-empty                                                                                                  |
| `config.worker.media.hardware.mountHostDevice` | bool   | `true`  | When true and `devicePath` is non-empty, creates a `hostPath` volume mounting the device at the same path inside the worker container. Set to `false` when using a Kubernetes device plugin (e.g. `intel.com/gpu`) or DRA |

### `config.worker.media.blackBarRemoval`

| Field                                                          | Type   | Default | Description                      |
| -------------------------------------------------------------- | ------ | ------- | -------------------------------- |
| `config.worker.media.blackBarRemoval.minimumPixels.horizontal` | int    | `10`    | Sets `MEDIA_MIN_CROP_X`          |
| `config.worker.media.blackBarRemoval.minimumPixels.vertical`   | int    | `10`    | Sets `MEDIA_MIN_CROP_Y`          |
| `config.worker.media.blackBarRemoval.detectTimeout`            | string | `30m`   | Sets `MEDIA_DETECT_CROP_TIMEOUT` |

### `config.worker.media.transcode`

| Field                                        | Type   | Default | Description                                                                                        |
| -------------------------------------------- | ------ | ------- | -------------------------------------------------------------------------------------------------- |
| `config.worker.media.transcode.timeout`      | string | `4h`    | Sets `MEDIA_TRANSCODE_TIMEOUT`                                                                     |
| `config.worker.media.transcode.videoQuality` | string | `""`    | H.265 CRF value (1–51, lower = better quality / larger file). Sets `MEDIA_H265_CRF` when non-empty |

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

### `resources`

Arbitrary bjw-s app-template resources. The chart deep-merges this with its generated resources — values here take precedence over chart defaults. You can use this to override controllers, add services, tune persistence, add RBAC, etc.

The top-level keys follow the [bjw-s common library schema](https://bjw-s-labs.github.io/helm-charts/docs/common-library/common-library-storage): `controllers`, `persistence`, `service`, `configMaps`, `secrets`, `rbac`, `serviceAccount`, `rawResources`, etc.

Default image repositories are set here:

| Field                                                               | Default                               |
| ------------------------------------------------------------------- | ------------------------------------- |
| `resources.controllers.watcher.containers.watcher.image.repository` | `ghcr.io/soliddowant/watcher`         |
| `resources.controllers.watcher.containers.watcher.image.tag`        | `""` (defaults to `Chart.AppVersion`) |
| `resources.controllers.worker.containers.worker.image.repository`   | `ghcr.io/soliddowant/worker`          |
| `resources.controllers.worker.containers.worker.image.tag`          | `""` (defaults to `Chart.AppVersion`) |

## Hard-coded internals

These values are intentionally not configurable in `values.yaml`:

| Setting              | Value                   |
| -------------------- | ----------------------- |
| Input mount path     | `/media/input`          |
| Watcher config mount | `/etc/media-processor/` |
| Watcher health port  | `8081`                  |
| Worker health port   | `8080`                  |
| Metrics port         | `9090`                  |
| Liveness probe path  | `/healthz`              |
| Readiness probe path | `/readyz`               |

## Using Secrets for credentials

Instead of putting token values directly in `values.yaml`, reference a pre-existing Secret:

```yaml
config:
  hatchet:
    token:
      secretKeyRef:
        name: media-processor-secrets
        key: hatchet-token

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
  --from-literal=hatchet-token=YOUR_TOKEN \
  --from-literal=radarr-api-key=YOUR_KEY \
  --from-literal=sonarr-api-key=YOUR_KEY
```

Alternatively, store the watcher YAML config (which may include watch paths) as a Secret by setting `config.watcher.configType: Secret`.

## Worked example: persistence, arr path translation, and hardware acceleration

This example uses a PVC for input and NFS for output, configures arr path translation for the bind-mount arrangement described in the [README](../README.md), and enables Intel QSV hardware acceleration.

```yaml
config:
  hatchet:
    token:
      secretKeyRef:
        name: media-processor-secrets
        key: hatchet-token

  inputVolume:
    type: persistentVolumeClaim
    existingClaim: downloads-pvc

  watcher:
    schedule: "*/30 * * * * *"
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
# external health check can reach it.
resources:
  service:
    worker-health:
      controller: worker
      ports:
        health:
          port: 8080
          protocol: HTTP
```

### Using a device plugin instead of hostPath

When using a Kubernetes device plugin (e.g. `intel.com/gpu`) or DRA to expose the hardware device, set `mountHostDevice: false` and configure device access via the `resources` passthrough:

```yaml
config:
  worker:
    media:
      hardware:
        devicePath: "/dev/dri/renderD128"
        mountHostDevice: false

resources:
  controllers:
    worker:
      containers:
        worker:
          resources:
            limits:
              gpu.intel.com/i915: "1"
```
