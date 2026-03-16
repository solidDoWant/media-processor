package ffmpeg

import "github.com/asticode/go-astiav"

// Hardware encoder names, used to look up a specific HW encoder in libavcodec.
// These correspond to the names registered in libavcodec for each
// hardware-accelerated encoder variant.
const (
	encoderNameH264QSV   = "h264_qsv"
	encoderNameH265QSV   = "hevc_qsv"
	encoderNameH264NVENC = "h264_nvenc"
	encoderNameH265NVENC = "hevc_nvenc"
	encoderNameH264VAAPI = "h264_vaapi"
	encoderNameH265VAAPI = "hevc_vaapi"
)

// Hardware decoder names, used to look up a specific HW decoder in libavcodec.
const (
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

// hwEncoderNameForCodec returns the hardware encoder name for the given output
// Codec in the given hardware profile. Returns "" if the codec has no HW
// encoder in this profile.
func hwEncoderNameForCodec(codec Codec, p hwProfile) string {
	switch codec {
	case CodecH264:
		return p.h264Encoder
	case CodecH265:
		return p.h265Encoder
	}
	return ""
}

// hwDecoderNameForCodecID returns the hardware decoder name for the given
// codec ID in the given hardware profile. Returns "" if the codec has no HW
// decoder in this profile.
func hwDecoderNameForCodecID(codecID astiav.CodecID, p hwProfile) string {
	switch codecID {
	case astiav.CodecIDH264:
		return p.h264Decoder
	case astiav.CodecIDH265:
		return p.h265Decoder
	}
	return ""
}
