package ffmpeg

import (
	"strings"

	"github.com/asticode/go-astiav"
)

// DetectHardwareEncoder probes the linked libavcodec for available hardware
// encoders and returns the highest-priority one found. It asks libavcodec for
// the best H.265 encoder (by codec ID) and checks whether the returned
// encoder's name indicates a hardware backend. If no hardware encoder is the
// default, HWAccelNone is returned.
//
// Note: only encoding hardware is detected here. Hardware decoding is a
// separate capability and is not required by this package — we always decode
// in software and optionally encode on hardware.
func DetectHardwareEncoder() (HWAccel, error) {
	enc := astiav.FindEncoder(astiav.CodecIDH265)
	if enc == nil {
		return HWAccelNone, nil
	}

	name := enc.Name()
	switch {
	case strings.Contains(name, "qsv"):
		return HWAccelQSV, nil
	case strings.Contains(name, "nvenc"):
		return HWAccelNVENC, nil
	case strings.Contains(name, "vaapi"):
		return HWAccelVAAPI, nil
	}

	return HWAccelNone, nil
}
