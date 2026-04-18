# Hardware acceleration

media-processor supports hardware-accelerated H.265 encoding via three backends. The backend is selected automatically based on which encoders are available at runtime; selection priority is QSV > NVENC > VAAPI. If no hardware encoder is found, the worker falls back to software encoding (libx265).

Hardware acceleration is **auto-enabled** when the appropriate encoder is available in the FFmpeg shared libraries the worker uses. No configuration is required to activate it — set `MEDIA_HARDWARE_DEVICE_PATH` only when you need to target a specific device (e.g. when multiple GPUs are present); when left empty a device is selected automatically.

Note that the `hardware_accelerated` metric label is derived from whether `MEDIA_HARDWARE_DEVICE_PATH` is set, not from whether a hardware encoder was actually selected at runtime. Set the device path if you want the label to reflect hardware use.

## Backends

### Intel QSV (oneVPL)

Uses Intel Quick Sync Video via the oneVPL runtime. Supported on Intel 6th-generation (Skylake) and later CPUs, and on Intel Arc GPUs.

**Device path:** typically `/dev/dri/renderD128`

**Prerequisites:**
- Intel media driver (`intel-media-driver` / `iHD`) installed on the host for Gen 9+ GPUs, or the legacy VA-API driver (`libva-intel-driver` / `i965`) for older hardware
- The device node must be accessible to the worker (add the container user to the `render` group, or set the appropriate device permission in your container runtime)

### NVIDIA NVENC

Uses NVIDIA hardware encoding via CUDA. HEVC (H.265) encoding requires Maxwell 2nd-generation (GM20x, e.g. GTX 950/960/970/980) or later; earlier NVENC silicon (Kepler and Maxwell 1st-gen) is H.264-only and cannot be used by this project.

**Device value:** a CUDA device ordinal as a decimal string, e.g. `"0"` for the first GPU or `"1"` for the second. `/dev/nvidia*` device-node paths are **not** accepted here. Leave `MEDIA_HARDWARE_DEVICE_PATH` unset to let the worker pick the default device.

**Prerequisites:**
- NVIDIA driver installed on the host (version 520+ recommended for AV1 support, which additionally requires Ada Lovelace hardware)
- NVIDIA Container Toolkit configured so the GPU is visible inside the container

### VAAPI

Uses the VA-API interface for hardware encoding on Linux. This backend is not AMD-specific and can be used with multiple vendors; for AMD, it is supported on GCN-generation and later GPUs with Mesa's Gallium VA-API driver (radeonsi) or the AMDGPU-PRO VA-API driver.

**Device path:** typically `/dev/dri/renderD128` (same node as QSV on Intel systems; may be `renderD129` or higher if multiple GPUs are present)

**Prerequisites:**
- Mesa Gallium VA-API driver (`mesa-va-drivers` / `libva-mesa-driver`) or AMDGPU-PRO's VA-API driver installed on the host
- The device node must be accessible to the worker

## Supported codecs by backend

The worker currently targets H.265 output for all transcodes, so only the H.265 row drives encoder selection. The other rows reflect what each backend is capable of.

| Codec | QSV | NVENC | VAAPI | Software      |
| ----- | --- | ----- | ----- | ------------- |
| H.264 | yes | yes   | yes   | yes (libx264) |
| H.265 | yes | yes   | yes   | yes (libx265) |
| AV1   | yes | yes   | yes   | —             |
| VP9   | yes | —     | yes   | —             |

## Quality tuning

Use `MEDIA_H265_CRF` to control output quality. The meaning of the value varies by encoder:

| Backend            | Parameter            | Range | Lower = better quality |
| ------------------ | -------------------- | ----- | ---------------------- |
| libx265            | CRF                  | 1–51  | yes                    |
| NVENC (hevc_nvenc) | CQ                   | 1–51  | yes                    |
| QSV (hevc_qsv)     | global_quality (ICQ) | 1–51  | yes                    |
| VAAPI (hevc_vaapi) | global_quality       | 1–51  | yes                    |

Leaving `MEDIA_H265_CRF` unset (the default) lets each encoder use its own built-in default. Valid explicit values are `1`–`51`; setting `MEDIA_H265_CRF=0` (or any other out-of-range value) is rejected at startup.

## Checking encoder availability

To verify which hardware encoders are detected at runtime, check the worker logs at `debug` level (`LOG_LEVEL=debug`). The worker logs which encoder profile was chosen and why.
