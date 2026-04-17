# media-processor Specification

## Tech Stack

| Component | Technology |
|-----------|------------|
| Language | Go 1.24 |
| Workflow orchestration | [Hatchet](https://hatchet.run) |
| Database | PostgreSQL |
| Filesystem watching | `fsnotify` |
| Media processing | `github.com/asticode/go-astiav` (FFmpeg 8 Cgo bindings) |
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

Wraps `libavcodec`, `libavformat`, and related libraries via `github.com/asticode/go-astiav` for in-process media transcoding.

- Exposes a builder API: `NewTranscode(in, out).VideoCodec(...).AudioCodec(...).Container(...).HardwareAccel(...).Build().Run(ctx)`
- Hardware encoder availability is determined in-process via `astiav.FindEncoderByName` — no subprocess probe
- No external binary required; CGO must be enabled and FFmpeg 8 shared libraries must be present at runtime

### `pkg/ffprobe`

Wraps `libavformat` and `libavcodec` via `github.com/asticode/go-astiav` for in-process media file inspection.

- Accepts a file path and context; returns structured `MediaInfo` (container format, duration, overall bitrate, streams)
- No external binary required; context cancellation is honoured via `astiav.IOInterrupter`

### `pkg/medialib`

Provides higher-level business logic over `pkg/ffmpeg` and `pkg/ffprobe`.

- Encapsulates transcoding decisions (codec selection, resolution, bitrate)
- Exposes operations such as `Transcode`, `ExtractAudio`, `GenerateThumbnail`
- Callers do not interact with ffmpeg/ffprobe directly

### `pkg/webhook`

Provides HTTP handler utilities for inbound webhook events.

- Minimal interface for receiving and dispatching webhook payloads
- Decoupled from any specific webhook source

## Sonarr/Radarr Integration Flow

This project is designed to sit transparently between a download client and Sonarr/Radarr, transcoding or transmuxing media before it is imported into the library.

### Directory layout

| Path | Visible to |
|------|------------|
| `/downloads` | Download client, watcher/worker |
| `/processed-output` | watcher/worker |
| `/downloads` (bind-mounted from `/processed-output`) | Sonarr/Radarr |

Sonarr/Radarr has `/processed-output` bind-mounted as its own `/downloads`. This means Sonarr/Radarr cannot see files in the real `/downloads` directory — it only sees files that have been placed in `/processed-output`. This is the mechanism that prevents premature import of unprocessed files.

### End-to-end flow

1. **User requests media.** The user requests a movie or TV episode via Sonarr or Radarr.
2. **Download initiated.** Sonarr/Radarr searches configured indexers, selects a release, and sends it to the configured download client.
3. **File lands in `/downloads`.** The download client saves the completed file to the real `/downloads` directory and reports the file path back to Sonarr/Radarr.
4. **Watcher detects the file.** The `cmd/watcher` process, which watches `/downloads` via `fsnotify`, detects the new file and submits a media-processor workflow job to Hatchet.
5. **Workflow runs.** The `cmd/worker` process picks up the job. It probes the file with `pkg/ffprobe`, then transcodes or transmuxes it if required via `pkg/ffmpeg`/`pkg/medialib`, writing the output to `/processed-output`. When `MEDIA_WATCHER_ROOT` is set to `/downloads`, the worker mirrors the input's relative subdirectory under `/processed-output` (e.g., `/downloads/my-media-item/video.mp4` produces `/processed-output/my-media-item/video.mkv`). **`MEDIA_WATCHER_ROOT` must be configured** for nested downloads to produce matching subdirectory structure; without it, all output is written flat into `/processed-output` and the import path in step 6 will not resolve correctly for nested inputs.
6. **Library import triggered.** The workflow's `notify` step calls `ArrLibrary.ImportByFilePath` with a path derived from the original input file path: the same directory and stem as the downloaded file, but with the extension replaced by `.mkv` to match the transcoded output (e.g., if the input was `/downloads/my-media-item/video.mp4`, the import path is `/downloads/my-media-item/video.mkv`). The path is translated if necessary via `LocalPathPrefix`/`RemotePathPrefix` to produce the path as Sonarr/Radarr sees it (e.g., `/downloads/my-media-item/video.mkv`), then a `DownloadedMoviesScan` (Radarr) or `DownloadedEpisodesScan` (Sonarr) command is sent with that path. The arr service scans the file at that path (which resolves to the transcoded output via the bind mount) and triggers the normal import pipeline.
7. **Sonarr/Radarr imports the file.** On receiving the refresh command, Sonarr/Radarr scans its `/downloads` path (which resolves to `/processed-output` on the host) and finds the processed file, then imports it into the library.

### Why the bind-mount is necessary

The download client reports the file location to Sonarr/Radarr using the path as it sees it (`/downloads/…`). Without intervention, Sonarr/Radarr would attempt to import the original unprocessed file as soon as the download completes. By bind-mounting `/processed-output` over Sonarr/Radarr's `/downloads`, the processed output directory and the download client's report path are made to coincide — so Sonarr/Radarr will not find (and therefore not import) any file until the workflow has finished processing it and written the result to `/processed-output`.

## Configuration

| Config item | Mechanism | Rationale |
|-------------|-----------|-----------|
| Watcher path-to-workflow mappings | YAML file (path passed via `--config` flag) | Structured, multi-key data that is unwieldy as environment variables |
| All other runtime config (DB URL, Hatchet endpoint, secrets, log level) | Environment variables | Standard 12-factor practice; integrates cleanly with Kubernetes secrets |

No config file merging is performed. Exactly one YAML config file path is accepted; all other configuration comes from the process environment.

### Watcher config example

```yaml
cronSchedule: "*/5 * * * * *"
watches:
  - name: movies
    path: /media/incoming/movies
    mediaType: movie
    ignorePatterns:
      - \.!qB$
      - (^|/)_unpack(/|$)
  - name: shows
    path: /media/incoming/tv
    mediaType: show
  - name: archive
    path: /media/incoming/archive
    mediaType: movie
    preserveSource: true
```

`cronSchedule`, `ignorePatterns`, and `preserveSource` are all optional. A minimal entry only needs `name`, `path`, and `mediaType`. `ignorePatterns` accepts Go regular expressions; a matching file is silently skipped, a matching directory skips its entire subtree. When `preserveSource: true` is set on a watch entry, the source file is kept after successful processing; omitting it or setting it to `false` retains the default behaviour of deleting the source file.
