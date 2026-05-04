package ffmpeg

import (
	"testing"

	"github.com/asticode/go-astiav"
	"github.com/stretchr/testify/assert"
)

// TestMatroskaSupportsCodec pins the matroska codec-tag table for the codec
// IDs that drive the subtitle transcode policy in buildStreamStates. The
// transcoder only invokes the libavcodec subtitle pipeline when the muxer
// reports a codec as unsupported; if upstream FFmpeg starts accepting
// mov_text by copy this test will fail and the policy will need rethinking.
func TestMatroskaSupportsCodec(t *testing.T) {
	tests := []struct {
		name    string
		codecID astiav.CodecID
		want    bool
	}{
		{"mov_text is the prod failure case", astiav.CodecIDMovText, false},
		{"subrip is matroska-native via S_TEXT/UTF8", astiav.CodecIDSubrip, true},
		{"ass is matroska-native via S_TEXT/ASS", astiav.CodecIDAss, true},
		{"hdmv_pgs is matroska-native via S_HDMV/PGS", astiav.CodecIDHdmvPgsSubtitle, true},
		{"dvd_subtitle (VobSub) is matroska-native via S_VOBSUB", astiav.CodecIDDvdSubtitle, true},
		{"dvb_subtitle is matroska-native via S_DVBSUB", astiav.CodecIDDvbSubtitle, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, matroskaSupportsCodec(test.codecID))
		})
	}
}

// TestEffectiveContainerIsMKV pins the recognised matroska output extensions.
// effectiveContainerIsMKV gates the source-side cover-art exclusion and the
// mov_text → ASS subtitle transcode; if it doesn't recognise the full
// matroska family of extensions when the container is inferred from the
// output filename, callers writing to .mka / .mks / .mk3d will hit the same
// header-write failures the explicit ContainerMKV path already avoids.
func TestEffectiveContainerIsMKV(t *testing.T) {
	tests := []struct {
		name       string
		container  Container
		outputPath string
		want       bool
	}{
		{name: "explicit ContainerMKV", container: ContainerMKV, outputPath: "/tmp/anything", want: true},
		{name: "explicit ContainerMP4 ignores extension", container: ContainerMP4, outputPath: "/tmp/foo.mkv", want: false},
		{name: "inferred from .mkv extension", container: "", outputPath: "/tmp/foo.mkv", want: true},
		{name: "inferred from .mka audio-only matroska", container: "", outputPath: "/tmp/foo.mka", want: true},
		{name: "inferred from .mks subtitles-only matroska", container: "", outputPath: "/tmp/foo.mks", want: true},
		{name: "inferred from .mk3d 3D matroska", container: "", outputPath: "/tmp/foo.mk3d", want: true},
		{name: "inferred extension is case-insensitive", container: "", outputPath: "/tmp/Foo.MKV", want: true},
		{name: "inferred from non-matroska extension", container: "", outputPath: "/tmp/foo.mp4", want: false},
		{name: "inferred from no extension", container: "", outputPath: "/tmp/foo", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			b := &TranscodeBuilder{container: test.container, outputPath: test.outputPath}
			assert.Equal(t, test.want, b.effectiveContainerIsMKV())
		})
	}
}

// TestIsTextSubtitleCodec pins the codec-descriptor flag the policy uses to
// decide whether an unsupported subtitle codec is transcodable to ASS. Text
// codecs (mov_text, subrip, ass) yield true; bitmap codecs and non-subtitle
// codecs yield false. A regression here would either send bitmap streams
// into the libavcodec subtitle pipeline (where they would fail or produce
// garbage) or silently drop transcodable text streams.
func TestIsTextSubtitleCodec(t *testing.T) {
	tests := []struct {
		name    string
		codecID astiav.CodecID
		want    bool
	}{
		{"mov_text", astiav.CodecIDMovText, true},
		{"subrip", astiav.CodecIDSubrip, true},
		{"ass", astiav.CodecIDAss, true},
		{"hdmv_pgs is bitmap, not text", astiav.CodecIDHdmvPgsSubtitle, false},
		{"dvd_subtitle (VobSub) is bitmap, not text", astiav.CodecIDDvdSubtitle, false},
		{"dvb_subtitle is bitmap, not text", astiav.CodecIDDvbSubtitle, false},
		{"non-subtitle codecs are not text-subtitle", astiav.CodecIDH264, false},
		{"unknown codec ID", astiav.CodecIDNone, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, isTextSubtitleCodec(test.codecID))
		})
	}
}
