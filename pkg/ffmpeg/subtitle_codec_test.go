package ffmpeg

import (
	"testing"

	"github.com/asticode/go-astiav"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestSubtitleConverter_GrowsEncodeBufferOnOverflow drives the
// AVERROR_BUFFER_TOO_SMALL path in convert(): it shrinks the converter's
// initial encode buffer to a size guaranteed to be too small for any real
// ASS dialogue, then runs the mov_text fixture's first subtitle packet
// through the converter and asserts the conversion succeeds and the buffer
// grew. Without the grow-and-retry loop, a converter started with a buffer
// smaller than the encoded payload would fail the transcode outright — the
// same failure mode that would hit production today on any subtitle whose
// ASS-encoded form exceeds the (currently fixed) 64 KiB initial buffer.
func TestSubtitleConverter_GrowsEncodeBufferOnOverflow(t *testing.T) {
	// Open the mov_text fixture and read its first subtitle packet (and a
	// copy of the bytes / timing) before constructing the converter; the
	// astiav packet's data is invalidated when its FormatContext is closed.
	fmtCtx := astiav.AllocFormatContext()
	require.NotNil(t, fmtCtx)

	defer fmtCtx.Free()

	// testMovTextSubtitleSourcePath lives in the external test package
	// (transcode_test.go is package ffmpeg_test); duplicate the literal here
	// rather than re-export the constant just for cross-package access.
	require.NoError(t, fmtCtx.OpenInput("testdata/video_with_movtext_subtitle.mp4", nil, nil))

	defer fmtCtx.CloseInput()

	require.NoError(t, fmtCtx.FindStreamInfo(nil))

	var subStream *astiav.Stream

	for _, stream := range fmtCtx.Streams() {
		if stream.CodecParameters().MediaType() == astiav.MediaTypeSubtitle {
			subStream = stream
			break
		}
	}

	require.NotNil(t, subStream, "fixture must carry a subtitle stream")

	pkt := astiav.AllocPacket()
	require.NotNil(t, pkt)

	defer pkt.Free()

	var (
		subPacketData     []byte
		subPacketPts      int64
		subPacketDuration int64
	)

	for {
		err := fmtCtx.ReadFrame(pkt)
		require.NoError(t, err, "fixture must contain at least one subtitle packet before EOF")

		if pkt.StreamIndex() == subStream.Index() {
			data := pkt.Data()
			subPacketData = make([]byte, len(data))
			copy(subPacketData, data)

			subPacketPts = pkt.Pts()
			subPacketDuration = pkt.Duration()

			pkt.Unref()

			break
		}

		pkt.Unref()
	}

	// Force a tiny initial buffer so the first encode of even a trivial
	// "Hello world" dialogue fails AVERROR_BUFFER_TOO_SMALL and exercises
	// the growth path. Smaller than any ASS dialogue header would be.
	const tinyInitial = 8

	originalSize := subtitleEncodeBufferSize
	subtitleEncodeBufferSize = tinyInitial

	defer func() { subtitleEncodeBufferSize = originalSize }()

	conv, err := newSubtitleConverter(
		subStream.CodecParameters().CodecID(),
		astiav.CodecIDAss,
		subStream.CodecParameters().ExtraData(),
		subStream.TimeBase(),
	)
	require.NoError(t, err)

	defer conv.free()

	require.Equal(t, tinyInitial, int(conv.encBufSize),
		"converter must honour the overridden initial buffer size for this test to be meaningful")

	encoded, err := conv.convert(subPacketData, subPacketPts, subPacketDuration)
	require.NoError(t, err, "convert must transparently grow the encode buffer rather than failing")
	assert.NotEmpty(t, encoded, "converter must produce a non-empty ASS payload")
	assert.Contains(t, string(encoded), "Hello world",
		"grown-buffer encode must still contain the source dialogue")
	assert.Greater(t, int(conv.encBufSize), tinyInitial,
		"convert must have grown the encode buffer beyond the initial size to fit the payload")
}

// TestSubtitleConverter_FreeIsIdempotent verifies that calling free() more
// than once on the same converter is safe (no double-free, no panic) and
// that the decoder/encoder pointers are observably nil after the first
// call. Implicit because libavcodec's avcodec_free_context (called via
// mpsub_codec_close) takes AVCodecContext** and writes NULL to the pointer
// it freed, so the existing `if sc.decoder != nil` guards short-circuit on
// the second call without any explicit assignment in free() itself.
//
// Pinning this in a test avoids drift if mpsub_codec_close is ever
// reimplemented to break the contract avcodec_free_context guarantees.
func TestSubtitleConverter_FreeIsIdempotent(t *testing.T) {
	conv, err := newSubtitleConverter(
		astiav.CodecIDMovText, astiav.CodecIDAss, nil,
		astiav.NewRational(1, 1000),
	)
	require.NoError(t, err)

	require.NotNil(t, conv.decoder, "decoder must be non-nil after construction")
	require.NotNil(t, conv.encoder, "encoder must be non-nil after construction")
	require.NotNil(t, conv.encBuf, "encode buffer must be non-nil after construction")

	conv.free()

	assert.Nil(t, conv.decoder, "decoder must be nil after free (avcodec_free_context clears via double-pointer)")
	assert.Nil(t, conv.encoder, "encoder must be nil after free (avcodec_free_context clears via double-pointer)")
	assert.Nil(t, conv.encBuf, "encode buffer must be nil after free")

	// Second free must be a no-op — the nil guards short-circuit. A double
	// free would otherwise abort the process via libavcodec's internal
	// asserts long before this assertion line runs.
	assert.NotPanics(t, conv.free, "free must be safe to call more than once")
}

// TestNextEncodeBufferSize pins the doubling-with-clamp policy used by
// growEncodeBuffer. The default initial buffer (64 KiB) happens to double
// cleanly onto the 4 MiB cap, but a non-power-of-two starting size would
// previously reject payloads that still fit under the cap because doubling
// crossed it without clamping. This test covers the clamp and the
// once-already-at-cap error so future changes to the doubling strategy
// can't silently regress either case.
func TestNextEncodeBufferSize(t *testing.T) {
	tests := []struct {
		name        string
		current     int
		wantSize    int
		wantErrFunc require.ErrorAssertionFunc
	}{
		{
			name:     "doubling stays well under cap",
			current:  64 * 1024,
			wantSize: 128 * 1024,
		},
		{
			name:     "doubling lands exactly on cap",
			current:  subtitleEncodeBufferMax / 2,
			wantSize: subtitleEncodeBufferMax,
		},
		{
			name:     "doubling exceeds cap and clamps to cap",
			current:  3 * 1024 * 1024,
			wantSize: subtitleEncodeBufferMax,
		},
		{
			name:        "already at cap returns error",
			current:     subtitleEncodeBufferMax,
			wantErrFunc: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errFunc := test.wantErrFunc
			if errFunc == nil {
				errFunc = require.NoError
			}

			got, err := nextEncodeBufferSize(test.current)
			errFunc(t, err)

			if test.wantErrFunc == nil {
				assert.Equal(t, test.wantSize, got)
			}
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
