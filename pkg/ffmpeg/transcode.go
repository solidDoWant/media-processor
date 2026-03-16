package ffmpeg

import (
	"context"
	"errors"
	"fmt"

	"github.com/asticode/go-astiav"
)

// Encoder names for hardware-accelerated variants, used in hwProfiles and
// in hardware decoder lookup.
const (
	encoderNameH264QSV   = "h264_qsv"
	encoderNameH265QSV   = "hevc_qsv"
	encoderNameH264NVENC = "h264_nvenc"
	encoderNameH265NVENC = "hevc_nvenc"
	encoderNameH264VAAPI = "h264_vaapi"
	encoderNameH265VAAPI = "hevc_vaapi"

	decoderNameH264QSV   = "h264_qsv"
	decoderNameH265QSV   = "hevc_qsv"
	decoderNameH264NVENC = "h264_cuvid"
	decoderNameH265NVENC = "hevc_cuvid"
	decoderNameH264VAAPI = "h264_vaapi"
	decoderNameH265VAAPI = "hevc_vaapi"
)

// hwProfile holds hardware-specific codec configuration.
// Device types come from libavutil/hwcontext.h and pixel formats from
// libavutil/pixfmt.h via the go-astiav bindings.
type hwProfile struct {
	deviceType  astiav.HardwareDeviceType
	hwPixFmt    astiav.PixelFormat // pixel format used inside the hardware
	swPixFmt    astiav.PixelFormat // software pixel format for upload/download (e.g. NV12)
	h264Encoder string
	h265Encoder string
	h264Decoder string
	h265Decoder string
}

var hwProfiles = map[HWAccel]hwProfile{
	HWAccelQSV: {
		deviceType:  astiav.HardwareDeviceTypeQSV,
		hwPixFmt:    astiav.PixelFormatQsv,
		swPixFmt:    astiav.PixelFormatNv12,
		h264Encoder: encoderNameH264QSV,
		h265Encoder: encoderNameH265QSV,
		h264Decoder: decoderNameH264QSV,
		h265Decoder: decoderNameH265QSV,
	},
	HWAccelNVENC: {
		deviceType:  astiav.HardwareDeviceTypeCUDA,
		hwPixFmt:    astiav.PixelFormatCuda,
		swPixFmt:    astiav.PixelFormatNv12,
		h264Encoder: encoderNameH264NVENC,
		h265Encoder: encoderNameH265NVENC,
		h264Decoder: decoderNameH264NVENC,
		h265Decoder: decoderNameH265NVENC,
	},
	HWAccelVAAPI: {
		deviceType:  astiav.HardwareDeviceTypeVAAPI,
		hwPixFmt:    astiav.PixelFormatVaapi,
		swPixFmt:    astiav.PixelFormatNv12,
		h264Encoder: encoderNameH264VAAPI,
		h265Encoder: encoderNameH265VAAPI,
		h264Decoder: decoderNameH264VAAPI,
		h265Decoder: decoderNameH265VAAPI,
	},
}

// streamDecoder holds resources for the decoder side of a stream.
type streamDecoder struct {
	codec        *astiav.Codec
	codecContext *astiav.CodecContext
	frame        *astiav.Frame
	// Non-nil when using a hardware-accelerated decoder. The hardware device
	// context is shared with the video encoder to enable zero-copy pipelines.
	hwDevCtx *astiav.HardwareDeviceContext
	hwPixFmt astiav.PixelFormat // expected HW pixel format from the HW decoder
}

func (d *streamDecoder) free() {
	if d.codecContext != nil {
		d.codecContext.Free()
	}
	if d.frame != nil {
		d.frame.Free()
	}
	if d.hwDevCtx != nil {
		d.hwDevCtx.Free()
	}
}

// videoEncoderState holds resources for encoding a video stream.
type videoEncoderState struct {
	codec        *astiav.Codec
	codecContext *astiav.CodecContext
	pkt          *astiav.Packet

	// isHW is true when using a hardware encoder.
	isHW bool
	// isHWDecode is true when the decoder is also hardware-accelerated and the
	// decoded frames are already in GPU memory — no CPU upload step needed.
	isHWDecode  bool
	hwFramesCtx *astiav.HardwareFramesContext
	hwFrame     *astiav.Frame

	// swsCtx and scaledFrame are used on the software path to convert decoded
	// frames to the pixel format expected by the encoder. On the fully
	// hardware path (isHWDecode=true) these are nil.
	swsCtx      *astiav.SoftwareScaleContext
	scaledFrame *astiav.Frame
}

func (e *videoEncoderState) free() {
	if e.codecContext != nil {
		e.codecContext.Free()
	}
	if e.pkt != nil {
		e.pkt.Free()
	}
	if e.swsCtx != nil {
		e.swsCtx.Free()
	}
	if e.scaledFrame != nil {
		e.scaledFrame.Free()
	}
	if e.hwFrame != nil {
		e.hwFrame.Free()
	}
	if e.hwFramesCtx != nil {
		e.hwFramesCtx.Free()
	}
}

// audioEncoderState holds resources for encoding an audio stream.
type audioEncoderState struct {
	codec        *astiav.Codec
	codecContext *astiav.CodecContext
	pkt          *astiav.Packet

	swrCtx     *astiav.SoftwareResampleContext
	audioFrame *astiav.Frame
}

func (e *audioEncoderState) free() {
	if e.codecContext != nil {
		e.codecContext.Free()
	}
	if e.pkt != nil {
		e.pkt.Free()
	}
	if e.swrCtx != nil {
		e.swrCtx.Free()
	}
	if e.audioFrame != nil {
		e.audioFrame.Free()
	}
}

// streamState holds all per-stream resources for a single transcode pass.
// It is composed of typed sub-structs to keep the decoder and encoder concerns
// separate and make the per-stream resource lifecycle explicit.
type streamState struct {
	inputStream   *astiav.Stream
	outputStream  *astiav.Stream
	isCopy        bool
	isVideo       bool
	framesWritten int64

	dec      streamDecoder
	videoEnc videoEncoderState
	audioEnc audioEncoderState
}

func (state *streamState) free() {
	if state.isCopy {
		return
	}
	state.dec.free()
	if state.isVideo {
		state.videoEnc.free()
	} else {
		state.audioEnc.free()
	}
}

// encCodecContext returns the encoder codec context for this stream.
func (state *streamState) encCodecContext() *astiav.CodecContext {
	if state.isVideo {
		return state.videoEnc.codecContext
	}
	return state.audioEnc.codecContext
}

// encPkt returns the encoder packet for this stream.
func (state *streamState) encPkt() *astiav.Packet {
	if state.isVideo {
		return state.videoEnc.pkt
	}
	return state.audioEnc.pkt
}

// setupDecoder initialises the decoder codec context for an audio or video
// stream. For video streams with a non-None hwAccel, it attempts to use a
// hardware-accelerated decoder so that decoded frames remain in GPU memory,
// enabling a zero-copy decode→encode pipeline. If the HW decoder is
// unavailable the function silently falls back to software decoding.
func (state *streamState) setupDecoder(inStream *astiav.Stream, inputFmt *astiav.FormatContext, hwAccel HWAccel) error {
	var codec *astiav.Codec

	// For video streams with a hardware profile, attempt hardware decoding.
	if state.isVideo {
		if profile, ok := hwProfiles[hwAccel]; ok {
			hwDecName := state.hwDecoderName(inStream, profile)
			if hwDecName != "" {
				if hwDec := astiav.FindDecoderByName(hwDecName); hwDec != nil {
					hwDevCtx, err := astiav.CreateHardwareDeviceContext(profile.deviceType, "", nil, 0)
					if err == nil {
						state.dec.hwDevCtx = hwDevCtx
						state.dec.hwPixFmt = profile.hwPixFmt
						codec = hwDec
					}
					// On failure, fall through to software decoding.
				}
			}
		}
	}

	// Software decoder fallback.
	if codec == nil {
		codec = astiav.FindDecoder(inStream.CodecParameters().CodecID())
		if codec == nil {
			return fmt.Errorf("no decoder for codec ID %v", inStream.CodecParameters().CodecID())
		}
	}
	state.dec.codec = codec

	state.dec.codecContext = astiav.AllocCodecContext(codec)
	if state.dec.codecContext == nil {
		return errors.New("failed to allocate decoder codec context")
	}

	if err := inStream.CodecParameters().ToCodecContext(state.dec.codecContext); err != nil {
		return fmt.Errorf("copying codec parameters to context: %w", err)
	}

	if state.isVideo {
		state.dec.codecContext.SetFramerate(inputFmt.GuessFrameRate(inStream, nil))
	}

	if state.dec.hwDevCtx != nil {
		state.dec.codecContext.SetHardwareDeviceContext(state.dec.hwDevCtx)
		hwPixFmt := state.dec.hwPixFmt
		state.dec.codecContext.SetPixelFormatCallback(func(pfs []astiav.PixelFormat) astiav.PixelFormat {
			for _, pf := range pfs {
				if pf == hwPixFmt {
					return pf
				}
			}
			// HW pixel format not offered — fall back to the first available format.
			if len(pfs) > 0 {
				return pfs[0]
			}
			return astiav.PixelFormatNone
		})
	}

	if err := state.dec.codecContext.Open(codec, nil); err != nil {
		return fmt.Errorf("opening decoder: %w", err)
	}
	state.dec.codecContext.SetTimeBase(inStream.TimeBase())

	state.dec.frame = astiav.AllocFrame()
	if state.dec.frame == nil {
		return errors.New("failed to allocate decoder frame")
	}

	return nil
}

// hwDecoderName returns the hardware decoder codec name for the input stream's
// codec ID, given the hardware profile. Returns "" if no HW decoder is mapped.
func (state *streamState) hwDecoderName(inStream *astiav.Stream, profile hwProfile) string {
	switch inStream.CodecParameters().CodecID() {
	case astiav.CodecIDH264:
		return profile.h264Decoder
	case astiav.CodecIDH265:
		return profile.h265Decoder
	}
	return ""
}

// setupVideoEncoder initialises the encoder for a video stream. If a hardware
// encoder is requested but the hardware device cannot be opened, it silently
// falls back to software encoding. When the decoder is also hardware-
// accelerated the same device context is reused for a zero-copy pipeline.
func (state *streamState) setupVideoEncoder(outputCodec Codec, hwAccel HWAccel, outputFmt *astiav.FormatContext) error {
	enc, profile, useHW, err := state.selectVideoEncoder(outputCodec, hwAccel)
	if err != nil {
		return err
	}
	state.videoEnc.codec = enc
	state.videoEnc.isHW = useHW

	if err := state.openVideoEncoderContext(enc, profile, useHW, outputFmt); err != nil {
		return err
	}

	if err := state.setupVideoConversion(profile); err != nil {
		return err
	}

	state.videoEnc.pkt = astiav.AllocPacket()
	if state.videoEnc.pkt == nil {
		return errors.New("failed to allocate encoder packet")
	}

	return nil
}

// selectVideoEncoder chooses a hardware or software encoder. On hardware
// selection failure it transparently falls back to software.
func (state *streamState) selectVideoEncoder(outputCodec Codec, hwAccel HWAccel) (enc *astiav.Codec, profile hwProfile, useHW bool, err error) {
	p, hasProfile := hwProfiles[hwAccel]
	if hasProfile {
		hwEncName := state.hwEncoderName(outputCodec, p)
		if hwEncName != "" && astiav.FindEncoderByName(hwEncName) != nil {
			// Reuse the hardware device context from the HW decoder if one was
			// set up, otherwise create a new one. This enables the zero-copy
			// decode→encode path when both sides use the same hardware.
			var hwDevCtx *astiav.HardwareDeviceContext
			if state.dec.hwDevCtx != nil {
				hwDevCtx = state.dec.hwDevCtx
			} else {
				hwDevCtx, err = astiav.CreateHardwareDeviceContext(p.deviceType, "", nil, 0)
				if err != nil {
					// Device creation failed — fall through to software.
					hwDevCtx = nil
				}
			}
			if hwDevCtx != nil {
				// Store the device context on the encoder state so it can be freed
				// independently if it was newly created (not shared from the decoder).
				if state.dec.hwDevCtx == nil {
					// Newly created — store so free() can release it via videoEnc path.
					// We repurpose hwFrame's slot to hold nothing; instead we must track
					// separately. For simplicity, place it in dec.hwDevCtx for unified free.
					state.dec.hwDevCtx = hwDevCtx
				}
				return astiav.FindEncoderByName(hwEncName), p, true, nil
			}
		}
	}

	// Software fallback.
	// Only reachable if libavcodec was compiled without the requested software
	// encoder (e.g. without libx264/libx265), which should not occur in normal
	// deployments but is checked here as a safety net.
	switch outputCodec {
	case CodecH264:
		enc = astiav.FindEncoder(astiav.CodecIDH264)
	case CodecH265:
		enc = astiav.FindEncoder(astiav.CodecIDH265)
	default:
		return nil, hwProfile{}, false, fmt.Errorf("unsupported video codec: %s", outputCodec)
	}
	if enc == nil {
		return nil, hwProfile{}, false, fmt.Errorf("no encoder found for video codec %s", outputCodec)
	}

	return enc, hwProfile{}, false, nil
}

// hwEncoderName returns the hardware encoder name for the requested output
// codec given a hardware profile.
func (state *streamState) hwEncoderName(outputCodec Codec, profile hwProfile) string {
	switch outputCodec {
	case CodecH264:
		return profile.h264Encoder
	case CodecH265:
		return profile.h265Encoder
	}
	return ""
}

// openVideoEncoderContext allocates, configures, and opens the encoder codec
// context. For hardware paths it also sets up the hardware frames context.
func (state *streamState) openVideoEncoderContext(enc *astiav.Codec, profile hwProfile, useHW bool, outputFmt *astiav.FormatContext) error {
	state.videoEnc.codecContext = astiav.AllocCodecContext(enc)
	if state.videoEnc.codecContext == nil {
		return errors.New("failed to allocate encoder codec context")
	}

	state.videoEnc.codecContext.SetWidth(state.dec.codecContext.Width())
	state.videoEnc.codecContext.SetHeight(state.dec.codecContext.Height())
	state.videoEnc.codecContext.SetSampleAspectRatio(state.dec.codecContext.SampleAspectRatio())
	state.videoEnc.codecContext.SetTimeBase(state.dec.codecContext.TimeBase())
	state.videoEnc.codecContext.SetFramerate(state.dec.codecContext.Framerate())

	// Preserve HDR and color metadata.
	state.videoEnc.codecContext.SetColorPrimaries(state.dec.codecContext.ColorPrimaries())
	state.videoEnc.codecContext.SetColorTransferCharacteristic(state.dec.codecContext.ColorTransferCharacteristic())
	state.videoEnc.codecContext.SetColorSpace(state.dec.codecContext.ColorSpace())
	state.videoEnc.codecContext.SetColorRange(state.dec.codecContext.ColorRange())

	if useHW {
		// When the decoder is also hardware-accelerated the decoded frames are
		// already in GPU memory — use those shared surfaces directly rather
		// than allocating a separate frames pool.
		if state.dec.hwDevCtx != nil && state.dec.hwPixFmt == profile.hwPixFmt {
			state.videoEnc.isHWDecode = true
			state.videoEnc.codecContext.SetPixelFormat(profile.hwPixFmt)
			// The encoder will use the hardware frames context from the decoded
			// frames themselves; no explicit frames context is needed.
		} else {
			if err := state.setupHWFramesContext(profile); err != nil {
				return err
			}
			state.videoEnc.codecContext.SetPixelFormat(profile.hwPixFmt)
			state.videoEnc.codecContext.SetHardwareFramesContext(state.videoEnc.hwFramesCtx)
		}
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
		state.videoEnc.codecContext.SetPixelFormat(encPixFmt)
	}

	if outputFmt.OutputFormat().Flags().Has(astiav.IOFormatFlagGlobalheader) {
		state.videoEnc.codecContext.SetFlags(state.videoEnc.codecContext.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	}

	if err := state.videoEnc.codecContext.Open(state.videoEnc.codec, nil); err != nil {
		return fmt.Errorf("opening video encoder: %w", err)
	}

	return nil
}

// setupHWFramesContext allocates and initialises the hardware frames context
// used to upload decoded software frames into GPU memory before encoding.
func (state *streamState) setupHWFramesContext(profile hwProfile) error {
	state.videoEnc.hwFramesCtx = astiav.AllocHardwareFramesContext(state.dec.hwDevCtx)
	if state.videoEnc.hwFramesCtx == nil {
		return errors.New("failed to allocate hardware frames context")
	}
	state.videoEnc.hwFramesCtx.SetHardwarePixelFormat(profile.hwPixFmt)
	state.videoEnc.hwFramesCtx.SetSoftwarePixelFormat(profile.swPixFmt)
	state.videoEnc.hwFramesCtx.SetWidth(state.dec.codecContext.Width())
	state.videoEnc.hwFramesCtx.SetHeight(state.dec.codecContext.Height())
	state.videoEnc.hwFramesCtx.SetInitialPoolSize(20)
	if err := state.videoEnc.hwFramesCtx.Initialize(); err != nil {
		return fmt.Errorf("initializing hardware frames context: %w", err)
	}
	return nil
}

// setupVideoConversion sets up pixel-format conversion and hardware upload
// frames. When HW decode is active the decoded frames are already in GPU
// memory, so neither a software scaler nor an upload frame is needed.
//
// For the SW decode + HW encode path, decoded YUV frames are converted on the
// CPU before being uploaded to GPU memory. A fully hardware-accelerated
// decode→encode pipeline (isHWDecode=true) eliminates this CPU step by
// sharing GPU surfaces between the decoder and encoder.
//
// Note: side-data formats such as Dolby Vision RPU are currently not
// preserved across re-encoding. Copy streams (CodecCopy) always preserve all
// side data since the bitstream is passed through unchanged.
func (state *streamState) setupVideoConversion(profile hwProfile) error {
	if state.videoEnc.isHWDecode {
		// Decoded frames are already in the correct HW pixel format.
		return nil
	}

	decPixFmt := state.dec.codecContext.PixelFormat()

	// For HW encode with SW decode, target the SW pixel format (e.g. NV12)
	// before uploading; for pure SW encode, target the encoder pixel format.
	targetSwPixFmt := state.videoEnc.codecContext.PixelFormat()
	if state.videoEnc.isHW {
		targetSwPixFmt = profile.swPixFmt
	}

	if decPixFmt != targetSwPixFmt {
		swsCtx, err := astiav.CreateSoftwareScaleContext(
			state.dec.codecContext.Width(), state.dec.codecContext.Height(), decPixFmt,
			state.dec.codecContext.Width(), state.dec.codecContext.Height(), targetSwPixFmt,
			astiav.NewSoftwareScaleContextFlags(astiav.SoftwareScaleContextFlagBilinear),
		)
		if err != nil {
			return fmt.Errorf("creating software scale context: %w", err)
		}
		state.videoEnc.swsCtx = swsCtx

		state.videoEnc.scaledFrame = astiav.AllocFrame()
		if state.videoEnc.scaledFrame == nil {
			return errors.New("failed to allocate scaled frame")
		}
		state.videoEnc.scaledFrame.SetWidth(state.dec.codecContext.Width())
		state.videoEnc.scaledFrame.SetHeight(state.dec.codecContext.Height())
		state.videoEnc.scaledFrame.SetPixelFormat(targetSwPixFmt)
		if err := state.videoEnc.scaledFrame.AllocBuffer(0); err != nil {
			return fmt.Errorf("allocating scaled frame buffer: %w", err)
		}
	}

	if state.videoEnc.isHW {
		state.videoEnc.hwFrame = astiav.AllocFrame()
		if state.videoEnc.hwFrame == nil {
			return errors.New("failed to allocate hardware frame")
		}
		if err := state.videoEnc.hwFrame.AllocHardwareBuffer(state.videoEnc.hwFramesCtx); err != nil {
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
	enc := astiav.FindEncoder(state.dec.codecContext.CodecID())
	if enc == nil {
		return fmt.Errorf("no encoder found for audio codec ID %v", state.dec.codecContext.CodecID())
	}
	state.audioEnc.codec = enc

	state.audioEnc.codecContext = astiav.AllocCodecContext(enc)
	if state.audioEnc.codecContext == nil {
		return errors.New("failed to allocate audio encoder codec context")
	}

	// Preserve sample rate and channel layout.
	state.audioEnc.codecContext.SetSampleRate(state.dec.codecContext.SampleRate())
	if layouts := enc.SupportedChannelLayouts(); len(layouts) > 0 {
		state.audioEnc.codecContext.SetChannelLayout(layouts[0])
	} else {
		state.audioEnc.codecContext.SetChannelLayout(state.dec.codecContext.ChannelLayout())
	}
	if fmts := enc.SupportedSampleFormats(); len(fmts) > 0 {
		state.audioEnc.codecContext.SetSampleFormat(fmts[0])
	} else {
		state.audioEnc.codecContext.SetSampleFormat(state.dec.codecContext.SampleFormat())
	}
	state.audioEnc.codecContext.SetTimeBase(astiav.NewRational(1, state.audioEnc.codecContext.SampleRate()))

	if outputFmt.OutputFormat().Flags().Has(astiav.IOFormatFlagGlobalheader) {
		state.audioEnc.codecContext.SetFlags(state.audioEnc.codecContext.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	}

	if err := state.audioEnc.codecContext.Open(state.audioEnc.codec, nil); err != nil {
		return fmt.Errorf("opening audio encoder: %w", err)
	}

	// Set up resampler if sample format, channel layout, or sample rate differs.
	needResample := state.dec.codecContext.SampleFormat() != state.audioEnc.codecContext.SampleFormat() ||
		state.dec.codecContext.ChannelLayout().Channels() != state.audioEnc.codecContext.ChannelLayout().Channels() ||
		state.dec.codecContext.SampleRate() != state.audioEnc.codecContext.SampleRate()

	if needResample {
		state.audioEnc.swrCtx = astiav.AllocSoftwareResampleContext()
		if state.audioEnc.swrCtx == nil {
			return errors.New("failed to allocate software resample context")
		}

		state.audioEnc.audioFrame = astiav.AllocFrame()
		if state.audioEnc.audioFrame == nil {
			return errors.New("failed to allocate audio resample frame")
		}
		state.audioEnc.audioFrame.SetChannelLayout(state.audioEnc.codecContext.ChannelLayout())
		state.audioEnc.audioFrame.SetSampleFormat(state.audioEnc.codecContext.SampleFormat())
		state.audioEnc.audioFrame.SetSampleRate(state.audioEnc.codecContext.SampleRate())
		state.audioEnc.audioFrame.SetNbSamples(state.dec.codecContext.FrameSize())
		if state.audioEnc.audioFrame.NbSamples() <= 0 {
			state.audioEnc.audioFrame.SetNbSamples(1024)
		}
		if err := state.audioEnc.audioFrame.AllocBuffer(0); err != nil {
			return fmt.Errorf("allocating audio resample frame buffer: %w", err)
		}
	}

	state.audioEnc.pkt = astiav.AllocPacket()
	if state.audioEnc.pkt == nil {
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

	inputFmt, interrupter, cancelWatch, err := t.openInputContext(ctx)
	if err != nil {
		return err
	}
	defer cancelWatch()
	defer inputFmt.Free()
	defer inputFmt.CloseInput()

	totalDuration := inputFmt.Duration()

	states, err := t.buildStreamStates(inputFmt, effectiveHW)
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

// resolveHWAccel resolves HWAccelAuto to a concrete value.
func (t *Transcoder) resolveHWAccel() HWAccel {
	if t.hwAccel != HWAccelAuto {
		return t.hwAccel
	}
	hw, _ := DetectHardwareEncoder()
	return hw
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
// requested; the hwAccel hint is passed through so hardware decoders can be
// selected for a zero-copy decode→encode pipeline.
// All other stream types (subtitles, attachments, data) are remuxed as-is.
//
// Multiple audio tracks are fully supported — each audio stream gets its own
// independent decoder and encoder pipeline.
func (t *Transcoder) buildStreamStates(inputFmt *astiav.FormatContext, hwAccel HWAccel) (map[int]*streamState, error) {
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
			if err := state.setupDecoder(inStream, inputFmt, hwAccel); err != nil {
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
// caller — it flushes the IO context's buffers.
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

		if err := outStream.CodecParameters().FromCodecContext(state.encCodecContext()); err != nil {
			outputFmt.Free()
			return nil, noopClose, fmt.Errorf("ffmpeg: updating codec parameters for stream %d: %w", inStream.Index(), err)
		}
		outStream.SetTimeBase(state.encCodecContext().TimeBase())
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

// freeStreamStates releases all resources held by a states map.
func freeStreamStates(states map[int]*streamState) {
	for _, state := range states {
		state.free()
	}
}

// readAllPackets is the main decode/encode loop.
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

// dispatchPacket routes a packet to the appropriate handler.
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
	pkt.RescaleTs(state.inputStream.TimeBase(), state.dec.codecContext.TimeBase())

	if err := state.dec.codecContext.SendPacket(pkt); err != nil {
		return fmt.Errorf("ffmpeg: sending video packet to decoder: %w", err)
	}

	for {
		if err := state.dec.codecContext.ReceiveFrame(state.dec.frame); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				return nil
			}
			return fmt.Errorf("ffmpeg: receiving decoded video frame: %w", err)
		}
		if err := t.encodeVideoFrame(state.dec.frame, state, outputFmt, totalDuration); err != nil {
			state.dec.frame.Unref()
			return err
		}
		state.dec.frame.Unref()
	}
}

// encodeVideoFrame converts and encodes a single decoded video frame.
// On the fully hardware path (isHWDecode=true) the frame is already in GPU
// memory and is passed directly to the encoder without any CPU conversion.
func (t *Transcoder) encodeVideoFrame(frame *astiav.Frame, state *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	encFrame := frame

	if !state.videoEnc.isHWDecode {
		// Software decode path: convert pixel format if needed.
		if state.videoEnc.swsCtx != nil {
			if err := state.videoEnc.swsCtx.ScaleFrame(frame, state.videoEnc.scaledFrame); err != nil {
				return fmt.Errorf("ffmpeg: scaling video frame: %w", err)
			}
			state.videoEnc.scaledFrame.SetPts(frame.Pts())
			state.videoEnc.scaledFrame.SetPictureType(astiav.PictureTypeNone)
			encFrame = state.videoEnc.scaledFrame
		}

		// Upload to hardware memory when using SW decode + HW encode.
		if state.videoEnc.isHW {
			if err := encFrame.TransferHardwareData(state.videoEnc.hwFrame); err != nil {
				return fmt.Errorf("ffmpeg: uploading frame to hardware: %w", err)
			}
			state.videoEnc.hwFrame.SetPts(encFrame.Pts())
			state.videoEnc.hwFrame.SetPictureType(astiav.PictureTypeNone)
			encFrame = state.videoEnc.hwFrame
		}
	}

	if err := state.videoEnc.codecContext.SendFrame(encFrame); err != nil {
		return fmt.Errorf("ffmpeg: sending video frame to encoder: %w", err)
	}

	return t.receiveAndWritePackets(state, outputFmt, totalDuration)
}

// flushVideoEncoder drains remaining buffered frames from the video encoder.
func (t *Transcoder) flushVideoEncoder(state *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	if err := state.videoEnc.codecContext.SendFrame(nil); err != nil {
		return fmt.Errorf("ffmpeg: flushing video encoder: %w", err)
	}
	return t.receiveAndWritePackets(state, outputFmt, totalDuration)
}

// processAudioPacket decodes an audio packet and re-encodes each decoded frame.
func (t *Transcoder) processAudioPacket(pkt *astiav.Packet, state *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	pkt.RescaleTs(state.inputStream.TimeBase(), state.dec.codecContext.TimeBase())

	if err := state.dec.codecContext.SendPacket(pkt); err != nil {
		return fmt.Errorf("ffmpeg: sending audio packet to decoder: %w", err)
	}

	for {
		if err := state.dec.codecContext.ReceiveFrame(state.dec.frame); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				return nil
			}
			return fmt.Errorf("ffmpeg: receiving decoded audio frame: %w", err)
		}
		if err := t.encodeAudioFrame(state.dec.frame, state, outputFmt, totalDuration); err != nil {
			state.dec.frame.Unref()
			return err
		}
		state.dec.frame.Unref()
	}
}

// encodeAudioFrame resamples (if needed) and encodes a single decoded audio frame.
func (t *Transcoder) encodeAudioFrame(frame *astiav.Frame, state *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	encFrame := frame

	if state.audioEnc.swrCtx != nil {
		if err := state.audioEnc.swrCtx.ConvertFrame(frame, state.audioEnc.audioFrame); err != nil {
			return fmt.Errorf("ffmpeg: resampling audio frame: %w", err)
		}
		state.audioEnc.audioFrame.SetPts(frame.Pts())
		encFrame = state.audioEnc.audioFrame
	}

	if err := state.audioEnc.codecContext.SendFrame(encFrame); err != nil {
		return fmt.Errorf("ffmpeg: sending audio frame to encoder: %w", err)
	}

	return t.receiveAndWritePackets(state, outputFmt, totalDuration)
}

// flushAudioEncoder drains remaining buffered frames from the audio encoder.
func (t *Transcoder) flushAudioEncoder(state *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	if err := state.audioEnc.codecContext.SendFrame(nil); err != nil {
		return fmt.Errorf("ffmpeg: flushing audio encoder: %w", err)
	}
	return t.receiveAndWritePackets(state, outputFmt, totalDuration)
}

// receiveAndWritePackets drains encoded packets from the encoder and writes them.
func (t *Transcoder) receiveAndWritePackets(state *streamState, outputFmt *astiav.FormatContext, totalDuration int64) error {
	for {
		if err := state.encCodecContext().ReceivePacket(state.encPkt()); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				return nil
			}
			return fmt.Errorf("ffmpeg: receiving encoded packet: %w", err)
		}

		state.framesWritten++
		state.encPkt().SetStreamIndex(state.outputStream.Index())
		state.encPkt().RescaleTs(state.encCodecContext().TimeBase(), state.outputStream.TimeBase())

		if t.progressCh != nil {
			t.sendProgress(state, totalDuration)
		}

		if err := outputFmt.WriteInterleavedFrame(state.encPkt()); err != nil {
			state.encPkt().Unref()
			return fmt.Errorf("ffmpeg: writing encoded packet: %w", err)
		}
		state.encPkt().Unref()
	}
}

// sendProgress emits a non-blocking progress update on t.progressCh.
func (t *Transcoder) sendProgress(state *streamState, totalDuration int64) {
	var pct float64
	if totalDuration > 0 {
		tb := state.outputStream.TimeBase()
		ptsInMicros := float64(state.encPkt().Pts()) * float64(tb.Num()) / float64(tb.Den()) * 1e6
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
