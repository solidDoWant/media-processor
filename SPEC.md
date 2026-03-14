# media-processor Specification

## Tech Stack

| Component | Technology |
|-----------|------------|
| Language | Go 1.24 |
| Workflow orchestration | [Hatchet](https://hatchet.run) |
| Database | PostgreSQL |
| Filesystem watching | `fsnotify` |
| Static analysis | `golangci-lint` |
| Dev environment | Nix (flake.nix) |

## Binary Responsibilities

### `cmd/watcher`

Watches configured filesystem paths for media file events using `fsnotify`. When a qualifying file event is detected, it maps the event to a workflow via YAML configuration and submits the corresponding job to Hatchet for processing.

Responsibilities:
- Load and parse the watcher YAML config at startup
- Register `fsnotify` watchers for all configured paths
- On file event, look up the matching workflow mapping and submit a Hatchet job
- Reconnect and retry on transient failures

### `cmd/worker`

Registers workflow step handlers with Hatchet and processes jobs dispatched by the watcher. Performs the actual media processing by orchestrating calls into the library packages.

Responsibilities:
- Connect to Hatchet and register all workflow step handlers
- Invoke `pkg/ffprobe` to inspect incoming media files
- Invoke `pkg/ffmpeg` via `pkg/medialib` to transcode or transform media
- Report step results and errors back to the workflow engine

## Library Package Contracts

### `pkg/ffmpeg`

Wraps invocation of the `ffmpeg` command-line tool.

- Accepts an arbitrary argument list and an optional working directory
- Returns combined stdout/stderr output and exit code
- Does not interpret output — callers are responsible for parsing

### `pkg/ffprobe`

Wraps invocation of the `ffprobe` command-line tool for media file inspection.

- Accepts a file path and returns structured metadata (codec, duration, streams, etc.)
- Output is parsed from ffprobe's JSON mode

### `pkg/medialib`

Provides higher-level business logic over `pkg/ffmpeg` and `pkg/ffprobe`.

- Encapsulates transcoding decisions (codec selection, resolution, bitrate)
- Exposes operations such as `Transcode`, `ExtractAudio`, `GenerateThumbnail`
- Callers do not interact with ffmpeg/ffprobe directly

### `pkg/webhook`

Provides HTTP handler utilities for inbound webhook events.

- Minimal interface for receiving and dispatching webhook payloads
- Decoupled from any specific webhook source

## Configuration

| Config item | Mechanism | Rationale |
|-------------|-----------|-----------|
| Watcher path-to-workflow mappings | YAML file (path passed via `--config` flag) | Structured, multi-key data that is unwieldy as environment variables |
| All other runtime config (DB URL, Hatchet endpoint, secrets, log level) | Environment variables | Standard 12-factor practice; integrates cleanly with Kubernetes secrets |

No config file merging is performed. Exactly one YAML config file path is accepted; all other configuration comes from the process environment.

### Watcher config example

```yaml
mappings:
  - path: /media/incoming/movies
    workflow: transcode-movie
  - path: /media/incoming/tv
    workflow: transcode-tv-episode
```
