package ffmpeg

import "github.com/asticode/go-astiav"

// detectHardwareEncoders returns all HWAccel values for which a hardware
// encoder is registered in libavcodec for the given output codec, in priority
// order (QSV > VAAPI). It probes libavcodec directly — no device is opened,
// only codec registration is checked.
func detectHardwareEncoders(codec Codec) []HWAccel {
	accelerators := make([]HWAccel, 0, len(hwAccelPriority))
	for _, accelerator := range hwAccelPriority {
		profile, ok := hwProfiles[accelerator]
		if !ok {
			continue
		}

		name := profile.encoders[codec]
		if name != "" && astiav.FindEncoderByName(name) != nil {
			accelerators = append(accelerators, accelerator)
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

	availableAccelerators := detectHardwareEncoders(codec)
	if len(availableAccelerators) == 0 {
		return HWAccelNone
	}

	for _, accelerator := range availableAccelerators {
		if accelerator == preferred {
			return accelerator
		}
	}

	// preferred is HWAccelAuto, or preferred is unavailable: fall back to the
	// highest-priority available accelerator.
	return availableAccelerators[0]
}
