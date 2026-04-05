package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/asticode/go-astiav"
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
		return CropParams{}, fmt.Errorf("ffmpeg: DetectCrop: no decoder for codec %s", videoStream.CodecParameters().CodecID())
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
// cropdetect filter graph (with frame sampling), and returns the last detected
// crop region. It returns an error if no crop metadata was produced.
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

	// Sample 1 in 20 frames (5% sampling rate) to balance accuracy and performance.
	// For a 2-hour movie at 24fps (~173K frames), this processes ~8,640 frames.
	// Always include the first 50 frames to ensure sufficient coverage for short
	// videos and to capture header/trailer content.
	const sampleInterval = 20

	const alwaysIncludeCount = 50

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

			// Implement frame sampling:
			// - Always include the first 50 frames
			// - Sample every 20th frame in the middle
			if frameCounter < alwaysIncludeCount || frameCounter%sampleInterval == 0 {
				if err := srcCtx.AddFrame(decFrame, astiav.NewBuffersrcFlags(astiav.BuffersrcFlagKeepRef)); err != nil {
					decFrame.Unref()

					if ctx.Err() != nil {
						return ctx.Err()
					}

					return fmt.Errorf("ffmpeg: DetectCrop: adding frame to filter: %w", err)
				}

				if err := drainFilter(); err != nil {
					decFrame.Unref()
					return err
				}
			}

			frameCounter++

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

		if err := codecCtx.SendPacket(pkt); err != nil {
			pkt.Unref()

			if ctx.Err() != nil {
				return CropParams{}, ctx.Err()
			}

			return CropParams{}, fmt.Errorf("ffmpeg: DetectCrop: sending packet to decoder: %w", err)
		}

		pkt.Unref()

		if err := drainDecoder(); err != nil {
			return CropParams{}, err
		}
	}

	// Flush the decoder.
	if err := codecCtx.SendPacket(nil); err != nil && !errors.Is(err, astiav.ErrEof) {
		if ctx.Err() != nil {
			return CropParams{}, ctx.Err()
		}

		return CropParams{}, fmt.Errorf("ffmpeg: DetectCrop: flushing decoder: %w", err)
	}

	if err := drainDecoder(); err != nil {
		return CropParams{}, err
	}

	// Signal EOF to the filter graph. Any error here is intentionally ignored:
	// the nil frame flush may return EAGAIN or EINVAL once all real frames have
	// been processed, and drainFilter below collects any remaining output.
	_ = srcCtx.AddFrame(nil, astiav.NewBuffersrcFlags())

	if err := drainFilter(); err != nil {
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

	return CropParams{W: w, H: h, X: x, Y: y}, true
}
