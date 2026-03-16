package ffmpeg

import "github.com/asticode/go-astiav"

// DetectHardwareEncoders returns all HWAccel values for which a hardware
// encoder is registered in libavcodec for the given output codec, in priority
// order (QSV > NVENC > VAAPI). It probes libavcodec directly — no device is
// opened, only codec registration is checked.
//
// Hardware encode and decode capabilities are independent. Use
// DetectHardwareDecoders to check decode-side availability.
func DetectHardwareEncoders(codec Codec) []HWAccel {
	result := make([]HWAccel, 0, len(hwAccelPriority))

	for _, hw := range hwAccelPriority {
		profile, ok := hwProfiles[hw]
		if !ok {
			continue
		}

		name := profile.encoders[codec]
		if name != "" && astiav.FindEncoderByName(name) != nil {
			result = append(result, hw)
		}
	}

	return result
}

// DetectHardwareDecoders returns all HWAccel values for which a hardware
// decoder is registered in libavcodec for the given codec, in priority
// order (QSV > NVENC > VAAPI).
func DetectHardwareDecoders(codec Codec) []HWAccel {
	result := make([]HWAccel, 0, len(hwAccelPriority))

	for _, hw := range hwAccelPriority {
		profile, ok := hwProfiles[hw]
		if !ok {
			continue
		}

		name := profile.decoders[codec]
		if name != "" && astiav.FindDecoderByName(name) != nil {
			result = append(result, hw)
		}
	}

	return result
}

// GetHardwareEncoder returns preferred if it is available as a hardware encoder
// for the given codec. If preferred is HWAccelAuto or unavailable, the first
// available hardware encoder (in priority order) is returned. Returns
// HWAccelNone if no hardware encoder is available.
func GetHardwareEncoder(codec Codec, preferred HWAccel) HWAccel {
	if preferred != HWAccelAuto && preferred != HWAccelNone {
		if p, ok := hwProfiles[preferred]; ok {
			name := p.encoders[codec]
			if name != "" && astiav.FindEncoderByName(name) != nil {
				return preferred
			}
		}
	}

	accs := DetectHardwareEncoders(codec)
	if len(accs) > 0 {
		return accs[0]
	}

	return HWAccelNone
}

// GetHardwareDecoder returns preferred if it is available as a hardware decoder
// for the given codec. If preferred is HWAccelAuto or unavailable, the first
// available hardware decoder (in priority order) is returned. Returns
// HWAccelNone if no hardware decoder is available.
func GetHardwareDecoder(codec Codec, preferred HWAccel) HWAccel {
	if preferred != HWAccelAuto && preferred != HWAccelNone {
		if p, ok := hwProfiles[preferred]; ok {
			name := p.decoders[codec]
			if name != "" && astiav.FindDecoderByName(name) != nil {
				return preferred
			}
		}
	}

	accs := DetectHardwareDecoders(codec)
	if len(accs) > 0 {
		return accs[0]
	}

	return HWAccelNone
}
