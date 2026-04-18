# Hardware acceleration

media-processor supports hardware-accelerated H.265 encoding via three backends. The backend is selected automatically based on which encoders are available at runtime; selection priority is QSV > NVENC > VAAPI. If no hardware encoder is found, the worker falls back to software encoding (libx265).

Hardware acceleration is opt-in: set `MEDIA_HARDWARE_DEVICE_PATH` to the appropriate device node. Leave it empty to use the software encoder.

## Backends

### Intel QSV (oneVPL)

Uses Intel Quick Sync Video via the oneVPL runtime. Supported on Intel 6th-generation (Skylake) and later CPUs, and on Intel Arc GPUs.

**Device path:** typically `/dev/dri/renderD128`

**Prerequisites:**
- Intel media driver (`intel-media-driver` / `iHD`) or legacy VA-API driver (`libva-intel-driver` / `i965`) installed on the host
- oneVPL GPU runtime (`libvpl`) present and linked against the FFmpeg build
- The device node must be accessible to the worker process (add the container user to the `render` group, or set the appropriate device permission in your container runtime)

**Kubernetes example:**

```yaml
env:
  - name: MEDIA_HARDWARE_DEVICE_PATH
    value: /dev/dri/renderD128
resources:
  limits:
    gpu.intel.com/i915: "1"
```

### NVIDIA NVENC

Uses NVIDIA hardware encoding via CUDA. Supported on Kepler-generation (GTX 600/700 series) and later GPUs.

**Device path:** the CUDA device, typically `/dev/nvidia0`

**Prerequisites:**
- NVIDIA driver installed on the host (version 520+ recommended for AV1 support)
- NVIDIA Container Toolkit configured so the GPU is visible inside the container
- FFmpeg built with CUDA/NVENC support (included in the Nix dev shell)

**Kubernetes example (with NVIDIA device plugin):**

```yaml
env:
  - name: MEDIA_HARDWARE_DEVICE_PATH
    value: /dev/nvidia0
resources:
  limits:
    nvidia.com/gpu: "1"
```

### AMD VAAPI

Uses AMD GPU encoding via the VA-API interface. Supported on GCN-generation and later AMD GPUs with the Mesa RADV or AMDGPU-PRO driver.

**Device path:** typically `/dev/dri/renderD128` (same node as QSV on Intel systems; may be `renderD129` or higher if multiple GPUs are present)

**Prerequisites:**
- Mesa RADV driver or AMDGPU-PRO installed on the host
- `libva-mesa-driver` present and accessible
- The device node must be accessible to the worker process

**Kubernetes example:**

```yaml
env:
  - name: MEDIA_HARDWARE_DEVICE_PATH
    value: /dev/dri/renderD128
resources:
  limits:
    amd.com/gpu: "1"
```

## Supported codecs by backend

| Codec | QSV | NVENC | VAAPI | Software |
|-------|-----|-------|-------|----------|
| H.264 | yes | yes | yes | yes (libx264) |
| H.265 | yes | yes | yes | yes (libx265) |
| AV1 | yes | yes | yes | — |
| VP9 | yes | — | yes | — |

The worker currently targets H.265 output for all transcodes.

## Quality tuning

Use `MEDIA_H265_CRF` to control output quality. The meaning of the value varies by encoder:

| Backend | Parameter | Range | Lower = better quality |
|---------|-----------|-------|------------------------|
| libx265 | CRF | 1–51 | yes |
| NVENC (hevc_nvenc) | CQ | 1–51 | yes |
| QSV (hevc_qsv) | global_quality (ICQ) | 1–51 | yes |
| VAAPI (hevc_vaapi) | global_quality | 1–51 | yes |

`MEDIA_H265_CRF=0` (the default) leaves the value unset and lets each encoder use its own built-in default.

## Checking encoder availability

To verify which hardware encoders are detected at runtime, check the worker logs at `debug` level (`LOG_LEVEL=debug`). The encoder selection step logs which profile was chosen and why.
