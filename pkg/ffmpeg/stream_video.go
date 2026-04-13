package ffmpeg

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/asticode/go-astiav"
)

// videoCropFilterState holds the filter graph used to apply a spatial crop to
// decoded video frames. The exact graph depends on the active hardware
// accelerator; see setupCropFilter for the per-path strategies.
type videoCropFilterState struct {
	graph   *astiav.FilterGraph
	srcCtx  *astiav.BuffersrcFilterContext
	sinkCtx *astiav.BuffersinkFilterContext
	frame   *astiav.Frame
}

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

// videoProcessingPlan captures the outputs of the decoder and crop-filter setup
// phases. It is populated by tryCuvidCropOption and setupCropFilter, then
// consumed by configureEncoderPixelFormat, setupVideoConversion, and
// encodeVideoFrame. Grouping these inter-phase values makes the data flow
// between setup stages explicit rather than relying on implicit field mutation
// order across videoStreamState.
type videoProcessingPlan struct {
	// cuvidCropApplied is true when NVDEC hardware applied the crop at decode
	// time via the cuvid AVOption (set by tryCuvidCropOption); setupCropFilter
	// skips the filter graph when this is true.
	cuvidCropApplied bool
	// cropFilter is the active libavfilter graph, or nil when no spatial crop
	// filter is needed (no crop requested, or cuvid handled it).
	cropFilter *videoCropFilterState
	// effectiveDecodedPixFmt is the pixel format of frames entering the encoder
	// pipeline. Set by setupCropFilter to the filter output format (e.g.
	// PixelFormatVaapi for VAAPI paths); zero value when no crop filter is active.
	effectiveDecodedPixFmt astiav.PixelFormat
	// encoderReceivesHWFrames is true when frames at the encoder input are
	// already in GPU pixel format. Set by configureEncoderPixelFormat; read by
	// setupVideoConversion and encodeVideoFrame.
	encoderReceivesHWFrames bool
}

// videoStreamState decodes and re-encodes a video stream.
type videoStreamState struct {
	copyStreamState

	// Decoder state.
	decoder            videoDecoderState
	encoder            videoEncoderState
	hardwareDevicePath string // device path for CreateHardwareDeviceContext; "" = auto-select

	// cropParams is the requested spatial crop region, or nil when no crop is needed.
	cropParams *CropParams
	// plan captures the outputs of the decoder and crop-filter setup phases and
	// is consumed by the encoder setup and per-frame processing. See videoProcessingPlan.
	plan videoProcessingPlan
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

	if vss.plan.cropFilter != nil {
		if vss.plan.cropFilter.graph != nil {
			vss.plan.cropFilter.graph.Free()
		}

		if vss.plan.cropFilter.frame != nil {
			vss.plan.cropFilter.frame.Free()
		}
	}
}

// setupDecoder selects and configures the decoder codec context for the video
// stream. For non-None hwAccel it attempts hardware decoding so that decoded
// frames remain in GPU memory, enabling a zero-copy decode→encode pipeline.
// Falls back silently to software decoding if HW decode is unavailable.
//
// setupDecoder allocates and configures the context but does not open it; the
// caller (buildStreamStates) opens the context, either via tryCuvidCropOption
// for NVENC with a crop region, or directly with Open(nil) otherwise.
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

	return vss.allocAndConfigDecoderContext(inStream, inputFmt)
}

// allocAndConfigDecoderContext allocates and configures a fresh decoder codec
// context using vss.decoder.codec. Any previously allocated context is freed
// first. Called by setupDecoder and tryCuvidCropOption (which needs a fresh
// context when the cuvid Open attempt fails).
func (vss *videoStreamState) allocAndConfigDecoderContext(inStream *astiav.Stream, inputFmt *astiav.FormatContext) error {
	codec := vss.decoder.codec

	if vss.decoder.codecContext != nil {
		vss.decoder.codecContext.Free()
	}

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

	return nil
}

// tryCuvidCropOption attempts to apply the cuvid decoder's built-in crop
// dictionary option (crop=TxBxLxR) for NVENC hardware acceleration with an
// active crop region. It must be called after setupDecoder (so that
// vss.decoder.codecContext is allocated and configured) and before
// setupCropFilter.
//
// On success the context is opened with the crop dict, plan.cuvidCropApplied
// is set to true, and (true, nil) is returned; the caller must NOT open the
// context again. On failure (unsupported codec or driver), the context is
// rebuilt into a freshly configured, unopened state so the caller can open it
// normally; setupCropFilter will use the hwdownload→crop→hwupload path instead.
//
// Returns (false, nil) when the conditions for cuvid crop are not met (no HW
// decode, not NVENC, or no crop region).
func (vss *videoStreamState) tryCuvidCropOption(inStream *astiav.Stream, inputFmt *astiav.FormatContext, hwAccel HWAccel) (applied bool, err error) {
	if vss.decoder.hwDevCtx == nil || hwAccel != HWAccelNVENC || vss.cropParams == nil {
		return false, nil
	}

	cp := vss.cropParams
	inW := inStream.CodecParameters().Width()
	inH := inStream.CodecParameters().Height()
	top := cp.Y
	bottom := inH - cp.Y - cp.H
	left := cp.X
	right := inW - cp.X - cp.W

	cropDict := astiav.NewDictionary()
	defer cropDict.Free()

	if setErr := cropDict.Set("crop", fmt.Sprintf("%dx%dx%dx%d", top, bottom, left, right), astiav.NewDictionaryFlags()); setErr != nil {
		return false, fmt.Errorf("ffmpeg: setting cuvid crop dictionary option: %w", setErr)
	}

	if openErr := vss.decoder.codecContext.Open(vss.decoder.codec, cropDict); openErr == nil {
		vss.plan.cuvidCropApplied = true
		return true, nil
	} else {
		slog.Debug("ffmpeg: cuvid crop dict option unsupported, will use hwdownload/crop/hwupload filter",
			"error", openErr)
	}

	// Open() leaves the context in an undefined state on failure; rebuild it so
	// the caller can perform a normal Open(nil).
	if err := vss.allocAndConfigDecoderContext(inStream, inputFmt); err != nil {
		return false, err
	}

	return false, nil
}

// cropFilterConfig describes the libavfilter graph used to apply a spatial crop.
// The strategy is chosen per hardware accelerator by selectCropFilterConfig.
type cropFilterConfig struct {
	str         string             // avfilter graph string
	srcPixFmt   astiav.PixelFormat // pixel format of frames entering buffersrc
	useHWFrames bool               // initialise buffersrc with decoder's hw_frames_ctx
	hwFilters   []string           // filter node names that require hw_device_ctx to be set
	outPixFmt   astiav.PixelFormat // effective pixel format after the graph (PixelFormatNone = unchanged SW)
}

// selectCropFilterConfig returns the cropFilterConfig for the given hardware
// accelerator and decoder state. It is a pure function — no FFmpeg state is
// accessed — which makes all six filter paths unit-testable in isolation.
//
// For VAAPI with SW decode (hwDecodeActive=false) the returned config includes
// scale_vaapi; the caller (setupCropFilter) is responsible for ensuring a VAAPI
// device context exists before building the graph, and must fall back to the SW
// config (HWAccelNone) if device creation fails.
func selectCropFilterConfig(hwAccel HWAccel, hwDecodeActive bool, decoderPixFmt astiav.PixelFormat, cp CropParams) cropFilterConfig {
	cropStr := fmt.Sprintf("crop=%d:%d:%d:%d", cp.W, cp.H, cp.X, cp.Y)

	switch {
	case hwAccel == HWAccelNVENC && hwDecodeActive:
		// CUDA fallback: cuvid is active but its crop dict option was unsupported.
		// Download frames to CPU, crop in software, re-upload to CUDA.
		return cropFilterConfig{
			str:         fmt.Sprintf("hwdownload,%s,hwupload", cropStr),
			srcPixFmt:   astiav.PixelFormatCuda,
			useHWFrames: true,
			hwFilters:   []string{"hwupload"},
			outPixFmt:   astiav.PixelFormatCuda,
		}

	case hwAccel == HWAccelVAAPI && hwDecodeActive:
		// VAAPI HW decode: download to CPU, apply SW crop, re-upload via scale_vaapi.
		return cropFilterConfig{
			str:         fmt.Sprintf("hwdownload,%s,scale_vaapi", cropStr),
			srcPixFmt:   astiav.PixelFormatVaapi,
			useHWFrames: true,
			hwFilters:   []string{"scale_vaapi"},
			outPixFmt:   astiav.PixelFormatVaapi,
		}

	case hwAccel == HWAccelVAAPI && !hwDecodeActive:
		// VAAPI SW decode: crop in software, upload via scale_vaapi.
		return cropFilterConfig{
			str:       fmt.Sprintf("%s,scale_vaapi", cropStr),
			srcPixFmt: decoderPixFmt,
			hwFilters: []string{"scale_vaapi"},
			outPixFmt: astiav.PixelFormatVaapi,
		}

	case hwAccel == HWAccelQSV && hwDecodeActive:
		// QSV: GPU-native crop and scale via vpp_qsv.
		return cropFilterConfig{
			str:         fmt.Sprintf("vpp_qsv=w=%d:h=%d:cx=%d:cy=%d", cp.W, cp.H, cp.X, cp.Y),
			srcPixFmt:   astiav.PixelFormatQsv,
			useHWFrames: true,
			hwFilters:   []string{"vpp_qsv"},
			outPixFmt:   astiav.PixelFormatQsv,
		}

	default:
		// Software path (SW hwAccel, QSV/NVENC without HW decode, or explicit fallback).
		return cropFilterConfig{
			str:       cropStr,
			srcPixFmt: decoderPixFmt,
		}
	}
}

// setupCropFilter builds the libavfilter graph that applies the crop region to
// decoded video frames. It must be called after setupDecoder so that the
// decoder's pixel format, dimensions, and hardware state are known.
//
// The filter strategy is chosen by selectCropFilterConfig based on the active
// hardware accelerator. For VAAPI with SW decode, a VAAPI device context is
// created here and stored in vss.decoder.hwDevCtx so the encoder can reuse it.
//
// plan.effectiveDecodedPixFmt is set to the output pixel format of the filter graph
// (e.g. PixelFormatVaapi for VAAPI paths). configureEncoderPixelFormat uses
// this to enable the zero-copy GPU path when the filter already outputs HW frames.
func (vss *videoStreamState) setupCropFilter(inStream *astiav.Stream, hwAccel HWAccel) error {
	cp := vss.cropParams // guaranteed non-nil by caller

	// CUDA: crop was already applied by the cuvid decoder's dict option.
	if vss.plan.cuvidCropApplied {
		return nil
	}

	hwDecodeActive := vss.decoder.hwDevCtx != nil

	// VAAPI with SW decode: create a device context for scale_vaapi if possible.
	// If creation fails, degrade to a pure software crop by clearing hwAccel so
	// selectCropFilterConfig falls through to the default SW case.
	if hwAccel == HWAccelVAAPI && !hwDecodeActive {
		if profile, ok := hwProfiles[hwAccel]; ok {
			devCtx, err := astiav.CreateHardwareDeviceContext(profile.deviceType, vss.hardwareDevicePath, nil, 0)
			if err != nil {
				slog.Debug("ffmpeg: VAAPI device context unavailable for crop filter, using SW-only crop", "error", err)
				hwAccel = HWAccelNone
			} else {
				// Store so the encoder (set up later in buildStreamStates) reuses it.
				vss.decoder.hwDevCtx = devCtx
				vss.decoder.hwPixFmt = profile.hwPixFmt
			}
		}
	}

	cfg := selectCropFilterConfig(hwAccel, hwDecodeActive, vss.decoder.codecContext.PixelFormat(), *cp)

	fg := astiav.AllocFilterGraph()
	if fg == nil {
		return errors.New("ffmpeg: failed to allocate crop filter graph")
	}

	buffersrc := astiav.FindFilterByName("buffer")
	if buffersrc == nil {
		fg.Free()

		return errors.New("ffmpeg: buffer filter not found for crop")
	}

	buffersink := astiav.FindFilterByName("buffersink")
	if buffersink == nil {
		fg.Free()

		return errors.New("ffmpeg: buffersink filter not found for crop")
	}

	srcCtx, err := fg.NewBuffersrcFilterContext(buffersrc, "in")
	if err != nil {
		fg.Free()

		return fmt.Errorf("ffmpeg: creating crop buffersrc context: %w", err)
	}

	sinkCtx, err := fg.NewBuffersinkFilterContext(buffersink, "out")
	if err != nil {
		fg.Free()

		return fmt.Errorf("ffmpeg: creating crop buffersink context: %w", err)
	}

	srcParams := astiav.AllocBuffersrcFilterContextParameters()
	defer srcParams.Free()

	srcParams.SetPixelFormat(cfg.srcPixFmt)
	srcParams.SetWidth(vss.decoder.codecContext.Width())
	srcParams.SetHeight(vss.decoder.codecContext.Height())
	srcParams.SetTimeBase(inStream.TimeBase())
	srcParams.SetSampleAspectRatio(vss.decoder.codecContext.SampleAspectRatio())

	if cfg.useHWFrames {
		srcParams.SetHardwareFramesContext(vss.decoder.codecContext.HardwareFramesContext())
	}

	if err := srcCtx.SetParameters(srcParams); err != nil {
		fg.Free()

		return fmt.Errorf("ffmpeg: setting crop buffersrc parameters: %w", err)
	}

	if err := srcCtx.Initialize(nil); err != nil {
		fg.Free()

		return fmt.Errorf("ffmpeg: initializing crop buffersrc: %w", err)
	}

	outputs := astiav.AllocFilterInOut()
	if outputs == nil {
		fg.Free()

		return errors.New("ffmpeg: failed to allocate crop filter in/out (outputs)")
	}

	defer outputs.Free()

	inputs := astiav.AllocFilterInOut()
	if inputs == nil {
		fg.Free()

		return errors.New("ffmpeg: failed to allocate crop filter in/out (inputs)")
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

	if err := fg.Parse(cfg.str, inputs, outputs); err != nil {
		fg.Free()

		return fmt.Errorf("ffmpeg: parsing crop filter graph %q: %w", cfg.str, err)
	}

	// Set the hardware device context on filters that require it (scale_vaapi,
	// vpp_qsv, hwupload). Must be done after Parse() and before Configure().
	if len(cfg.hwFilters) > 0 && vss.decoder.hwDevCtx != nil {
		hwFilterSet := make(map[string]bool, len(cfg.hwFilters))
		for _, name := range cfg.hwFilters {
			hwFilterSet[name] = true
		}

		for _, fc := range fg.Filters() {
			if hwFilterSet[fc.Filter().Name()] {
				fc.SetHardwareDeviceContext(vss.decoder.hwDevCtx)
			}
		}
	}

	if err := fg.Configure(); err != nil {
		fg.Free()

		return fmt.Errorf("ffmpeg: configuring crop filter graph: %w", err)
	}

	filterFrame := astiav.AllocFrame()
	if filterFrame == nil {
		fg.Free()

		return errors.New("ffmpeg: failed to allocate crop filter output frame")
	}

	if cfg.outPixFmt != astiav.PixelFormatNone {
		vss.plan.effectiveDecodedPixFmt = cfg.outPixFmt
	}

	vss.plan.cropFilter = &videoCropFilterState{
		graph:   fg,
		srcCtx:  srcCtx,
		sinkCtx: sinkCtx,
		frame:   filterFrame,
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

	encW := vss.decoder.codecContext.Width()
	encH := vss.decoder.codecContext.Height()

	if vss.cropParams != nil {
		encW = vss.cropParams.W
		encH = vss.cropParams.H
	}

	vss.encoder.codecContext.SetWidth(encW)
	vss.encoder.codecContext.SetHeight(encH)
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

	var openDict *astiav.Dictionary

	// libx265 has its own logging that writes directly to stderr, bypassing
	// FFmpeg's av_log callback. Align its log level with the application logger
	// so that x265 info-level noise is suppressed unless debug logging is on.
	if enc.Name() == "libx265" {
		openDict = astiav.NewDictionary()

		if err := openDict.Set("x265-params", "log-level="+x265LogLevel(), astiav.NewDictionaryFlags()); err != nil {
			return fmt.Errorf("setting x265-params: %w", err)
		}
	}

	if err := vss.encoder.codecContext.Open(vss.encoder.codec, openDict); err != nil {
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

	// HW encode with HW decode and no crop filter: decoded frames are already
	// in GPU memory — share those surfaces directly with the encoder. This also
	// covers the CUDA cuvid crop-dict path where the crop is applied by the
	// decoder itself (plan.cuvidCropApplied=true, plan.cropFilter=nil).
	if vss.decoder.hwDevCtx != nil && vss.decoder.hwPixFmt == profile.hwPixFmt && vss.plan.cropFilter == nil {
		vss.plan.encoderReceivesHWFrames = true
		vss.encoder.codecContext.SetPixelFormat(profile.hwPixFmt)

		return nil
	}

	// HW encode with a crop filter whose output is already in HW pixel format
	// (scale_vaapi, vpp_qsv, hwupload). The filter handles the CPU↔GPU transfer;
	// frames arrive at the encoder ready for zero-copy encoding.
	if vss.plan.effectiveDecodedPixFmt != astiav.PixelFormatNone && vss.plan.effectiveDecodedPixFmt == profile.hwPixFmt {
		vss.plan.encoderReceivesHWFrames = true
		vss.encoder.codecContext.SetPixelFormat(profile.hwPixFmt)
		// Provide the hardware device context so the encoder can initialise its
		// hardware-specific state (e.g. create hw_frames_ctx from hw_device_ctx).
		if vss.decoder.hwDevCtx != nil {
			vss.encoder.codecContext.SetHardwareDeviceContext(vss.decoder.hwDevCtx)
		}

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

	hwFCW := vss.decoder.codecContext.Width()
	hwFCH := vss.decoder.codecContext.Height()

	if vss.cropParams != nil {
		hwFCW = vss.cropParams.W
		hwFCH = vss.cropParams.H
	}

	vss.encoder.hardwareFrameContext.SetHardwarePixelFormat(profile.hwPixFmt)
	vss.encoder.hardwareFrameContext.SetSoftwarePixelFormat(profile.swPixFmt)
	vss.encoder.hardwareFrameContext.SetWidth(hwFCW)
	vss.encoder.hardwareFrameContext.SetHeight(hwFCH)
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
// decode→encode pipeline (plan.encoderReceivesHWFrames=true) eliminates this CPU step by
// sharing GPU surfaces between the decoder and encoder.
//
// Note: side-data formats such as Dolby Vision RPU are currently not
// preserved across re-encoding. Copy streams (CodecCopy) always preserve all
// side data since the bitstream is passed through unchanged.
func (vss *videoStreamState) setupVideoConversion(profile hwProfile) error {
	if vss.plan.encoderReceivesHWFrames {
		// Decoded frames are already in the correct HW pixel format.
		return nil
	}

	// When a crop filter is active, plan.effectiveDecodedPixFmt holds the pixel
	// format of the frames coming OUT of the filter graph (e.g. NV12 after
	// hwdownload). Otherwise fall back to the decoder's native format.
	decoderPixelFormat := vss.plan.effectiveDecodedPixFmt
	if decoderPixelFormat == astiav.PixelFormatNone {
		decoderPixelFormat = vss.decoder.codecContext.PixelFormat()
	}

	// Source dimensions entering the scaler: crop output dimensions when a
	// crop filter is active, decoder input dimensions otherwise.
	srcW := vss.decoder.codecContext.Width()
	srcH := vss.decoder.codecContext.Height()

	if vss.cropParams != nil {
		srcW = vss.cropParams.W
		srcH = vss.cropParams.H
	}

	// For HW encode with SW decode, target the SW pixel format (e.g. NV12)
	// before uploading; for pure SW encode, target the encoder pixel format.
	encoderPixelFormat := vss.encoder.codecContext.PixelFormat()
	if vss.encoder.usesHardwareAccelerator {
		encoderPixelFormat = profile.swPixFmt
	}

	if decoderPixelFormat != encoderPixelFormat {
		softwareFrameContext, err := astiav.CreateSoftwareScaleContext(
			srcW, srcH, decoderPixelFormat,
			srcW, srcH, encoderPixelFormat,
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

		vss.encoder.frame.SetWidth(srcW)
		vss.encoder.frame.SetHeight(srcH)
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
// On the fully hardware path (plan.encoderReceivesHWFrames=true) the frame is already in GPU
// memory and is passed directly to the encoder without any CPU conversion.
// When a crop filter is active, the frame is first pushed through the filter
// graph and the cropped output is used for the remainder of the pipeline.
func (vss *videoStreamState) encodeVideoFrame(frame *astiav.Frame, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	encFrame := frame

	if vss.plan.cropFilter != nil {
		if err := vss.plan.cropFilter.srcCtx.AddFrame(frame, astiav.NewBuffersrcFlags(astiav.BuffersrcFlagKeepRef)); err != nil {
			return fmt.Errorf("ffmpeg: adding frame to crop filter: %w", err)
		}

		if err := vss.plan.cropFilter.sinkCtx.GetFrame(vss.plan.cropFilter.frame, astiav.NewBuffersinkFlags()); err != nil {
			if errors.Is(err, astiav.ErrEagain) {
				// Filter needs more input before it can produce output (should not
				// occur for a simple crop filter, but handled gracefully).
				return nil
			}

			return fmt.Errorf("ffmpeg: getting frame from crop filter: %w", err)
		}

		defer vss.plan.cropFilter.frame.Unref()

		encFrame = vss.plan.cropFilter.frame
	}

	if !vss.plan.encoderReceivesHWFrames {
		// Software decode path: convert pixel format if needed.
		if vss.encoder.softwareFrameContext != nil {
			if err := vss.encoder.softwareFrameContext.ScaleFrame(encFrame, vss.encoder.frame); err != nil {
				return fmt.Errorf("ffmpeg: scaling video frame: %w", err)
			}

			vss.encoder.frame.SetPts(encFrame.Pts())
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
