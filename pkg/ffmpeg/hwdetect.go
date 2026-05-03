package ffmpeg

import "github.com/asticode/go-astiav"

// detectHardwareEncoders returns all HWAccel values for which a hardware
// encoder is registered in libavcodec for the given output codec, in priority
// order (QSV > NVENC > VAAPI). It probes libavcodec directly — no device is
// opened, only codec registration is checked.
func detectHardwareEncoders(codec Codec) []HWAccel {
	accelerators := make([]HWAccel, 0, len(hwAccelPriority))
	for _, hw := range hwAccelPriority {
		profile, ok := hwProfiles[hw]
		if !ok {
			continue
		}

		name := profile.encoders[codec]
		if name != "" && astiav.FindEncoderByName(name) != nil {
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
	if preferred == HWAccelNone {
		return HWAccelNone
	}

	accs := detectHardwareEncoders(codec)
	if len(accs) == 0 {
		return HWAccelNone
	}

	for _, acc := range accs {
		if acc == preferred {
			return acc
		}
	}

	// preferred is HWAccelAuto, or preferred is unavailable: fall back to the
	// highest-priority available accelerator.
	return accs[0]
}
