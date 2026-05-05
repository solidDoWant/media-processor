# Hardware acceleration

media-processor supports hardware-accelerated H.265 encoding via two backends. The backend is selected automatically based on which encoders are available at runtime; selection priority is QSV > VAAPI. If no hardware encoder is found, the worker falls back to software encoding (libx265).

## Device selection

On startup, transcode-enabled workers resolve the device path used for hardware-accelerated encoding in this order:

1. **Operator override** — when `MEDIA_HARDWARE_DEVICE_PATH` is set, the worker uses that path verbatim. The path is validated as a character device at startup so a typo (e.g. `/dev/dri/render128` missing the `D`) fails fast rather than on the first transcode. The override is **not** restricted to Intel devices, so operators may point it at non-i915 hardware that future backends will support.
2. **Auto-detection** — when the override is unset, the worker scans `/sys/class/drm/` for render nodes whose backing kernel driver is `i915` and uses the lowest-numbered match (e.g. `/dev/dri/renderD128`).
3. **Software-only** — when neither yields a path, the worker logs `no Intel GPU detected — software-only mode` and the transcode activity uses the software encoder (libx265).

Auto-detection runs only on workers that have the `transcode` activity enabled (see `WORKER_ACTIVITIES`); workers that don't transcode skip detection entirely.

The chosen path is logged at startup with a `source` field of either `override` or `auto-detected`, so operators can confirm at a glance which branch the worker took.

## Backends

### Intel QSV (oneVPL)

Uses Intel Quick Sync Video via the oneVPL runtime. Supported on Intel 6th-generation (Skylake) and later CPUs, and on Intel Arc GPUs.

**Device path:** typically `/dev/dri/renderD128`

**Prerequisites:**
- Intel media driver (`intel-media-driver` / `iHD`) installed on the host for Gen 9+ GPUs, or the legacy VA-API driver (`libva-intel-driver` / `i965`) for older hardware
- The device node must be accessible to the worker (add the container user to the `render` group, or set the appropriate device permission in your container runtime)

### VAAPI

Uses the VA-API interface for hardware encoding on Linux. On Intel systems this is the same kernel device node as QSV; the encoder backend is selected automatically based on what FFmpeg exposes.

**Device path:** typically `/dev/dri/renderD128` (same node as QSV on Intel systems; may be `renderD129` or higher if multiple GPUs are present)

**Prerequisites:**
- A VA-API driver installed on the host (e.g. Mesa Gallium `mesa-va-drivers` / `libva-mesa-driver`)
- The device node must be accessible to the worker

## Supported codecs by backend

The worker currently targets H.265 output for all transcodes, so only the H.265 row drives encoder selection. The other rows reflect what each backend is capable of.

| Codec | QSV | VAAPI | Software      |
| ----- | --- | ----- | ------------- |
| H.264 | yes | yes   | yes (libx264) |
| H.265 | yes | yes   | yes (libx265) |
| AV1   | yes | yes   | —             |
| VP9   | yes | yes   | —             |

## Quality tuning

Use `MEDIA_H265_CRF` to control output quality. The meaning of the value varies by encoder:

| Backend            | Parameter            | Range | Lower = better quality |
| ------------------ | -------------------- | ----- | ---------------------- |
| libx265            | CRF                  | 1–51  | yes                    |
| QSV (hevc_qsv)     | global_quality (ICQ) | 1–51  | yes                    |
| VAAPI (hevc_vaapi) | global_quality       | 1–51  | yes                    |

Leaving `MEDIA_H265_CRF` unset (the default) lets each encoder use its own built-in default. Valid explicit values are `1`–`51`; setting `MEDIA_H265_CRF=0` (or any other out-of-range value) is rejected at startup.

## Checking encoder availability

To verify which hardware encoders are detected at runtime, check the worker logs at `debug` level (`LOG_LEVEL=debug`). The worker logs which encoder profile was chosen and why.

## Load probe permissions

Transcode-enabled workers can sample worker load to gate concurrency. Two probe implementations exist:

### Intel i915 GPU probe

Reads per-engine VCS busy counters from the i915 PMU via `perf_event_open`. The probe binds to the same i915 device the worker transcodes against — on hosts with multiple Intel GPUs the matching per-device PMU is selected automatically by mapping the transcode device path through its PCI bus address (see Device selection above) to the kernel-registered PMU. Single-GPU hosts use the legacy bare `i915` PMU; multi-GPU hosts use BDF-suffixed names like `i915_0000_03_00.0`.

**Permission requirements** — the worker process must satisfy at least one of:

- Hold `CAP_PERFMON` (preferred for containerized deployments — grant via the container runtime's capabilities mechanism).
- Run on a host with `kernel.perf_event_paranoid` ≤ 1.

If neither holds, `perf_event_open` returns `EACCES` at probe initialization and the worker raises a fallback signal (logged with the underlying error). The supplier consuming the probe is responsible for falling back to its static concurrency cap.

### Container CPU probe (cgroup v2)

Used in workers running in software-only mode. Reads `cpu.stat` and `cpu.max` from the unified cgroup v2 hierarchy at `/sys/fs/cgroup` and reports utilization relative to the container's CPU bandwidth quota.

**Requirements:**

- Cgroup v2 must be mounted (the unified hierarchy — most modern container runtimes provide this by default).
- The container must have a CPU quota set (`cpu.max` cannot be `max <period>`). When the quota is unset, the probe initialization fails so the supplier falls back to its static cap; pin a CPU quota in your container runtime / Kubernetes resource limits to keep the load probe active.

The fallback behavior — what the supplier does when a probe is unavailable or fails — is described in [Static-cap fallback](#static-cap-fallback) below.

## Static-cap fallback

When the load probe cannot initialize (missing capability, missing kernel feature, unconstrained cgroup) or fails mid-stream (`perf_event_open` returns an error after start, the device disappears, etc.), the transcode supplier falls back to a static cap and stops consulting the probe. The static cap is `MEDIA_TRANSCODE_LIMITER_STATIC_CAP` (default `5`); reservation requests are admitted as long as the in-flight count is below the cap, and blocked otherwise.

The transition is observable through metrics: `media_worker_transcode_admission_mode{mode="probe"}` flips from `1` to `0` and `mode="static"` flips from `0` to `1` within one sample interval of the probe failure. The underlying error is logged once at `warn`. See [metrics.md](metrics.md#worker--transcode-admission-controller) for the full per-pod metric set and [configuration.md](configuration.md#transcode-admission-controller) for the limiter knobs.

A failed probe is **not** a fatal startup error — the worker boots in static-cap-only mode rather than refusing to start. This means a worker without the right capabilities (or running in a host with cgroup v1, no CPU quota, etc.) still admits transcodes up to the configured cap; admission control just stops being load-aware.

End-to-end admission control with the i915 probe can be exercised on a GPU-equipped runner via `make test-integration-gpu`, which adds the `gpu` build tag to the integration test suite. The default `make test-integration` does not compile the GPU-dependent tests so it can run on any host.
