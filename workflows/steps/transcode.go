package steps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
	"github.com/solidDoWant/media-processor/pkg/ffprobe"
	"github.com/solidDoWant/media-processor/pkg/medialib"
)

// SelectVideoCodec returns CodecCopy when the video is already H.264 or H.265 in an
// MKV container, and CodecH265 otherwise.
func SelectVideoCodec(videoCodecName, format string) ffmpeg.Codec {
	if strings.Contains(format, string(ffmpeg.ContainerMKV)) {
		if videoCodecName == ffprobe.CodecNameH264 || videoCodecName == ffprobe.CodecNameH265 {
			return ffmpeg.CodecCopy
		}
	}

	return ffmpeg.CodecH265
}

// nonEnglishAudioIndices returns the input stream indices of non-English audio
// streams. If at least one stream carries the "eng" language tag, all streams
// without that tag are returned so they can be excluded from the output.
// If no stream is tagged "eng" (including the case where no streams carry any
// language tag), nil is returned so all streams are preserved as a safe fallback.
func nonEnglishAudioIndices(streams []AudioStreamInfo) []int {
	hasEnglish := false

	for _, s := range streams {
		if s.Language == "eng" {
			hasEnglish = true
			break
		}
	}

	if !hasEnglish {
		return nil
	}

	var exclude []int

	for _, s := range streams {
		if s.Language != "eng" {
			exclude = append(exclude, s.Index)
		}
	}

	return exclude
}

// downmixSourceIndex returns a pointer to the input stream Index of the first
// AudioStreamInfo element with EffectiveChannelCount >= 4, when no retained audio
// stream has EffectiveChannelCount <= 3 (stereo-compatible). Returns nil if any
// stream is stereo-compatible, if no surround stream exists, or if the slice is
// empty. Streams with unknown layouts have EffectiveChannelCount set to a
// conservative surround value by RunProbe, so they are treated as surround
// candidates without blocking or triggering synthesis incorrectly.
func downmixSourceIndex(streams []AudioStreamInfo) *int {
	var firstSurround *int

	for _, s := range streams {
		if s.EffectiveChannelCount > 0 && s.EffectiveChannelCount <= 3 {
			return nil
		}

		if s.EffectiveChannelCount >= 4 && firstSurround == nil {
			idx := s.Index
			firstSurround = &idx
		}
	}

	return firstSurround
}

// nonEnglishSubtitleIndices returns the input stream indices of all subtitle
// streams not tagged "eng", including untagged streams. Unlike audio, there is
// no safe-fallback: subtitles are always excluded unless explicitly tagged "eng".
func nonEnglishSubtitleIndices(streams []StreamInfo) []int {
	var exclude []int

	for _, s := range streams {
		if s.Language != "eng" {
			exclude = append(exclude, s.Index)
		}
	}

	return exclude
}

// firstEnglishIndex returns a pointer to the input stream Index of the first
// StreamInfo element tagged "eng", or nil when no English stream is found.
func firstEnglishIndex(streams []StreamInfo) *int {
	for _, stream := range streams {
		if stream.Language == "eng" {
			return &stream.Index
		}
	}

	return nil
}

// TranscodeOutput is the output of a successful RunTranscode call.
type TranscodeOutput struct {
	// DestCodec is the video codec written to the output file (e.g. "hevc", "copy").
	DestCodec string `json:"dest_codec"`
	// DestContainer is the container format of the output file (always "mkv").
	DestContainer string `json:"dest_container"`
	// DestFilePath is the path of the output file.
	DestFilePath string `json:"dest_file_path"`
	// SourceFileSizeBytes is the size of the input file in bytes, measured before transcoding.
	SourceFileSizeBytes int64 `json:"source_file_size_bytes"`
	// DestFileSizeBytes is the size of the output file in bytes, measured after transcoding.
	DestFileSizeBytes int64 `json:"dest_file_size_bytes"`
	// TranscodeDurationSeconds is the wall-clock time spent in RunTranscode (the ffmpeg call
	// plus surrounding stat/rename operations), in seconds.
	TranscodeDurationSeconds float64 `json:"transcode_duration_seconds"`
	// ArtworkFetchSkipped is true when artwork fetch was attempted but yielded no
	// embeddable image (library unreachable, no poster available, or unsupported type).
	ArtworkFetchSkipped bool `json:"artwork_fetch_skipped,omitempty"`
	// CropApplied is true when a crop filter was applied during transcoding.
	CropApplied bool `json:"crop_applied,omitempty"`
	// HardwareAccelerated is true when at least one video stream was encoded
	// using a hardware encoder (e.g. NVENC, QSV, VAAPI) at runtime. False when
	// the encoder fell back to software (e.g. libx265) even if
	// MEDIA_HARDWARE_DEVICE_PATH is set.
	HardwareAccelerated bool `json:"hardware_accelerated,omitempty"`
}

// codecName returns a human-readable name for a codec, matching the names used
// by ffprobe (e.g. "hevc", "copy").
func codecName(c ffmpeg.Codec) string {
	switch c {
	case ffmpeg.CodecCopy:
		return "copy"
	case ffmpeg.CodecH265:
		return "hevc"
	default:
		return c.String()
	}
}

// RunTranscode transcodes filePath into outputDir, writing to a temp file named
// "._<stem>.mkv.tmp" and atomically renaming it to "<stem>.mkv" on success.
// The output always carries a .mkv extension to match the forced MKV container.
// Writing directly to the output directory avoids a cross-filesystem copy and
// guarantees the rename is atomic on Linux (same directory).
// probe is the output of RunProbe for filePath.
// cropParams is the crop region to apply, or nil for no cropping.
// When cropParams is non-nil and videoCodec would be CodecCopy, the codec is
// promoted to CodecH265 so the crop filter can be applied.
// watcherRoot is the root directory that the watcher monitors. When non-empty,
// the subdirectory of filePath relative to watcherRoot is mirrored under outputDir,
// preserving the download client's directory structure. When empty, the output is
// written flat into outputDir (no subdirectory).
// hardwareDevicePath is the device path passed to CreateHardwareDeviceContext;
// an empty string uses the libav default (auto-select).
// h265CRF is the constant-quality value for H.265 encoders. 0 means use the
// encoder's built-in default; valid explicit values are 1–51 (lower = higher
// quality). For libx265 this is the CRF; for hevc_nvenc it is the CQ value;
// for hevc_qsv and hevc_vaapi it is the global_quality (ICQ) value. Values
// outside 1–51 are silently ignored and the encoder default is used.
// progressLogInterval controls how often a progress log line is emitted during
// transcoding. Zero disables progress logging.
// library is the arr library used to fetch poster artwork. When nil, no fetch
// is attempted and transcoding proceeds without an embedded attachment, and
// ArtworkFetchSkipped is not set. When non-nil and the fetch yields no
// embeddable image, transcoding proceeds without an embedded attachment and
// ArtworkFetchSkipped is set to true.
func RunTranscode(ctx context.Context, filePath string, probe ProbeOutput, cropParams *ffmpeg.CropParams, outputDir string, watcherRoot string, hardwareDevicePath string, h265CRF int, progressLogInterval time.Duration, library medialib.ArrLibrary) (TranscodeOutput, error) {
	transcodeStart := time.Now()

	srcInfo, err := os.Stat(filePath)
	if err != nil {
		return TranscodeOutput{}, fmt.Errorf("stat source file: %w", err)
	}

	srcSize := srcInfo.Size()

	videoCodec := SelectVideoCodec(probe.VideoCodec, probe.Format)

	// Crop requires re-encoding: promote CodecCopy to CodecH265 so the crop
	// filter can be applied.
	if cropParams != nil && videoCodec == ffmpeg.CodecCopy {
		videoCodec = ffmpeg.CodecH265
	}

	slog.DebugContext(ctx, "video codec selected",
		slog.String("video_codec", codecName(videoCodec)),
		slog.Bool("hardware_device_configured", hardwareDevicePath != ""),
	)

	audioExclude := nonEnglishAudioIndices(probe.AudioStreams)
	excludeIndices := append(audioExclude, nonEnglishSubtitleIndices(probe.SubtitleStreams)...)

	// Compute retained audio streams to decide whether a downmix is needed.
	excludeSet := make(map[int]bool, len(audioExclude))
	for _, idx := range audioExclude {
		excludeSet[idx] = true
	}

	retainedAudio := make([]AudioStreamInfo, 0, len(probe.AudioStreams))
	for _, s := range probe.AudioStreams {
		if !excludeSet[s.Index] {
			retainedAudio = append(retainedAudio, s)
		}
	}

	// Extract base StreamInfo slice for generic functions (firstEnglishIndex).
	audioBaseStreams := make([]StreamInfo, len(probe.AudioStreams))
	for i, s := range probe.AudioStreams {
		audioBaseStreams[i] = s.StreamInfo
	}

	// Compute the effective output directory. When watcherRoot is set, mirror the
	// relative subdirectory of filePath under outputDir so that downloads placed in
	// subdirectories (e.g. /downloads/my-show/ep.mkv) produce output under the
	// equivalent subdirectory (e.g. /processed-output/my-show/ep.mkv).
	effectiveOutputDir := outputDir

	if watcherRoot != "" {
		absWatcherRoot, absErr := filepath.Abs(watcherRoot)
		if absErr != nil {
			return TranscodeOutput{}, fmt.Errorf("compute absolute watcher root: %w", absErr)
		}

		absFileDir, absErr := filepath.Abs(filepath.Dir(filePath))
		if absErr != nil {
			return TranscodeOutput{}, fmt.Errorf("compute absolute file directory: %w", absErr)
		}

		relDir, relErr := filepath.Rel(absWatcherRoot, absFileDir)
		if relErr != nil {
			return TranscodeOutput{}, fmt.Errorf("compute relative output subdir: %w", relErr)
		}

		// Prevent directory traversal: reject any relDir that is absolute or escapes
		// watcherRoot via ".." components (e.g. filePath outside watcherRoot).
		if filepath.IsAbs(relDir) || relDir == ".." || strings.HasPrefix(relDir, ".."+string(os.PathSeparator)) {
			return TranscodeOutput{}, fmt.Errorf(
				"refusing to derive output subdir outside watcher root (filePath=%q, watcherRoot=%q, outputDir=%q, relDir=%q)",
				filePath, watcherRoot, outputDir, relDir,
			)
		}

		absOutputDir, absErr := filepath.Abs(outputDir)
		if absErr != nil {
			return TranscodeOutput{}, fmt.Errorf("compute absolute outputDir: %w", absErr)
		}

		candidateDir := filepath.Clean(filepath.Join(absOutputDir, relDir))

		// Double-check: ensure the resulting directory is still within outputDir.
		relToRoot, relErr := filepath.Rel(absOutputDir, candidateDir)
		if relErr != nil {
			return TranscodeOutput{}, fmt.Errorf("verify effective output subdir: %w", relErr)
		}

		if relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(os.PathSeparator)) || filepath.IsAbs(relToRoot) {
			return TranscodeOutput{}, fmt.Errorf("refusing to write outside outputDir: %q", candidateDir)
		}

		effectiveOutputDir = candidateDir
	}

	if mkErr := os.MkdirAll(effectiveOutputDir, 0o755); mkErr != nil {
		return TranscodeOutput{}, fmt.Errorf("create output dir: %w", mkErr)
	}

	inputBase := filepath.Base(filePath)
	mkvBase := strings.TrimSuffix(inputBase, filepath.Ext(inputBase)) + ".mkv"
	tempPath := filepath.Join(effectiveOutputDir, "._"+mkvBase+".tmp")
	finalPath := filepath.Join(effectiveOutputDir, mkvBase)

	// Build per-stream title map for retained audio streams. Language is only
	// included when streams span multiple distinct languages; it adds no useful
	// information when every track shares the same language (or all are "und").
	needsLang := disambiguateLang(retainedAudio)
	audioTitles := make(map[int]string, len(retainedAudio))

	for _, s := range retainedAudio {
		var langName string
		if needsLang {
			langName = audioLangName(s.Language)
		}

		if s.ReportedChannelCount == 0 {
			if langName != "" {
				audioTitles[s.Index] = langName
			}

			continue
		}

		label := channelConfigLabel(s.ReportedChannelCount, s.HasLFE)
		audioTitles[s.Index] = buildAudioStreamTitle(s.Title, langName, label)
	}

	// Build per-stream title map for retained subtitle streams.
	subtitleExclude := nonEnglishSubtitleIndices(probe.SubtitleStreams)
	subtitleExcludeSet := make(map[int]bool, len(subtitleExclude))

	for _, idx := range subtitleExclude {
		subtitleExcludeSet[idx] = true
	}

	subtitleTitles := make(map[int]string, len(probe.SubtitleStreams))
	for _, s := range probe.SubtitleStreams {
		if subtitleExcludeSet[s.Index] {
			continue
		}

		title := buildSubtitleStreamTitle(s.Title, iso639Name(s.Language))
		if title != "" {
			subtitleTitles[s.Index] = title
		}
	}

	// Resolve the language name for the downmix source stream so the downmix
	// title can be prefixed with the human-readable language name.
	downmixSrcIdx := downmixSourceIndex(retainedAudio)

	var downmixLangName string

	if downmixSrcIdx != nil {
		for _, s := range retainedAudio {
			if s.Index == *downmixSrcIdx {
				if needsLang {
					downmixLangName = audioLangName(s.Language)
				}

				break
			}
		}
	}

	var progressCh chan ffmpeg.Progress

	var transcodeSucceeded bool

	if progressLogInterval > 0 {
		progressCh = make(chan ffmpeg.Progress, 64)

		done := make(chan struct{})
		defer close(done)

		go func() {
			ticker := time.NewTicker(progressLogInterval)
			defer ticker.Stop()

			var latest ffmpeg.Progress

			hasUpdate := false
			lastLogFrames := int64(0)

			var lastLogTime time.Time

			logProgress := func() {
				now := time.Now()

				var fps float64

				if !lastLogTime.IsZero() {
					interval := now.Sub(lastLogTime)

					if interval > 0 {
						fps = float64(latest.FramesProcessed-lastLogFrames) / interval.Seconds()
					}
				}

				lastLogFrames = latest.FramesProcessed
				lastLogTime = now

				slog.InfoContext(ctx, "transcode progress",
					slog.Float64("percent_complete", latest.PercentComplete),
					slog.Duration("elapsed", time.Since(transcodeStart)),
					slog.Int64("frames_processed", latest.FramesProcessed),
					slog.Float64("fps", fps),
				)
			}

			for {
				select {
				case p := <-progressCh:
					if !hasUpdate {
						lastLogTime = time.Now()
					}

					latest = p
					hasUpdate = true
				case <-ticker.C:
					if hasUpdate {
						logProgress()
					}
				case <-done:
					if transcodeSucceeded {
						latest.PercentComplete = 100
						logProgress()
					}

					return
				}
			}
		}()
	}

	var (
		artworkSkipped bool
		artBytes       []byte
		artMime        string
	)

	if library != nil {
		var artErr error

		artBytes, artMime, artErr = library.GetPosterImage(ctx, filePath)
		if artErr != nil || len(artBytes) == 0 {
			if artErr != nil {
				slog.WarnContext(ctx, "artwork fetch failed, proceeding without cover art", "error", artErr)
			} else {
				slog.WarnContext(ctx, "no poster image available, proceeding without cover art")
			}

			artworkSkipped = true
		}
	}

	transcoder := ffmpeg.NewTranscode(filePath, tempPath).
		ToVideoCodec(videoCodec).
		ToContainer(ffmpeg.ContainerMKV).
		HardwareAccel(ffmpeg.HWAccelAuto).
		ExcludeStreams(excludeIndices...).
		WithDefaultAudioStream(firstEnglishIndex(audioBaseStreams)).
		WithDefaultSubtitleStream(firstEnglishIndex(probe.SubtitleStreams)).
		WithDownmix(downmixSrcIdx).
		WithAudioStreamTitles(audioTitles).
		WithSubtitleStreamTitles(subtitleTitles).
		WithDownmixTitle(downmixLangName).
		WithAutoDownmixTitle().
		WithHardwareDevice(hardwareDevicePath).
		WithH265CRF(h265CRF).
		WithCoverArt(artBytes, artMime).
		WithCrop(cropParams).
		WithProgressChan(progressCh).
		Build()

	if err := transcoder.Run(ctx); err != nil {
		if removeErr := os.Remove(tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return TranscodeOutput{}, errors.Join(
				fmt.Errorf("transcode: %w", err),
				fmt.Errorf("cleanup temp file: %w", removeErr),
			)
		}

		return TranscodeOutput{}, fmt.Errorf("transcode: %w", err)
	}

	if err := os.Rename(tempPath, finalPath); err != nil {
		if removeErr := os.Remove(tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return TranscodeOutput{}, errors.Join(
				fmt.Errorf("move output file: %w", err),
				fmt.Errorf("cleanup temp file: %w", removeErr),
			)
		}

		return TranscodeOutput{}, fmt.Errorf("move output file: %w", err)
	}

	dstInfo, err := os.Stat(finalPath)
	if err != nil {
		return TranscodeOutput{}, fmt.Errorf("stat output file: %w", err)
	}

	transcodeSucceeded = true

	return TranscodeOutput{
		DestCodec:                codecName(videoCodec),
		DestContainer:            "mkv",
		DestFilePath:             finalPath,
		SourceFileSizeBytes:      srcSize,
		DestFileSizeBytes:        dstInfo.Size(),
		TranscodeDurationSeconds: time.Since(transcodeStart).Seconds(),
		ArtworkFetchSkipped:      artworkSkipped,
		CropApplied:              cropParams != nil,
		HardwareAccelerated:      transcoder.HardwareAccelerated(),
	}, nil
}
