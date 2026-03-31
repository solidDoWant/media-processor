package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/asticode/go-astiav"
)

// CropParams describes a rectangular crop region within a video frame.
// All values are in pixels.
type CropParams struct {
	W, H, X, Y int
}

// DetectCrop runs a cropdetect filter pass over the video at inputPath and
// returns the detected crop region. The returned CropParams reflect the most
// conservative (widest) non-black region across all decoded frames.
//
// If no cropdetect metadata is emitted (e.g. the video has no black bars),
// DetectCrop falls back to the full input dimensions with X=0, Y=0.
//
// A cancelled context causes DetectCrop to return promptly with ctx.Err().
func DetectCrop(ctx context.Context, inputPath string) (CropParams, error) {
	if err := ctx.Err(); err != nil {
		return CropParams{}, err
	}

	// Open input with IOInterrupter so context cancellation aborts blocking calls.
	inputFmt := astiav.AllocFormatContext()
	if inputFmt == nil {
		return CropParams{}, errors.New("ffmpeg: failed to allocate input format context")
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

	defer close(watchDone)
	defer inputFmt.Free()
	defer inputFmt.CloseInput()

	if err := inputFmt.OpenInput(inputPath, nil, nil); err != nil {
		if ctx.Err() != nil {
			return CropParams{}, ctx.Err()
		}

		return CropParams{}, fmt.Errorf("ffmpeg: opening input %q: %w", inputPath, err)
	}

	if err := inputFmt.FindStreamInfo(nil); err != nil {
		if ctx.Err() != nil {
			return CropParams{}, ctx.Err()
		}

		return CropParams{}, fmt.Errorf("ffmpeg: finding stream info: %w", err)
	}

	// Find the first video stream.
	var videoStream *astiav.Stream

	for _, s := range inputFmt.Streams() {
		if s.CodecParameters().MediaType() == astiav.MediaTypeVideo {
			videoStream = s

			break
		}
	}

	if videoStream == nil {
		return CropParams{}, errors.New("ffmpeg: no video stream found in input")
	}

	// Set up software decoder.
	codec := astiav.FindDecoder(videoStream.CodecParameters().CodecID())
	if codec == nil {
		return CropParams{}, errors.New("ffmpeg: no decoder found for video stream")
	}

	decCtx := astiav.AllocCodecContext(codec)
	if decCtx == nil {
		return CropParams{}, errors.New("ffmpeg: failed to allocate decoder context")
	}

	defer decCtx.Free()

	if err := videoStream.CodecParameters().ToCodecContext(decCtx); err != nil {
		return CropParams{}, fmt.Errorf("ffmpeg: populating decoder context: %w", err)
	}

	decCtx.SetTimeBase(videoStream.TimeBase())

	if err := decCtx.Open(codec, nil); err != nil {
		return CropParams{}, fmt.Errorf("ffmpeg: opening decoder: %w", err)
	}

	// Build filtergraph: buffer -> cropdetect -> buffersink.
	filterGraph := astiav.AllocFilterGraph()
	if filterGraph == nil {
		return CropParams{}, errors.New("ffmpeg: failed to allocate filter graph")
	}

	defer filterGraph.Free()

	buffersrc := astiav.FindFilterByName("buffer")
	if buffersrc == nil {
		return CropParams{}, errors.New("ffmpeg: buffer filter not found")
	}

	buffersink := astiav.FindFilterByName("buffersink")
	if buffersink == nil {
		return CropParams{}, errors.New("ffmpeg: buffersink filter not found")
	}

	buffersrcCtx, err := filterGraph.NewBuffersrcFilterContext(buffersrc, "in")
	if err != nil {
		return CropParams{}, fmt.Errorf("ffmpeg: creating buffersrc context: %w", err)
	}

	buffersinkCtx, err := filterGraph.NewBuffersinkFilterContext(buffersink, "out")
	if err != nil {
		return CropParams{}, fmt.Errorf("ffmpeg: creating buffersink context: %w", err)
	}

	// Configure buffersrc parameters from the decoder context.
	srcParams := astiav.AllocBuffersrcFilterContextParameters()
	defer srcParams.Free()

	srcParams.SetHeight(decCtx.Height())
	srcParams.SetPixelFormat(decCtx.PixelFormat())
	srcParams.SetSampleAspectRatio(decCtx.SampleAspectRatio())
	srcParams.SetTimeBase(videoStream.TimeBase())
	srcParams.SetWidth(decCtx.Width())

	if err := buffersrcCtx.SetParameters(srcParams); err != nil {
		return CropParams{}, fmt.Errorf("ffmpeg: setting buffersrc parameters: %w", err)
	}

	if err := buffersrcCtx.Initialize(nil); err != nil {
		return CropParams{}, fmt.Errorf("ffmpeg: initializing buffersrc: %w", err)
	}

	// Wire the graph: buffersrc output -> cropdetect -> buffersink input.
	outputs := astiav.AllocFilterInOut()
	if outputs == nil {
		return CropParams{}, errors.New("ffmpeg: failed to allocate filter outputs")
	}

	defer outputs.Free()

	inputs := astiav.AllocFilterInOut()
	if inputs == nil {
		return CropParams{}, errors.New("ffmpeg: failed to allocate filter inputs")
	}

	defer inputs.Free()

	outputs.SetName("in")
	outputs.SetFilterContext(buffersrcCtx.FilterContext())
	outputs.SetPadIdx(0)
	outputs.SetNext(nil)

	inputs.SetName("out")
	inputs.SetFilterContext(buffersinkCtx.FilterContext())
	inputs.SetPadIdx(0)
	inputs.SetNext(nil)

	if err := filterGraph.Parse("cropdetect", inputs, outputs); err != nil {
		return CropParams{}, fmt.Errorf("ffmpeg: parsing cropdetect filter: %w", err)
	}

	if err := filterGraph.Configure(); err != nil {
		return CropParams{}, fmt.Errorf("ffmpeg: configuring filter graph: %w", err)
	}

	// Decode all frames and push them through the filter graph.
	decFrame := astiav.AllocFrame()
	defer decFrame.Free()

	filterFrame := astiav.AllocFrame()
	defer filterFrame.Free()

	pkt := astiav.AllocPacket()
	defer pkt.Free()

	fullW := decCtx.Width()
	fullH := decCtx.Height()
	result := CropParams{W: fullW, H: fullH, X: 0, Y: 0}

	for {
		if err := inputFmt.ReadFrame(pkt); err != nil {
			if errors.Is(err, astiav.ErrEof) {
				break
			}

			if interrupter.Interrupted() {
				return CropParams{}, ctx.Err()
			}

			return CropParams{}, fmt.Errorf("ffmpeg: reading frame: %w", err)
		}

		if pkt.StreamIndex() != videoStream.Index() {
			pkt.Unref()

			continue
		}

		if err := decCtx.SendPacket(pkt); err != nil {
			pkt.Unref()

			if interrupter.Interrupted() {
				return CropParams{}, ctx.Err()
			}

			return CropParams{}, fmt.Errorf("ffmpeg: sending packet to decoder: %w", err)
		}

		pkt.Unref()

		for {
			if err := decCtx.ReceiveFrame(decFrame); err != nil {
				if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
					break
				}

				if interrupter.Interrupted() {
					return CropParams{}, ctx.Err()
				}

				return CropParams{}, fmt.Errorf("ffmpeg: receiving frame from decoder: %w", err)
			}

			if err := buffersrcCtx.AddFrame(decFrame, astiav.NewBuffersrcFlags(astiav.BuffersrcFlagKeepRef)); err != nil {
				decFrame.Unref()

				if interrupter.Interrupted() {
					return CropParams{}, ctx.Err()
				}

				return CropParams{}, fmt.Errorf("ffmpeg: adding frame to filter: %w", err)
			}

			decFrame.Unref()

			for {
				if err := buffersinkCtx.GetFrame(filterFrame, astiav.NewBuffersinkFlags()); err != nil {
					if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
						break
					}

					if interrupter.Interrupted() {
						return CropParams{}, ctx.Err()
					}

					return CropParams{}, fmt.Errorf("ffmpeg: getting frame from filter: %w", err)
				}

				if cp, ok := parseCropMetadata(filterFrame); ok {
					result = cp
				}

				filterFrame.Unref()
			}
		}
	}

	// Flush the decoder.
	if err := decCtx.SendPacket(nil); err != nil && !errors.Is(err, astiav.ErrEof) {
		if interrupter.Interrupted() {
			return CropParams{}, ctx.Err()
		}

		return CropParams{}, fmt.Errorf("ffmpeg: flushing decoder: %w", err)
	}

	for {
		if err := decCtx.ReceiveFrame(decFrame); err != nil {
			if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
				break
			}

			if interrupter.Interrupted() {
				return CropParams{}, ctx.Err()
			}

			return CropParams{}, fmt.Errorf("ffmpeg: receiving flush frame: %w", err)
		}

		if err := buffersrcCtx.AddFrame(decFrame, astiav.NewBuffersrcFlags(astiav.BuffersrcFlagKeepRef)); err != nil {
			decFrame.Unref()

			continue
		}

		decFrame.Unref()

		for {
			if err := buffersinkCtx.GetFrame(filterFrame, astiav.NewBuffersinkFlags()); err != nil {
				break
			}

			if cp, ok := parseCropMetadata(filterFrame); ok {
				result = cp
			}

			filterFrame.Unref()
		}
	}

	// Flush the filter graph; ignore errors since we have the final result.
	_ = buffersrcCtx.AddFrame(nil, astiav.NewBuffersrcFlags())

	for {
		if err := buffersinkCtx.GetFrame(filterFrame, astiav.NewBuffersinkFlags()); err != nil {
			break
		}

		if cp, ok := parseCropMetadata(filterFrame); ok {
			result = cp
		}

		filterFrame.Unref()
	}

	return result, nil
}

// parseCropMetadata reads lavfi.cropdetect.{w,h,x,y} from a filter output
// frame's metadata dictionary. Returns the parsed CropParams and true on
// success, or the zero value and false if any key is absent or unparseable.
func parseCropMetadata(f *astiav.Frame) (CropParams, bool) {
	meta := f.Metadata()
	if meta == nil {
		return CropParams{}, false
	}

	flags := astiav.NewDictionaryFlags()
	wEntry := meta.Get("lavfi.cropdetect.w", nil, flags)
	hEntry := meta.Get("lavfi.cropdetect.h", nil, flags)
	xEntry := meta.Get("lavfi.cropdetect.x", nil, flags)
	yEntry := meta.Get("lavfi.cropdetect.y", nil, flags)

	if wEntry == nil || hEntry == nil || xEntry == nil || yEntry == nil {
		return CropParams{}, false
	}

	w, err := strconv.Atoi(wEntry.Value())
	if err != nil {
		return CropParams{}, false
	}

	h, err := strconv.Atoi(hEntry.Value())
	if err != nil {
		return CropParams{}, false
	}

	x, err := strconv.Atoi(xEntry.Value())
	if err != nil {
		return CropParams{}, false
	}

	y, err := strconv.Atoi(yEntry.Value())
	if err != nil {
		return CropParams{}, false
	}

	return CropParams{W: w, H: h, X: x, Y: y}, true
}
