package workflows

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/media-processor/pkg/ffmpeg"
	"github.com/solidDoWant/media-processor/pkg/ffprobe"
)

func TestSelectVideoCodec(t *testing.T) {
	tests := []struct {
		name     string
		info     *ffprobe.MediaInfo
		expected ffmpeg.Codec
	}{
		{
			name: "H.264 in MKV container is copied without re-encoding",
			info: &ffprobe.MediaInfo{
				Format: "matroska,webm",
				Streams: []ffprobe.StreamInfo{
					{CodecType: ffprobe.CodecTypeVideo, CodecName: "h264"},
				},
			},
			expected: ffmpeg.CodecCopy,
		},
		{
			name: "H.265/HEVC in MKV container is copied without re-encoding",
			info: &ffprobe.MediaInfo{
				Format: "matroska,webm",
				Streams: []ffprobe.StreamInfo{
					{CodecType: ffprobe.CodecTypeVideo, CodecName: "hevc"},
				},
			},
			expected: ffmpeg.CodecCopy,
		},
		{
			name: "H.264 in MP4 container is transcoded to H.265",
			info: &ffprobe.MediaInfo{
				Format: "mov,mp4,m4a,3gp,3g2,mj2",
				Streams: []ffprobe.StreamInfo{
					{CodecType: ffprobe.CodecTypeVideo, CodecName: "h264"},
				},
			},
			expected: ffmpeg.CodecH265,
		},
		{
			name: "MPEG-4 video in MKV container is transcoded to H.265",
			info: &ffprobe.MediaInfo{
				Format: "matroska,webm",
				Streams: []ffprobe.StreamInfo{
					{CodecType: ffprobe.CodecTypeVideo, CodecName: "mpeg4"},
				},
			},
			expected: ffmpeg.CodecH265,
		},
		{
			name: "audio-only MKV is transcoded (no video stream to copy)",
			info: &ffprobe.MediaInfo{
				Format: "matroska,webm",
				Streams: []ffprobe.StreamInfo{
					{CodecType: ffprobe.CodecTypeAudio, CodecName: "aac"},
				},
			},
			expected: ffmpeg.CodecH265,
		},
		{
			name: "H.264 with audio track in MKV is copied (first video stream wins)",
			info: &ffprobe.MediaInfo{
				Format: "matroska,webm",
				Streams: []ffprobe.StreamInfo{
					{CodecType: ffprobe.CodecTypeAudio, CodecName: "aac"},
					{CodecType: ffprobe.CodecTypeVideo, CodecName: "h264"},
				},
			},
			expected: ffmpeg.CodecCopy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := selectVideoCodec(tt.info)
			assert.Equal(t, tt.expected, got)
		})
	}
}
