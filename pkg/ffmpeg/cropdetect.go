package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/asticode/go-astiav"
)

// errCropConverged is a sentinel returned by drainFilter when the cropdetect
// result has been stable for stabilityThreshold consecutive frames, signalling
// that the decode loop can exit early.
var errCropConverged = errors.New("ffmpeg: DetectCrop: crop parameters converged")

const (
	// Phase 1 — decode all packets until keyframeSwitchCount frames have been
	// decoded. This covers short clips and fade-in / B-frame-heavy content where
	// all visual information is in non-keyframe packets. Phase 2 — forward only
	// keyframe (I-frame) packets and skip non-keyframe packets entirely. Coverage
	// for videos with a very low keyframe rate comes from the phase-1 full-decode
	// window, not from a phase-2 fallback path.
	//
	// 200 frames covers ~6-8 s at common frame rates, which is enough to capture
	// any opening non-keyframe content. It must exceed the frame count of any
	// short test fixture to ensure the keyframe-only fast path is not activated
	// prematurely for those files.
	keyframeSwitchCount = 200

	// The first alwaysIncludeCount frames are unconditionally forwarded to the
	// filter. cropdetect silently discards its first 2 filter inputs (the default
	// skip=2 setting), so at least 3 inputs are required before any metadata is
	// emitted. This guarantees that very short videos — those with fewer frames
	// than sampleInterval — still produce at least one valid crop measurement.
	alwaysIncludeCount = 3

	// 1 in sampleInterval decoded frames is sent to the filter during phase 1
	// (after the alwaysIncludeCount window). Sparse sampling spreads the phase-1
	// window instead of sending every frame to the filter during the initial
	// all-packet decode pass.
	sampleInterval = 20

	// Exit once the crop result is unchanged for this many consecutive phase-2
	// filter inputs. Convergence is only checked in phase 2 (keyframe-only
	// decode) so that the full phase-1 window always runs regardless of how
	// stable the opening content appears. Letterboxing is static so a short run
	// of identical keyframe results is a reliable convergence signal.
	stabilityThreshold = 5
)

// DetectCrop runs a cropdetect filter pass over the video at inputPath and
// returns the detected crop region. The returned CropParams reflect the most
// conservative (widest) non-black region across all decoded frames.
//
// A cancelled context causes DetectCrop to return promptly with ctx.Err().
func DetectCrop(ctx context.Context, inputPath string) (CropParams, error) {
	if err := ctx.Err(); err != nil {
		return CropParams{}, err
	}

	inputFmt, cancelWatch, err := openInputForDetectCrop(ctx, inputPath)
	if err != nil {
		return CropParams{}, err
	}

	defer func() {
		inputFmt.CloseInput()
		inputFmt.Free()
		cancelWatch()
	}()

	var videoStream *astiav.Stream

	for _, stream := range inputFmt.Streams() {
		if stream.CodecParameters().MediaType() == astiav.MediaTypeVideo {
			videoStream = stream

			break
		}
	}

	if videoStream == nil {
		return CropParams{}, errors.New("ffmpeg: DetectCrop: no video stream found")
	}

	codec := astiav.FindDecoder(videoStream.CodecParameters().CodecID())
	if codec == nil {
		return CropParams{}, fmt.Errorf("ffmpeg: DetectCrop: no decoder for codec %v", videoStream.CodecParameters().CodecID())
	}

	codecCtx := astiav.AllocCodecContext(codec)
	if codecCtx == nil {
		return CropParams{}, errors.New("ffmpeg: DetectCrop: failed to allocate codec context")
	}

	defer codecCtx.Free()

	if err := videoStream.CodecParameters().ToCodecContext(codecCtx); err != nil {
		return CropParams{}, fmt.Errorf("ffmpeg: DetectCrop: populating codec context: %w", err)
	}

	codecCtx.SetTimeBase(videoStream.TimeBase())

	// Enable slice-parallel decoding. With keyframe-only decoding there is no
	// multi-frame pipeline, so FF_THREAD_SLICE (intra-frame parallelism) is more
	// effective than FF_THREAD_FRAME. 0 lets FFmpeg choose the thread count.
	codecCtx.SetThreadCount(0)
	codecCtx.SetThreadType(astiav.ThreadTypeSlice)

	if err := codecCtx.Open(codec, nil); err != nil {
		return CropParams{}, fmt.Errorf("ffmpeg: DetectCrop: opening codec: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return CropParams{}, err
	}

	fg, srcCtx, sinkCtx, err := buildCropdetectFilterGraph(codecCtx, videoStream)
	if err != nil {
		return CropParams{}, err
	}

	defer fg.Free()

	return runCropdetectLoop(ctx, inputFmt, codecCtx, videoStream.Index(), srcCtx, sinkCtx)
}

// openInputForDetectCrop opens the input file and returns the format context
// along with a cancelWatch function that must be called to release the
// IOInterrupter goroutine.
func openInputForDetectCrop(ctx context.Context, inputPath string) (*astiav.FormatContext, func(), error) {
	inputFmt := astiav.AllocFormatContext()
	if inputFmt == nil {
		return nil, nil, errors.New("ffmpeg: DetectCrop: failed to allocate format context")
	}

	interrupter := astiav.NewIOInterrupter()
	inputFmt.SetIOInterrupter(interrupter)

	watchDone := make(chan struct{})

	go func() {
		select {
		case <-ctx.Done():
			interrupter.Interrupt()
		case <-watchDone:
		}

		interrupter.Free()
	}()

	cancelWatch := func() { close(watchDone) }

	if err := inputFmt.OpenInput(inputPath, nil, nil); err != nil {
		cancelWatch()
		inputFmt.Free()

		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}

		return nil, nil, fmt.Errorf("ffmpeg: DetectCrop: opening input %q: %w", inputPath, err)
	}

	if err := inputFmt.FindStreamInfo(nil); err != nil {
		cancelWatch()
		inputFmt.CloseInput()
		inputFmt.Free()

		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}

		return nil, nil, fmt.Errorf("ffmpeg: DetectCrop: finding stream info: %w", err)
	}

	return inputFmt, cancelWatch, nil
}

// buildCropdetectFilterGraph wires a buffer → cropdetect → buffersink filter
// graph. It uses the pixel format from the stream's codec parameters (not the
// codec context, which may be unset before the first frame is decoded).
func buildCropdetectFilterGraph(
	codecCtx *astiav.CodecContext,
	videoStream *astiav.Stream,
) (*astiav.FilterGraph, *astiav.BuffersrcFilterContext, *astiav.BuffersinkFilterContext, error) {
	fg := astiav.AllocFilterGraph()
	if fg == nil {
		return nil, nil, nil, errors.New("ffmpeg: DetectCrop: failed to allocate filter graph")
	}

	buffersrc := astiav.FindFilterByName("buffer")
	if buffersrc == nil {
		fg.Free()

		return nil, nil, nil, errors.New("ffmpeg: DetectCrop: buffer filter not found")
	}

	buffersink := astiav.FindFilterByName("buffersink")
	if buffersink == nil {
		fg.Free()

		return nil, nil, nil, errors.New("ffmpeg: DetectCrop: buffersink filter not found")
	}

	srcCtx, err := fg.NewBuffersrcFilterContext(buffersrc, "in")
	if err != nil {
		fg.Free()

		return nil, nil, nil, fmt.Errorf("ffmpeg: DetectCrop: creating buffersrc context: %w", err)
	}

	sinkCtx, err := fg.NewBuffersinkFilterContext(buffersink, "out")
	if err != nil {
		fg.Free()

		return nil, nil, nil, fmt.Errorf("ffmpeg: DetectCrop: creating buffersink context: %w", err)
	}

	srcParams := astiav.AllocBuffersrcFilterContextParameters()
	defer srcParams.Free()

	srcParams.SetHeight(codecCtx.Height())
	srcParams.SetPixelFormat(videoStream.CodecParameters().PixelFormat())
	srcParams.SetSampleAspectRatio(codecCtx.SampleAspectRatio())
	srcParams.SetTimeBase(videoStream.TimeBase())
	srcParams.SetWidth(codecCtx.Width())

	if err := srcCtx.SetParameters(srcParams); err != nil {
		fg.Free()

		return nil, nil, nil, fmt.Errorf("ffmpeg: DetectCrop: setting buffersrc parameters: %w", err)
	}

	if err := srcCtx.Initialize(nil); err != nil {
		fg.Free()

		return nil, nil, nil, fmt.Errorf("ffmpeg: DetectCrop: initializing buffersrc: %w", err)
	}

	outputs := astiav.AllocFilterInOut()
	if outputs == nil {
		fg.Free()

		return nil, nil, nil, errors.New("ffmpeg: DetectCrop: failed to allocate filter outputs")
	}

	defer outputs.Free()

	inputs := astiav.AllocFilterInOut()
	if inputs == nil {
		fg.Free()

		return nil, nil, nil, errors.New("ffmpeg: DetectCrop: failed to allocate filter inputs")
	}

	defer inputs.Free()

	outputs.SetName("in")
	outputs.SetFilterContext(srcCtx.FilterContext())
	outputs.SetPadIdx(0)
	outputs.SetNext(nil)

	inputs.SetName("out")
	inputs.SetFilterContext(sinkCtx.FilterContext())
	inputs.SetPadIdx(0)
	inputs.SetNext(nil)

	if err := fg.Parse("cropdetect", inputs, outputs); err != nil {
		fg.Free()

		return nil, nil, nil, fmt.Errorf("ffmpeg: DetectCrop: parsing filter graph: %w", err)
	}

	if err := fg.Configure(); err != nil {
		fg.Free()

		return nil, nil, nil, fmt.Errorf("ffmpeg: DetectCrop: configuring filter graph: %w", err)
	}

	return fg, srcCtx, sinkCtx, nil
}

// runCropdetectLoop decodes frames from inputFmt, pushes them through the
// cropdetect filter graph, and returns the detected crop region.
// See the package-level constants for the two-phase decoding strategy and
// convergence parameters.
func runCropdetectLoop(
	ctx context.Context,
	inputFmt *astiav.FormatContext,
	codecCtx *astiav.CodecContext,
	videoStreamIndex int,
	srcCtx *astiav.BuffersrcFilterContext,
	sinkCtx *astiav.BuffersinkFilterContext,
) (CropParams, error) {
	decFrame := astiav.AllocFrame()
	defer decFrame.Free()

	filterFrame := astiav.AllocFrame()
	defer filterFrame.Free()

	pkt := astiav.AllocPacket()
	defer pkt.Free()

	var result CropParams

	haveCrop := false
	frameCounter := 0
	consecutiveStable := 0
	inPhase2 := false

	drainFilter := func() error {
		for {
			if err := sinkCtx.GetFrame(filterFrame, astiav.NewBuffersinkFlags()); err != nil {
				if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
					return nil
				}

				if ctx.Err() != nil {
					return ctx.Err()
				}

				return fmt.Errorf("ffmpeg: DetectCrop: getting frame from filter: %w", err)
			}

			// cropdetect accumulates its estimate across frames: each output
			// frame's metadata already reflects the widest non-black region
			// seen so far, so the last value is the fully-converged result.
			if cropParameters, ok := parseCropMetadata(filterFrame); ok {
				// Only track stability in phase 2. This guarantees the full
				// phase-1 window always runs, preventing a narrow intro (title
				// cards, logos) from triggering early convergence before the
				// main content is seen.
				if inPhase2 {
					if haveCrop && cropParameters == result {
						consecutiveStable++
						if consecutiveStable >= stabilityThreshold {
							filterFrame.Unref()
							return errCropConverged
						}
					} else {
						// First phase-2 result, or crop just widened: start the
						// stability counter at 1 (this reading counts toward the
						// threshold).
						consecutiveStable = 1
					}
				}

				result = cropParameters
				haveCrop = true
			}

			filterFrame.Unref()
		}
	}

	drainDecoder := func() error {
		for {
			if err := codecCtx.ReceiveFrame(decFrame); err != nil {
				if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
					return nil
				}

				if ctx.Err() != nil {
					return ctx.Err()
				}

				return fmt.Errorf("ffmpeg: DetectCrop: receiving frame: %w", err)
			}

			// Phase 2 forwards every decoded frame (all are keyframes).
			// Phase 1 forwards the first alwaysIncludeCount frames plus 1 in
			// sampleInterval of the remainder.
			var sendToFilter bool
			if frameCounter >= keyframeSwitchCount {
				sendToFilter = true
			} else {
				sendToFilter = frameCounter < alwaysIncludeCount || frameCounter%sampleInterval == 0
			}

			// Record phase before incrementing so drainFilter sees the correct
			// phase for this frame.
			inPhase2 = frameCounter >= keyframeSwitchCount
			frameCounter++

			if sendToFilter {
				if err := srcCtx.AddFrame(decFrame, astiav.NewBuffersrcFlags(astiav.BuffersrcFlagKeepRef)); err != nil {
					decFrame.Unref()

					if ctx.Err() != nil {
						return ctx.Err()
					}

					return fmt.Errorf("ffmpeg: DetectCrop: adding frame to filter: %w", err)
				}

				if err := drainFilter(); err != nil {
					decFrame.Unref()
					return err // propagates errCropConverged
				}
			}

			decFrame.Unref()
		}
	}

	for {
		if err := inputFmt.ReadFrame(pkt); err != nil {
			if errors.Is(err, astiav.ErrEof) {
				break
			}

			if ctx.Err() != nil {
				return CropParams{}, ctx.Err()
			}

			return CropParams{}, fmt.Errorf("ffmpeg: DetectCrop: reading frame: %w", err)
		}

		if pkt.StreamIndex() != videoStreamIndex {
			pkt.Unref()

			continue
		}

		// Phase 2: skip non-keyframe packets entirely. Sending orphan P/B
		// packets whose reference frames were already discarded would cause
		// the decoder to produce corrupted output; cropdetect could then
		// misread that noise as visible content and widen the crop region
		// incorrectly.
		if frameCounter >= keyframeSwitchCount && !pkt.Flags().Has(astiav.PacketFlagKey) {
			pkt.Unref()

			continue
		}

		if err := codecCtx.SendPacket(pkt); err != nil {
			pkt.Unref()

			if ctx.Err() != nil {
				return CropParams{}, ctx.Err()
			}

			return CropParams{}, fmt.Errorf("ffmpeg: DetectCrop: sending packet to decoder: %w", err)
		}

		pkt.Unref()

		if err := drainDecoder(); err != nil {
			if errors.Is(err, errCropConverged) {
				break // crop parameters have stabilized, no need to continue
			}

			return CropParams{}, err
		}
	}

	// Flush the decoder. errCropConverged during flush is not an error: the
	// result is already fully converged.
	if err := codecCtx.SendPacket(nil); err != nil && !errors.Is(err, astiav.ErrEof) {
		if ctx.Err() != nil {
			return CropParams{}, ctx.Err()
		}

		return CropParams{}, fmt.Errorf("ffmpeg: DetectCrop: flushing decoder: %w", err)
	}

	if err := drainDecoder(); err != nil && !errors.Is(err, errCropConverged) {
		return CropParams{}, err
	}

	// Signal EOF to the filter graph. Any error here is intentionally ignored:
	// the nil frame flush may return EAGAIN or EINVAL once all real frames have
	// been processed, and drainFilter below collects any remaining output.
	_ = srcCtx.AddFrame(nil, astiav.NewBuffersrcFlags())

	if err := drainFilter(); err != nil && !errors.Is(err, errCropConverged) {
		return CropParams{}, err
	}

	if !haveCrop {
		return CropParams{}, errors.New("ffmpeg: DetectCrop: no crop metadata produced")
	}

	return result, nil
}

// parseCropMetadata reads lavfi.cropdetect.{w,h,x,y} from a filter output
// frame's metadata dictionary. Returns the parsed CropParams and true on
// success, or the zero value and false if any key is absent or unparseable.
func parseCropMetadata(frame *astiav.Frame) (CropParams, bool) {
	meta := frame.Metadata()
	if meta == nil {
		return CropParams{}, false
	}

	flags := astiav.NewDictionaryFlags()

	get := func(key string) (int, bool) {
		entry := meta.Get(key, nil, flags)
		if entry == nil {
			return 0, false
		}

		value, err := strconv.Atoi(entry.Value())
		if err != nil {
			return 0, false
		}

		return value, true
	}

	w, ok := get("lavfi.cropdetect.w")
	if !ok {
		return CropParams{}, false
	}

	h, ok := get("lavfi.cropdetect.h")
	if !ok {
		return CropParams{}, false
	}

	x, ok := get("lavfi.cropdetect.x")
	if !ok {
		return CropParams{}, false
	}

	y, ok := get("lavfi.cropdetect.y")
	if !ok {
		return CropParams{}, false
	}

	// cropdetect initialises its running bounds to the inverted extent of the
	// frame (x1=width-1, x2=0, etc.) and only updates them when it finds
	// non-black pixels. When no content has been found yet the resulting w and
	// h are negative. Reject those frames so they are not treated as valid
	// crop measurements.
	if w <= 0 || h <= 0 {
		return CropParams{}, false
	}

	return CropParams{W: w, H: h, X: x, Y: y}, true
}
