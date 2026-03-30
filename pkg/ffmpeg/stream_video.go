package ffmpeg

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/asticode/go-astiav"
)

type videoEncoderState struct {
	codecID                 astiav.CodecID
	codec                   *astiav.Codec
	codecContext            *astiav.CodecContext
	packet                  *astiav.Packet
	frame                   *astiav.Frame
	usesHardwareAccelerator bool
	hardwareFrameContext    *astiav.HardwareFramesContext
	softwareFrameContext    *astiav.SoftwareScaleContext
}

type videoDecoderState struct {
	streamDecoderState

	// Non-nil when using a hardware-accelerated decoder. Shared with the video
	// encoder to enable zero-copy decode→encode pipelines.
	hwDevCtx *astiav.HardwareDeviceContext
	hwPixFmt astiav.PixelFormat // expected HW pixel format from the HW decoder
}

// videoStreamState decodes and re-encodes a video stream.
type videoStreamState struct {
	copyStreamState

	// Decoder state.
	decoder            videoDecoderState
	encoder            videoEncoderState
	isHWDecode         bool
	hardwareDevicePath string // device path for CreateHardwareDeviceContext; "" = auto-select
}

func (vds *videoDecoderState) free() {
	vds.streamDecoderState.free()

	if vds.hwDevCtx != nil {
		vds.hwDevCtx.Free()
	}
}

func (vss *videoStreamState) encoderContext() *astiav.CodecContext { return vss.encoder.codecContext }

func (vss *videoStreamState) free() {
	vss.decoder.free()

	if vss.encoder.codecContext != nil {
		vss.encoder.codecContext.Free()
	}

	if vss.encoder.packet != nil {
		vss.encoder.packet.Free()
	}

	if vss.encoder.softwareFrameContext != nil {
		vss.encoder.softwareFrameContext.Free()
	}

	if vss.encoder.frame != nil {
		vss.encoder.frame.Free()
	}

	if vss.encoder.hardwareFrameContext != nil {
		vss.encoder.hardwareFrameContext.Free()
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
				hwDevCtx, err := astiav.CreateHardwareDeviceContext(profile.deviceType, vss.hardwareDevicePath, nil, 0)
				if err == nil {
					vss.decoder.hwDevCtx = hwDevCtx
					vss.decoder.hwPixFmt = profile.hwPixFmt
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

	vss.decoder.codec = codec

	vss.decoder.codecContext = astiav.AllocCodecContext(codec)
	if vss.decoder.codecContext == nil {
		return errors.New("failed to allocate decoder codec context")
	}

	if err := inStream.CodecParameters().ToCodecContext(vss.decoder.codecContext); err != nil {
		return fmt.Errorf("copying codec parameters to context: %w", err)
	}

	vss.decoder.codecContext.SetFramerate(inputFmt.GuessFrameRate(inStream, nil))

	if vss.decoder.hwDevCtx != nil {
		vss.decoder.codecContext.SetHardwareDeviceContext(vss.decoder.hwDevCtx)
		vss.decoder.codecContext.SetPixelFormatCallback(func(pixelFormats []astiav.PixelFormat) astiav.PixelFormat {
			for _, pixelFormat := range pixelFormats {
				if pixelFormat == vss.decoder.hwPixFmt {
					return pixelFormat
				}
			}
			// HW pixel format not offered — fall back to the first available format.
			// Update vss.decoder.hwPixFmt so configureEncoderPixelFormat correctly
			// detects that the decoder is not outputting the HW format.
			if len(pixelFormats) > 0 {
				slog.Debug("ffmpeg: preferred hardware pixel format not offered by decoder, using fallback",
					"preferred", vss.decoder.hwPixFmt, "fallback", pixelFormats[0])
				vss.decoder.hwPixFmt = pixelFormats[0]

				return pixelFormats[0]
			}

			return astiav.PixelFormatNone
		})
	}

	if err := vss.decoder.codecContext.Open(codec, nil); err != nil {
		return fmt.Errorf("opening decoder: %w", err)
	}

	vss.decoder.codecContext.SetTimeBase(inStream.TimeBase())

	vss.decoder.frame = astiav.AllocFrame()
	if vss.decoder.frame == nil {
		return errors.New("failed to allocate decoder frame")
	}

	return nil
}

// setupEncoder implements the stream interface for video. If a hardware encoder
// is requested but unavailable, it falls back to software encoding transparently.
func (vss *videoStreamState) setupEncoder(hwAccel HWAccel, outputFmt *astiav.FormatContext) error {
	codec, profile, supportsHardwareAcceleration, err := vss.selectVideoEncoder(hwAccel)
	if err != nil {
		return err
	}

	vss.encoder.codec = codec
	vss.encoder.usesHardwareAccelerator = supportsHardwareAcceleration

	if err := vss.openVideoEncoderContext(codec, profile, supportsHardwareAcceleration, outputFmt); err != nil {
		return err
	}

	if err := vss.setupVideoConversion(profile); err != nil {
		return err
	}

	vss.encoder.packet = astiav.AllocPacket()
	if vss.encoder.packet == nil {
		return errors.New("failed to allocate encoder packet")
	}

	return nil
}

// selectVideoEncoder chooses a hardware or software encoder. On hardware
// selection failure it transparently falls back to software.
func (vss *videoStreamState) selectVideoEncoder(hwAccel HWAccel) (enc *astiav.Codec, profile hwProfile, useHW bool, err error) {
	effective := GetHardwareEncoder(vss.encoder.codecID, hwAccel)
	if p, hasProfile := hwProfiles[effective]; hasProfile {
		hwEncName := hwEncoderNameForCodec(vss.encoder.codecID, p)
		if hwEncName != "" && astiav.FindEncoderByName(hwEncName) != nil {
			// Reuse the hardware device context from the HW decoder if one was
			// set up, otherwise create a new one. This enables the zero-copy
			// decode→encode path when both sides use the same hardware.
			hwDevCtx := vss.decoder.hwDevCtx
			if hwDevCtx == nil {
				hwDevCtx, err = astiav.CreateHardwareDeviceContext(p.deviceType, vss.hardwareDevicePath, nil, 0)
				if err != nil {
					slog.Debug("ffmpeg: hardware encoder device context creation failed, falling back to software",
						"encoder", hwEncName, "error", err)

					hwDevCtx = nil
				}
			}

			if hwDevCtx != nil {
				if vss.decoder.hwDevCtx == nil {
					// Newly created — store so free() releases it via dec.free().
					vss.decoder.hwDevCtx = hwDevCtx
				}

				return astiav.FindEncoderByName(hwEncName), p, true, nil
			}
		}
	}

	// Software fallback: only unreachable if libavcodec was compiled without the
	// requested encoder (e.g. without libx264/libx265), which should not occur
	// in normal deployments but is checked here as a safety net.
	enc = astiav.FindEncoder(vss.encoder.codecID)
	if enc == nil {
		return nil, hwProfile{}, false, fmt.Errorf("no encoder found for video codec %v", vss.encoder.codecID)
	}

	return enc, hwProfile{}, false, nil
}

// openVideoEncoderContext allocates, configures, and opens the encoder codec
// context. For hardware paths it also sets up the hardware frames context.
func (vss *videoStreamState) openVideoEncoderContext(enc *astiav.Codec, profile hwProfile, useHW bool, outputFmt *astiav.FormatContext) error {
	vss.encoder.codecContext = astiav.AllocCodecContext(enc)
	if vss.encoder.codecContext == nil {
		return errors.New("failed to allocate encoder codec context")
	}

	vss.encoder.codecContext.SetWidth(vss.decoder.codecContext.Width())
	vss.encoder.codecContext.SetHeight(vss.decoder.codecContext.Height())
	vss.encoder.codecContext.SetSampleAspectRatio(vss.decoder.codecContext.SampleAspectRatio())
	vss.encoder.codecContext.SetTimeBase(vss.decoder.codecContext.TimeBase())
	vss.encoder.codecContext.SetFramerate(vss.decoder.codecContext.Framerate())
	// Preserve HDR and color metadata.
	vss.encoder.codecContext.SetColorPrimaries(vss.decoder.codecContext.ColorPrimaries())
	vss.encoder.codecContext.SetColorTransferCharacteristic(vss.decoder.codecContext.ColorTransferCharacteristic())
	vss.encoder.codecContext.SetColorSpace(vss.decoder.codecContext.ColorSpace())
	vss.encoder.codecContext.SetColorRange(vss.decoder.codecContext.ColorRange())

	if err := vss.configureEncoderPixelFormat(enc, profile, useHW); err != nil {
		return err
	}

	if outputFmt.OutputFormat().Flags().Has(astiav.IOFormatFlagGlobalheader) {
		vss.encoder.codecContext.SetFlags(vss.encoder.codecContext.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	}

	if err := vss.encoder.codecContext.Open(vss.encoder.codec, nil); err != nil {
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

		vss.encoder.codecContext.SetPixelFormat(encPixFmt)

		return nil
	}

	// HW encode with HW decode: decoded frames are already in GPU memory —
	// share those surfaces directly with the encoder.
	if vss.decoder.hwDevCtx != nil && vss.decoder.hwPixFmt == profile.hwPixFmt {
		vss.isHWDecode = true
		vss.encoder.codecContext.SetPixelFormat(profile.hwPixFmt)

		return nil
	}

	// HW encode with SW decode: allocate a frames pool for CPU→GPU upload.
	if err := vss.setupHWFramesContext(profile); err != nil {
		return err
	}

	vss.encoder.codecContext.SetPixelFormat(profile.hwPixFmt)
	vss.encoder.codecContext.SetHardwareFramesContext(vss.encoder.hardwareFrameContext)

	return nil
}

// setupHWFramesContext allocates and initialises the hardware frames context
// used to upload decoded software frames into GPU memory before encoding.
func (vss *videoStreamState) setupHWFramesContext(profile hwProfile) error {
	vss.encoder.hardwareFrameContext = astiav.AllocHardwareFramesContext(vss.decoder.hwDevCtx)
	if vss.encoder.hardwareFrameContext == nil {
		return errors.New("failed to allocate hardware frames context")
	}

	vss.encoder.hardwareFrameContext.SetHardwarePixelFormat(profile.hwPixFmt)
	vss.encoder.hardwareFrameContext.SetSoftwarePixelFormat(profile.swPixFmt)
	vss.encoder.hardwareFrameContext.SetWidth(vss.decoder.codecContext.Width())
	vss.encoder.hardwareFrameContext.SetHeight(vss.decoder.codecContext.Height())
	vss.encoder.hardwareFrameContext.SetInitialPoolSize(20)

	if err := vss.encoder.hardwareFrameContext.Initialize(); err != nil {
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

	decoderPixelFormat := vss.decoder.codecContext.PixelFormat()

	// For HW encode with SW decode, target the SW pixel format (e.g. NV12)
	// before uploading; for pure SW encode, target the encoder pixel format.
	encoderPixelFormat := vss.encoder.codecContext.PixelFormat()
	if vss.encoder.usesHardwareAccelerator {
		encoderPixelFormat = profile.swPixFmt
	}

	if decoderPixelFormat != encoderPixelFormat {
		softwareFrameContext, err := astiav.CreateSoftwareScaleContext(
			vss.decoder.codecContext.Width(), vss.decoder.codecContext.Height(), decoderPixelFormat,
			vss.decoder.codecContext.Width(), vss.decoder.codecContext.Height(), encoderPixelFormat,
			astiav.NewSoftwareScaleContextFlags(astiav.SoftwareScaleContextFlagBilinear),
		)
		if err != nil {
			return fmt.Errorf("creating software scale context: %w", err)
		}

		vss.encoder.softwareFrameContext = softwareFrameContext

		vss.encoder.frame = astiav.AllocFrame()
		if vss.encoder.frame == nil {
			return errors.New("failed to allocate scaled frame")
		}

		vss.encoder.frame.SetWidth(vss.decoder.codecContext.Width())
		vss.encoder.frame.SetHeight(vss.decoder.codecContext.Height())
		vss.encoder.frame.SetPixelFormat(encoderPixelFormat)

		if err := vss.encoder.frame.AllocBuffer(0); err != nil {
			return fmt.Errorf("allocating scaled frame buffer: %w", err)
		}
	}

	if vss.encoder.usesHardwareAccelerator {
		vss.encoder.frame = astiav.AllocFrame()
		if vss.encoder.frame == nil {
			return errors.New("failed to allocate hardware frame")
		}

		if err := vss.encoder.frame.AllocHardwareBuffer(vss.encoder.hardwareFrameContext); err != nil {
			return fmt.Errorf("allocating hardware frame buffer: %w", err)
		}
	}

	return nil
}

// processPacket implements the stream interface for video. It decodes the
// packet and re-encodes each decoded frame.
func (vss *videoStreamState) processPacket(packet *astiav.Packet, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	packet.RescaleTs(vss.inStream.TimeBase(), vss.decoder.codecContext.TimeBase())

	if err := vss.decoder.codecContext.SendPacket(packet); err != nil {
		return fmt.Errorf("ffmpeg: sending video packet to decoder: %w", err)
	}

	for {
		if err := vss.decoder.codecContext.ReceiveFrame(vss.decoder.frame); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				return nil
			}

			return fmt.Errorf("ffmpeg: receiving decoded video frame: %w", err)
		}

		if err := vss.encodeVideoFrame(vss.decoder.frame, outputFmt, progressCh, totalDuration); err != nil {
			vss.decoder.frame.Unref()
			return err
		}

		vss.decoder.frame.Unref()
	}
}

// encodeVideoFrame converts and encodes a single decoded video frame.
// On the fully hardware path (isHWDecode=true) the frame is already in GPU
// memory and is passed directly to the encoder without any CPU conversion.
func (vss *videoStreamState) encodeVideoFrame(frame *astiav.Frame, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	encFrame := frame

	if !vss.isHWDecode {
		// Software decode path: convert pixel format if needed.
		if vss.encoder.softwareFrameContext != nil {
			if err := vss.encoder.softwareFrameContext.ScaleFrame(frame, vss.encoder.frame); err != nil {
				return fmt.Errorf("ffmpeg: scaling video frame: %w", err)
			}

			vss.encoder.frame.SetPts(frame.Pts())
			vss.encoder.frame.SetPictureType(astiav.PictureTypeNone)
			encFrame = vss.encoder.frame
		}

		// Upload to hardware memory when using SW decode + HW encode.
		if vss.encoder.usesHardwareAccelerator {
			if err := encFrame.TransferHardwareData(vss.encoder.frame); err != nil {
				return fmt.Errorf("ffmpeg: uploading frame to hardware: %w", err)
			}

			vss.encoder.frame.SetPts(encFrame.Pts())
			vss.encoder.frame.SetPictureType(astiav.PictureTypeNone)
			encFrame = vss.encoder.frame
		}
	}

	if err := vss.encoder.codecContext.SendFrame(encFrame); err != nil {
		return fmt.Errorf("ffmpeg: sending video frame to encoder: %w", err)
	}

	return vss.receiveAndWritePackets(vss.encoder.codecContext, vss.encoder.packet, outputFmt, progressCh, totalDuration)
}

// flush implements the stream interface for video. It first drains any frames
// buffered inside the decoder (common for H.264/H.265 with B-frames), then
// flushes the encoder.
func (vss *videoStreamState) flush(outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	// Signal EOF to the decoder so it releases all buffered frames.
	if err := vss.decoder.codecContext.SendPacket(nil); err != nil {
		return fmt.Errorf("ffmpeg: flushing video decoder: %w", err)
	}

	for {
		if err := vss.decoder.codecContext.ReceiveFrame(vss.decoder.frame); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				break
			}

			return fmt.Errorf("ffmpeg: receiving flushed video frame: %w", err)
		}

		if err := vss.encodeVideoFrame(vss.decoder.frame, outputFmt, progressCh, totalDuration); err != nil {
			vss.decoder.frame.Unref()
			return err
		}

		vss.decoder.frame.Unref()
	}

	// Flush the encoder.
	if err := vss.encoder.codecContext.SendFrame(nil); err != nil {
		return fmt.Errorf("ffmpeg: flushing video encoder: %w", err)
	}

	return vss.receiveAndWritePackets(vss.encoder.codecContext, vss.encoder.packet, outputFmt, progressCh, totalDuration)
}
