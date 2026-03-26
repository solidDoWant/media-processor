// Package ffprobe wraps the go-astiav CGo bindings to inspect media files.
package ffprobe

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/asticode/go-astiav"
)

// CodecType identifies the kind of media carried by a stream.
type CodecType string

const (
	CodecTypeUnknown    CodecType = ""
	CodecTypeVideo      CodecType = "video"
	CodecTypeAudio      CodecType = "audio"
	CodecTypeData       CodecType = "data"
	CodecTypeSubtitle   CodecType = "subtitle"
	CodecTypeAttachment CodecType = "attachment"
)

// Video codec name constants as reported by ffprobe in StreamInfo.CodecName.
const (
	CodecNameH264 = "h264"
	CodecNameH265 = "hevc"
)

// MediaInfo holds top-level metadata for a media container.
type MediaInfo struct {
	Format        string
	Duration      time.Duration
	BitsPerSecond int64
	Tags          map[string]string
	Streams       []StreamInfo
}

// StreamInfo holds per-stream metadata.
type StreamInfo struct {
	CodecName     string
	CodecType     CodecType
	BitsPerSecond int64
	// Video-only fields (zero for non-video streams).
	WidthPixels     int
	HeightPixels    int
	FramesPerSecond float64
	// Audio-only fields (zero for non-audio streams).
	AudioSampleRateHz int
	AudioChannelCount int
}

// Probe opens the media file at path, reads its container and stream metadata,
// and returns a populated MediaInfo. It returns a non-nil error if the file
// does not exist, is not a recognized media format, or if ctx is cancelled.
func Probe(ctx context.Context, path string) (*MediaInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Allocate format context.
	formatContext := astiav.AllocFormatContext()
	if formatContext == nil {
		return nil, errors.New("ffprobe: failed to allocate format context")
	}
	defer formatContext.Free()

	// Set up IO interrupter so FFmpeg respects context cancellation.
	// Free() is called from the goroutine after it exits to avoid a race
	// between Interrupt() and Free().
	interrupter := astiav.NewIOInterrupter()
	formatContext.SetIOInterrupter(interrupter)

	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			interrupter.Interrupt()
		case <-watchDone:
		}
		interrupter.Free()
	}()

	// Open input file.
	if err := formatContext.OpenInput(path, nil, nil); err != nil {
		if interrupter.Interrupted() {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("ffprobe: opening %q: %w", path, err)
	}
	defer formatContext.CloseInput()

	// Populate stream info from container headers.
	if err := formatContext.FindStreamInfo(nil); err != nil {
		if interrupter.Interrupted() {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("ffprobe: finding stream info for %q: %w", path, err)
	}

	info := &MediaInfo{
		Duration:      time.Duration(formatContext.Duration()) * time.Microsecond,
		BitsPerSecond: formatContext.BitRate(),
		Tags:          dictionaryToMap(formatContext.Metadata()),
	}

	if inputFormat := formatContext.InputFormat(); inputFormat != nil {
		info.Format = inputFormat.Name()
	}

	for _, stream := range formatContext.Streams() {
		codecParams := stream.CodecParameters()
		streamInfo := StreamInfo{
			CodecName:     codecParams.CodecID().Name(),
			CodecType:     CodecType(codecParams.MediaType().String()),
			BitsPerSecond: codecParams.BitRate(),
		}
		switch codecParams.MediaType() {
		case astiav.MediaTypeVideo:
			streamInfo.WidthPixels = codecParams.Width()
			streamInfo.HeightPixels = codecParams.Height()
			streamInfo.FramesPerSecond = stream.AvgFrameRate().Float64()
		case astiav.MediaTypeAudio:
			streamInfo.AudioSampleRateHz = codecParams.SampleRate()
			streamInfo.AudioChannelCount = codecParams.ChannelLayout().Channels()
		}
		info.Streams = append(info.Streams, streamInfo)
	}

	return info, nil
}

// dictionaryToMap converts an astiav Dictionary into a Go map. Returns nil if
// the dictionary is nil.
func dictionaryToMap(dict *astiav.Dictionary) map[string]string {
	if dict == nil {
		return nil
	}
	result := make(map[string]string)
	var prev *astiav.DictionaryEntry
	for {
		entry := dict.Get("", prev, astiav.NewDictionaryFlags(astiav.DictionaryFlagIgnoreSuffix))
		if entry == nil {
			break
		}
		result[entry.Key()] = entry.Value()
		prev = entry
	}
	return result
}
