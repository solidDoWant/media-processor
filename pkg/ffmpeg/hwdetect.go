package ffmpeg

import "github.com/asticode/go-astiav"

// DetectHardwareEncoder probes the linked libavcodec for available hardware
// encoders and returns the highest-priority one found. Priority order:
// QSV > NVENC > VAAPI > None.
//
// Detection uses astiav.FindEncoderByName in-process — it returns non-nil only
// if the encoder was compiled into the linked libavcodec. No subprocess or
// device probe is performed.
func DetectHardwareEncoder() (HWAccel, error) {
	if astiav.FindEncoderByName("hevc_qsv") != nil {
		return HWAccelQSV, nil
	}
	if astiav.FindEncoderByName("hevc_nvenc") != nil {
		return HWAccelNVENC, nil
	}
	if astiav.FindEncoderByName("hevc_vaapi") != nil {
		return HWAccelVAAPI, nil
	}
	return HWAccelNone, nil
}
