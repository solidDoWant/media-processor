package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/asticode/go-astiav"
)

// DetectCrop runs a software cropdetect filter-graph pre-pass over the first
// video stream in inputPath and returns the final accumulated crop region.
// The filter accumulates the most conservative (widest-bar) detection across
// all frames; the final frame's lavfi.cropdetect.{w,h,x,y} metadata is
// returned as the result.
//
// Context cancellation is honoured via astiav.IOInterrupter: if ctx is
// cancelled before the pass completes, DetectCrop returns promptly with
// ctx.Err(), consistent with the pattern used in Transcoder.openInputContext.
func DetectCrop(ctx context.Context, inputPath string) (CropParams, error) {
	if err := ctx.Err(); err != nil {
		return CropParams{}, err
	}

	// Open input with context-cancellation watchdog.
	inputFmt, cancelWatch, err := openInputForDetectCrop(ctx, inputPath)
	if err != nil {
		return CropParams{}, err
	}
	defer cancelWatch()
	defer inputFmt.CloseInput()
	defer inputFmt.Free()

	// Locate the first video stream.
	var videoStream *astiav.Stream
	for _, s := range inputFmt.Streams() {
		if s.CodecParameters().MediaType() == astiav.MediaTypeVideo {
			videoStream = s
			break
		}
	}
	if videoStream == nil {
		return CropParams{}, errors.New("ffmpeg: DetectCrop: no video stream found")
	}

	// Set up software decoder.
	codec := astiav.FindDecoder(videoStream.CodecParameters().CodecID())
	if codec == nil {
		return CropParams{}, fmt.Errorf("ffmpeg: DetectCrop: no decoder for codec %s",
			videoStream.CodecParameters().CodecID())
	}

	codecCtx := astiav.AllocCodecContext(codec)
	if codecCtx == nil {
		return CropParams{}, errors.New("ffmpeg: DetectCrop: failed to allocate codec context")
	}
	defer codecCtx.Free()

	if err := videoStream.CodecParameters().ToCodecContext(codecCtx); err != nil {
		return CropParams{}, fmt.Errorf("ffmpeg: DetectCrop: copying codec parameters: %w", err)
	}

	if err := codecCtx.Open(codec, nil); err != nil {
		if ctx.Err() != nil {
			return CropParams{}, ctx.Err()
		}
		return CropParams{}, fmt.Errorf("ffmpeg: DetectCrop: opening decoder: %w", err)
	}

	// Build buffer → cropdetect → buffersink filtergraph.
	fg, srcCtx, sinkCtx, err := buildCropdetectFilterGraph(codecCtx, videoStream)
	if err != nil {
		return CropParams{}, err
	}
	defer fg.Free()

	return runCropdetectLoop(ctx, inputFmt, codecCtx, videoStream.Index(), srcCtx, sinkCtx)
}

// openInputForDetectCrop opens inputPath and arms an IOInterrupter so that a
// cancelled ctx aborts blocking FFmpeg calls. The returned cancelWatch func
// must be called (e.g. via defer) once the format context is no longer needed.
func openInputForDetectCrop(ctx context.Context, inputPath string) (*astiav.FormatContext, func(), error) {
	inputFmt := astiav.AllocFormatContext()
	if inputFmt == nil {
		return nil, nil, errors.New("ffmpeg: DetectCrop: failed to allocate format context")
	}

	interrupter := astiav.NewIOInterrupter()
	inputFmt.SetIOInterrupter(interrupter)

	// Free() is called from the goroutine after it exits to avoid a race
	// between Interrupt() and Free() when ctx is cancelled concurrently.
	watchDone := make(chan struct{})
	cancelWatch := func() { close(watchDone) }

	go func() {
		select {
		case <-ctx.Done():
			interrupter.Interrupt()
		case <-watchDone:
		}
		interrupter.Free()
	}()

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

// buildCropdetectFilterGraph creates a buffer → cropdetect → buffersink
// filtergraph configured for the given software video decoder context and
// input stream time base.
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

	// Configure buffersrc from the stream codec parameters (pixel format must come
	// from codec parameters, not from the codec context, as the context pixel
	// format may not be populated until frames are decoded).
	params := astiav.AllocBuffersrcFilterContextParameters()
	defer params.Free()
	params.SetHeight(videoStream.CodecParameters().Height())
	params.SetPixelFormat(videoStream.CodecParameters().PixelFormat())
	params.SetSampleAspectRatio(videoStream.CodecParameters().SampleAspectRatio())
	params.SetTimeBase(videoStream.TimeBase())
	params.SetWidth(videoStream.CodecParameters().Width())

	if err := srcCtx.SetParameters(params); err != nil {
		fg.Free()
		return nil, nil, nil, fmt.Errorf("ffmpeg: DetectCrop: setting buffersrc parameters: %w", err)
	}
	if err := srcCtx.Initialize(nil); err != nil {
		fg.Free()
		return nil, nil, nil, fmt.Errorf("ffmpeg: DetectCrop: initializing buffersrc: %w", err)
	}

	// Wire outputs (buffersrc pad) and inputs (buffersink pad) for Parse.
	outputs := astiav.AllocFilterInOut()
	if outputs == nil {
		fg.Free()
		return nil, nil, nil, errors.New("ffmpeg: DetectCrop: failed to allocate filter in/out")
	}
	defer outputs.Free()

	inputs := astiav.AllocFilterInOut()
	if inputs == nil {
		fg.Free()
		return nil, nil, nil, errors.New("ffmpeg: DetectCrop: failed to allocate filter in/out")
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

	// avfilter_graph_config calls print_formats() at AV_LOG_DEBUG, which emits a
	// message listing every supported pixel format (~2500 bytes). go-astiav's C
	// log callback uses a fixed 1024-byte stack buffer (vsprintf), so messages
	// longer than 1024 bytes cause a stack overflow. Temporarily raise the log
	// level to Verbose (just above Debug) to suppress those messages, then
	// restore the caller's level once Configure returns.
	prevLevel := astiav.GetLogLevel()
	astiav.SetLogLevel(astiav.LogLevelVerbose)

	configErr := fg.Configure()

	astiav.SetLogLevel(prevLevel)

	if configErr != nil {
		fg.Free()
		return nil, nil, nil, fmt.Errorf("ffmpeg: DetectCrop: configuring filter graph: %w", configErr)
	}

	return fg, srcCtx, sinkCtx, nil
}

// runCropdetectLoop reads all packets from inputFmt, decodes video frames on
// the stream at videoStreamIdx, pushes them through the cropdetect filtergraph,
// and returns the last CropParams extracted from lavfi.cropdetect.* metadata.
// The last frame's values represent the most conservative accumulated crop.
func runCropdetectLoop(
	ctx context.Context,
	inputFmt *astiav.FormatContext,
	codecCtx *astiav.CodecContext,
	videoStreamIdx int,
	srcCtx *astiav.BuffersrcFilterContext,
	sinkCtx *astiav.BuffersinkFilterContext,
) (CropParams, error) {
	pkt := astiav.AllocPacket()
	if pkt == nil {
		return CropParams{}, errors.New("ffmpeg: DetectCrop: failed to allocate packet")
	}
	defer pkt.Free()

	decFrame := astiav.AllocFrame()
	if decFrame == nil {
		return CropParams{}, errors.New("ffmpeg: DetectCrop: failed to allocate decode frame")
	}
	defer decFrame.Free()

	filterFrame := astiav.AllocFrame()
	if filterFrame == nil {
		return CropParams{}, errors.New("ffmpeg: DetectCrop: failed to allocate filter frame")
	}
	defer filterFrame.Free()

	var last CropParams
	var haveCrop bool

	// drainFilter pulls all available frames from the filtergraph and updates last.
	drainFilter := func() error {
		for {
			if err := sinkCtx.GetFrame(filterFrame, astiav.NewBuffersinkFlags()); err != nil {
				if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
					return nil
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("ffmpeg: DetectCrop: getting filtered frame: %w", err)
			}
			if cp, ok := parseCropMetadata(filterFrame); ok {
				last = cp
				haveCrop = true
			}
			filterFrame.Unref()
		}
	}

	// drainDecoder pulls all decoded frames from the codec and pushes them
	// through the filtergraph.
	drainDecoder := func() error {
		for {
			if err := codecCtx.ReceiveFrame(decFrame); err != nil {
				if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
					return nil
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("ffmpeg: DetectCrop: receiving decoded frame: %w", err)
			}
			if err := srcCtx.AddFrame(decFrame, astiav.NewBuffersrcFlags(astiav.BuffersrcFlagKeepRef)); err != nil {
				decFrame.Unref()
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("ffmpeg: DetectCrop: adding frame to filter: %w", err)
			}
			decFrame.Unref()
			if err := drainFilter(); err != nil {
				return err
			}
		}
	}

	// Main packet read loop.
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

		if pkt.StreamIndex() != videoStreamIdx {
			pkt.Unref()
			continue
		}

		sendErr := codecCtx.SendPacket(pkt)
		pkt.Unref()

		if sendErr != nil {
			if ctx.Err() != nil {
				return CropParams{}, ctx.Err()
			}
			return CropParams{}, fmt.Errorf("ffmpeg: DetectCrop: sending packet to decoder: %w", sendErr)
		}

		if err := drainDecoder(); err != nil {
			return CropParams{}, err
		}
	}

	// Flush the decoder to drain any buffered frames.
	if err := codecCtx.SendPacket(nil); err == nil {
		if err := drainDecoder(); err != nil {
			return CropParams{}, err
		}
	}

	// Flush the filtergraph by signalling EOF (nil frame).
	if err := srcCtx.AddFrame(nil, astiav.NewBuffersrcFlags()); err == nil {
		if err := drainFilter(); err != nil {
			return CropParams{}, err
		}
	}

	if !haveCrop {
		return CropParams{}, errors.New("ffmpeg: DetectCrop: no crop metadata produced by filter")
	}

	return last, nil
}

// parseCropMetadata extracts lavfi.cropdetect.{w,h,x,y} from the frame's
// metadata dictionary. Returns the parsed CropParams and true on success, or
// the zero value and false if any key is absent or not a valid integer.
func parseCropMetadata(f *astiav.Frame) (CropParams, bool) {
	meta := f.Metadata()
	if meta == nil {
		return CropParams{}, false
	}

	get := func(key string) (int, bool) {
		e := meta.Get(key, nil, astiav.NewDictionaryFlags())
		if e == nil {
			return 0, false
		}
		v, err := strconv.Atoi(e.Value())
		if err != nil {
			return 0, false
		}
		return v, true
	}

	w, okW := get("lavfi.cropdetect.w")
	h, okH := get("lavfi.cropdetect.h")
	x, okX := get("lavfi.cropdetect.x")
	y, okY := get("lavfi.cropdetect.y")

	if !okW || !okH || !okX || !okY {
		return CropParams{}, false
	}

	return CropParams{W: w, H: h, X: x, Y: y}, true
}
