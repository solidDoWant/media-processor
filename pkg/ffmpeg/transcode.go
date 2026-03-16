package ffmpeg

import (
	"context"
	"errors"
	"fmt"

	"github.com/asticode/go-astiav"
)

// hwProfile holds hardware-specific encoder configuration.
// Constants come from libavutil/hwcontext.h (device types) and
// libavutil/pixfmt.h (pixel formats) via go-astiav bindings.
type hwProfile struct {
	deviceType  astiav.HardwareDeviceType
	hwPixFmt    astiav.PixelFormat // pixel format used inside the hardware
	swPixFmt    astiav.PixelFormat // software pixel format fed to the hardware encoder (e.g. NV12)
	h264Encoder string
	h265Encoder string
}

var hwProfiles = map[HWAccel]hwProfile{
	HWAccelQSV: {
		deviceType:  astiav.HardwareDeviceTypeQSV,
		hwPixFmt:    astiav.PixelFormatQsv,
		swPixFmt:    astiav.PixelFormatNv12,
		h264Encoder: "h264_qsv",
		h265Encoder: "hevc_qsv",
	},
	HWAccelNVENC: {
		deviceType:  astiav.HardwareDeviceTypeCUDA,
		hwPixFmt:    astiav.PixelFormatCuda,
		swPixFmt:    astiav.PixelFormatNv12,
		h264Encoder: "h264_nvenc",
		h265Encoder: "hevc_nvenc",
	},
	HWAccelVAAPI: {
		deviceType:  astiav.HardwareDeviceTypeVAAPI,
		hwPixFmt:    astiav.PixelFormatVaapi,
		swPixFmt:    astiav.PixelFormatNv12,
		h264Encoder: "h264_vaapi",
		h265Encoder: "hevc_vaapi",
	},
}

// streamState holds all per-stream resources for a single transcode pass.
// It is shared across several helper methods to avoid threading raw FFmpeg
// pointers through every call site.
type streamState struct {
	inputStream  *astiav.Stream
	outputStream *astiav.Stream
	isCopy       bool
	isVideo      bool

	// Decoder
	decCodec        *astiav.Codec
	decCodecContext *astiav.CodecContext
	decFrame        *astiav.Frame

	// Encoder
	encCodec        *astiav.Codec
	encCodecContext *astiav.CodecContext
	encPkt          *astiav.Packet

	// Video pixel-format conversion (software)
	swsCtx      *astiav.SoftwareScaleContext
	scaledFrame *astiav.Frame

	// Hardware acceleration
	isHW        bool
	hwDevCtx    *astiav.HardwareDeviceContext
	hwFramesCtx *astiav.HardwareFramesContext
	hwFrame     *astiav.Frame

	// Audio sample-format conversion
	swrCtx     *astiav.SoftwareResampleContext
	audioFrame *astiav.Frame

	framesWritten int64
}

func (state *streamState) free() {
	if state.isCopy {
		return
	}
	if state.decCodecContext != nil {
		state.decCodecContext.Free()
	}
	if state.decFrame != nil {
		state.decFrame.Free()
	}
	if state.encCodecContext != nil {
		state.encCodecContext.Free()
	}
	if state.encPkt != nil {
		state.encPkt.Free()
	}
	if state.swsCtx != nil {
		state.swsCtx.Free()
	}
	if state.scaledFrame != nil {
		state.scaledFrame.Free()
	}
	if state.hwFrame != nil {
		state.hwFrame.Free()
	}
	if state.hwFramesCtx != nil {
		state.hwFramesCtx.Free()
	}
	if state.hwDevCtx != nil {
		state.hwDevCtx.Free()
	}
	if state.swrCtx != nil {
		state.swrCtx.Free()
	}
	if state.audioFrame != nil {
		state.audioFrame.Free()
	}
}

// setupDecoder initialises the decoder codec context for an audio or video stream.
func (state *streamState) setupDecoder(inStream *astiav.Stream, inputFmt *astiav.FormatContext) error {
	state.decCodec = astiav.FindDecoder(inStream.CodecParameters().CodecID())
	if state.decCodec == nil {
		return fmt.Errorf("no decoder for codec ID %v", inStream.CodecParameters().CodecID())
	}

	state.decCodecContext = astiav.AllocCodecContext(state.decCodec)
	if state.decCodecContext == nil {
		return errors.New("failed to allocate decoder codec context")
	}

	if err := inStream.CodecParameters().ToCodecContext(state.decCodecContext); err != nil {
		return fmt.Errorf("copying codec parameters to context: %w", err)
	}

	if state.isVideo {
		state.decCodecContext.SetFramerate(inputFmt.GuessFrameRate(inStream, nil))
	}

	if err := state.decCodecContext.Open(state.decCodec, nil); err != nil {
		return fmt.Errorf("opening decoder: %w", err)
	}
	state.decCodecContext.SetTimeBase(inStream.TimeBase())

	state.decFrame = astiav.AllocFrame()
	if state.decFrame == nil {
		return errors.New("failed to allocate decoder frame")
	}

	return nil
}

// setupVideoEncoder initialises the encoder for a video stream. If a hardware
// encoder is requested but the hardware device cannot be opened (e.g. no GPU
// present), it silently falls back to software encoding.
func (state *streamState) setupVideoEncoder(outputCodec Codec, hwAccel HWAccel, outputFmt *astiav.FormatContext) error {
	enc, profile, useHW, err := state.selectVideoEncoder(outputCodec, hwAccel)
	if err != nil {
		return err
	}
	state.encCodec = enc
	state.isHW = useHW

	if err := state.openVideoEncoderContext(enc, profile, useHW, outputFmt); err != nil {
		return err
	}

	if err := state.setupVideoConversion(profile); err != nil {
		return err
	}

	state.encPkt = astiav.AllocPacket()
	if state.encPkt == nil {
		return errors.New("failed to allocate encoder packet")
	}

	return nil
}

// selectVideoEncoder chooses a hardware or software encoder for the requested
// codec. On hardware selection failure it transparently falls back to software.
func (state *streamState) selectVideoEncoder(outputCodec Codec, hwAccel HWAccel) (enc *astiav.Codec, profile hwProfile, useHW bool, err error) {
	p, hasProfile := hwProfiles[hwAccel]
	if hasProfile {
		var hwEncName string
		switch outputCodec {
		case CodecH264:
			hwEncName = p.h264Encoder
		case CodecH265:
			hwEncName = p.h265Encoder
		}
		if hwEncName != "" && astiav.FindEncoderByName(hwEncName) != nil {
			hwDevCtx, devErr := astiav.CreateHardwareDeviceContext(p.deviceType, "", nil, 0)
			if devErr == nil {
				state.hwDevCtx = hwDevCtx
				return astiav.FindEncoderByName(hwEncName), p, true, nil
			}
			// Device creation failed: fall through to software encoding.
		}
	}

	// Software fallback.
	switch outputCodec {
	case CodecH264:
		enc = astiav.FindEncoder(astiav.CodecIDH264)
	case CodecH265:
		enc = astiav.FindEncoder(astiav.CodecIDH265)
	default:
		return nil, hwProfile{}, false, fmt.Errorf("unsupported video codec: %s", outputCodec)
	}
	if enc == nil {
		// Only reachable if libavcodec was compiled without the requested
		// software encoder (e.g. without libx264/libx265).
		return nil, hwProfile{}, false, fmt.Errorf("no encoder found for video codec %s", outputCodec)
	}

	return enc, hwProfile{}, false, nil
}

// openVideoEncoderContext allocates, configures, and opens the encoder codec
// context. For hardware paths it also sets up the hardware frames context.
func (state *streamState) openVideoEncoderContext(enc *astiav.Codec, profile hwProfile, useHW bool, outputFmt *astiav.FormatContext) error {
	state.encCodecContext = astiav.AllocCodecContext(enc)
	if state.encCodecContext == nil {
		return errors.New("failed to allocate encoder codec context")
	}

	state.encCodecContext.SetWidth(state.decCodecContext.Width())
	state.encCodecContext.SetHeight(state.decCodecContext.Height())
	state.encCodecContext.SetSampleAspectRatio(state.decCodecContext.SampleAspectRatio())
	state.encCodecContext.SetTimeBase(state.decCodecContext.TimeBase())
	state.encCodecContext.SetFramerate(state.decCodecContext.Framerate())

	// Preserve HDR and color metadata.
	state.encCodecContext.SetColorPrimaries(state.decCodecContext.ColorPrimaries())
	state.encCodecContext.SetColorTransferCharacteristic(state.decCodecContext.ColorTransferCharacteristic())
	state.encCodecContext.SetColorSpace(state.decCodecContext.ColorSpace())
	state.encCodecContext.SetColorRange(state.decCodecContext.ColorRange())

	if useHW {
		if err := state.setupHWFramesContext(profile); err != nil {
			return err
		}
		state.encCodecContext.SetPixelFormat(profile.hwPixFmt)
		state.encCodecContext.SetHardwareFramesContext(state.hwFramesCtx)
	} else {
		// Software path: prefer YUV420P; fall back to the encoder's first
		// supported format if it does not support YUV420P.
		encPixFmt := astiav.PixelFormatYuv420P
		for _, f := range enc.SupportedPixelFormats() {
			if f == astiav.PixelFormatYuv420P {
				encPixFmt = astiav.PixelFormatYuv420P
				break
			}
		}
		state.encCodecContext.SetPixelFormat(encPixFmt)
	}

	if outputFmt.OutputFormat().Flags().Has(astiav.IOFormatFlagGlobalheader) {
		state.encCodecContext.SetFlags(state.encCodecContext.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	}

	if err := state.encCodecContext.Open(state.encCodec, nil); err != nil {
		return fmt.Errorf("opening video encoder: %w", err)
	}

	return nil
}

// setupHWFramesContext allocates and initialises the hardware frames context
// used to upload decoded frames into GPU memory before encoding.
func (state *streamState) setupHWFramesContext(profile hwProfile) error {
	state.hwFramesCtx = astiav.AllocHardwareFramesContext(state.hwDevCtx)
	if state.hwFramesCtx == nil {
		return errors.New("failed to allocate hardware frames context")
	}
	state.hwFramesCtx.SetHardwarePixelFormat(profile.hwPixFmt)
	state.hwFramesCtx.SetSoftwarePixelFormat(profile.swPixFmt)
	state.hwFramesCtx.SetWidth(state.decCodecContext.Width())
	state.hwFramesCtx.SetHeight(state.decCodecContext.Height())
	state.hwFramesCtx.SetInitialPoolSize(20)
	if err := state.hwFramesCtx.Initialize(); err != nil {
		return fmt.Errorf("initializing hardware frames context: %w", err)
	}
	return nil
}

// setupVideoConversion sets up any pixel-format conversion and/or hardware
// upload frames needed between decoder output and encoder input.
//
// Note: the current pipeline always uses software decoding. For hardware
// encoding paths this means decoded YUV frames are converted on the CPU
// before being uploaded to GPU memory. A fully hardware-accelerated pipeline
// (hardware decode → hardware encode) would eliminate this CPU step but
// requires hardware decoder support, which is left as a future improvement.
func (state *streamState) setupVideoConversion(profile hwProfile) error {
	decPixFmt := state.decCodecContext.PixelFormat()

	// For hardware encoding, target the software pixel format (e.g. NV12)
	// before uploading; for software encoding, target the encoder's pixel format.
	targetSwPixFmt := state.encCodecContext.PixelFormat()
	if state.isHW {
		targetSwPixFmt = profile.swPixFmt
	}

	if decPixFmt != targetSwPixFmt {
		swsCtx, err := astiav.CreateSoftwareScaleContext(
			state.decCodecContext.Width(), state.decCodecContext.Height(), decPixFmt,
			state.decCodecContext.Width(), state.decCodecContext.Height(), targetSwPixFmt,
			astiav.NewSoftwareScaleContextFlags(astiav.SoftwareScaleContextFlagBilinear),
		)
		if err != nil {
			return fmt.Errorf("creating software scale context: %w", err)
		}
		state.swsCtx = swsCtx

		state.scaledFrame = astiav.AllocFrame()
		if state.scaledFrame == nil {
			return errors.New("failed to allocate scaled frame")
		}
		state.scaledFrame.SetWidth(state.decCodecContext.Width())
		state.scaledFrame.SetHeight(state.decCodecContext.Height())
		state.scaledFrame.SetPixelFormat(targetSwPixFmt)
		if err := state.scaledFrame.AllocBuffer(0); err != nil {
			return fmt.Errorf("allocating scaled frame buffer: %w", err)
		}
	}

	if state.isHW {
		state.hwFrame = astiav.AllocFrame()
		if state.hwFrame == nil {
			return errors.New("failed to allocate hardware frame")
		}
		if err := state.hwFrame.AllocHardwareBuffer(state.hwFramesCtx); err != nil {
			return fmt.Errorf("allocating hardware frame buffer: %w", err)
		}
	}

	return nil
}

// setupAudioEncoder initialises the encoder for an audio stream.
func (state *streamState) setupAudioEncoder(outputCodec Codec, outputFmt *astiav.FormatContext) error {
	switch outputCodec {
	case CodecH264, CodecH265:
		return fmt.Errorf("unsupported audio codec: %s", outputCodec)
	}

	// Re-encode using the same codec as the input (transcode → same format,
	// potentially with a different container).
	enc := astiav.FindEncoder(state.decCodecContext.CodecID())
	if enc == nil {
		return fmt.Errorf("no encoder found for audio codec ID %v", state.decCodecContext.CodecID())
	}
	state.encCodec = enc

	state.encCodecContext = astiav.AllocCodecContext(enc)
	if state.encCodecContext == nil {
		return errors.New("failed to allocate audio encoder codec context")
	}

	// Preserve sample rate and channel layout.
	state.encCodecContext.SetSampleRate(state.decCodecContext.SampleRate())
	if layouts := enc.SupportedChannelLayouts(); len(layouts) > 0 {
		state.encCodecContext.SetChannelLayout(layouts[0])
	} else {
		state.encCodecContext.SetChannelLayout(state.decCodecContext.ChannelLayout())
	}
	if fmts := enc.SupportedSampleFormats(); len(fmts) > 0 {
		state.encCodecContext.SetSampleFormat(fmts[0])
	} else {
		state.encCodecContext.SetSampleFormat(state.decCodecContext.SampleFormat())
	}
	state.encCodecContext.SetTimeBase(astiav.NewRational(1, state.encCodecContext.SampleRate()))

	if outputFmt.OutputFormat().Flags().Has(astiav.IOFormatFlagGlobalheader) {
		state.encCodecContext.SetFlags(state.encCodecContext.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	}

	if err := state.encCodecContext.Open(state.encCodec, nil); err != nil {
		return fmt.Errorf("opening audio encoder: %w", err)
	}

	// Set up resampler if sample format, channel layout, or sample rate differs.
	needResample := state.decCodecContext.SampleFormat() != state.encCodecContext.SampleFormat() ||
		state.decCodecContext.ChannelLayout().Channels() != state.encCodecContext.ChannelLayout().Channels() ||
		state.decCodecContext.SampleRate() != state.encCodecContext.SampleRate()

	if needResample {
		state.swrCtx = astiav.AllocSoftwareResampleContext()
		if state.swrCtx == nil {
			return errors.New("failed to allocate software resample context")
		}

		state.audioFrame = astiav.AllocFrame()
		if state.audioFrame == nil {
			return errors.New("failed to allocate audio resample frame")
		}
		state.audioFrame.SetChannelLayout(state.encCodecContext.ChannelLayout())
		state.audioFrame.SetSampleFormat(state.encCodecContext.SampleFormat())
		state.audioFrame.SetSampleRate(state.encCodecContext.SampleRate())
		state.audioFrame.SetNbSamples(state.decCodecContext.FrameSize())
		if state.audioFrame.NbSamples() <= 0 {
			state.audioFrame.SetNbSamples(1024)
		}
		if err := state.audioFrame.AllocBuffer(0); err != nil {
			return fmt.Errorf("allocating audio resample frame buffer: %w", err)
		}
	}

	state.encPkt = astiav.AllocPacket()
	if state.encPkt == nil {
		return errors.New("failed to allocate encoder packet")
	}

	return nil
}

// TranscodeBuilder constructs a Transcoder using a fluent API.
type TranscodeBuilder struct {
	inputPath, outputPath string
	videoCodec            Codec
	audioCodec            Codec
	container             Container
	hwAccel               HWAccel
	progressCh            chan<- Progress
	logger                func(astiav.LogLevel, string)
	startHook             func()
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

// WithLogger sets a callback that receives FFmpeg log messages. When set,
// FFmpeg log output is forwarded to the callback instead of being suppressed.
// Because the underlying FFmpeg log callback is process-global, this should
// not be combined with concurrent transcodes that each use a different logger.
func (b *TranscodeBuilder) WithLogger(fn func(astiav.LogLevel, string)) *TranscodeBuilder {
	b.logger = fn
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
	effectiveHW := t.resolveHWAccel()
	t.configureLogging()

	inputFmt, interrupter, cancelWatch, err := t.openInputContext(ctx)
	if err != nil {
		return err
	}
	defer cancelWatch()
	defer inputFmt.Free()
	defer inputFmt.CloseInput()

	totalDuration := inputFmt.Duration()

	states, err := t.buildStreamStates(inputFmt)
	if err != nil {
		return err
	}
	defer freeStreamStates(states)

	outputFmt, closeIO, err := t.setupOutputContext(states, inputFmt, effectiveHW)
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

	if err := t.readAllPackets(ctx, inputFmt, outputFmt, states, interrupter, totalDuration); err != nil {
		return err
	}

	if err := t.flushAllEncoders(ctx, outputFmt, states, interrupter, totalDuration); err != nil {
		return err
	}

	return outputFmt.WriteTrailer()
}

// resolveHWAccel resolves HWAccelAuto to a concrete value by probing available
// hardware encoders.
func (t *Transcoder) resolveHWAccel() HWAccel {
	if t.hwAccel != HWAccelAuto {
		return t.hwAccel
	}
	hw, _ := DetectHardwareEncoder()
	return hw
}

// configureLogging sets up FFmpeg log handling. When a logger callback is
// provided it is installed globally; otherwise all output is suppressed.
func (t *Transcoder) configureLogging() {
	if t.logger != nil {
		astiav.SetLogCallback(func(_ astiav.Classer, level astiav.LogLevel, _, msg string) {
			t.logger(level, msg)
		})
		return
	}
	astiav.SetLogLevel(astiav.LogLevelQuiet)
}

// openInputContext opens the input file and arms the IOInterrupter so that a
// cancelled context aborts blocking FFmpeg calls. The returned cancelWatch
// function must be deferred by the caller.
func (t *Transcoder) openInputContext(ctx context.Context) (*astiav.FormatContext, *astiav.IOInterrupter, func(), error) {
	inputFmt := astiav.AllocFormatContext()
	if inputFmt == nil {
		return nil, nil, nil, errors.New("ffmpeg: failed to allocate input format context")
	}

	interrupter := astiav.NewIOInterrupter()
	inputFmt.SetIOInterrupter(interrupter)

	watchDone := make(chan struct{})
	cancelWatch := func() {
		close(watchDone)
		interrupter.Free()
	}
	go func() {
		select {
		case <-ctx.Done():
			interrupter.Interrupt()
		case <-watchDone:
		}
	}()

	if err := inputFmt.OpenInput(t.inputPath, nil, nil); err != nil {
		cancelWatch()
		inputFmt.Free()
		if interrupter.Interrupted() {
			return nil, nil, nil, ctx.Err()
		}
		return nil, nil, nil, fmt.Errorf("ffmpeg: opening input %q: %w", t.inputPath, err)
	}

	if err := inputFmt.FindStreamInfo(nil); err != nil {
		cancelWatch()
		inputFmt.CloseInput()
		inputFmt.Free()
		if interrupter.Interrupted() {
			return nil, nil, nil, ctx.Err()
		}
		return nil, nil, nil, fmt.Errorf("ffmpeg: finding stream info: %w", err)
	}

	return inputFmt, interrupter, cancelWatch, nil
}

// buildStreamStates creates a streamState for every input stream.
// Audio and video streams are set up with a decoder when re-encoding is
// requested. All other stream types (subtitles, attachments, data) are
// always passed through as-is.
//
// Multiple audio tracks are fully supported — each audio stream gets its own
// independent streamState and encoder pipeline.
func (t *Transcoder) buildStreamStates(inputFmt *astiav.FormatContext) (map[int]*streamState, error) {
	states := make(map[int]*streamState)

	for _, inStream := range inputFmt.Streams() {
		mediaType := inStream.CodecParameters().MediaType()

		outputCodec := t.audioCodec
		if mediaType == astiav.MediaTypeVideo {
			outputCodec = t.videoCodec
		}

		// Non-AV streams (subtitles, attachments, data) are always remuxed as-is.
		isCopy := outputCodec == CodecCopy ||
			(mediaType != astiav.MediaTypeVideo && mediaType != astiav.MediaTypeAudio)

		state := &streamState{
			inputStream: inStream,
			isVideo:     mediaType == astiav.MediaTypeVideo,
			isCopy:      isCopy,
		}

		if !isCopy {
			if err := state.setupDecoder(inStream, inputFmt); err != nil {
				freeStreamStates(states)
				return nil, fmt.Errorf("ffmpeg: setting up decoder for stream %d: %w", inStream.Index(), err)
			}
		}

		states[inStream.Index()] = state
	}

	return states, nil
}

// setupOutputContext opens the output format context, creates output streams,
// sets up encoders for re-encoded streams, and opens the IO context for
// file-based output. The returned closeIO function must be deferred by the
// caller and called before the format context is freed — it flushes the IO
// context's buffers.
func (t *Transcoder) setupOutputContext(states map[int]*streamState, inputFmt *astiav.FormatContext, effectiveHW HWAccel) (*astiav.FormatContext, func(), error) {
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
		state, ok := states[inStream.Index()]
		if !ok {
			continue
		}

		if !state.isCopy {
			var setupErr error
			if state.isVideo {
				setupErr = state.setupVideoEncoder(t.videoCodec, effectiveHW, outputFmt)
			} else {
				setupErr = state.setupAudioEncoder(t.audioCodec, outputFmt)
			}
			if setupErr != nil {
				outputFmt.Free()
				return nil, noopClose, fmt.Errorf("ffmpeg: setting up encoder for stream %d: %w", inStream.Index(), setupErr)
			}
		}

		outStream := outputFmt.NewStream(nil)
		if outStream == nil {
			outputFmt.Free()
			return nil, noopClose, errors.New("ffmpeg: failed to create output stream")
		}
		state.outputStream = outStream

		if state.isCopy {
			if err := inStream.CodecParameters().Copy(outStream.CodecParameters()); err != nil {
				outputFmt.Free()
				return nil, noopClose, fmt.Errorf("ffmpeg: copying codec parameters for stream %d: %w", inStream.Index(), err)
			}
			// Clear the source-container codec tag (e.g. mp4a) which would be
			// incompatible with the output container (e.g. matroska).
			outStream.CodecParameters().SetCodecTag(0)
			outStream.SetTimeBase(inStream.TimeBase())
			continue
		}

		if err := outStream.CodecParameters().FromCodecContext(state.encCodecContext); err != nil {
			outputFmt.Free()
			return nil, noopClose, fmt.Errorf("ffmpeg: updating codec parameters for stream %d: %w", inStream.Index(), err)
		}
		outStream.SetTimeBase(state.encCodecContext.TimeBase())
	}

	// Open the IO context for file-based output formats. The caller is
	// responsible for calling closeIO() before freeing the format context so
	// that all buffered output is flushed.
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

// freeStreamStates releases all resources held by a states map.
func freeStreamStates(states map[int]*streamState) {
	for _, state := range states {
		state.free()
	}
}

// readAllPackets is the main decode/encode loop. It reads packets from the
// input and either remuxes or re-encodes them.
func (t *Transcoder) readAllPackets(ctx context.Context, inputFmt, outputFmt *astiav.FormatContext, states map[int]*streamState, interrupter *astiav.IOInterrupter, totalDuration int64) error {
	pkt := astiav.AllocPacket()
	defer pkt.Free()

	for {
		if err := inputFmt.ReadFrame(pkt); err != nil {
			if errors.Is(err, astiav.ErrEof) {
				return nil
			}
			if interrupter.Interrupted() {
				return ctx.Err()
			}
			return fmt.Errorf("ffmpeg: reading frame: %w", err)
		}

		state, ok := states[pkt.StreamIndex()]
		if !ok {
			pkt.Unref()
			continue
		}

		err := t.dispatchPacket(pkt, state, outputFmt, totalDuration)
		pkt.Unref()

		if err != nil {
			if interrupter.Interrupted() {
				return ctx.Err()
			}
			return err
		}
	}
}

// dispatchPacket routes a packet to the appropriate handler based on stream
// type and copy/re-encode mode.
func (t *Transcoder) dispatchPacket(pkt *astiav.Packet, state *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	if state.isCopy {
		return remuxPacket(pkt, state, outputFmt)
	}
	if state.isVideo {
		return t.processVideoPacket(pkt, state, outputFmt, totalDuration)
	}
	return t.processAudioPacket(pkt, state, outputFmt, totalDuration)
}

// flushAllEncoders drains buffered frames from every active encoder.
func (t *Transcoder) flushAllEncoders(ctx context.Context, outputFmt *astiav.FormatContext, states map[int]*streamState, interrupter *astiav.IOInterrupter, totalDuration int64) error {
	for _, state := range states {
		if state.isCopy {
			continue
		}

		var err error
		if state.isVideo {
			err = t.flushVideoEncoder(state, outputFmt, totalDuration)
		} else {
			err = t.flushAudioEncoder(state, outputFmt, totalDuration)
		}

		if err != nil {
			if interrupter.Interrupted() {
				return ctx.Err()
			}
			return err
		}
	}
	return nil
}

// remuxPacket copies a packet directly to the output without decoding/encoding.
func remuxPacket(pkt *astiav.Packet, state *streamState, outputFmt *astiav.FormatContext) error {
	pkt.RescaleTs(state.inputStream.TimeBase(), state.outputStream.TimeBase())
	pkt.SetStreamIndex(state.outputStream.Index())
	if err := outputFmt.WriteInterleavedFrame(pkt); err != nil {
		return fmt.Errorf("ffmpeg: writing remuxed packet for stream %d: %w", state.outputStream.Index(), err)
	}
	return nil
}

// processVideoPacket decodes a video packet and re-encodes each decoded frame.
func (t *Transcoder) processVideoPacket(pkt *astiav.Packet, state *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	pkt.RescaleTs(state.inputStream.TimeBase(), state.decCodecContext.TimeBase())

	if err := state.decCodecContext.SendPacket(pkt); err != nil {
		return fmt.Errorf("ffmpeg: sending video packet to decoder: %w", err)
	}

	for {
		if err := state.decCodecContext.ReceiveFrame(state.decFrame); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				return nil
			}
			return fmt.Errorf("ffmpeg: receiving decoded video frame: %w", err)
		}
		if err := t.encodeVideoFrame(state.decFrame, state, outputFmt, totalDuration); err != nil {
			state.decFrame.Unref()
			return err
		}
		state.decFrame.Unref()
	}
}

// encodeVideoFrame converts and encodes a single decoded video frame.
func (t *Transcoder) encodeVideoFrame(frame *astiav.Frame, state *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	encFrame := frame

	if state.swsCtx != nil {
		if err := state.swsCtx.ScaleFrame(frame, state.scaledFrame); err != nil {
			return fmt.Errorf("ffmpeg: scaling video frame: %w", err)
		}
		state.scaledFrame.SetPts(frame.Pts())
		state.scaledFrame.SetPictureType(astiav.PictureTypeNone)
		encFrame = state.scaledFrame
	}

	if state.isHW {
		if err := encFrame.TransferHardwareData(state.hwFrame); err != nil {
			return fmt.Errorf("ffmpeg: uploading frame to hardware: %w", err)
		}
		state.hwFrame.SetPts(encFrame.Pts())
		state.hwFrame.SetPictureType(astiav.PictureTypeNone)
		encFrame = state.hwFrame
	}

	if err := state.encCodecContext.SendFrame(encFrame); err != nil {
		return fmt.Errorf("ffmpeg: sending video frame to encoder: %w", err)
	}

	return t.receiveAndWritePackets(state, outputFmt, totalDuration)
}

// flushVideoEncoder drains remaining buffered frames from the video encoder.
func (t *Transcoder) flushVideoEncoder(state *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	if err := state.encCodecContext.SendFrame(nil); err != nil {
		return fmt.Errorf("ffmpeg: flushing video encoder: %w", err)
	}
	return t.receiveAndWritePackets(state, outputFmt, totalDuration)
}

// processAudioPacket decodes an audio packet and re-encodes each decoded frame.
func (t *Transcoder) processAudioPacket(pkt *astiav.Packet, state *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	pkt.RescaleTs(state.inputStream.TimeBase(), state.decCodecContext.TimeBase())

	if err := state.decCodecContext.SendPacket(pkt); err != nil {
		return fmt.Errorf("ffmpeg: sending audio packet to decoder: %w", err)
	}

	for {
		if err := state.decCodecContext.ReceiveFrame(state.decFrame); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				return nil
			}
			return fmt.Errorf("ffmpeg: receiving decoded audio frame: %w", err)
		}
		if err := t.encodeAudioFrame(state.decFrame, state, outputFmt, totalDuration); err != nil {
			state.decFrame.Unref()
			return err
		}
		state.decFrame.Unref()
	}
}

// encodeAudioFrame resamples (if needed) and encodes a single decoded audio frame.
func (t *Transcoder) encodeAudioFrame(frame *astiav.Frame, state *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	encFrame := frame

	if state.swrCtx != nil {
		if err := state.swrCtx.ConvertFrame(frame, state.audioFrame); err != nil {
			return fmt.Errorf("ffmpeg: resampling audio frame: %w", err)
		}
		state.audioFrame.SetPts(frame.Pts())
		encFrame = state.audioFrame
	}

	if err := state.encCodecContext.SendFrame(encFrame); err != nil {
		return fmt.Errorf("ffmpeg: sending audio frame to encoder: %w", err)
	}

	return t.receiveAndWritePackets(state, outputFmt, totalDuration)
}

// flushAudioEncoder drains remaining buffered frames from the audio encoder.
func (t *Transcoder) flushAudioEncoder(state *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	if err := state.encCodecContext.SendFrame(nil); err != nil {
		return fmt.Errorf("ffmpeg: flushing audio encoder: %w", err)
	}
	return t.receiveAndWritePackets(state, outputFmt, totalDuration)
}

// receiveAndWritePackets drains encoded packets from the encoder and writes them.
func (t *Transcoder) receiveAndWritePackets(state *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	for {
		if err := state.encCodecContext.ReceivePacket(state.encPkt); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				return nil
			}
			return fmt.Errorf("ffmpeg: receiving encoded packet: %w", err)
		}

		state.framesWritten++
		state.encPkt.SetStreamIndex(state.outputStream.Index())
		state.encPkt.RescaleTs(state.encCodecContext.TimeBase(), state.outputStream.TimeBase())

		if t.progressCh != nil {
			t.sendProgress(state, totalDuration)
		}

		if err := outputFmt.WriteInterleavedFrame(state.encPkt); err != nil {
			state.encPkt.Unref()
			return fmt.Errorf("ffmpeg: writing encoded packet: %w", err)
		}
		state.encPkt.Unref()
	}
}

// sendProgress emits a non-blocking progress update on t.progressCh.
func (t *Transcoder) sendProgress(state *streamState, totalDuration int64) {
	var pct float64
	if totalDuration > 0 {
		tb := state.outputStream.TimeBase()
		ptsInMicros := float64(state.encPkt.Pts()) * float64(tb.Num()) / float64(tb.Den()) * 1e6
		pct = ptsInMicros / float64(totalDuration) * 100
		if pct > 100 {
			pct = 100
		}
		if pct < 0 {
			pct = 0
		}
	}
	select {
	case t.progressCh <- Progress{FramesProcessed: state.framesWritten, PercentComplete: pct}:
	default:
	}
}
