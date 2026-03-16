package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/asticode/go-astiav"
)

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

// streamBase holds fields common to all stream processing types.
type streamBase struct {
	inStream  *astiav.Stream
	outStream *astiav.Stream
	frames    int64 // encoded frames written, used for progress reporting
}

func (s *streamBase) inputStream() *astiav.Stream         { return s.inStream }
func (s *streamBase) outputStream() *astiav.Stream        { return s.outStream }
func (s *streamBase) setOutputStream(out *astiav.Stream)  { s.outStream = out }

// stream is the interface for all per-stream processing types.
// copyStreamState, videoStreamState, and audioStreamState implement it.
type stream interface {
	inputStream() *astiav.Stream
	outputStream() *astiav.Stream
	setOutputStream(*astiav.Stream)
	// setupEncoder configures the encoder and allocates encoder resources.
	// For copy streams this is a no-op.
	setupEncoder(hwAccel HWAccel, outputFmt *astiav.FormatContext) error
	// encoderContext returns the encoder codec context used to populate output
	// stream parameters. Returns nil for copy streams.
	encoderContext() *astiav.CodecContext
	// processPacket handles an incoming demuxed packet.
	processPacket(pkt *astiav.Packet, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error
	// flush drains any buffered encoder output. No-op for copy streams.
	flush(outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error
	free()
}

// ---- copyStreamState -------------------------------------------------------

// copyStreamState passes packets through to the output without re-encoding.
// Used for subtitle, attachment, data, and copy-codec audio/video streams.
type copyStreamState struct {
	streamBase
}

func (s *copyStreamState) setupEncoder(_ HWAccel, _ *astiav.FormatContext) error { return nil }
func (s *copyStreamState) encoderContext() *astiav.CodecContext                   { return nil }

func (s *copyStreamState) processPacket(pkt *astiav.Packet, outputFmt *astiav.FormatContext, _ chan<- Progress, _ int64) error {
	return remuxPacket(pkt, s.inStream, s.outStream, outputFmt)
}

func (s *copyStreamState) flush(_ *astiav.FormatContext, _ chan<- Progress, _ int64) error {
	return nil
}

func (s *copyStreamState) free() {}

// ---- videoStreamState ------------------------------------------------------

// videoStreamState decodes and re-encodes a video stream.
type videoStreamState struct {
	streamBase
	dec         streamDecoder
	enc         videoEncoderState
	outputCodec Codec
}

func (s *videoStreamState) encoderContext() *astiav.CodecContext { return s.enc.codecContext }

func (s *videoStreamState) free() {
	s.dec.free()
	s.enc.free()
}

// setupDecoder initialises the decoder codec context for the video stream.
// For non-None hwAccel it attempts hardware decoding so that decoded frames
// remain in GPU memory, enabling a zero-copy decode→encode pipeline.
// Falls back silently to software decoding if HW decode is unavailable.
func (s *videoStreamState) setupDecoder(inStream *astiav.Stream, inputFmt *astiav.FormatContext, hwAccel HWAccel) error {
	var codec *astiav.Codec

	if profile, ok := hwProfiles[hwAccel]; ok {
		hwDecName := hwDecoderNameForCodecID(inStream.CodecParameters().CodecID(), profile)
		if hwDecName != "" {
			if hwDec := astiav.FindDecoderByName(hwDecName); hwDec != nil {
				hwDevCtx, err := astiav.CreateHardwareDeviceContext(profile.deviceType, "", nil, 0)
				if err == nil {
					s.dec.hwDevCtx = hwDevCtx
					s.dec.hwPixFmt = profile.hwPixFmt
					codec = hwDec
				}
				// On failure, fall through to software decoding.
			}
		}
	}

	if codec == nil {
		codec = astiav.FindDecoder(inStream.CodecParameters().CodecID())
		if codec == nil {
			return fmt.Errorf("no decoder for codec ID %v", inStream.CodecParameters().CodecID())
		}
	}
	s.dec.codec = codec

	s.dec.codecContext = astiav.AllocCodecContext(codec)
	if s.dec.codecContext == nil {
		return errors.New("failed to allocate decoder codec context")
	}

	if err := inStream.CodecParameters().ToCodecContext(s.dec.codecContext); err != nil {
		return fmt.Errorf("copying codec parameters to context: %w", err)
	}

	s.dec.codecContext.SetFramerate(inputFmt.GuessFrameRate(inStream, nil))

	if s.dec.hwDevCtx != nil {
		s.dec.codecContext.SetHardwareDeviceContext(s.dec.hwDevCtx)
		hwPixFmt := s.dec.hwPixFmt
		s.dec.codecContext.SetPixelFormatCallback(func(pfs []astiav.PixelFormat) astiav.PixelFormat {
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

	if err := s.dec.codecContext.Open(codec, nil); err != nil {
		return fmt.Errorf("opening decoder: %w", err)
	}
	s.dec.codecContext.SetTimeBase(inStream.TimeBase())

	s.dec.frame = astiav.AllocFrame()
	if s.dec.frame == nil {
		return errors.New("failed to allocate decoder frame")
	}

	return nil
}

// setupEncoder implements the stream interface for video. If a hardware encoder
// is requested but unavailable, it falls back to software encoding transparently.
func (s *videoStreamState) setupEncoder(hwAccel HWAccel, outputFmt *astiav.FormatContext) error {
	enc, profile, useHW, err := s.selectVideoEncoder(hwAccel)
	if err != nil {
		return err
	}
	s.enc.codec = enc
	s.enc.isHW = useHW

	if err := s.openVideoEncoderContext(enc, profile, useHW, outputFmt); err != nil {
		return err
	}

	if err := s.setupVideoConversion(profile); err != nil {
		return err
	}

	s.enc.pkt = astiav.AllocPacket()
	if s.enc.pkt == nil {
		return errors.New("failed to allocate encoder packet")
	}

	return nil
}

// selectVideoEncoder chooses a hardware or software encoder. On hardware
// selection failure it transparently falls back to software.
func (s *videoStreamState) selectVideoEncoder(hwAccel HWAccel) (enc *astiav.Codec, profile hwProfile, useHW bool, err error) {
	p, hasProfile := hwProfiles[hwAccel]
	if hasProfile {
		hwEncName := hwEncoderNameForCodec(s.outputCodec, p)
		if hwEncName != "" && astiav.FindEncoderByName(hwEncName) != nil {
			// Reuse the hardware device context from the HW decoder if one was
			// set up, otherwise create a new one. This enables the zero-copy
			// decode→encode path when both sides use the same hardware.
			var hwDevCtx *astiav.HardwareDeviceContext
			if s.dec.hwDevCtx != nil {
				hwDevCtx = s.dec.hwDevCtx
			} else {
				hwDevCtx, err = astiav.CreateHardwareDeviceContext(p.deviceType, "", nil, 0)
				if err != nil {
					hwDevCtx = nil
				}
			}
			if hwDevCtx != nil {
				if s.dec.hwDevCtx == nil {
					// Newly created — store so free() releases it via dec.free().
					s.dec.hwDevCtx = hwDevCtx
				}
				return astiav.FindEncoderByName(hwEncName), p, true, nil
			}
		}
	}

	// Software fallback.
	// Only reachable if libavcodec was compiled without the requested software
	// encoder (e.g. without libx264/libx265), which should not occur in normal
	// deployments but is checked here as a safety net.
	switch s.outputCodec {
	case CodecH264:
		enc = astiav.FindEncoder(astiav.CodecIDH264)
	case CodecH265:
		enc = astiav.FindEncoder(astiav.CodecIDH265)
	default:
		return nil, hwProfile{}, false, fmt.Errorf("unsupported video codec: %s", s.outputCodec)
	}
	if enc == nil {
		return nil, hwProfile{}, false, fmt.Errorf("no encoder found for video codec %s", s.outputCodec)
	}

	return enc, hwProfile{}, false, nil
}

// openVideoEncoderContext allocates, configures, and opens the encoder codec
// context. For hardware paths it also sets up the hardware frames context.
func (s *videoStreamState) openVideoEncoderContext(enc *astiav.Codec, profile hwProfile, useHW bool, outputFmt *astiav.FormatContext) error {
	s.enc.codecContext = astiav.AllocCodecContext(enc)
	if s.enc.codecContext == nil {
		return errors.New("failed to allocate encoder codec context")
	}

	s.enc.codecContext.SetWidth(s.dec.codecContext.Width())
	s.enc.codecContext.SetHeight(s.dec.codecContext.Height())
	s.enc.codecContext.SetSampleAspectRatio(s.dec.codecContext.SampleAspectRatio())
	s.enc.codecContext.SetTimeBase(s.dec.codecContext.TimeBase())
	s.enc.codecContext.SetFramerate(s.dec.codecContext.Framerate())

	// Preserve HDR and color metadata.
	s.enc.codecContext.SetColorPrimaries(s.dec.codecContext.ColorPrimaries())
	s.enc.codecContext.SetColorTransferCharacteristic(s.dec.codecContext.ColorTransferCharacteristic())
	s.enc.codecContext.SetColorSpace(s.dec.codecContext.ColorSpace())
	s.enc.codecContext.SetColorRange(s.dec.codecContext.ColorRange())

	if useHW {
		// When the decoder is also hardware-accelerated the decoded frames are
		// already in GPU memory — use those shared surfaces directly rather
		// than allocating a separate frames pool.
		if s.dec.hwDevCtx != nil && s.dec.hwPixFmt == profile.hwPixFmt {
			s.enc.isHWDecode = true
			s.enc.codecContext.SetPixelFormat(profile.hwPixFmt)
			// The encoder will use the hardware frames context from the decoded
			// frames themselves; no explicit frames context is needed.
		} else {
			if err := s.setupHWFramesContext(profile); err != nil {
				return err
			}
			s.enc.codecContext.SetPixelFormat(profile.hwPixFmt)
			s.enc.codecContext.SetHardwareFramesContext(s.enc.hwFramesCtx)
		}
	} else {
		// Software path: prefer YUV420P; fall back to the encoder's first
		// supported format if it does not support YUV420P.
		encPixFmt := astiav.PixelFormatYuv420P
		fmts := enc.SupportedPixelFormats()
		if len(fmts) > 0 && !slices.Contains(fmts, astiav.PixelFormatYuv420P) {
			encPixFmt = fmts[0]
		}
		s.enc.codecContext.SetPixelFormat(encPixFmt)
	}

	if outputFmt.OutputFormat().Flags().Has(astiav.IOFormatFlagGlobalheader) {
		s.enc.codecContext.SetFlags(s.enc.codecContext.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	}

	if err := s.enc.codecContext.Open(s.enc.codec, nil); err != nil {
		return fmt.Errorf("opening video encoder: %w", err)
	}

	return nil
}

// setupHWFramesContext allocates and initialises the hardware frames context
// used to upload decoded software frames into GPU memory before encoding.
func (s *videoStreamState) setupHWFramesContext(profile hwProfile) error {
	s.enc.hwFramesCtx = astiav.AllocHardwareFramesContext(s.dec.hwDevCtx)
	if s.enc.hwFramesCtx == nil {
		return errors.New("failed to allocate hardware frames context")
	}
	s.enc.hwFramesCtx.SetHardwarePixelFormat(profile.hwPixFmt)
	s.enc.hwFramesCtx.SetSoftwarePixelFormat(profile.swPixFmt)
	s.enc.hwFramesCtx.SetWidth(s.dec.codecContext.Width())
	s.enc.hwFramesCtx.SetHeight(s.dec.codecContext.Height())
	s.enc.hwFramesCtx.SetInitialPoolSize(20)
	if err := s.enc.hwFramesCtx.Initialize(); err != nil {
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
func (s *videoStreamState) setupVideoConversion(profile hwProfile) error {
	if s.enc.isHWDecode {
		// Decoded frames are already in the correct HW pixel format.
		return nil
	}

	decPixFmt := s.dec.codecContext.PixelFormat()

	// For HW encode with SW decode, target the SW pixel format (e.g. NV12)
	// before uploading; for pure SW encode, target the encoder pixel format.
	targetSwPixFmt := s.enc.codecContext.PixelFormat()
	if s.enc.isHW {
		targetSwPixFmt = profile.swPixFmt
	}

	if decPixFmt != targetSwPixFmt {
		swsCtx, err := astiav.CreateSoftwareScaleContext(
			s.dec.codecContext.Width(), s.dec.codecContext.Height(), decPixFmt,
			s.dec.codecContext.Width(), s.dec.codecContext.Height(), targetSwPixFmt,
			astiav.NewSoftwareScaleContextFlags(astiav.SoftwareScaleContextFlagBilinear),
		)
		if err != nil {
			return fmt.Errorf("creating software scale context: %w", err)
		}
		s.enc.swsCtx = swsCtx

		s.enc.scaledFrame = astiav.AllocFrame()
		if s.enc.scaledFrame == nil {
			return errors.New("failed to allocate scaled frame")
		}
		s.enc.scaledFrame.SetWidth(s.dec.codecContext.Width())
		s.enc.scaledFrame.SetHeight(s.dec.codecContext.Height())
		s.enc.scaledFrame.SetPixelFormat(targetSwPixFmt)
		if err := s.enc.scaledFrame.AllocBuffer(0); err != nil {
			return fmt.Errorf("allocating scaled frame buffer: %w", err)
		}
	}

	if s.enc.isHW {
		s.enc.hwFrame = astiav.AllocFrame()
		if s.enc.hwFrame == nil {
			return errors.New("failed to allocate hardware frame")
		}
		if err := s.enc.hwFrame.AllocHardwareBuffer(s.enc.hwFramesCtx); err != nil {
			return fmt.Errorf("allocating hardware frame buffer: %w", err)
		}
	}

	return nil
}

// processPacket implements the stream interface for video. It decodes the
// packet and re-encodes each decoded frame.
func (s *videoStreamState) processPacket(pkt *astiav.Packet, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	pkt.RescaleTs(s.inStream.TimeBase(), s.dec.codecContext.TimeBase())

	if err := s.dec.codecContext.SendPacket(pkt); err != nil {
		return fmt.Errorf("ffmpeg: sending video packet to decoder: %w", err)
	}

	for {
		if err := s.dec.codecContext.ReceiveFrame(s.dec.frame); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				return nil
			}
			return fmt.Errorf("ffmpeg: receiving decoded video frame: %w", err)
		}
		if err := s.encodeVideoFrame(s.dec.frame, outputFmt, progressCh, totalDuration); err != nil {
			s.dec.frame.Unref()
			return err
		}
		s.dec.frame.Unref()
	}
}

// encodeVideoFrame converts and encodes a single decoded video frame.
// On the fully hardware path (isHWDecode=true) the frame is already in GPU
// memory and is passed directly to the encoder without any CPU conversion.
func (s *videoStreamState) encodeVideoFrame(frame *astiav.Frame, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	encFrame := frame

	if !s.enc.isHWDecode {
		// Software decode path: convert pixel format if needed.
		if s.enc.swsCtx != nil {
			if err := s.enc.swsCtx.ScaleFrame(frame, s.enc.scaledFrame); err != nil {
				return fmt.Errorf("ffmpeg: scaling video frame: %w", err)
			}
			s.enc.scaledFrame.SetPts(frame.Pts())
			s.enc.scaledFrame.SetPictureType(astiav.PictureTypeNone)
			encFrame = s.enc.scaledFrame
		}

		// Upload to hardware memory when using SW decode + HW encode.
		if s.enc.isHW {
			if err := encFrame.TransferHardwareData(s.enc.hwFrame); err != nil {
				return fmt.Errorf("ffmpeg: uploading frame to hardware: %w", err)
			}
			s.enc.hwFrame.SetPts(encFrame.Pts())
			s.enc.hwFrame.SetPictureType(astiav.PictureTypeNone)
			encFrame = s.enc.hwFrame
		}
	}

	if err := s.enc.codecContext.SendFrame(encFrame); err != nil {
		return fmt.Errorf("ffmpeg: sending video frame to encoder: %w", err)
	}

	return receiveAndWritePackets(s.enc.codecContext, s.enc.pkt, s.outStream, &s.frames, outputFmt, progressCh, totalDuration)
}

// flush implements the stream interface for video. It drains buffered frames
// from the encoder.
func (s *videoStreamState) flush(outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	if err := s.enc.codecContext.SendFrame(nil); err != nil {
		return fmt.Errorf("ffmpeg: flushing video encoder: %w", err)
	}
	return receiveAndWritePackets(s.enc.codecContext, s.enc.pkt, s.outStream, &s.frames, outputFmt, progressCh, totalDuration)
}

// ---- audioStreamState ------------------------------------------------------

// audioStreamState decodes and re-encodes an audio stream.
type audioStreamState struct {
	streamBase
	dec         streamDecoder
	enc         audioEncoderState
	outputCodec Codec
}

func (s *audioStreamState) encoderContext() *astiav.CodecContext { return s.enc.codecContext }

func (s *audioStreamState) free() {
	s.dec.free()
	s.enc.free()
}

// setupDecoder initialises the software decoder codec context for the audio stream.
func (s *audioStreamState) setupDecoder(inStream *astiav.Stream) error {
	codec := astiav.FindDecoder(inStream.CodecParameters().CodecID())
	if codec == nil {
		return fmt.Errorf("no decoder for codec ID %v", inStream.CodecParameters().CodecID())
	}
	s.dec.codec = codec

	s.dec.codecContext = astiav.AllocCodecContext(codec)
	if s.dec.codecContext == nil {
		return errors.New("failed to allocate decoder codec context")
	}

	if err := inStream.CodecParameters().ToCodecContext(s.dec.codecContext); err != nil {
		return fmt.Errorf("copying codec parameters to context: %w", err)
	}

	if err := s.dec.codecContext.Open(codec, nil); err != nil {
		return fmt.Errorf("opening decoder: %w", err)
	}
	s.dec.codecContext.SetTimeBase(inStream.TimeBase())

	s.dec.frame = astiav.AllocFrame()
	if s.dec.frame == nil {
		return errors.New("failed to allocate decoder frame")
	}

	return nil
}

// setupEncoder implements the stream interface for audio.
func (s *audioStreamState) setupEncoder(_ HWAccel, outputFmt *astiav.FormatContext) error {
	switch s.outputCodec {
	case CodecH264, CodecH265:
		return fmt.Errorf("unsupported audio codec: %s", s.outputCodec)
	}

	// Re-encode using the same codec as the input (transcode → same format,
	// potentially with a different container).
	enc := astiav.FindEncoder(s.dec.codecContext.CodecID())
	if enc == nil {
		return fmt.Errorf("no encoder found for audio codec ID %v", s.dec.codecContext.CodecID())
	}
	s.enc.codec = enc

	s.enc.codecContext = astiav.AllocCodecContext(enc)
	if s.enc.codecContext == nil {
		return errors.New("failed to allocate audio encoder codec context")
	}

	// Preserve sample rate and channel layout.
	s.enc.codecContext.SetSampleRate(s.dec.codecContext.SampleRate())
	if layouts := enc.SupportedChannelLayouts(); len(layouts) > 0 {
		s.enc.codecContext.SetChannelLayout(layouts[0])
	} else {
		s.enc.codecContext.SetChannelLayout(s.dec.codecContext.ChannelLayout())
	}
	if fmts := enc.SupportedSampleFormats(); len(fmts) > 0 {
		s.enc.codecContext.SetSampleFormat(fmts[0])
	} else {
		s.enc.codecContext.SetSampleFormat(s.dec.codecContext.SampleFormat())
	}
	s.enc.codecContext.SetTimeBase(astiav.NewRational(1, s.enc.codecContext.SampleRate()))

	if outputFmt.OutputFormat().Flags().Has(astiav.IOFormatFlagGlobalheader) {
		s.enc.codecContext.SetFlags(s.enc.codecContext.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	}

	if err := s.enc.codecContext.Open(s.enc.codec, nil); err != nil {
		return fmt.Errorf("opening audio encoder: %w", err)
	}

	// Set up resampler if sample format, channel layout, or sample rate differs.
	needResample := s.dec.codecContext.SampleFormat() != s.enc.codecContext.SampleFormat() ||
		s.dec.codecContext.ChannelLayout().Channels() != s.enc.codecContext.ChannelLayout().Channels() ||
		s.dec.codecContext.SampleRate() != s.enc.codecContext.SampleRate()

	if needResample {
		s.enc.swrCtx = astiav.AllocSoftwareResampleContext()
		if s.enc.swrCtx == nil {
			return errors.New("failed to allocate software resample context")
		}

		s.enc.audioFrame = astiav.AllocFrame()
		if s.enc.audioFrame == nil {
			return errors.New("failed to allocate audio resample frame")
		}
		s.enc.audioFrame.SetChannelLayout(s.enc.codecContext.ChannelLayout())
		s.enc.audioFrame.SetSampleFormat(s.enc.codecContext.SampleFormat())
		s.enc.audioFrame.SetSampleRate(s.enc.codecContext.SampleRate())
		s.enc.audioFrame.SetNbSamples(s.dec.codecContext.FrameSize())
		if s.enc.audioFrame.NbSamples() <= 0 {
			s.enc.audioFrame.SetNbSamples(1024)
		}
		if err := s.enc.audioFrame.AllocBuffer(0); err != nil {
			return fmt.Errorf("allocating audio resample frame buffer: %w", err)
		}
	}

	s.enc.pkt = astiav.AllocPacket()
	if s.enc.pkt == nil {
		return errors.New("failed to allocate encoder packet")
	}

	return nil
}

// processPacket implements the stream interface for audio. It decodes the
// packet and re-encodes each decoded frame.
func (s *audioStreamState) processPacket(pkt *astiav.Packet, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	pkt.RescaleTs(s.inStream.TimeBase(), s.dec.codecContext.TimeBase())

	if err := s.dec.codecContext.SendPacket(pkt); err != nil {
		return fmt.Errorf("ffmpeg: sending audio packet to decoder: %w", err)
	}

	for {
		if err := s.dec.codecContext.ReceiveFrame(s.dec.frame); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				return nil
			}
			return fmt.Errorf("ffmpeg: receiving decoded audio frame: %w", err)
		}
		if err := s.encodeAudioFrame(s.dec.frame, outputFmt, progressCh, totalDuration); err != nil {
			s.dec.frame.Unref()
			return err
		}
		s.dec.frame.Unref()
	}
}

// encodeAudioFrame resamples (if needed) and encodes a single decoded audio frame.
func (s *audioStreamState) encodeAudioFrame(frame *astiav.Frame, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	encFrame := frame

	if s.enc.swrCtx != nil {
		if err := s.enc.swrCtx.ConvertFrame(frame, s.enc.audioFrame); err != nil {
			return fmt.Errorf("ffmpeg: resampling audio frame: %w", err)
		}
		s.enc.audioFrame.SetPts(frame.Pts())
		encFrame = s.enc.audioFrame
	}

	if err := s.enc.codecContext.SendFrame(encFrame); err != nil {
		return fmt.Errorf("ffmpeg: sending audio frame to encoder: %w", err)
	}

	return receiveAndWritePackets(s.enc.codecContext, s.enc.pkt, s.outStream, &s.frames, outputFmt, progressCh, totalDuration)
}

// flush implements the stream interface for audio. It drains buffered frames
// from the encoder.
func (s *audioStreamState) flush(outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	if err := s.enc.codecContext.SendFrame(nil); err != nil {
		return fmt.Errorf("ffmpeg: flushing audio encoder: %w", err)
	}
	return receiveAndWritePackets(s.enc.codecContext, s.enc.pkt, s.outStream, &s.frames, outputFmt, progressCh, totalDuration)
}

// ---- shared helpers --------------------------------------------------------

// remuxPacket copies a packet directly to the output without decoding/encoding.
func remuxPacket(pkt *astiav.Packet, inStream, outStream *astiav.Stream, outputFmt *astiav.FormatContext) error {
	pkt.RescaleTs(inStream.TimeBase(), outStream.TimeBase())
	pkt.SetStreamIndex(outStream.Index())
	if err := outputFmt.WriteInterleavedFrame(pkt); err != nil {
		return fmt.Errorf("ffmpeg: writing remuxed packet for stream %d: %w", outStream.Index(), err)
	}
	return nil
}

// receiveAndWritePackets drains encoded packets from the encoder and writes them
// to the output. framesPtr is incremented for each packet written.
func receiveAndWritePackets(encCtx *astiav.CodecContext, encPkt *astiav.Packet, outStream *astiav.Stream, framesPtr *int64, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	for {
		if err := encCtx.ReceivePacket(encPkt); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				return nil
			}
			return fmt.Errorf("ffmpeg: receiving encoded packet: %w", err)
		}

		*framesPtr++
		encPkt.SetStreamIndex(outStream.Index())
		encPkt.RescaleTs(encCtx.TimeBase(), outStream.TimeBase())

		if progressCh != nil {
			sendProgress(progressCh, *framesPtr, encPkt, outStream, totalDuration)
		}

		if err := outputFmt.WriteInterleavedFrame(encPkt); err != nil {
			encPkt.Unref()
			return fmt.Errorf("ffmpeg: writing encoded packet: %w", err)
		}
		encPkt.Unref()
	}
}

// sendProgress emits a non-blocking progress update on ch.
func sendProgress(ch chan<- Progress, frames int64, pkt *astiav.Packet, outStream *astiav.Stream, totalDuration int64) {
	var pct float64
	if totalDuration > 0 {
		tb := outStream.TimeBase()
		ptsInMicros := float64(pkt.Pts()) * float64(tb.Num()) / float64(tb.Den()) * 1e6
		pct = ptsInMicros / float64(totalDuration) * 100
		if pct > 100 {
			pct = 100
		}
		if pct < 0 {
			pct = 0
		}
	}
	select {
	case ch <- Progress{FramesProcessed: frames, PercentComplete: pct}:
	default:
	}
}

// freeStreams releases all resources held by a streams map.
func freeStreams(streams map[int]stream) {
	for _, s := range streams {
		s.free()
	}
}

// ---- TranscodeBuilder and Transcoder ---------------------------------------

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

	streams, err := t.buildStreamStates(inputFmt, effectiveHW)
	if err != nil {
		return err
	}
	defer freeStreams(streams)

	outputFmt, closeIO, err := t.setupOutputContext(streams, inputFmt, effectiveHW)
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

	if err := t.readAllPackets(ctx, inputFmt, outputFmt, streams, interrupter, totalDuration); err != nil {
		return err
	}

	if err := t.flushAllEncoders(ctx, outputFmt, streams, interrupter, totalDuration); err != nil {
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
	return GetHardwareEncoder(t.videoCodec, HWAccelNone)
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
		mediaType := inStream.CodecParameters().MediaType()
		base := streamBase{inStream: inStream}

		var s stream
		switch {
		case mediaType == astiav.MediaTypeVideo && t.videoCodec != CodecCopy:
			vs := &videoStreamState{streamBase: base, outputCodec: t.videoCodec}
			if err := vs.setupDecoder(inStream, inputFmt, hwAccel); err != nil {
				freeStreams(streams)
				return nil, fmt.Errorf("ffmpeg: setting up decoder for stream %d: %w", inStream.Index(), err)
			}
			s = vs
		case mediaType == astiav.MediaTypeAudio && t.audioCodec != CodecCopy:
			as := &audioStreamState{streamBase: base, outputCodec: t.audioCodec}
			if err := as.setupDecoder(inStream); err != nil {
				freeStreams(streams)
				return nil, fmt.Errorf("ffmpeg: setting up decoder for stream %d: %w", inStream.Index(), err)
			}
			s = as
		default:
			s = &copyStreamState{streamBase: base}
		}

		streams[inStream.Index()] = s
	}

	return streams, nil
}

// setupOutputContext opens the output format context, creates output streams,
// sets up encoders for re-encoded streams, and opens the IO context for
// file-based output. The returned closeIO function must be deferred by the
// caller — it flushes the IO context's buffers.
func (t *Transcoder) setupOutputContext(streams map[int]stream, inputFmt *astiav.FormatContext, effectiveHW HWAccel) (*astiav.FormatContext, func(), error) {
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

		if encCtx := s.encoderContext(); encCtx != nil {
			// Re-encoded stream: populate output parameters from the encoder.
			if err := outStream.CodecParameters().FromCodecContext(encCtx); err != nil {
				outputFmt.Free()
				return nil, noopClose, fmt.Errorf("ffmpeg: updating codec parameters for stream %d: %w", inStream.Index(), err)
			}
			outStream.SetTimeBase(encCtx.TimeBase())
		} else {
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

// readAllPackets is the main decode/encode loop.
func (t *Transcoder) readAllPackets(ctx context.Context, inputFmt, outputFmt *astiav.FormatContext, streams map[int]stream, interrupter *astiav.IOInterrupter, totalDuration int64) error {
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

		s, ok := streams[pkt.StreamIndex()]
		if !ok {
			pkt.Unref()
			continue
		}

		err := s.processPacket(pkt, outputFmt, t.progressCh, totalDuration)
		pkt.Unref()

		if err != nil {
			if interrupter.Interrupted() {
				return ctx.Err()
			}
			return err
		}
	}
}

// flushAllEncoders drains buffered frames from every active encoder.
func (t *Transcoder) flushAllEncoders(ctx context.Context, outputFmt *astiav.FormatContext, streams map[int]stream, interrupter *astiav.IOInterrupter, totalDuration int64) error {
	for _, s := range streams {
		if err := s.flush(outputFmt, t.progressCh, totalDuration); err != nil {
			if interrupter.Interrupted() {
				return ctx.Err()
			}
			return err
		}
	}
	return nil
}
