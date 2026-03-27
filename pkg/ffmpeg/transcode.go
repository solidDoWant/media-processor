package ffmpeg

import (
	"context"
	"errors"
	"fmt"

	"github.com/asticode/go-astiav"
)

// TranscodeBuilder constructs a Transcoder using a fluent API.
type TranscodeBuilder struct {
	inputPath, outputPath string
	videoCodec            Codec
	audioCodec            Codec
	container             Container
	hwAccel               HWAccel
	progressCh            chan<- Progress
	startHook             func()
	excludeStreams        map[int]bool
	defaultAudioStream    *int // input stream index to mark as default audio; nil = preserve input dispositions
	defaultSubtitleStream *int // input stream index to mark as default subtitle; nil = preserve input dispositions
	downmixSourceIdx      *int // input stream index to synthesize a downmixed audio stream from; nil = no downmix
}

// NewTranscode returns a builder for a transcode job from inputPath to outputPath.
// Default codecs are CodecCopy for both video and audio. When no container is
// set, the output format is inferred from the output file extension.
func NewTranscode(inputPath, outputPath string) *TranscodeBuilder {
	return &TranscodeBuilder{
		inputPath:  inputPath,
		outputPath: outputPath,
		videoCodec: CodecCopy,
		audioCodec: CodecCopy,
	}
}

// ToVideoCodec sets the output video codec.
func (b *TranscodeBuilder) ToVideoCodec(c Codec) *TranscodeBuilder {
	b.videoCodec = c
	return b
}

// ToAudioCodec sets the output audio codec.
func (b *TranscodeBuilder) ToAudioCodec(c Codec) *TranscodeBuilder {
	b.audioCodec = c
	return b
}

// ToContainer sets the output container format. If not called, the container
// is inferred from the output file extension.
func (b *TranscodeBuilder) ToContainer(c Container) *TranscodeBuilder {
	b.container = c
	return b
}

// HardwareAccel sets the hardware acceleration mode.
func (b *TranscodeBuilder) HardwareAccel(h HWAccel) *TranscodeBuilder {
	b.hwAccel = h
	return b
}

// WithProgressChan sets a channel to receive periodic progress updates.
// Updates are sent non-blocking; a full channel silently drops updates.
func (b *TranscodeBuilder) WithProgressChan(ch chan<- Progress) *TranscodeBuilder {
	b.progressCh = ch
	return b
}

// WithStartHook sets a function that is called once the transcoder has
// finished all setup and is about to enter the main packet read loop.
// Intended for testing (e.g. triggering context cancellation at a
// deterministic point) and light instrumentation.
func (b *TranscodeBuilder) WithStartHook(fn func()) *TranscodeBuilder {
	b.startHook = fn
	return b
}

// ExcludeStreams marks the given input stream indices to be dropped from the
// output. Packets from excluded streams are silently discarded during muxing.
func (b *TranscodeBuilder) ExcludeStreams(indices ...int) *TranscodeBuilder {
	if b.excludeStreams == nil {
		b.excludeStreams = make(map[int]bool)
	}
	for _, idx := range indices {
		b.excludeStreams[idx] = true
	}
	return b
}

// WithDefaultAudioStream marks the audio stream at the given input stream index
// as the default audio track in the output. All other audio stream dispositions
// have their default flag cleared. A nil argument is a no-op: audio stream
// dispositions are copied from the input unchanged.
func (b *TranscodeBuilder) WithDefaultAudioStream(idx *int) *TranscodeBuilder {
	b.defaultAudioStream = idx
	return b
}

// WithDefaultSubtitleStream marks the subtitle stream at the given input stream
// index as the default subtitle track in the output. All other subtitle stream
// dispositions have their default flag cleared. A nil argument is a no-op:
// subtitle stream dispositions are copied from the input unchanged.
func (b *TranscodeBuilder) WithDefaultSubtitleStream(idx *int) *TranscodeBuilder {
	b.defaultSubtitleStream = idx
	return b
}

// WithDownmix synthesizes an additional downmixed audio stream from the input
// stream at the given index. The downmix targets a 2.1 channel layout with
// AC-3 encoding; if the encoder does not support 2.1, stereo is used instead.
// The synthesized stream is appended after all other output streams.
// A nil argument is a no-op: no downmix stream is synthesized.
func (b *TranscodeBuilder) WithDownmix(idx *int) *TranscodeBuilder {
	b.downmixSourceIdx = idx
	return b
}

// Build returns a runnable Transcoder.
func (b *TranscodeBuilder) Build() *Transcoder {
	return &Transcoder{TranscodeBuilder: *b}
}

// Transcoder is a ready-to-run transcode job produced by TranscodeBuilder.Build.
// It embeds TranscodeBuilder so all configuration is accessible in one place.
type Transcoder struct {
	TranscodeBuilder
}

// Run executes the transcode job. It blocks until the job completes, the
// context is cancelled, or an error occurs. A cancelled context causes Run to
// return promptly with ctx.Err().
func (t *Transcoder) Run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	effectiveHW := t.resolveHWAccel()

	inputFmt, interrupter, cancelWatch, err := t.openInputContext(ctx)
	if err != nil {
		return err
	}
	defer cancelWatch()
	defer inputFmt.Free()
	defer inputFmt.CloseInput()

	totalDuration := inputFmt.Duration()

	streams, err := t.buildStreamStates(inputFmt, effectiveHW)
	if err != nil {
		return err
	}
	defer freeStreams(streams)

	var downmix *audioStreamState
	if t.downmixSourceIdx != nil {
		downmix, err = t.buildDownmixState(inputFmt, *t.downmixSourceIdx)
		if err != nil {
			return err
		}
		defer downmix.free()
	}

	outputFmt, closeIO, err := t.setupOutputContext(streams, downmix, inputFmt, effectiveHW)
	if err != nil {
		return err
	}
	defer outputFmt.Free()
	defer closeIO()

	if err := outputFmt.WriteHeader(nil); err != nil {
		return fmt.Errorf("ffmpeg: writing header: %w", err)
	}

	if t.startHook != nil {
		t.startHook()
	}

	if err := t.readAllPackets(ctx, inputFmt, outputFmt, streams, downmix, interrupter, totalDuration); err != nil {
		return err
	}

	if err := t.flushAllEncoders(ctx, outputFmt, streams, downmix, interrupter, totalDuration); err != nil {
		return err
	}

	return outputFmt.WriteTrailer()
}

// resolveHWAccel resolves HWAccelAuto to a concrete value by detecting the
// best available hardware encoder for the configured video codec.
func (t *Transcoder) resolveHWAccel() HWAccel {
	if t.hwAccel != HWAccelAuto {
		return t.hwAccel
	}
	return GetHardwareEncoder(t.videoCodec, HWAccelAuto)
}

// openInputContext opens the input file and arms the IOInterrupter so that a
// cancelled context aborts blocking FFmpeg calls.
func (t *Transcoder) openInputContext(ctx context.Context) (*astiav.FormatContext, *astiav.IOInterrupter, func(), error) {
	inputFmt := astiav.AllocFormatContext()
	if inputFmt == nil {
		return nil, nil, nil, errors.New("ffmpeg: failed to allocate input format context")
	}

	interrupter := astiav.NewIOInterrupter()
	inputFmt.SetIOInterrupter(interrupter)

	// Free() is called from the goroutine after it exits to avoid a race
	// between Interrupt() and Free() if the context is cancelled concurrently.
	watchDone := make(chan struct{})
	cancelWatch := func() {
		close(watchDone)
	}
	go func() {
		select {
		case <-ctx.Done():
			interrupter.Interrupt()
		case <-watchDone:
		}
		interrupter.Free()
	}()

	if err := inputFmt.OpenInput(t.inputPath, nil, nil); err != nil {
		cancelWatch()
		inputFmt.Free()
		if ctx.Err() != nil {
			return nil, nil, nil, ctx.Err()
		}
		return nil, nil, nil, fmt.Errorf("ffmpeg: opening input %q: %w", t.inputPath, err)
	}

	if err := inputFmt.FindStreamInfo(nil); err != nil {
		cancelWatch()
		inputFmt.CloseInput()
		inputFmt.Free()
		if ctx.Err() != nil {
			return nil, nil, nil, ctx.Err()
		}
		return nil, nil, nil, fmt.Errorf("ffmpeg: finding stream info: %w", err)
	}

	return inputFmt, interrupter, cancelWatch, nil
}

// buildDownmixState creates an audioStreamState that will decode the input
// stream at sourceIdx and re-encode it as a downmixed AC-3 stream targeting
// a 2.1 channel layout (with stereo fallback).
func (t *Transcoder) buildDownmixState(inputFmt *astiav.FormatContext, sourceIdx int) (*audioStreamState, error) {
	for _, inStream := range inputFmt.Streams() {
		if inStream.Index() != sourceIdx {
			continue
		}
		layout2Point1 := astiav.ChannelLayout2Point1
		state := &audioStreamState{
			copyStreamState: copyStreamState{inStream: inStream},
			encoder: audioEncoderState{
				codecID:             CodecAC3,
				targetChannelLayout: &layout2Point1,
			},
		}
		if err := state.setupDecoder(inStream); err != nil {
			return nil, fmt.Errorf("ffmpeg: setting up downmix decoder for stream %d: %w", sourceIdx, err)
		}
		return state, nil
	}
	return nil, fmt.Errorf("ffmpeg: downmix source stream %d not found in input", sourceIdx)
}

// buildStreamStates creates a stream for every input stream.
// Audio and video streams are set up with a decoder when re-encoding is
// requested; the hwAccel hint is passed through so hardware decoders can be
// selected for a zero-copy decode→encode pipeline.
// All other stream types (subtitles, attachments, data) are remuxed as-is.
//
// Multiple audio tracks are fully supported — each audio stream gets its own
// independent decoder and encoder pipeline.
func (t *Transcoder) buildStreamStates(inputFmt *astiav.FormatContext, hwAccel HWAccel) (map[int]stream, error) {
	streams := make(map[int]stream)

	for _, inStream := range inputFmt.Streams() {
		if t.excludeStreams[inStream.Index()] {
			continue
		}

		mediaType := inStream.CodecParameters().MediaType()
		base := copyStreamState{inStream: inStream}

		var s stream
		switch {
		case mediaType == astiav.MediaTypeVideo && t.videoCodec != CodecCopy:
			videoState := &videoStreamState{copyStreamState: base, encoder: videoEncoderState{codecID: t.videoCodec}}
			if err := videoState.setupDecoder(inStream, inputFmt, hwAccel); err != nil {
				freeStreams(streams)
				return nil, fmt.Errorf("ffmpeg: setting up decoder for stream %d: %w", inStream.Index(), err)
			}
			s = videoState
		case mediaType == astiav.MediaTypeAudio && t.audioCodec != CodecCopy:
			audioState := &audioStreamState{copyStreamState: base, encoder: audioEncoderState{codecID: t.audioCodec}}
			if err := audioState.setupDecoder(inStream); err != nil {
				freeStreams(streams)
				return nil, fmt.Errorf("ffmpeg: setting up decoder for stream %d: %w", inStream.Index(), err)
			}
			s = audioState
		default:
			s = &copyStreamState{inStream: inStream}
		}

		streams[inStream.Index()] = s
	}

	return streams, nil
}

// setupOutputContext opens the output format context, creates output streams,
// sets up encoders for re-encoded streams, and opens the IO context for
// file-based output. The returned closeIO function must be deferred by the
// caller — it flushes the IO context's buffers.
func (t *Transcoder) setupOutputContext(streams map[int]stream, downmix *audioStreamState, inputFmt *astiav.FormatContext, effectiveHW HWAccel) (*astiav.FormatContext, func(), error) {
	noopClose := func() {}

	outputFmt, err := astiav.AllocOutputFormatContext(nil, string(t.container), t.outputPath)
	if err != nil {
		return nil, noopClose, fmt.Errorf("ffmpeg: allocating output format context: %w", err)
	}
	if outputFmt == nil {
		return nil, noopClose, errors.New("ffmpeg: nil output format context")
	}

	// Create output streams in input order.
	for _, inStream := range inputFmt.Streams() {
		s, ok := streams[inStream.Index()]
		if !ok {
			continue
		}

		if err := s.setupEncoder(effectiveHW, outputFmt); err != nil {
			outputFmt.Free()
			return nil, noopClose, fmt.Errorf("ffmpeg: setting up encoder for stream %d: %w", inStream.Index(), err)
		}

		outStream := outputFmt.NewStream(nil)
		if outStream == nil {
			outputFmt.Free()
			return nil, noopClose, errors.New("ffmpeg: failed to create output stream")
		}
		s.setOutputStream(outStream)

		// Copy disposition from the input stream and apply any default-flag overrides.
		outStream.SetDispositionFlags(t.outputDisposition(inStream))

		if encCtx := s.encoderContext(); encCtx != nil {
			// Re-encoded stream: populate output parameters from the encoder.
			if err := outStream.CodecParameters().FromCodecContext(encCtx); err != nil {
				outputFmt.Free()
				return nil, noopClose, fmt.Errorf("ffmpeg: updating codec parameters for stream %d: %w", inStream.Index(), err)
			}
			outStream.SetTimeBase(encCtx.TimeBase())
			continue
		}

		// Copy stream: copy parameters from the input stream.
		if err := inStream.CodecParameters().Copy(outStream.CodecParameters()); err != nil {
			outputFmt.Free()
			return nil, noopClose, fmt.Errorf("ffmpeg: copying codec parameters for stream %d: %w", inStream.Index(), err)
		}

		// Clear the source-container codec tag (e.g. mp4a) which would be
		// incompatible with the output container (e.g. matroska).
		outStream.CodecParameters().SetCodecTag(0)
		outStream.SetTimeBase(inStream.TimeBase())
	}

	// Add the downmix output stream after all regular streams.
	if downmix != nil {
		if err := downmix.setupEncoder(effectiveHW, outputFmt); err != nil {
			outputFmt.Free()
			return nil, noopClose, fmt.Errorf("ffmpeg: setting up downmix encoder: %w", err)
		}

		downmixOut := outputFmt.NewStream(nil)
		if downmixOut == nil {
			outputFmt.Free()
			return nil, noopClose, errors.New("ffmpeg: failed to create downmix output stream")
		}
		downmix.setOutputStream(downmixOut)

		if err := downmixOut.CodecParameters().FromCodecContext(downmix.encoderContext()); err != nil {
			outputFmt.Free()
			return nil, noopClose, fmt.Errorf("ffmpeg: updating codec parameters for downmix stream: %w", err)
		}
		downmixOut.SetTimeBase(downmix.encoderContext().TimeBase())

		// Set default disposition only if no regular audio stream is already default.
		hasDefaultAudio := false
		for _, inStream := range inputFmt.Streams() {
			if inStream.CodecParameters().MediaType() != astiav.MediaTypeAudio {
				continue
			}
			if _, ok := streams[inStream.Index()]; !ok {
				continue
			}
			if t.outputDisposition(inStream).Has(astiav.DispositionFlagDefault) {
				hasDefaultAudio = true
				break
			}
		}
		downmixDisp := astiav.DispositionFlags(0)
		if !hasDefaultAudio {
			downmixDisp = downmixDisp.Add(astiav.DispositionFlagDefault)
		}
		downmixOut.SetDispositionFlags(downmixDisp)

		// Inherit language tag from the source stream.
		if meta := downmix.inStream.Metadata(); meta != nil {
			if srcLang := meta.Get("language", nil, astiav.NewDictionaryFlags()); srcLang != nil {
				langDict := astiav.NewDictionary()
				if err := langDict.Set("language", srcLang.Value(), astiav.NewDictionaryFlags()); err == nil {
					downmixOut.SetMetadata(langDict)
				}
			}
		}
	}

	// Open the IO context for file-based output formats.
	closeIO := noopClose
	if !outputFmt.OutputFormat().Flags().Has(astiav.IOFormatFlagNofile) {
		ioCtx, err := astiav.OpenIOContext(t.outputPath, astiav.NewIOContextFlags(astiav.IOContextFlagWrite), nil, nil)
		if err != nil {
			outputFmt.Free()
			return nil, noopClose, fmt.Errorf("ffmpeg: opening output io context: %w", err)
		}

		closeIO = func() { _ = ioCtx.Close() }
		outputFmt.SetPb(ioCtx)
	}

	return outputFmt, closeIO, nil
}

// outputDisposition computes the disposition flags for an output stream by
// copying from the corresponding input stream and applying any default-flag
// override configured via WithDefaultAudioStream or WithDefaultSubtitleStream.
func (t *Transcoder) outputDisposition(inStream *astiav.Stream) astiav.DispositionFlags {
	disp := inStream.DispositionFlags()

	var defaultStream *int
	switch inStream.CodecParameters().MediaType() {
	case astiav.MediaTypeAudio:
		defaultStream = t.defaultAudioStream
	case astiav.MediaTypeSubtitle:
		defaultStream = t.defaultSubtitleStream
	}

	if defaultStream == nil {
		return disp
	}

	if inStream.Index() == *defaultStream {
		return disp.Add(astiav.DispositionFlagDefault)
	}

	return disp.Del(astiav.DispositionFlagDefault)
}

// readAllPackets is the main decode/encode loop.
func (t *Transcoder) readAllPackets(ctx context.Context, inputFmt, outputFmt *astiav.FormatContext, streams map[int]stream, downmix *audioStreamState, interrupter *astiav.IOInterrupter, totalDuration int64) error {
	packet := astiav.AllocPacket()
	defer packet.Free()

	// Pre-allocate a packet for downmix cloning to avoid per-packet allocation.
	var downmixPkt *astiav.Packet
	if downmix != nil {
		downmixPkt = astiav.AllocPacket()
		defer downmixPkt.Free()
	}

	for {
		if err := inputFmt.ReadFrame(packet); err != nil {
			if errors.Is(err, astiav.ErrEof) {
				return nil
			}

			if interrupter.Interrupted() {
				return ctx.Err()
			}

			return fmt.Errorf("ffmpeg: reading frame: %w", err)
		}

		// If this packet feeds the downmix pipeline, clone it first so that
		// the downmix decoder can rescale timestamps independently.
		if downmix != nil && packet.StreamIndex() == downmix.inStream.Index() {
			if err := downmixPkt.Ref(packet); err == nil {
				if dmErr := downmix.processPacket(downmixPkt, outputFmt, t.progressCh, totalDuration); dmErr != nil {
					downmixPkt.Unref()
					packet.Unref()
					if interrupter.Interrupted() {
						return ctx.Err()
					}
					return dmErr
				}
				downmixPkt.Unref()
			}
		}

		s, ok := streams[packet.StreamIndex()]
		if !ok {
			packet.Unref()
			continue
		}

		err := s.processPacket(packet, outputFmt, t.progressCh, totalDuration)
		packet.Unref()

		if err == nil {
			continue
		}

		if interrupter.Interrupted() {
			return ctx.Err()
		}

		return err
	}
}

// flushAllEncoders drains buffered frames from every active encoder.
func (t *Transcoder) flushAllEncoders(ctx context.Context, outputFmt *astiav.FormatContext, streams map[int]stream, downmix *audioStreamState, interrupter *astiav.IOInterrupter, totalDuration int64) error {
	toFlush := make([]stream, 0, len(streams)+1)
	for _, s := range streams {
		toFlush = append(toFlush, s)
	}
	if downmix != nil {
		toFlush = append(toFlush, downmix)
	}

	for _, s := range toFlush {
		err := s.flush(outputFmt, t.progressCh, totalDuration)

		if err == nil {
			continue
		}

		if interrupter.Interrupted() {
			return ctx.Err()
		}

		return err
	}
	return nil
}
