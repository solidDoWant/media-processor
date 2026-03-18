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
	return detectHardwareAccelerator(codec, astiav.FindEncoderByName)
}

// DetectHardwareDecoders returns all HWAccel values for which a hardware
// decoder is registered in libavcodec for the given codec, in priority
// order (QSV > NVENC > VAAPI). It probes libavcodec directly — no device is
// opened, only codec registration is checked.
//
// Hardware encode and decode capabilities are independent. Use
// DetectHardwareEncoders to check encode-side availability.
func DetectHardwareDecoders(codec Codec) []HWAccel {
	return detectHardwareAccelerator(codec, astiav.FindDecoderByName)
}

func detectHardwareAccelerator(codec Codec, findAccelByCodec func(string) *astiav.Codec) []HWAccel {
	accelerators := make([]HWAccel, 0, len(hwAccelPriority))
	for _, hw := range hwAccelPriority {
		profile, ok := hwProfiles[hw]
		if !ok {
			continue
		}

		name := profile.encoders[codec]
		if name != "" && findAccelByCodec(name) != nil {
			accelerators = append(accelerators, hw)
		}
	}

	return accelerators
}

// GetHardwareEncoder returns preferred if it is available as a hardware encoder
// for the given codec. If preferred is HWAccelAuto or unavailable, the first
// available hardware encoder (in priority order) is returned. Returns
// HWAccelNone if no hardware encoder is available.
func GetHardwareEncoder(codec Codec, preferred HWAccel) HWAccel {
	return getHardwareAccelerator(codec, preferred, DetectHardwareEncoders)
}

// GetHardwareDecoder returns preferred if it is available as a hardware decoder
// for the given codec. If preferred is HWAccelAuto or unavailable, the first
// available hardware decoder (in priority order) is returned. Returns
// HWAccelNone if no hardware decoder is available.
func GetHardwareDecoder(codec Codec, preferred HWAccel) HWAccel {
	return getHardwareAccelerator(codec, preferred, DetectHardwareDecoders)
}

func getHardwareAccelerator(codec Codec, preferred HWAccel, detectFunc func(Codec) []HWAccel) HWAccel {
	if preferred == HWAccelNone {
		return HWAccelNone
	}

	accs := detectFunc(codec)
	if len(accs) == 0 {
		return HWAccelNone
	}

	// If preferred is available, return it.
	for _, acc := range accs {
		if acc == preferred {
			return acc
		}
	}

	// In the case of auto or preferred not being available, return the first available hardware accelerator.
	return accs[0]
}
