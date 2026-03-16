package ffmpeg

import "github.com/asticode/go-astiav"

// hwProfile holds hardware-specific codec configuration.
// Device types come from libavutil/hwcontext.h; pixel formats from libavutil/pixfmt.h.
// encoders and decoders are keyed by codec ID so new codecs can be added without
// changing the struct definition.
type hwProfile struct {
	deviceType astiav.HardwareDeviceType
	hwPixFmt   astiav.PixelFormat        // pixel format used inside the hardware
	swPixFmt   astiav.PixelFormat        // software pixel format for upload/download (e.g. NV12)
	encoders   map[astiav.CodecID]string // codec ID → hardware encoder name
	decoders   map[astiav.CodecID]string // codec ID → hardware decoder name
}

// hwAccelPriority defines the order in which hardware accelerators are
// selected when multiple are available. The first entry has the highest
// priority.
var hwAccelPriority = []HWAccel{HWAccelQSV, HWAccelNVENC, HWAccelVAAPI}

var hwProfiles = map[HWAccel]hwProfile{
	HWAccelQSV: {
		deviceType: astiav.HardwareDeviceTypeQSV,
		hwPixFmt:   astiav.PixelFormatQsv,
		swPixFmt:   astiav.PixelFormatNv12,
		encoders: map[astiav.CodecID]string{
			astiav.CodecIDH264:       "h264_qsv",
			astiav.CodecIDH265:       "hevc_qsv",
			astiav.CodecIDVp9:        "vp9_qsv",
			astiav.CodecIDAv1:        "av1_qsv",
			astiav.CodecIDMpeg2Video: "mpeg2_qsv",
			astiav.CodecIDMjpeg:      "mjpeg_qsv",
		},
		decoders: map[astiav.CodecID]string{
			astiav.CodecIDH264:       "h264_qsv",
			astiav.CodecIDH265:       "hevc_qsv",
			astiav.CodecIDVp8:        "vp8_qsv",
			astiav.CodecIDVp9:        "vp9_qsv",
			astiav.CodecIDAv1:        "av1_qsv",
			astiav.CodecIDMpeg2Video: "mpeg2_qsv",
			astiav.CodecIDVc1:        "vc1_qsv",
			astiav.CodecIDMjpeg:      "mjpeg_qsv",
		},
	},
	HWAccelNVENC: {
		deviceType: astiav.HardwareDeviceTypeCUDA,
		hwPixFmt:   astiav.PixelFormatCuda,
		swPixFmt:   astiav.PixelFormatNv12,
		encoders: map[astiav.CodecID]string{
			astiav.CodecIDH264: "h264_nvenc",
			astiav.CodecIDH265: "hevc_nvenc",
			astiav.CodecIDAv1:  "av1_nvenc",
		},
		decoders: map[astiav.CodecID]string{
			astiav.CodecIDH264:       "h264_cuvid",
			astiav.CodecIDH265:       "hevc_cuvid",
			astiav.CodecIDVp8:        "vp8_cuvid",
			astiav.CodecIDVp9:        "vp9_cuvid",
			astiav.CodecIDAv1:        "av1_cuvid",
			astiav.CodecIDMpeg2Video: "mpeg2_cuvid",
			astiav.CodecIDMjpeg:      "mjpeg_cuvid",
			astiav.CodecIDMpeg4:      "mpeg4_cuvid",
			astiav.CodecIDVc1:        "vc1_cuvid",
		},
	},
	HWAccelVAAPI: {
		deviceType: astiav.HardwareDeviceTypeVAAPI,
		hwPixFmt:   astiav.PixelFormatVaapi,
		swPixFmt:   astiav.PixelFormatNv12,
		encoders: map[astiav.CodecID]string{
			astiav.CodecIDH264:       "h264_vaapi",
			astiav.CodecIDH265:       "hevc_vaapi",
			astiav.CodecIDVp9:        "vp9_vaapi",
			astiav.CodecIDAv1:        "av1_vaapi",
			astiav.CodecIDMjpeg:      "mjpeg_vaapi",
			astiav.CodecIDMpeg2Video: "mpeg2_vaapi",
		},
		decoders: map[astiav.CodecID]string{
			astiav.CodecIDH264:       "h264_vaapi",
			astiav.CodecIDH265:       "hevc_vaapi",
			astiav.CodecIDVp9:        "vp9_vaapi",
			astiav.CodecIDAv1:        "av1_vaapi",
			astiav.CodecIDMpeg2Video: "mpeg2_vaapi",
			astiav.CodecIDMjpeg:      "mjpeg_vaapi",
		},
	},
}

// codecToCodecID converts an output Codec to the corresponding astiav.CodecID.
// Returns astiav.CodecIDNone for codecs with no direct mapping (e.g. CodecCopy).
func codecToCodecID(codec Codec) astiav.CodecID {
	switch codec {
	case CodecH264:
		return astiav.CodecIDH264
	case CodecH265:
		return astiav.CodecIDH265
	}
	return astiav.CodecIDNone
}

// hwEncoderNameForCodec returns the hardware encoder name for the given output
// Codec in the given hardware profile. Returns "" if the codec has no HW
// encoder in this profile.
func hwEncoderNameForCodec(codec Codec, p hwProfile) string {
	return p.encoders[codecToCodecID(codec)]
}

// hwDecoderNameForCodecID returns the hardware decoder name for the given
// codec ID in the given hardware profile. Returns "" if the codec has no HW
// decoder in this profile.
func hwDecoderNameForCodecID(codecID astiav.CodecID, p hwProfile) string {
	return p.decoders[codecID]
}
