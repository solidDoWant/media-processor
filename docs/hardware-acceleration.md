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
