package ffmpeg

import (
	"strings"

	"github.com/asticode/go-astiav"
)

// DetectHardwareEncoder probes the linked libavcodec for available hardware
// encoders and returns the highest-priority one found. It asks libavcodec for
// the default encoder for H.265 and H.264 (in that preference order) and
// checks whether the returned encoder's name indicates a hardware backend.
//
// Note: only encoding hardware is detected here. Hardware decoding is a
// separate capability. This function returns the HWAccel type that can be
// used for encoding; the matching hardware decoder is selected per-stream
// during transcode setup.
func DetectHardwareEncoder() (HWAccel, error) {
	for _, codecID := range []astiav.CodecID{astiav.CodecIDH265, astiav.CodecIDH264} {
		enc := astiav.FindEncoder(codecID)
		if enc == nil {
			continue
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
	}
	return HWAccelNone, nil
}
