# Autoscaling (KEDA ScaledJob)

The Helm chart can render selected worker controllers as KEDA `ScaledJob` resources that scale 0..N based on Temporal task-queue backlog. This is opt-in per controller — by default every worker is a `Deployment` with a fixed replica count.

`ScaledJob` is the right shape for media-processor's long-running activities (transcodes especially): each spawned Job runs to completion, so KEDA never SIGTERMs a mid-flight encode the way an HPA-driven scale-down on a Deployment would. Workers exit themselves between bursts via the `WORKER_IDLE_EXIT_AFTER` idle drain (see [Idle exit](#idle-exit)), and KEDA re-spawns Jobs when backlog reappears.

## When to use it

Cold-start latency on a scale-from-zero is real: a fresh pod pays pod-startup + Go runtime + Temporal dial + (for `transcode`) load-probe init before it polls its first task. ScaledJob is a good fit when the activity duration amortizes that cost.

| Activity     | Typical duration | ScaledJob fit                                                        |
| ------------ | ---------------- | --------------------------------------------------------------------- |
| `transcode`  | minutes to hours | Yes — startup cost is negligible vs run time.                         |
| `detect-crop`| seconds          | Sometimes — a short queue burst can amortize startup over a few jobs. |
| `probe`      | sub-second       | No — startup dominates. Keep on a Deployment worker.                  |
| `notify`, `cleanup`, `notify-failure` | sub-second | No — same reason.                                          |
| `workflow`   | always-on poller | Possible but see [Trade-offs](#trade-offs); workflow workers lose their sticky cache on idle exit. |

A common split (see [Worked example](#worked-example) below) is: one always-on `general` Deployment running the workflow + every cheap activity, and one `transcode` ScaledJob that goes 0..N.

## Cluster prerequisites

The chart only renders the resources; the operator and CRDs must already be installed cluster-side.

- **KEDA &gt;= 2.16.** The chart emits `keda.sh/v1alpha1` `ScaledJob` and `TriggerAuthentication`. The Temporal scaler is shipped with KEDA itself; no extra installs.
- **Temporal server &gt;= 1.24.** The KEDA Temporal scaler reads `ApproximateBacklogCount` from `DescribeTaskQueue`, which the server only populates from 1.24 onwards. Earlier servers leave the field zero, so KEDA never spawns jobs.
- **A Temporal credential KEDA can use.** The credential lives under `config.temporal.keda.apiKey` (separate from the workers' own `config.temporal.apiKey`) so KEDA can be granted a less-privileged scope (e.g. read-only `DescribeTaskQueue`) without sharing the workers' API key. The chart fails at template time if you enable `type: scaledjob` without populating it. See [KEDA-side Temporal credential](#keda-side-temporal-credential).

## Enabling autoscaling

Set `type: scaledjob` on the per-controller block and (optionally) override the KEDA / Job knobs:

```yaml
workers:
  transcode:
    activities: ["transcode"]

resources:
  controllers:
    transcode:
      type: scaledjob
      keda:
        maxReplicaCount: 4
        pollingInterval: 30
      job:
        backoffLimit: 0
        activeDeadlineSeconds: 28800
        ttlSecondsAfterFinished: 600
```

`type: scaledjob` is only valid for entries that match a `workers.<name>` key. The chart fails at template time if a `scaledjob` controller is not declared in `workers`, or if the `type` string is not one of the supported set (`deployment`, `daemonset`, `statefulset`, `cronjob`, `job`, `scaledjob`).

The container inside each scaledjob worker is named `main`, so per-controller env / resource / probe overrides go under `resources.controllers.<name>.containers.main` exactly as for a Deployment-typed worker.

### `keda.*` — fields lifted onto the `ScaledJob` spec

| Field                         | Default | Description                                                                                                                                                                                                |
| ----------------------------- | ------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `maxReplicaCount`             | _unset_ | Hard ceiling on concurrent Jobs spawned by KEDA. Omit to use KEDA's built-in default (`100`). Pair with `MEDIA_TRANSCODE_LIMITER_STATIC_CAP=1` so each pod runs exactly one transcode (see [`MEDIA_TRANSCODE_LIMITER_STATIC_CAP` under KEDA](#media_transcode_limiter_static_cap-under-keda)). |
| `pollingInterval`             | _unset_ | Seconds between Temporal `DescribeTaskQueue` polls. Omit to use KEDA's default (`30`). Lower values reduce scale-up latency at the cost of more Temporal RPCs.                                              |
| `successfulJobsHistoryLimit`  | _unset_ | Number of completed Jobs KEDA retains for inspection. Omit to use KEDA's default.                                                                                                                          |
| `failedJobsHistoryLimit`      | _unset_ | Number of failed Jobs KEDA retains for inspection. Omit to use KEDA's default.                                                                                                                              |
| `scalingStrategy`             | _unset_ | KEDA scaling strategy block (e.g. `{ strategy: accurate }`). Passed through verbatim. See the [KEDA ScaledJob docs](https://keda.sh/docs/latest/reference/scaledjob-spec/#scalingstrategy) for the full schema. |
| `targetQueueSize`             | `5`     | Number of pending tasks per concurrent Job KEDA targets. Lower values scale up sooner.                                                                                                                     |
| `activationTargetQueueSize`   | `0`     | Backlog size at which KEDA spawns the first Job from zero. `0` means any backlog wakes the controller; positive values raise the floor.                                                                    |

### `job.*` — fields lifted onto `jobTargetRef.spec`

These are standard Kubernetes `JobSpec` fields, applied to every Job KEDA spawns.

| Field                       | Default | Description                                                                                                                                                                                                                                              |
| --------------------------- | ------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `backoffLimit`              | _unset_ | Number of retries before a Job is considered failed. `0` is recommended for `transcode` so a single activity failure does not respawn the pod and re-attempt the same encode (Temporal's own retry policy handles activity-level retries).                |
| `parallelism`               | _unset_ | Number of pods per Job. Leave unset (defaults to 1) so each Job runs one worker process; KEDA controls horizontal scaling via the `ScaledJob`, not via `parallelism`.                                                                                                                                                          |
| `completions`               | _unset_ | Number of successful pod completions required for the Job to succeed. Same advice as `parallelism` — leave unset.                                                                                                                                       |
| `activeDeadlineSeconds`     | _unset_ | Hard ceiling on Job wall-clock time. Set this to roughly `MEDIA_TRANSCODE_TIMEOUT + drain budget` so a stuck transcode does not pin a pod indefinitely. The example above uses `28800` (8 hours).                                                       |
| `ttlSecondsAfterFinished`   | _unset_ | Seconds completed Jobs are retained before kubelet garbage-collects them. The example above uses `600` (10 minutes); KEDA's `successfulJobsHistoryLimit` independently caps the count.                                                                  |

## KEDA-side Temporal credential

KEDA's Temporal scaler authenticates separately from the worker pods. Configure under `config.temporal.keda.*`:

```yaml
config:
  temporal:
    address: "temporal-frontend.temporal.svc.cluster.local:7233"
    keda:
      apiKey:
        secretKeyRef:
          name: keda-temporal-credentials
          key: apiKey
      tls:
        enabled: true
        caCert:
          secretKeyRef:
            name: temporal-ca
            key: ca.crt
```

The chart emits one per-release `keda.sh/v1alpha1` `TriggerAuthentication` (named `<release>-temporal`) that every scaledjob's triggers reference. It is only emitted when at least one `type: scaledjob` controller exists, so the default-render of the chart is unchanged.

| Field                                        | Default | Description                                                                                                                                                                                                                          |
| -------------------------------------------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `config.temporal.keda.apiKey.value`          | `""`    | Literal API key. The chart materialises a release-scoped Secret (`<release>-keda-temporal-apikey`) containing it; the `TriggerAuthentication` points at that Secret. Intended for development.                                       |
| `config.temporal.keda.apiKey.secretKeyRef`   | `{}`    | Reference to an existing Secret holding the API key. Mutually exclusive with `value`. Required in production.                                                                                                                        |
| `config.temporal.keda.tls.enabled`           | `false` | When `true`, the scaler connects via TLS. Required when the workers' own connection uses TLS — KEDA does not inherit `config.temporal.tls.*`.                                                                                        |
| `config.temporal.keda.tls.serverName`        | `""`    | Override the SNI / cert-verification hostname. Maps to the scaler's `tlsServerName` trigger metadata.                                                                                                                                |
| `config.temporal.keda.tls.disableHostVerification` | `false` | Skip server certificate hostname verification. Maps to the scaler's `unsafeSsl=true` trigger metadata. Development-only.                                                                                                              |
| `config.temporal.keda.tls.clientCertificate.secretName` | `""` | Name of a `kubernetes.io/tls` Secret containing client cert / key for mTLS. Wired into the `TriggerAuthentication`'s `secretTargetRef` as `cert` and `key`.                                                                          |
| `config.temporal.keda.tls.caCert.secretKeyRef` | `{}`  | Free-form reference to a Secret entry holding the trusted CA bundle. Wired into the `TriggerAuthentication`'s `secretTargetRef` as `ca`.                                                                                              |

The chart fails at template time if `config.temporal.keda.apiKey` is empty while at least one enabled `type: scaledjob` controller exists. Sharing credentials with the workers is fine — point `keda.apiKey.secretKeyRef` and `keda.tls.*` at the same Secrets used under `config.temporal.apiKey` and `config.temporal.tls.*`.

## Triggers

For each `type: scaledjob` controller, the chart emits one Temporal trigger per resolved activity token, derived from `workers.<name>.activities`:

- Each activity token (e.g. `transcode`, `probe`) maps to a trigger with `taskQueue: {TEMPORAL_TASK_QUEUE}-{token}` and `queueTypes: activity`.
- The `workflow` token (when present in the resolved activity set) maps to a trigger with `taskQueue: {TEMPORAL_TASK_QUEUE}` and `queueTypes: workflow`.

Every trigger uses the per-release `TriggerAuthentication` and inherits the `keda.targetQueueSize` / `keda.activationTargetQueueSize` configured on the controller. KEDA's effective desired-replica count is the maximum across all of a ScaledJob's triggers, so a multi-activity worker (e.g. `["transcode", "detect-crop"]`) scales when *any* of its queues backs up.

## PodDisruptionBudget

Every `type: scaledjob` controller gets a `PodDisruptionBudget` with `maxUnavailable: 0` by default. This is intentionally aggressive — see [Trade-offs](#trade-offs) for the undrainable-node consequence — and prevents voluntary disruptions (drain, rollout, autoscaler bin-pack) from interrupting an in-flight activity.

Two opt-out forms are accepted:

```yaml
resources:
  controllers:
    transcode:
      type: scaledjob
      podDisruptionBudget: null   # drops the PDB entirely
```

```yaml
resources:
  controllers:
    transcode:
      type: scaledjob
      podDisruptionBudget:
        enabled: false            # equivalent
```

Custom `minAvailable` overrides are honoured, but the chart fails at template time if you set `minAvailable` without explicitly nulling the chart-default `maxUnavailable: 0` (the apiserver rejects PDB specs with both fields set):

```yaml
resources:
  controllers:
    transcode:
      type: scaledjob
      podDisruptionBudget:
        maxUnavailable: null      # clear the chart default
        minAvailable: 1
```

`type: deployment` workers continue to use the pre-existing default (PDB `enabled` when `replicas > 1`, `minAvailable: 1`).

## Idle exit

`WORKER_IDLE_EXIT_AFTER` makes the worker initiate the same drain path as SIGTERM after a configurable wall-clock duration with zero activity- or workflow-task starts and zero in-flight tasks. Without it, KEDA-spawned Jobs would run forever — wasting cluster capacity and defeating the point of scale-to-zero.

The chart sets `WORKER_IDLE_EXIT_AFTER` automatically for every `type: scaledjob` controller:

| Worker activity set            | Default `WORKER_IDLE_EXIT_AFTER` |
| ------------------------------ | -------------------------------- |
| Includes `workflow` (or `all`) | `15m`                            |
| Activity-only                  | `5m`                             |

The longer default for workflow-bearing workers is a hedge against the SDK's sticky workflow cache being thrown away on every idle exit (see [Trade-offs](#trade-offs)).

Override per-controller via `containers.main.env`:

```yaml
resources:
  controllers:
    transcode:
      type: scaledjob
      containers:
        main:
          env:
            WORKER_IDLE_EXIT_AFTER: "10m"
```

Set it to the empty string to disable the feature on a specific scaledjob worker — but doing so means the spawned Jobs run forever, which only makes sense if `keda.maxReplicaCount` is `1` and you want a single always-on Job (in which case prefer `type: deployment`).

For non-scaledjob workers, `WORKER_IDLE_EXIT_AFTER` is unset by default and the worker runs forever (today's behaviour).

The `media_worker_idle_exit_seconds_remaining` Prometheus gauge exposes the current countdown when the feature is enabled. See [docs/metrics.md](metrics.md#worker--transcode-admission-controller).

## Worked example

A complete split-deployment values fragment: one always-on `general` Deployment running the workflow + every cheap activity, and one `transcode` ScaledJob that goes 0..4.

```yaml
config:
  temporal:
    address: "temporal-frontend.temporal.svc.cluster.local:7233"
    namespace: "default"
    taskQueue: "media-processor"
    apiKey:
      secretKeyRef:
        name: media-processor-temporal
        key: apiKey
    keda:
      # KEDA's scaler authenticates separately. Point this at a Secret holding
      # an API key with read-only DescribeTaskQueue permissions.
      apiKey:
        secretKeyRef:
          name: keda-temporal-credentials
          key: apiKey

  worker:
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
    media:
      hardware:
        devicePath: "/dev/dri/renderD128"
        mountHostDevice: true
      transcode:
        timeout: "4h"

workers:
  general:
    activities: ["all", "!transcode"]
  transcode:
    activities: ["transcode"]

resources:
  controllers:
    general:
      replicas: 1
      containers:
        main:
          resources:
            requests:
              cpu: 100m
              memory: 256Mi

    transcode:
      type: scaledjob
      keda:
        maxReplicaCount: 4
        pollingInterval: 30
        successfulJobsHistoryLimit: 3
        failedJobsHistoryLimit: 5
      job:
        # No retries — a Temporal activity failure is already retried by the
        # workflow's retry policy. Letting Kubernetes also retry would
        # multiply attempts without coordinating with Temporal.
        backoffLimit: 0
        # Hard ceiling per Job; pairs with MEDIA_TRANSCODE_TIMEOUT (4h)
        # plus drain budget.
        activeDeadlineSeconds: 28800
        ttlSecondsAfterFinished: 600
      pod:
        # Allow the longest expected transcode plus drain to complete on
        # SIGTERM. See helm.md "Termination and drain".
        terminationGracePeriodSeconds: 21720
      containers:
        main:
          # Run one transcode per pod so KEDA's pod count equals the
          # concurrent transcode count. See "MEDIA_TRANSCODE_LIMITER_STATIC_CAP
          # under KEDA" below.
          env:
            MEDIA_TRANSCODE_LIMITER_STATIC_CAP: "1"
            # Optional: extend the chart default of 5m. Useful when bursts
            # arrive in clusters wider than the polling interval so a pod
            # is not evicted between back-to-back transcodes.
            WORKER_IDLE_EXIT_AFTER: "10m"
          resources:
            limits:
              cpu: 4
              memory: 8Gi
              gpu.intel.com/i915: "1"

  # Make the general worker's pod template the place where the workflow
  # search-attribute / Memo registration is verified — it is the always-on
  # one. (No chart change needed; included here so the values file documents
  # the intent.)
```

`general` keeps `type: deployment` (the default), so it polls continuously and serves cheap activities and the workflow loop without paying KEDA's spawn latency. `transcode` is the only controller that scales to zero.

### `MEDIA_TRANSCODE_LIMITER_STATIC_CAP` under KEDA

The transcode admission controller enforces a per-pod ceiling on concurrent transcodes (default `5`). Under KEDA, the cleaner shape is one transcode per pod:

- Set `MEDIA_TRANSCODE_LIMITER_STATIC_CAP=1`.
- Let `keda.maxReplicaCount` express the cluster-wide concurrency cap.

This way the number of running pods equals the number of in-flight transcodes; KEDA's bin-packing is the only thing deciding admission, and a stuck encode blocks exactly one pod instead of fanning out internally. The chart does not silently override the value — set it explicitly as shown in the example.

## Trade-offs

`type: scaledjob` is opt-in for a reason. The three operationally significant trade-offs are:

### Cold-start latency

Every scale-from-zero pays pod-startup + Go runtime + Temporal dial + (for transcode) load-probe init. For a `transcode` activity that runs for hours, that overhead is a rounding error. For sub-second activities (`probe`, `notify`, `cleanup`, `notify-failure`), startup dominates the activity itself and ScaledJob is the wrong tool — keep those activities on a Deployment-typed worker. The general/transcode split shown in the worked example exists exactly to avoid putting `probe` on a ScaledJob.

### Workflow worker sticky-cache loss

The Temporal Go SDK keeps a per-worker sticky task-queue cache of recently executed workflow tasks. A workflow task that lands on its previous worker replays only the tail of history; one that lands on a fresh worker replays the entire history.

When workflow workers idle-exit, that cache dies with the pod. The next workflow task replays history from scratch on whatever pod KEDA spawns next. For media-processor's workflows the histories are small enough that the replay cost is negligible — but if you fork the codebase into something with long workflow histories, this is the surface area you should watch.

The chart's default `WORKER_IDLE_EXIT_AFTER=15m` for workflow-bearing scaledjob workers softens this by trading off cluster idleness, and the worked example above leaves the workflow on the always-on `general` Deployment so the cache never loses its warmth in the first place.

### Undrainable-node window

With the default `maxUnavailable: 0` PDB, a node hosting an in-flight transcode is undrainable until the activity completes. Cluster-autoscaler bin-pack, node upgrades, and `kubectl drain` all stall on that node.

The escape hatch is the activity's Temporal `HeartbeatTimeout`, which is set to `2 * MEDIA_PROGRESS_LOG_INTERVAL` (default `1m`, configurable per-worker). When a transcode's heartbeat stops landing — which happens quickly if the pod is force-evicted — Temporal fails the activity and the workflow's retry policy re-dispatches it. The PDB never traps a node beyond the heartbeat-timeout window, so worst-case undrainability is roughly twice the configured `MEDIA_PROGRESS_LOG_INTERVAL`.

If your maintenance windows cannot tolerate even that delay (e.g. minute-level node turnover), opt out of the chart-default PDB on the transcode worker. You will trade off the risk of voluntary mid-flight evictions against drain-time predictability — see [PodDisruptionBudget](#poddisruptionbudget) for the opt-out forms.
