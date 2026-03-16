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

func (sd *streamDecoder) free() {
	if sd.codecContext != nil {
		sd.codecContext.Free()
	}
	if sd.frame != nil {
		sd.frame.Free()
	}
	if sd.hwDevCtx != nil {
		sd.hwDevCtx.Free()
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

func (vss *videoStreamState) encoderContext() *astiav.CodecContext { return vss.encCodecContext }

func (vss *videoStreamState) free() {
	vss.dec.free()
	if vss.encCodecContext != nil {
		vss.encCodecContext.Free()
	}
	if vss.encPkt != nil {
		vss.encPkt.Free()
	}
	if vss.swsCtx != nil {
		vss.swsCtx.Free()
	}
	if vss.scaledFrame != nil {
		vss.scaledFrame.Free()
	}
	if vss.hwFrame != nil {
		vss.hwFrame.Free()
	}
	if vss.hwFramesCtx != nil {
		vss.hwFramesCtx.Free()
	}
}

// setupDecoder initialises the decoder codec context for the video stream.
// For non-None hwAccel it attempts hardware decoding so that decoded frames
// remain in GPU memory, enabling a zero-copy decode→encode pipeline.
// Falls back silently to software decoding if HW decode is unavailable.
func (vss *videoStreamState) setupDecoder(inStream *astiav.Stream, inputFmt *astiav.FormatContext, hwAccel HWAccel) error {
	var codec *astiav.Codec

	if profile, ok := hwProfiles[hwAccel]; ok {
		hwDecName := hwDecoderNameForCodec(inStream.CodecParameters().CodecID(), profile)
		if hwDecName != "" {
			if hwDec := astiav.FindDecoderByName(hwDecName); hwDec != nil {
				hwDevCtx, err := astiav.CreateHardwareDeviceContext(profile.deviceType, "", nil, 0)
				if err == nil {
					vss.dec.hwDevCtx = hwDevCtx
					vss.dec.hwPixFmt = profile.hwPixFmt
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
	vss.dec.codec = codec

	vss.dec.codecContext = astiav.AllocCodecContext(codec)
	if vss.dec.codecContext == nil {
		return errors.New("failed to allocate decoder codec context")
	}

	if err := inStream.CodecParameters().ToCodecContext(vss.dec.codecContext); err != nil {
		return fmt.Errorf("copying codec parameters to context: %w", err)
	}

	vss.dec.codecContext.SetFramerate(inputFmt.GuessFrameRate(inStream, nil))

	if vss.dec.hwDevCtx != nil {
		vss.dec.codecContext.SetHardwareDeviceContext(vss.dec.hwDevCtx)
		hwPixFmt := vss.dec.hwPixFmt
		vss.dec.codecContext.SetPixelFormatCallback(func(pfs []astiav.PixelFormat) astiav.PixelFormat {
			for _, pf := range pfs {
				if pf == hwPixFmt {
					return pf
				}
			}
			// HW pixel format not offered — fall back to the first available format.
			if len(pfs) > 0 {
				slog.Debug("ffmpeg: preferred hardware pixel format not offered by decoder, using fallback",
					"preferred", hwPixFmt, "fallback", pfs[0])
				return pfs[0]
			}
			return astiav.PixelFormatNone
		})
	}

	if err := vss.dec.codecContext.Open(codec, nil); err != nil {
		return fmt.Errorf("opening decoder: %w", err)
	}
	vss.dec.codecContext.SetTimeBase(inStream.TimeBase())

	vss.dec.frame = astiav.AllocFrame()
	if vss.dec.frame == nil {
		return errors.New("failed to allocate decoder frame")
	}

	return nil
}

// setupEncoder implements the stream interface for video. If a hardware encoder
// is requested but unavailable, it falls back to software encoding transparently.
func (vss *videoStreamState) setupEncoder(hwAccel HWAccel, outputFmt *astiav.FormatContext) error {
	enc, profile, useHW, err := vss.selectVideoEncoder(hwAccel)
	if err != nil {
		return err
	}
	vss.encCodec = enc
	vss.isHWEncode = useHW

	if err := vss.openVideoEncoderContext(enc, profile, useHW, outputFmt); err != nil {
		return err
	}

	if err := vss.setupVideoConversion(profile); err != nil {
		return err
	}

	vss.encPkt = astiav.AllocPacket()
	if vss.encPkt == nil {
		return errors.New("failed to allocate encoder packet")
	}

	return nil
}

// selectVideoEncoder chooses a hardware or software encoder. On hardware
// selection failure it transparently falls back to software.
func (vss *videoStreamState) selectVideoEncoder(hwAccel HWAccel) (enc *astiav.Codec, profile hwProfile, useHW bool, err error) {
	effective := GetHardwareEncoder(vss.outputCodec, hwAccel)
	if p, hasProfile := hwProfiles[effective]; hasProfile {
		hwEncName := hwEncoderNameForCodec(vss.outputCodec, p)
		if hwEncName != "" && astiav.FindEncoderByName(hwEncName) != nil {
			// Reuse the hardware device context from the HW decoder if one was
			// set up, otherwise create a new one. This enables the zero-copy
			// decode→encode path when both sides use the same hardware.
			hwDevCtx := vss.dec.hwDevCtx
			if hwDevCtx == nil {
				hwDevCtx, err = astiav.CreateHardwareDeviceContext(p.deviceType, "", nil, 0)
				if err != nil {
					slog.Debug("ffmpeg: hardware encoder device context creation failed, falling back to software",
						"encoder", hwEncName, "error", err)
					hwDevCtx = nil
				}
			}
			if hwDevCtx != nil {
				if vss.dec.hwDevCtx == nil {
					// Newly created — store so free() releases it via dec.free().
					vss.dec.hwDevCtx = hwDevCtx
				}
				return astiav.FindEncoderByName(hwEncName), p, true, nil
			}
		}
	}

	// Software fallback: only unreachable if libavcodec was compiled without the
	// requested encoder (e.g. without libx264/libx265), which should not occur
	// in normal deployments but is checked here as a safety net.
	enc = astiav.FindEncoder(vss.outputCodec)
	if enc == nil {
		return nil, hwProfile{}, false, fmt.Errorf("no encoder found for video codec %v", vss.outputCodec)
	}

	return enc, hwProfile{}, false, nil
}

// openVideoEncoderContext allocates, configures, and opens the encoder codec
// context. For hardware paths it also sets up the hardware frames context.
func (vss *videoStreamState) openVideoEncoderContext(enc *astiav.Codec, profile hwProfile, useHW bool, outputFmt *astiav.FormatContext) error {
	vss.encCodecContext = astiav.AllocCodecContext(enc)
	if vss.encCodecContext == nil {
		return errors.New("failed to allocate encoder codec context")
	}

	vss.encCodecContext.SetWidth(vss.dec.codecContext.Width())
	vss.encCodecContext.SetHeight(vss.dec.codecContext.Height())
	vss.encCodecContext.SetSampleAspectRatio(vss.dec.codecContext.SampleAspectRatio())
	vss.encCodecContext.SetTimeBase(vss.dec.codecContext.TimeBase())
	vss.encCodecContext.SetFramerate(vss.dec.codecContext.Framerate())

	// Preserve HDR and color metadata.
	vss.encCodecContext.SetColorPrimaries(vss.dec.codecContext.ColorPrimaries())
	vss.encCodecContext.SetColorTransferCharacteristic(vss.dec.codecContext.ColorTransferCharacteristic())
	vss.encCodecContext.SetColorSpace(vss.dec.codecContext.ColorSpace())
	vss.encCodecContext.SetColorRange(vss.dec.codecContext.ColorRange())

	if err := vss.configureEncoderPixelFormat(enc, profile, useHW); err != nil {
		return err
	}

	if outputFmt.OutputFormat().Flags().Has(astiav.IOFormatFlagGlobalheader) {
		vss.encCodecContext.SetFlags(vss.encCodecContext.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	}

	if err := vss.encCodecContext.Open(vss.encCodec, nil); err != nil {
		return fmt.Errorf("opening video encoder: %w", err)
	}

	return nil
}

// configureEncoderPixelFormat sets the pixel format on the encoder codec
// context and, for the SW decode + HW encode path, allocates the hardware
// frames context for CPU→GPU upload.
func (vss *videoStreamState) configureEncoderPixelFormat(enc *astiav.Codec, profile hwProfile, useHW bool) error {
	if !useHW {
		// Software path: prefer YUV420P; fall back to the encoder's first
		// supported format if it does not support YUV420P.
		encPixFmt := astiav.PixelFormatYuv420P
		fmts := enc.SupportedPixelFormats()
		if len(fmts) > 0 && !slices.Contains(fmts, astiav.PixelFormatYuv420P) {
			encPixFmt = fmts[0]
		}
		vss.encCodecContext.SetPixelFormat(encPixFmt)
		return nil
	}

	// HW encode with HW decode: decoded frames are already in GPU memory —
	// share those surfaces directly with the encoder.
	if vss.dec.hwDevCtx != nil && vss.dec.hwPixFmt == profile.hwPixFmt {
		vss.isHWDecode = true
		vss.encCodecContext.SetPixelFormat(profile.hwPixFmt)
		return nil
	}

	// HW encode with SW decode: allocate a frames pool for CPU→GPU upload.
	if err := vss.setupHWFramesContext(profile); err != nil {
		return err
	}
	vss.encCodecContext.SetPixelFormat(profile.hwPixFmt)
	vss.encCodecContext.SetHardwareFramesContext(vss.hwFramesCtx)
	return nil
}

// setupHWFramesContext allocates and initialises the hardware frames context
// used to upload decoded software frames into GPU memory before encoding.
func (vss *videoStreamState) setupHWFramesContext(profile hwProfile) error {
	vss.hwFramesCtx = astiav.AllocHardwareFramesContext(vss.dec.hwDevCtx)
	if vss.hwFramesCtx == nil {
		return errors.New("failed to allocate hardware frames context")
	}
	vss.hwFramesCtx.SetHardwarePixelFormat(profile.hwPixFmt)
	vss.hwFramesCtx.SetSoftwarePixelFormat(profile.swPixFmt)
	vss.hwFramesCtx.SetWidth(vss.dec.codecContext.Width())
	vss.hwFramesCtx.SetHeight(vss.dec.codecContext.Height())
	vss.hwFramesCtx.SetInitialPoolSize(20)
	if err := vss.hwFramesCtx.Initialize(); err != nil {
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
func (vss *videoStreamState) setupVideoConversion(profile hwProfile) error {
	if vss.isHWDecode {
		// Decoded frames are already in the correct HW pixel format.
		return nil
	}

	decPixFmt := vss.dec.codecContext.PixelFormat()

	// For HW encode with SW decode, target the SW pixel format (e.g. NV12)
	// before uploading; for pure SW encode, target the encoder pixel format.
	targetSwPixFmt := vss.encCodecContext.PixelFormat()
	if vss.isHWEncode {
		targetSwPixFmt = profile.swPixFmt
	}

	if decPixFmt != targetSwPixFmt {
		swsCtx, err := astiav.CreateSoftwareScaleContext(
			vss.dec.codecContext.Width(), vss.dec.codecContext.Height(), decPixFmt,
			vss.dec.codecContext.Width(), vss.dec.codecContext.Height(), targetSwPixFmt,
			astiav.NewSoftwareScaleContextFlags(astiav.SoftwareScaleContextFlagBilinear),
		)
		if err != nil {
			return fmt.Errorf("creating software scale context: %w", err)
		}
		vss.swsCtx = swsCtx

		vss.scaledFrame = astiav.AllocFrame()
		if vss.scaledFrame == nil {
			return errors.New("failed to allocate scaled frame")
		}
		vss.scaledFrame.SetWidth(vss.dec.codecContext.Width())
		vss.scaledFrame.SetHeight(vss.dec.codecContext.Height())
		vss.scaledFrame.SetPixelFormat(targetSwPixFmt)
		if err := vss.scaledFrame.AllocBuffer(0); err != nil {
			return fmt.Errorf("allocating scaled frame buffer: %w", err)
		}
	}

	if vss.isHWEncode {
		vss.hwFrame = astiav.AllocFrame()
		if vss.hwFrame == nil {
			return errors.New("failed to allocate hardware frame")
		}
		if err := vss.hwFrame.AllocHardwareBuffer(vss.hwFramesCtx); err != nil {
			return fmt.Errorf("allocating hardware frame buffer: %w", err)
		}
	}

	return nil
}

// processPacket implements the stream interface for video. It decodes the
// packet and re-encodes each decoded frame.
func (vss *videoStreamState) processPacket(packet *astiav.Packet, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	packet.RescaleTs(vss.inStream.TimeBase(), vss.dec.codecContext.TimeBase())

	if err := vss.dec.codecContext.SendPacket(packet); err != nil {
		return fmt.Errorf("ffmpeg: sending video packet to decoder: %w", err)
	}

	for {
		if err := vss.dec.codecContext.ReceiveFrame(vss.dec.frame); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				return nil
			}
			return fmt.Errorf("ffmpeg: receiving decoded video frame: %w", err)
		}
		if err := vss.encodeVideoFrame(vss.dec.frame, outputFmt, progressCh, totalDuration); err != nil {
			vss.dec.frame.Unref()
			return err
		}
		vss.dec.frame.Unref()
	}
}

// encodeVideoFrame converts and encodes a single decoded video frame.
// On the fully hardware path (isHWDecode=true) the frame is already in GPU
// memory and is passed directly to the encoder without any CPU conversion.
func (vss *videoStreamState) encodeVideoFrame(frame *astiav.Frame, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	encFrame := frame

	if !vss.isHWDecode {
		// Software decode path: convert pixel format if needed.
		if vss.swsCtx != nil {
			if err := vss.swsCtx.ScaleFrame(frame, vss.scaledFrame); err != nil {
				return fmt.Errorf("ffmpeg: scaling video frame: %w", err)
			}
			vss.scaledFrame.SetPts(frame.Pts())
			vss.scaledFrame.SetPictureType(astiav.PictureTypeNone)
			encFrame = vss.scaledFrame
		}

		// Upload to hardware memory when using SW decode + HW encode.
		if vss.isHWEncode {
			if err := encFrame.TransferHardwareData(vss.hwFrame); err != nil {
				return fmt.Errorf("ffmpeg: uploading frame to hardware: %w", err)
			}
			vss.hwFrame.SetPts(encFrame.Pts())
			vss.hwFrame.SetPictureType(astiav.PictureTypeNone)
			encFrame = vss.hwFrame
		}
	}

	if err := vss.encCodecContext.SendFrame(encFrame); err != nil {
		return fmt.Errorf("ffmpeg: sending video frame to encoder: %w", err)
	}

	return vss.receiveAndWritePackets(vss.encCodecContext, vss.encPkt, outputFmt, progressCh, totalDuration)
}

// flush implements the stream interface for video. It drains buffered frames
// from the encoder.
func (vss *videoStreamState) flush(outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	if err := vss.encCodecContext.SendFrame(nil); err != nil {
		return fmt.Errorf("ffmpeg: flushing video encoder: %w", err)
	}
	return vss.receiveAndWritePackets(vss.encCodecContext, vss.encPkt, outputFmt, progressCh, totalDuration)
}
