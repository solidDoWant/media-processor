package ffmpeg

import (
	"testing"

	"github.com/asticode/go-astiav"
	"github.com/stretchr/testify/assert"
)

// TestSelectCropFilterConfig verifies that selectCropFilterConfig returns the
// correct filter graph string and buffersrc configuration for each of the six
// hardware-accelerator crop paths. The function is pure (no FFmpeg state) so
// all paths can be covered without real hardware.
func TestSelectCropFilterConfig(t *testing.T) {
	cp := CropParams{W: 1920, H: 800, X: 0, Y: 140}
	swPixFmt := astiav.PixelFormatYuv420P

	tests := []struct {
		name            string
		hwAccel         HWAccel
		hwDecodeActive  bool
		decoderPixFmt   astiav.PixelFormat
		wantStr         string
		wantSrcPixFmt   astiav.PixelFormat
		wantUseHWFrames bool
		wantHWFilters   []string
		wantOutPixFmt   astiav.PixelFormat
	}{
		{
			name:           "software path",
			hwAccel:        HWAccelNone,
			hwDecodeActive: false,
			decoderPixFmt:  swPixFmt,
			wantStr:        "crop=1920:800:0:140",
			wantSrcPixFmt:  swPixFmt,
		},
		{
			name:            "CUDA cuvid fallback (hwdownload/crop/hwupload)",
			hwAccel:         HWAccelNVENC,
			hwDecodeActive:  true,
			decoderPixFmt:   astiav.PixelFormatCuda,
			wantStr:         "hwdownload,crop=1920:800:0:140,hwupload",
			wantSrcPixFmt:   astiav.PixelFormatCuda,
			wantUseHWFrames: true,
			wantHWFilters:   []string{"hwupload"},
			wantOutPixFmt:   astiav.PixelFormatCuda,
		},
		{
			name:           "CUDA without HW decode falls back to SW",
			hwAccel:        HWAccelNVENC,
			hwDecodeActive: false,
			decoderPixFmt:  swPixFmt,
			wantStr:        "crop=1920:800:0:140",
			wantSrcPixFmt:  swPixFmt,
		},
		{
			name:            "VAAPI HW decode (hwdownload/crop/scale_vaapi)",
			hwAccel:         HWAccelVAAPI,
			hwDecodeActive:  true,
			decoderPixFmt:   astiav.PixelFormatVaapi,
			wantStr:         "hwdownload,crop=1920:800:0:140,scale_vaapi",
			wantSrcPixFmt:   astiav.PixelFormatVaapi,
			wantUseHWFrames: true,
			wantHWFilters:   []string{"scale_vaapi"},
			wantOutPixFmt:   astiav.PixelFormatVaapi,
		},
		{
			name:           "VAAPI SW decode (crop/scale_vaapi)",
			hwAccel:        HWAccelVAAPI,
			hwDecodeActive: false,
			decoderPixFmt:  swPixFmt,
			wantStr:        "crop=1920:800:0:140,scale_vaapi",
			wantSrcPixFmt:  swPixFmt,
			wantHWFilters:  []string{"scale_vaapi"},
			wantOutPixFmt:  astiav.PixelFormatVaapi,
		},
		{
			name:            "QSV HW decode (vpp_qsv)",
			hwAccel:         HWAccelQSV,
			hwDecodeActive:  true,
			decoderPixFmt:   astiav.PixelFormatQsv,
			wantStr:         "vpp_qsv=w=1920:h=800:cx=0:cy=140",
			wantSrcPixFmt:   astiav.PixelFormatQsv,
			wantUseHWFrames: true,
			wantHWFilters:   []string{"vpp_qsv"},
			wantOutPixFmt:   astiav.PixelFormatQsv,
		},
		{
			name:           "QSV without HW decode falls back to SW",
			hwAccel:        HWAccelQSV,
			hwDecodeActive: false,
			decoderPixFmt:  swPixFmt,
			wantStr:        "crop=1920:800:0:140",
			wantSrcPixFmt:  swPixFmt,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := selectCropFilterConfig(tc.hwAccel, tc.hwDecodeActive, tc.decoderPixFmt, cp)

			assert.Equal(t, tc.wantStr, got.str, "filter string")
			assert.Equal(t, tc.wantSrcPixFmt, got.srcPixFmt, "srcPixFmt")
			assert.Equal(t, tc.wantUseHWFrames, got.useHWFrames, "useHWFrames")
			assert.Equal(t, tc.wantHWFilters, got.hwFilters, "hwFilters")
			assert.Equal(t, tc.wantOutPixFmt, got.outPixFmt, "outPixFmt")
		})
	}
}
