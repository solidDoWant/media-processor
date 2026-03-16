package ffmpeg

import (
	"context"
	"errors"
	"fmt"

	"github.com/asticode/go-astiav"
)

// hwProfile holds hardware-specific encoder configuration.
type hwProfile struct {
	deviceType  astiav.HardwareDeviceType
	hwPixFmt    astiav.PixelFormat
	swPixFmt    astiav.PixelFormat
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

func (ss *streamState) free() {
	if ss.isCopy {
		return
	}
	if ss.decCodecContext != nil {
		ss.decCodecContext.Free()
	}
	if ss.decFrame != nil {
		ss.decFrame.Free()
	}
	if ss.encCodecContext != nil {
		ss.encCodecContext.Free()
	}
	if ss.encPkt != nil {
		ss.encPkt.Free()
	}
	if ss.swsCtx != nil {
		ss.swsCtx.Free()
	}
	if ss.scaledFrame != nil {
		ss.scaledFrame.Free()
	}
	if ss.hwFrame != nil {
		ss.hwFrame.Free()
	}
	if ss.hwFramesCtx != nil {
		ss.hwFramesCtx.Free()
	}
	if ss.hwDevCtx != nil {
		ss.hwDevCtx.Free()
	}
	if ss.swrCtx != nil {
		ss.swrCtx.Free()
	}
	if ss.audioFrame != nil {
		ss.audioFrame.Free()
	}
}

// TranscodeBuilder constructs a Transcoder using a fluent API.
type TranscodeBuilder struct {
	inputPath, outputPath string
	videoCodec            Codec
	audioCodec            Codec
	container             Container
	hwAccel               HWAccel
	progressCh            chan<- Progress
}

// NewTranscode returns a builder for a transcode job from inputPath to outputPath.
// Default codecs are CodecCopy for both video and audio.
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

// ToContainer sets the output container format.
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

// Build returns a runnable Transcoder.
func (b *TranscodeBuilder) Build() *Transcoder {
	return &Transcoder{
		inputPath:  b.inputPath,
		outputPath: b.outputPath,
		videoCodec: b.videoCodec,
		audioCodec: b.audioCodec,
		container:  b.container,
		hwAccel:    b.hwAccel,
		progressCh: b.progressCh,
	}
}

// Transcoder is a ready-to-run transcode job produced by TranscodeBuilder.Build.
type Transcoder struct {
	inputPath, outputPath string
	videoCodec            Codec
	audioCodec            Codec
	container             Container
	hwAccel               HWAccel
	progressCh            chan<- Progress
}

// Run executes the transcode job. It blocks until the job completes, the
// context is cancelled, or an error occurs. A cancelled context causes Run to
// return promptly with ctx.Err().
func (t *Transcoder) Run(ctx context.Context) error {
	// Resolve HWAccelAuto
	effectiveHW := t.hwAccel
	if t.hwAccel == HWAccelAuto {
		hw, _ := DetectHardwareEncoder()
		effectiveHW = hw
	}

	// Suppress noisy FFmpeg log output
	astiav.SetLogLevel(astiav.LogLevelQuiet)

	// Open input format context
	inputFmt := astiav.AllocFormatContext()
	if inputFmt == nil {
		return errors.New("ffmpeg: failed to allocate input format context")
	}
	defer inputFmt.Free()

	// IOInterrupter for context cancellation
	interrupter := astiav.NewIOInterrupter()
	inputFmt.SetIOInterrupter(interrupter)
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

	if err := inputFmt.OpenInput(t.inputPath, nil, nil); err != nil {
		if interrupter.Interrupted() {
			return ctx.Err()
		}
		return fmt.Errorf("ffmpeg: opening input %q: %w", t.inputPath, err)
	}
	defer inputFmt.CloseInput()

	if err := inputFmt.FindStreamInfo(nil); err != nil {
		if interrupter.Interrupted() {
			return ctx.Err()
		}
		return fmt.Errorf("ffmpeg: finding stream info: %w", err)
	}

	// totalDuration is in AV_TIME_BASE units (microseconds).
	totalDuration := inputFmt.Duration()

	// Phase 1: set up decoders for all audio/video streams.
	states := make(map[int]*streamState)
	for _, is := range inputFmt.Streams() {
		mt := is.CodecParameters().MediaType()
		if mt != astiav.MediaTypeVideo && mt != astiav.MediaTypeAudio {
			continue
		}
		isVideo := mt == astiav.MediaTypeVideo
		outputCodec := t.audioCodec
		if isVideo {
			outputCodec = t.videoCodec
		}

		ss := &streamState{
			inputStream: is,
			isVideo:     isVideo,
			isCopy:      outputCodec == CodecCopy,
		}

		if !ss.isCopy {
			if err := setupDecoder(ss, is, inputFmt); err != nil {
				for _, existing := range states {
					existing.free()
				}
				return fmt.Errorf("ffmpeg: setting up decoder for stream %d: %w", is.Index(), err)
			}
		}

		states[is.Index()] = ss
	}
	defer func() {
		for _, ss := range states {
			ss.free()
		}
	}()

	// Phase 2: open output format context.
	outputFmt, err := astiav.AllocOutputFormatContext(nil, string(t.container), t.outputPath)
	if err != nil {
		return fmt.Errorf("ffmpeg: allocating output format context: %w", err)
	}
	if outputFmt == nil {
		return errors.New("ffmpeg: nil output format context")
	}
	defer outputFmt.Free()

	// Phase 3: set up encoders and create output streams (in input stream order).
	for _, is := range inputFmt.Streams() {
		ss, ok := states[is.Index()]
		if !ok {
			continue
		}

		outputCodec := t.audioCodec
		if ss.isVideo {
			outputCodec = t.videoCodec
		}

		if !ss.isCopy {
			var setupErr error
			if ss.isVideo {
				setupErr = setupVideoEncoder(ss, outputCodec, effectiveHW, outputFmt)
			} else {
				setupErr = setupAudioEncoder(ss, outputCodec, outputFmt)
			}
			if setupErr != nil {
				return fmt.Errorf("ffmpeg: setting up encoder for stream %d: %w", is.Index(), setupErr)
			}
		}

		os := outputFmt.NewStream(nil)
		if os == nil {
			return errors.New("ffmpeg: failed to create output stream")
		}
		ss.outputStream = os

		if ss.isCopy {
			if err := is.CodecParameters().Copy(os.CodecParameters()); err != nil {
				return fmt.Errorf("ffmpeg: copying codec parameters for stream %d: %w", is.Index(), err)
			}
			// Clear the source-container codec tag (e.g. mp4a) which would be
			// incompatible with the output container (e.g. matroska).
			os.CodecParameters().SetCodecTag(0)
			os.SetTimeBase(is.TimeBase())
		} else {
			if err := os.CodecParameters().FromCodecContext(ss.encCodecContext); err != nil {
				return fmt.Errorf("ffmpeg: updating codec parameters for stream %d: %w", is.Index(), err)
			}
			os.SetTimeBase(ss.encCodecContext.TimeBase())
		}
	}

	// Open output IO context for file-based outputs.
	if !outputFmt.OutputFormat().Flags().Has(astiav.IOFormatFlagNofile) {
		ioCtx, err := astiav.OpenIOContext(t.outputPath, astiav.NewIOContextFlags(astiav.IOContextFlagWrite), nil, nil)
		if err != nil {
			return fmt.Errorf("ffmpeg: opening output io context: %w", err)
		}
		defer func() { _ = ioCtx.Close() }()
		outputFmt.SetPb(ioCtx)
	}

	if err := outputFmt.WriteHeader(nil); err != nil {
		return fmt.Errorf("ffmpeg: writing header: %w", err)
	}

	// Main packet read loop.
	pkt := astiav.AllocPacket()
	defer pkt.Free()

	for {
		if err := inputFmt.ReadFrame(pkt); err != nil {
			if errors.Is(err, astiav.ErrEof) {
				break
			}
			if interrupter.Interrupted() {
				return ctx.Err()
			}
			return fmt.Errorf("ffmpeg: reading frame: %w", err)
		}

		ss, ok := states[pkt.StreamIndex()]
		if !ok {
			pkt.Unref()
			continue
		}

		var processErr error
		if ss.isCopy {
			processErr = remuxPacket(pkt, ss, outputFmt)
		} else if ss.isVideo {
			processErr = t.processVideoPacket(pkt, ss, outputFmt, totalDuration)
		} else {
			processErr = t.processAudioPacket(pkt, ss, outputFmt, totalDuration)
		}
		pkt.Unref()

		if processErr != nil {
			if interrupter.Interrupted() {
				return ctx.Err()
			}
			return processErr
		}
	}

	// Flush all encoders.
	for _, ss := range states {
		if ss.isCopy {
			continue
		}
		var flushErr error
		if ss.isVideo {
			flushErr = t.flushVideoEncoder(ss, outputFmt, totalDuration)
		} else {
			flushErr = t.flushAudioEncoder(ss, outputFmt, totalDuration)
		}
		if flushErr != nil {
			if interrupter.Interrupted() {
				return ctx.Err()
			}
			return flushErr
		}
	}

	if err := outputFmt.WriteTrailer(); err != nil {
		return fmt.Errorf("ffmpeg: writing trailer: %w", err)
	}

	return nil
}

// setupDecoder initialises the decoder codec context for a stream.
func setupDecoder(ss *streamState, is *astiav.Stream, inputFmt *astiav.FormatContext) error {
	ss.decCodec = astiav.FindDecoder(is.CodecParameters().CodecID())
	if ss.decCodec == nil {
		return fmt.Errorf("no decoder for codec ID %v", is.CodecParameters().CodecID())
	}

	ss.decCodecContext = astiav.AllocCodecContext(ss.decCodec)
	if ss.decCodecContext == nil {
		return errors.New("failed to allocate decoder codec context")
	}

	if err := is.CodecParameters().ToCodecContext(ss.decCodecContext); err != nil {
		return fmt.Errorf("copying codec parameters to context: %w", err)
	}

	if ss.isVideo {
		ss.decCodecContext.SetFramerate(inputFmt.GuessFrameRate(is, nil))
	}

	if err := ss.decCodecContext.Open(ss.decCodec, nil); err != nil {
		return fmt.Errorf("opening decoder: %w", err)
	}
	ss.decCodecContext.SetTimeBase(is.TimeBase())

	ss.decFrame = astiav.AllocFrame()
	if ss.decFrame == nil {
		return errors.New("failed to allocate decoder frame")
	}

	return nil
}

// setupVideoEncoder initialises the encoder for a video stream. If a hardware
// encoder is requested but the hardware device cannot be opened (e.g. no GPU
// present), it silently falls back to software encoding.
func setupVideoEncoder(ss *streamState, outputCodec Codec, hwAccel HWAccel, outputFmt *astiav.FormatContext) error {
	profile, hasProfile := hwProfiles[hwAccel]

	// Attempt hardware encoder if a profile exists.
	useHW := false
	if hasProfile {
		var encName string
		switch outputCodec {
		case CodecH264:
			encName = profile.h264Encoder
		case CodecH265:
			encName = profile.h265Encoder
		}
		if encName != "" && astiav.FindEncoderByName(encName) != nil {
			hwDevCtx, err := astiav.CreateHardwareDeviceContext(profile.deviceType, "", nil, 0)
			if err == nil {
				ss.hwDevCtx = hwDevCtx
				useHW = true
			}
			// On error, silently fall through to software encoding.
		}
	}

	// Choose encoder.
	var enc *astiav.Codec
	if useHW {
		var encName string
		switch outputCodec {
		case CodecH264:
			encName = profile.h264Encoder
		case CodecH265:
			encName = profile.h265Encoder
		}
		enc = astiav.FindEncoderByName(encName)
	}
	if enc == nil {
		// Software fallback.
		useHW = false
		if ss.hwDevCtx != nil {
			ss.hwDevCtx.Free()
			ss.hwDevCtx = nil
		}
		switch outputCodec {
		case CodecH264:
			enc = astiav.FindEncoder(astiav.CodecIDH264)
		case CodecH265:
			enc = astiav.FindEncoder(astiav.CodecIDH265)
		default:
			return fmt.Errorf("unsupported video codec: %s", outputCodec)
		}
	}
	if enc == nil {
		return fmt.Errorf("no encoder found for video codec %s", outputCodec)
	}
	ss.encCodec = enc
	ss.isHW = useHW

	ss.encCodecContext = astiav.AllocCodecContext(enc)
	if ss.encCodecContext == nil {
		return errors.New("failed to allocate encoder codec context")
	}

	ss.encCodecContext.SetWidth(ss.decCodecContext.Width())
	ss.encCodecContext.SetHeight(ss.decCodecContext.Height())
	ss.encCodecContext.SetSampleAspectRatio(ss.decCodecContext.SampleAspectRatio())
	ss.encCodecContext.SetTimeBase(ss.decCodecContext.TimeBase())
	ss.encCodecContext.SetFramerate(ss.decCodecContext.Framerate())

	if useHW {
		// Set up hardware frames context.
		ss.hwFramesCtx = astiav.AllocHardwareFramesContext(ss.hwDevCtx)
		if ss.hwFramesCtx == nil {
			return errors.New("failed to allocate hardware frames context")
		}
		ss.hwFramesCtx.SetHardwarePixelFormat(profile.hwPixFmt)
		ss.hwFramesCtx.SetSoftwarePixelFormat(profile.swPixFmt)
		ss.hwFramesCtx.SetWidth(ss.decCodecContext.Width())
		ss.hwFramesCtx.SetHeight(ss.decCodecContext.Height())
		ss.hwFramesCtx.SetInitialPoolSize(20)
		if err := ss.hwFramesCtx.Initialize(); err != nil {
			return fmt.Errorf("initializing hardware frames context: %w", err)
		}
		ss.encCodecContext.SetPixelFormat(profile.hwPixFmt)
		ss.encCodecContext.SetHardwareFramesContext(ss.hwFramesCtx)
	} else {
		// Software path: pick the encoder's preferred pixel format.
		encPixFmt := astiav.PixelFormatYuv420P
		for _, f := range enc.SupportedPixelFormats() {
			if f == astiav.PixelFormatYuv420P {
				encPixFmt = astiav.PixelFormatYuv420P
				break
			}
		}
		ss.encCodecContext.SetPixelFormat(encPixFmt)
	}

	if outputFmt.OutputFormat().Flags().Has(astiav.IOFormatFlagGlobalheader) {
		ss.encCodecContext.SetFlags(ss.encCodecContext.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	}

	if err := ss.encCodecContext.Open(ss.encCodec, nil); err != nil {
		return fmt.Errorf("opening video encoder: %w", err)
	}

	// Set up pixel-format conversion if the decoder output differs from encoder input.
	decPixFmt := ss.decCodecContext.PixelFormat()
	targetSwPixFmt := ss.encCodecContext.PixelFormat()
	if ss.isHW {
		targetSwPixFmt = profile.swPixFmt // convert to NV12 before hardware upload
	}

	if decPixFmt != targetSwPixFmt {
		swsCtx, err := astiav.CreateSoftwareScaleContext(
			ss.decCodecContext.Width(), ss.decCodecContext.Height(), decPixFmt,
			ss.decCodecContext.Width(), ss.decCodecContext.Height(), targetSwPixFmt,
			astiav.NewSoftwareScaleContextFlags(astiav.SoftwareScaleContextFlagBilinear),
		)
		if err != nil {
			return fmt.Errorf("creating software scale context: %w", err)
		}
		ss.swsCtx = swsCtx

		ss.scaledFrame = astiav.AllocFrame()
		if ss.scaledFrame == nil {
			return errors.New("failed to allocate scaled frame")
		}
		ss.scaledFrame.SetWidth(ss.decCodecContext.Width())
		ss.scaledFrame.SetHeight(ss.decCodecContext.Height())
		ss.scaledFrame.SetPixelFormat(targetSwPixFmt)
		if err := ss.scaledFrame.AllocBuffer(0); err != nil {
			return fmt.Errorf("allocating scaled frame buffer: %w", err)
		}
	}

	if ss.isHW {
		ss.hwFrame = astiav.AllocFrame()
		if ss.hwFrame == nil {
			return errors.New("failed to allocate hardware frame")
		}
		if err := ss.hwFrame.AllocHardwareBuffer(ss.hwFramesCtx); err != nil {
			return fmt.Errorf("allocating hardware frame buffer: %w", err)
		}
	}

	ss.encPkt = astiav.AllocPacket()
	if ss.encPkt == nil {
		return errors.New("failed to allocate encoder packet")
	}

	return nil
}

// setupAudioEncoder initialises the encoder for an audio stream.
func setupAudioEncoder(ss *streamState, outputCodec Codec, outputFmt *astiav.FormatContext) error {
	var enc *astiav.Codec
	switch outputCodec {
	case CodecH264, CodecH265:
		return fmt.Errorf("unsupported audio codec: %s", outputCodec)
	default:
		// For audio, use codec ID-based lookup (e.g., aac → CodecIDAac).
		enc = astiav.FindEncoder(ss.decCodecContext.CodecID())
	}
	if enc == nil {
		return fmt.Errorf("no encoder found for audio codec ID %v", ss.decCodecContext.CodecID())
	}
	ss.encCodec = enc

	ss.encCodecContext = astiav.AllocCodecContext(enc)
	if ss.encCodecContext == nil {
		return errors.New("failed to allocate audio encoder codec context")
	}

	// Preserve sample rate and channel layout.
	ss.encCodecContext.SetSampleRate(ss.decCodecContext.SampleRate())
	if layouts := enc.SupportedChannelLayouts(); len(layouts) > 0 {
		ss.encCodecContext.SetChannelLayout(layouts[0])
	} else {
		ss.encCodecContext.SetChannelLayout(ss.decCodecContext.ChannelLayout())
	}
	if fmts := enc.SupportedSampleFormats(); len(fmts) > 0 {
		ss.encCodecContext.SetSampleFormat(fmts[0])
	} else {
		ss.encCodecContext.SetSampleFormat(ss.decCodecContext.SampleFormat())
	}
	ss.encCodecContext.SetTimeBase(astiav.NewRational(1, ss.encCodecContext.SampleRate()))

	if outputFmt.OutputFormat().Flags().Has(astiav.IOFormatFlagGlobalheader) {
		ss.encCodecContext.SetFlags(ss.encCodecContext.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	}

	if err := ss.encCodecContext.Open(ss.encCodec, nil); err != nil {
		return fmt.Errorf("opening audio encoder: %w", err)
	}

	// Set up resampler if sample format or channel layout differs.
	needResample := ss.decCodecContext.SampleFormat() != ss.encCodecContext.SampleFormat() ||
		ss.decCodecContext.ChannelLayout().Channels() != ss.encCodecContext.ChannelLayout().Channels() ||
		ss.decCodecContext.SampleRate() != ss.encCodecContext.SampleRate()

	if needResample {
		ss.swrCtx = astiav.AllocSoftwareResampleContext()
		if ss.swrCtx == nil {
			return errors.New("failed to allocate software resample context")
		}

		ss.audioFrame = astiav.AllocFrame()
		if ss.audioFrame == nil {
			return errors.New("failed to allocate audio resample frame")
		}
		ss.audioFrame.SetChannelLayout(ss.encCodecContext.ChannelLayout())
		ss.audioFrame.SetSampleFormat(ss.encCodecContext.SampleFormat())
		ss.audioFrame.SetSampleRate(ss.encCodecContext.SampleRate())
		ss.audioFrame.SetNbSamples(ss.decCodecContext.FrameSize())
		if ss.audioFrame.NbSamples() <= 0 {
			ss.audioFrame.SetNbSamples(1024)
		}
		if err := ss.audioFrame.AllocBuffer(0); err != nil {
			return fmt.Errorf("allocating audio resample frame buffer: %w", err)
		}
	}

	ss.encPkt = astiav.AllocPacket()
	if ss.encPkt == nil {
		return errors.New("failed to allocate encoder packet")
	}

	return nil
}

// remuxPacket copies a packet directly to the output without decoding/encoding.
func remuxPacket(pkt *astiav.Packet, ss *streamState, outputFmt *astiav.FormatContext) error {
	pkt.RescaleTs(ss.inputStream.TimeBase(), ss.outputStream.TimeBase())
	pkt.SetStreamIndex(ss.outputStream.Index())
	if err := outputFmt.WriteInterleavedFrame(pkt); err != nil {
		return fmt.Errorf("ffmpeg: writing remuxed packet for stream %d: %w", ss.outputStream.Index(), err)
	}
	return nil
}

// processVideoPacket decodes a video packet and re-encodes each decoded frame.
func (t *Transcoder) processVideoPacket(pkt *astiav.Packet, ss *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	pkt.RescaleTs(ss.inputStream.TimeBase(), ss.decCodecContext.TimeBase())

	if err := ss.decCodecContext.SendPacket(pkt); err != nil {
		return fmt.Errorf("ffmpeg: sending video packet to decoder: %w", err)
	}

	for {
		if err := ss.decCodecContext.ReceiveFrame(ss.decFrame); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				break
			}
			return fmt.Errorf("ffmpeg: receiving decoded video frame: %w", err)
		}
		if err := t.encodeVideoFrame(ss.decFrame, ss, outputFmt, totalDuration); err != nil {
			ss.decFrame.Unref()
			return err
		}
		ss.decFrame.Unref()
	}
	return nil
}

// encodeVideoFrame converts and encodes a single decoded video frame.
func (t *Transcoder) encodeVideoFrame(frame *astiav.Frame, ss *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	encFrame := frame

	// Pixel-format conversion (software scale).
	if ss.swsCtx != nil {
		if err := ss.swsCtx.ScaleFrame(frame, ss.scaledFrame); err != nil {
			return fmt.Errorf("ffmpeg: scaling video frame: %w", err)
		}
		ss.scaledFrame.SetPts(frame.Pts())
		ss.scaledFrame.SetPictureType(astiav.PictureTypeNone)
		encFrame = ss.scaledFrame
	}

	// Upload to hardware memory.
	if ss.isHW {
		if err := encFrame.TransferHardwareData(ss.hwFrame); err != nil {
			return fmt.Errorf("ffmpeg: uploading frame to hardware: %w", err)
		}
		ss.hwFrame.SetPts(encFrame.Pts())
		ss.hwFrame.SetPictureType(astiav.PictureTypeNone)
		encFrame = ss.hwFrame
	}

	if err := ss.encCodecContext.SendFrame(encFrame); err != nil {
		return fmt.Errorf("ffmpeg: sending video frame to encoder: %w", err)
	}

	return t.receiveAndWritePackets(ss, outputFmt, totalDuration)
}

// flushVideoEncoder drains remaining buffered frames from the video encoder.
func (t *Transcoder) flushVideoEncoder(ss *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	if err := ss.encCodecContext.SendFrame(nil); err != nil {
		return fmt.Errorf("ffmpeg: flushing video encoder: %w", err)
	}
	return t.receiveAndWritePackets(ss, outputFmt, totalDuration)
}

// processAudioPacket decodes an audio packet and re-encodes each decoded frame.
func (t *Transcoder) processAudioPacket(pkt *astiav.Packet, ss *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	pkt.RescaleTs(ss.inputStream.TimeBase(), ss.decCodecContext.TimeBase())

	if err := ss.decCodecContext.SendPacket(pkt); err != nil {
		return fmt.Errorf("ffmpeg: sending audio packet to decoder: %w", err)
	}

	for {
		if err := ss.decCodecContext.ReceiveFrame(ss.decFrame); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				break
			}
			return fmt.Errorf("ffmpeg: receiving decoded audio frame: %w", err)
		}
		if err := t.encodeAudioFrame(ss.decFrame, ss, outputFmt, totalDuration); err != nil {
			ss.decFrame.Unref()
			return err
		}
		ss.decFrame.Unref()
	}
	return nil
}

// encodeAudioFrame resamples (if needed) and encodes a single decoded audio frame.
func (t *Transcoder) encodeAudioFrame(frame *astiav.Frame, ss *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	encFrame := frame

	if ss.swrCtx != nil {
		if err := ss.swrCtx.ConvertFrame(frame, ss.audioFrame); err != nil {
			return fmt.Errorf("ffmpeg: resampling audio frame: %w", err)
		}
		ss.audioFrame.SetPts(frame.Pts())
		encFrame = ss.audioFrame
	}

	if err := ss.encCodecContext.SendFrame(encFrame); err != nil {
		return fmt.Errorf("ffmpeg: sending audio frame to encoder: %w", err)
	}

	return t.receiveAndWritePackets(ss, outputFmt, totalDuration)
}

// flushAudioEncoder drains remaining buffered frames from the audio encoder.
func (t *Transcoder) flushAudioEncoder(ss *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	if err := ss.encCodecContext.SendFrame(nil); err != nil {
		return fmt.Errorf("ffmpeg: flushing audio encoder: %w", err)
	}
	return t.receiveAndWritePackets(ss, outputFmt, totalDuration)
}

// receiveAndWritePackets drains encoded packets from the encoder and writes them.
func (t *Transcoder) receiveAndWritePackets(ss *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	for {
		if err := ss.encCodecContext.ReceivePacket(ss.encPkt); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				break
			}
			return fmt.Errorf("ffmpeg: receiving encoded packet: %w", err)
		}

		ss.framesWritten++
		ss.encPkt.SetStreamIndex(ss.outputStream.Index())
		ss.encPkt.RescaleTs(ss.encCodecContext.TimeBase(), ss.outputStream.TimeBase())

		if t.progressCh != nil {
			t.sendProgress(ss, totalDuration)
		}

		if err := outputFmt.WriteInterleavedFrame(ss.encPkt); err != nil {
			ss.encPkt.Unref()
			return fmt.Errorf("ffmpeg: writing encoded packet: %w", err)
		}
		ss.encPkt.Unref()
	}
	return nil
}

// sendProgress emits a non-blocking progress update on t.progressCh.
func (t *Transcoder) sendProgress(ss *streamState, totalDuration int64) {
	var pct float64
	if totalDuration > 0 {
		tb := ss.outputStream.TimeBase()
		ptsInMicros := float64(ss.encPkt.Pts()) * float64(tb.Num()) / float64(tb.Den()) * 1e6
		pct = ptsInMicros / float64(totalDuration) * 100
		if pct > 100 {
			pct = 100
		}
		if pct < 0 {
			pct = 0
		}
	}
	select {
	case t.progressCh <- Progress{FramesProcessed: ss.framesWritten, PercentComplete: pct}:
	default:
	}
}
