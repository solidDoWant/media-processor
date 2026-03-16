package ffmpeg

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/asticode/go-astiav"
)

// streamDecoder holds resources for the decoder side of a stream. Used by
// both videoStreamState and audioStreamState.
type streamDecoder struct {
	codec        *astiav.Codec
	codecContext *astiav.CodecContext
	frame        *astiav.Frame
	// Non-nil when using a hardware-accelerated decoder. Shared with the video
	// encoder to enable zero-copy decode→encode pipelines.
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

// videoStreamState decodes and re-encodes a video stream.
type videoStreamState struct {
	copyStreamState

	// Decoder state.
	dec streamDecoder

	// Encoder state.
	encCodec        *astiav.Codec
	encCodecContext *astiav.CodecContext
	encPkt          *astiav.Packet

	// isHWEncode is true when using a hardware encoder.
	isHWEncode bool
	// isHWDecode is true when the decoder is also hardware-accelerated and the
	// decoded frames are already in GPU memory — no CPU upload step needed.
	isHWDecode  bool
	hwFramesCtx *astiav.HardwareFramesContext
	hwFrame     *astiav.Frame

	// swsCtx and scaledFrame are used on the software path to convert decoded
	// frames to the pixel format expected by the encoder. Nil on the fully
	// hardware path (isHWDecode=true).
	swsCtx      *astiav.SoftwareScaleContext
	scaledFrame *astiav.Frame

	outputCodec Codec
}

func (s *videoStreamState) encoderContext() *astiav.CodecContext { return s.encCodecContext }

func (s *videoStreamState) free() {
	s.dec.free()
	if s.encCodecContext != nil {
		s.encCodecContext.Free()
	}
	if s.encPkt != nil {
		s.encPkt.Free()
	}
	if s.swsCtx != nil {
		s.swsCtx.Free()
	}
	if s.scaledFrame != nil {
		s.scaledFrame.Free()
	}
	if s.hwFrame != nil {
		s.hwFrame.Free()
	}
	if s.hwFramesCtx != nil {
		s.hwFramesCtx.Free()
	}
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
				} else {
					slog.Debug("ffmpeg: hardware decoder setup failed, falling back to software",
						"decoder", hwDecName, "error", err)
				}
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
	s.encCodec = enc
	s.isHWEncode = useHW

	if err := s.openVideoEncoderContext(enc, profile, useHW, outputFmt); err != nil {
		return err
	}

	if err := s.setupVideoConversion(profile); err != nil {
		return err
	}

	s.encPkt = astiav.AllocPacket()
	if s.encPkt == nil {
		return errors.New("failed to allocate encoder packet")
	}

	return nil
}

// selectVideoEncoder chooses a hardware or software encoder. On hardware
// selection failure it transparently falls back to software.
func (s *videoStreamState) selectVideoEncoder(hwAccel HWAccel) (enc *astiav.Codec, profile hwProfile, useHW bool, err error) {
	if p, hasProfile := hwProfiles[hwAccel]; hasProfile {
		hwEncName := hwEncoderNameForCodec(s.outputCodec, p)
		if hwEncName != "" && astiav.FindEncoderByName(hwEncName) != nil {
			// Reuse the hardware device context from the HW decoder if one was
			// set up, otherwise create a new one. This enables the zero-copy
			// decode→encode path when both sides use the same hardware.
			hwDevCtx := s.dec.hwDevCtx
			if hwDevCtx == nil {
				hwDevCtx, err = astiav.CreateHardwareDeviceContext(p.deviceType, "", nil, 0)
				if err != nil {
					slog.Debug("ffmpeg: hardware encoder device context creation failed, falling back to software",
						"encoder", hwEncName, "error", err)
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

	// Software fallback: only unreachable if libavcodec was compiled without the
	// requested encoder (e.g. without libx264/libx265), which should not occur
	// in normal deployments but is checked here as a safety net.
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
	s.encCodecContext = astiav.AllocCodecContext(enc)
	if s.encCodecContext == nil {
		return errors.New("failed to allocate encoder codec context")
	}

	s.encCodecContext.SetWidth(s.dec.codecContext.Width())
	s.encCodecContext.SetHeight(s.dec.codecContext.Height())
	s.encCodecContext.SetSampleAspectRatio(s.dec.codecContext.SampleAspectRatio())
	s.encCodecContext.SetTimeBase(s.dec.codecContext.TimeBase())
	s.encCodecContext.SetFramerate(s.dec.codecContext.Framerate())

	// Preserve HDR and color metadata.
	s.encCodecContext.SetColorPrimaries(s.dec.codecContext.ColorPrimaries())
	s.encCodecContext.SetColorTransferCharacteristic(s.dec.codecContext.ColorTransferCharacteristic())
	s.encCodecContext.SetColorSpace(s.dec.codecContext.ColorSpace())
	s.encCodecContext.SetColorRange(s.dec.codecContext.ColorRange())

	if err := s.configureEncoderPixelFormat(enc, profile, useHW); err != nil {
		return err
	}

	if outputFmt.OutputFormat().Flags().Has(astiav.IOFormatFlagGlobalheader) {
		s.encCodecContext.SetFlags(s.encCodecContext.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	}

	if err := s.encCodecContext.Open(s.encCodec, nil); err != nil {
		return fmt.Errorf("opening video encoder: %w", err)
	}

	return nil
}

// configureEncoderPixelFormat sets the pixel format on the encoder codec
// context and, for the SW decode + HW encode path, allocates the hardware
// frames context for CPU→GPU upload.
func (s *videoStreamState) configureEncoderPixelFormat(enc *astiav.Codec, profile hwProfile, useHW bool) error {
	if !useHW {
		// Software path: prefer YUV420P; fall back to the encoder's first
		// supported format if it does not support YUV420P.
		encPixFmt := astiav.PixelFormatYuv420P
		fmts := enc.SupportedPixelFormats()
		if len(fmts) > 0 && !slices.Contains(fmts, astiav.PixelFormatYuv420P) {
			encPixFmt = fmts[0]
		}
		s.encCodecContext.SetPixelFormat(encPixFmt)
		return nil
	}

	// HW encode with HW decode: decoded frames are already in GPU memory —
	// share those surfaces directly with the encoder.
	if s.dec.hwDevCtx != nil && s.dec.hwPixFmt == profile.hwPixFmt {
		s.isHWDecode = true
		s.encCodecContext.SetPixelFormat(profile.hwPixFmt)
		return nil
	}

	// HW encode with SW decode: allocate a frames pool for CPU→GPU upload.
	if err := s.setupHWFramesContext(profile); err != nil {
		return err
	}
	s.encCodecContext.SetPixelFormat(profile.hwPixFmt)
	s.encCodecContext.SetHardwareFramesContext(s.hwFramesCtx)
	return nil
}

// setupHWFramesContext allocates and initialises the hardware frames context
// used to upload decoded software frames into GPU memory before encoding.
func (s *videoStreamState) setupHWFramesContext(profile hwProfile) error {
	s.hwFramesCtx = astiav.AllocHardwareFramesContext(s.dec.hwDevCtx)
	if s.hwFramesCtx == nil {
		return errors.New("failed to allocate hardware frames context")
	}
	s.hwFramesCtx.SetHardwarePixelFormat(profile.hwPixFmt)
	s.hwFramesCtx.SetSoftwarePixelFormat(profile.swPixFmt)
	s.hwFramesCtx.SetWidth(s.dec.codecContext.Width())
	s.hwFramesCtx.SetHeight(s.dec.codecContext.Height())
	s.hwFramesCtx.SetInitialPoolSize(20)
	if err := s.hwFramesCtx.Initialize(); err != nil {
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
	if s.isHWDecode {
		// Decoded frames are already in the correct HW pixel format.
		return nil
	}

	decPixFmt := s.dec.codecContext.PixelFormat()

	// For HW encode with SW decode, target the SW pixel format (e.g. NV12)
	// before uploading; for pure SW encode, target the encoder pixel format.
	targetSwPixFmt := s.encCodecContext.PixelFormat()
	if s.isHWEncode {
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
		s.swsCtx = swsCtx

		s.scaledFrame = astiav.AllocFrame()
		if s.scaledFrame == nil {
			return errors.New("failed to allocate scaled frame")
		}
		s.scaledFrame.SetWidth(s.dec.codecContext.Width())
		s.scaledFrame.SetHeight(s.dec.codecContext.Height())
		s.scaledFrame.SetPixelFormat(targetSwPixFmt)
		if err := s.scaledFrame.AllocBuffer(0); err != nil {
			return fmt.Errorf("allocating scaled frame buffer: %w", err)
		}
	}

	if s.isHWEncode {
		s.hwFrame = astiav.AllocFrame()
		if s.hwFrame == nil {
			return errors.New("failed to allocate hardware frame")
		}
		if err := s.hwFrame.AllocHardwareBuffer(s.hwFramesCtx); err != nil {
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

	if !s.isHWDecode {
		// Software decode path: convert pixel format if needed.
		if s.swsCtx != nil {
			if err := s.swsCtx.ScaleFrame(frame, s.scaledFrame); err != nil {
				return fmt.Errorf("ffmpeg: scaling video frame: %w", err)
			}
			s.scaledFrame.SetPts(frame.Pts())
			s.scaledFrame.SetPictureType(astiav.PictureTypeNone)
			encFrame = s.scaledFrame
		}

		// Upload to hardware memory when using SW decode + HW encode.
		if s.isHWEncode {
			if err := encFrame.TransferHardwareData(s.hwFrame); err != nil {
				return fmt.Errorf("ffmpeg: uploading frame to hardware: %w", err)
			}
			s.hwFrame.SetPts(encFrame.Pts())
			s.hwFrame.SetPictureType(astiav.PictureTypeNone)
			encFrame = s.hwFrame
		}
	}

	if err := s.encCodecContext.SendFrame(encFrame); err != nil {
		return fmt.Errorf("ffmpeg: sending video frame to encoder: %w", err)
	}

	return s.receiveAndWritePackets(s.encCodecContext, s.encPkt, outputFmt, progressCh, totalDuration)
}

// flush implements the stream interface for video. It drains buffered frames
// from the encoder.
func (s *videoStreamState) flush(outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	if err := s.encCodecContext.SendFrame(nil); err != nil {
		return fmt.Errorf("ffmpeg: flushing video encoder: %w", err)
	}
	return s.receiveAndWritePackets(s.encCodecContext, s.encPkt, outputFmt, progressCh, totalDuration)
}
