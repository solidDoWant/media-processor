// Package ffprobe wraps the go-astiav CGo bindings to inspect media files.
package ffprobe

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/asticode/go-astiav"
)

// MediaInfo holds the top-level metadata for a media container.
type MediaInfo struct {
	Format   string
	Duration time.Duration
	BitRate  int64
	Streams  []StreamInfo
}

// StreamInfo holds per-stream metadata.
type StreamInfo struct {
	CodecName string
	CodecType string  // "video", "audio", "subtitle", etc.
	Width     int     // non-zero for video streams
	Height    int     // non-zero for video streams
	FrameRate float64 // non-zero for video streams
}

// Probe opens the media file at path, reads its container and stream metadata,
// and returns a populated MediaInfo. It returns a non-nil error if the file
// does not exist, is not a recognized media format, or if ctx is cancelled.
func Probe(ctx context.Context, path string) (MediaInfo, error) {
	// Allocate format context.
	fc := astiav.AllocFormatContext()
	if fc == nil {
		return MediaInfo{}, errors.New("ffprobe: failed to allocate format context")
	}
	defer fc.Free()

	// Set up IO interrupter so FFmpeg respects context cancellation.
	interrupter := astiav.NewIOInterrupter()
	defer interrupter.Free()
	fc.SetIOInterrupter(interrupter)

	// Watch for context cancellation in the background.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			interrupter.Interrupt()
		case <-watchDone:
		}
	}()

	// Bail early if context is already cancelled.
	if err := ctx.Err(); err != nil {
		return MediaInfo{}, err
	}

	// Open input file.
	if err := fc.OpenInput(path, nil, nil); err != nil {
		if interrupter.Interrupted() {
			return MediaInfo{}, ctx.Err()
		}
		return MediaInfo{}, fmt.Errorf("ffprobe: opening %q: %w", path, err)
	}
	defer fc.CloseInput()

	// Populate stream info from container headers.
	if err := fc.FindStreamInfo(nil); err != nil {
		if interrupter.Interrupted() {
			return MediaInfo{}, ctx.Err()
		}
		return MediaInfo{}, fmt.Errorf("ffprobe: finding stream info for %q: %w", path, err)
	}

	info := MediaInfo{
		Duration: time.Duration(fc.Duration()) * time.Microsecond,
		BitRate:  fc.BitRate(),
	}

	if f := fc.InputFormat(); f != nil {
		info.Format = f.Name()
	}

	for _, s := range fc.Streams() {
		cp := s.CodecParameters()
		si := StreamInfo{
			CodecName: cp.CodecID().Name(),
			CodecType: cp.MediaType().String(),
		}
		if cp.MediaType() == astiav.MediaTypeVideo {
			si.Width = cp.Width()
			si.Height = cp.Height()
			si.FrameRate = s.AvgFrameRate().Float64()
		}
		info.Streams = append(info.Streams, si)
	}

	return info, nil
}
