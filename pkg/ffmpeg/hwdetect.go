package ffmpeg

import "github.com/asticode/go-astiav"

// DetectHardwareEncoder checks whether a hardware encoder is available for the
// given output codec. It probes libavcodec directly — no device is opened, only
// codec registration is checked.
//
// Hardware encode and decode capabilities are independent: a hardware
// accelerator may support encoding a codec without supporting decoding it, or
// vice versa. Use DetectHardwareDecoder to check decode-side availability.
func DetectHardwareEncoder(codec Codec) (HWAccel, error) {
	for hw, profile := range hwProfiles {
		name := hwEncoderNameForCodec(codec, profile)
		if name != "" && astiav.FindEncoderByName(name) != nil {
			return hw, nil
		}
	}
	return HWAccelNone, nil
}

// DetectHardwareDecoder checks whether a hardware decoder is available for the
// given input codec. See DetectHardwareEncoder for the distinction between
// encode and decode capability.
func DetectHardwareDecoder(codec Codec) (HWAccel, error) {
	var codecID astiav.CodecID
	switch codec {
	case CodecH264:
		codecID = astiav.CodecIDH264
	case CodecH265:
		codecID = astiav.CodecIDH265
	default:
		return HWAccelNone, nil
	}

	for hw, profile := range hwProfiles {
		name := hwDecoderNameForCodecID(codecID, profile)
		if name != "" && astiav.FindDecoderByName(name) != nil {
			return hw, nil
		}
	}
	return HWAccelNone, nil
}
