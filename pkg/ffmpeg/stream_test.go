package ffmpeg

import (
	"path/filepath"
	"testing"

	"github.com/asticode/go-astiav"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMonotonicDtsClamp covers the pure timestamp-clamp logic used by
// receiveAndWritePackets to repair non-monotonic DTS coming out of encoders
// (notably hevc_qsv on variable-frame-rate sources, where Intel's libmfx
// runtime computes DecodeTimeStamp assuming a uniform input cadence).
//
// The function is a pure transform on three int64s, so every branch can be
// exercised here without touching a hardware encoder, a FormatContext, or any
// FFmpeg I/O.
func TestMonotonicDtsClamp(t *testing.T) {
	tests := []struct {
		name           string
		lastWrittenDts int64
		encDts         int64
		encPts         int64
		wantDts        int64
		wantPts        int64
		wantClamped    bool
	}{
		{
			name:           "first packet on stream — no previous DTS",
			lastWrittenDts: astiav.NoPtsValue,
			encDts:         100,
			encPts:         100,
			wantDts:        100,
			wantPts:        100,
			wantClamped:    false,
		},
		{
			name:           "encoder DTS strictly greater than previous — pass through",
			lastWrittenDts: 100,
			encDts:         141,
			encPts:         141,
			wantDts:        141,
			wantPts:        141,
			wantClamped:    false,
		},
		{
			name:           "encoder DTS equal to previous — clamp upward, PTS already in front",
			lastWrittenDts: 100,
			encDts:         100,
			encPts:         200,
			wantDts:        101,
			wantPts:        200,
			wantClamped:    true,
		},
		{
			name:           "encoder DTS lower than previous — clamp upward, PTS already in front",
			lastWrittenDts: 200,
			encDts:         150,
			encPts:         300,
			wantDts:        201,
			wantPts:        300,
			wantClamped:    true,
		},
		{
			name:           "encoder DTS lower than previous and PTS lower than corrected — both clamped",
			lastWrittenDts: 200,
			encDts:         150,
			encPts:         180,
			wantDts:        201,
			wantPts:        201,
			wantClamped:    true,
		},
		{
			name:           "encoder DTS lower than previous and PTS equals corrected — PTS unchanged",
			lastWrittenDts: 200,
			encDts:         150,
			encPts:         201,
			wantDts:        201,
			wantPts:        201,
			wantClamped:    true,
		},
		{
			name:           "packet has no DTS with a previous DTS — synthesize one past the previous",
			lastWrittenDts: 100,
			encDts:         astiav.NoPtsValue,
			encPts:         200,
			wantDts:        101,
			wantPts:        200,
			wantClamped:    true,
		},
		{
			name:           "first packet has no DTS — anchor synthesized DTS to its PTS",
			lastWrittenDts: astiav.NoPtsValue,
			encDts:         astiav.NoPtsValue,
			encPts:         200,
			wantDts:        200,
			wantPts:        200,
			wantClamped:    true,
		},
		{
			name:           "synthesized DTS past previous lands ahead of an earlier PTS — PTS clamped up",
			lastWrittenDts: 100,
			encDts:         astiav.NoPtsValue,
			encPts:         83,
			wantDts:        101,
			wantPts:        101,
			wantClamped:    true,
		},
		{
			name:           "first packet has neither DTS nor PTS — nothing to synthesize from",
			lastWrittenDts: astiav.NoPtsValue,
			encDts:         astiav.NoPtsValue,
			encPts:         astiav.NoPtsValue,
			wantDts:        astiav.NoPtsValue,
			wantPts:        astiav.NoPtsValue,
			wantClamped:    false,
		},
		{
			name:           "clamp fires but encoder PTS unset — leave PTS unset",
			lastWrittenDts: 200,
			encDts:         150,
			encPts:         astiav.NoPtsValue,
			wantDts:        201,
			wantPts:        astiav.NoPtsValue,
			wantClamped:    true,
		},
		{
			name:           "monotonic DTS but PTS behind DTS — PTS clamped up to DTS",
			lastWrittenDts: 100,
			encDts:         150,
			encPts:         120,
			wantDts:        150,
			wantPts:        150,
			wantClamped:    true,
		},
		{
			name:           "first packet with PTS behind DTS — PTS clamped up to DTS",
			lastWrittenDts: astiav.NoPtsValue,
			encDts:         150,
			encPts:         120,
			wantDts:        150,
			wantPts:        150,
			wantClamped:    true,
		},
		{
			name:           "monotonic DTS and PTS ahead — pass through unchanged",
			lastWrittenDts: 100,
			encDts:         150,
			encPts:         300,
			wantDts:        150,
			wantPts:        300,
			wantClamped:    false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotDts, gotPts, gotClamped := monotonicDtsClamp(test.lastWrittenDts, test.encDts, test.encPts)
			assert.Equal(t, test.wantDts, gotDts, "DTS")
			assert.Equal(t, test.wantPts, gotPts, "PTS")
			assert.Equal(t, test.wantClamped, gotClamped, "clamped flag")
		})
	}
}

// TestMonotonicDtsClamp_FMABFailureSequence replays the exact non-monotonic
// DTS sequence the hevc_qsv encoder emitted on the production Fullmetal
// Alchemist:Brotherhood S01E45 source (matroska 1/1000 timebase). Verifies
// that the per-packet clamp is sufficient to repair the entire run rather
// than just the first offending packet.
func TestMonotonicDtsClamp_FMABFailureSequence(t *testing.T) {
	type encOut struct{ dts, pts int64 }
	// Approximation of the production failure pattern: the encoder produces
	// monotonic DTS through ~packet K, then emits a packet whose DTS is
	// hundreds of ticks behind, then several more out-of-order packets, then
	// resumes monotonic output. The first column is what the encoder hands us;
	// the second is the corresponding PTS (also irregular due to VFR input).
	encoderOutputs := []encOut{
		{dts: 400029, pts: 400030},
		{dts: 400071, pts: 400113},
		{dts: 400113, pts: 400071},
		{dts: 400155, pts: 400155},
		{dts: 400176, pts: 400238}, // last good DTS before regression
		{dts: 400029, pts: 400029}, // <-- regression: DTS goes backward
		{dts: 400071, pts: 400113},
		{dts: 400113, pts: 400196},
		{dts: 400238, pts: 400280},
		{dts: 400280, pts: 400405},
	}

	lastWritten := int64(astiav.NoPtsValue)
	clampedCount := 0
	muxedDts := make([]int64, 0, len(encoderOutputs))

	for _, encoderOutput := range encoderOutputs {
		newDts, _, clamped := monotonicDtsClamp(lastWritten, encoderOutput.dts, encoderOutput.pts)
		if clamped {
			clampedCount++
		}

		muxedDts = append(muxedDts, newDts)
		lastWritten = newDts
	}

	// Three packets regress the DTS, and one earlier packet ({dts: 400113,
	// pts: 400071}) carries a PTS behind its own (already-monotonic) DTS — the
	// clamp repairs that too so the muxer does not reject it with EINVAL.
	assert.Equal(t, 4, clampedCount, "clamp must fire on every regressing or pts-behind-dts packet")

	for i := 1; i < len(muxedDts); i++ {
		assert.Greater(t, muxedDts[i], muxedDts[i-1],
			"muxed DTS must be strictly monotonic at packet %d (got %d after %d)",
			i, muxedDts[i], muxedDts[i-1])
	}
}

// TestMonotonicDtsClamp_MissingDtsSequence replays the leading packets of a
// production NF 2160p HEVC WEB-DL (The Haunting S01E06) copied into matroska.
// The first reordered B-frames carry no DTS at all (AV_NOPTS_VALUE) while their
// PTS is set. Forwarding the missing DTS to the muxer made libavformat derive
// one from the PTS, which ran backwards across the reordered frames and was
// rejected ("non monotonically increasing dts to muxer in stream 0: 85 >= 83").
// Verifies the clamp synthesizes a strictly monotonic DTS for the whole run.
func TestMonotonicDtsClamp_MissingDtsSequence(t *testing.T) {
	const noDts = astiav.NoPtsValue

	type srcPkt struct{ dts, pts int64 }
	// Source packet timestamps as demuxed (matroska 1/1000 timebase), in file
	// order. Packets 1 and 2 have no DTS; their PTS (167, 83) is the value the
	// muxer would otherwise derive a regressing DTS from.
	sourcePackets := []srcPkt{
		{dts: 0, pts: 0},
		{dts: noDts, pts: 167},
		{dts: noDts, pts: 83},
		{dts: 42, pts: 42},
		{dts: 83, pts: 125},
		{dts: 125, pts: 334},
		{dts: 167, pts: 250},
		{dts: 209, pts: 209},
	}

	lastWritten := int64(astiav.NoPtsValue)
	muxedDts := make([]int64, 0, len(sourcePackets))

	for _, sourcePacket := range sourcePackets {
		newDts, newPts, _ := monotonicDtsClamp(lastWritten, sourcePacket.dts, sourcePacket.pts)

		require.NotEqual(t, int64(astiav.NoPtsValue), newDts,
			"every muxed packet must carry a DTS — the muxer rejects unset timestamps")
		assert.GreaterOrEqual(t, newPts, newDts, "PTS must never sit behind its own DTS")

		muxedDts = append(muxedDts, newDts)
		lastWritten = newDts
	}

	for i := 1; i < len(muxedDts); i++ {
		assert.Greater(t, muxedDts[i], muxedDts[i-1],
			"muxed DTS must be strictly monotonic at packet %d (got %d after %d)",
			i, muxedDts[i], muxedDts[i-1])
	}
}

// TestCopyStreamState_processPacket_RepairsNonMonotonicSourceDts verifies that
// the pure-copy packet path repairs non-monotonic DTS coming from the input
// source before submitting packets to the muxer. Without the repair,
// libavformat's strict DTS monotonicity check rejects the second submission
// with EINVAL ("Application provided invalid, non monotonically increasing
// dts to muxer").
//
// The motivating production failure is a PGS subtitle stream whose authored
// Block timestamps go briefly backwards between adjacent display sets. The
// ffmpeg CLI silently clamps these at its stream-output layer (sost); our
// in-process transcoder reached the muxer directly, so the activity failed
// on the first regression. The same clamp logic already exists for encoded
// streams (receiveAndWritePackets, monotonicDtsClamp) — this test exercises
// the parallel path on copy streams.
func TestCopyStreamState_processPacket_RepairsNonMonotonicSourceDts(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "out.mkv")

	inputFmt := astiav.AllocFormatContext()
	require.NotNil(t, inputFmt)

	defer inputFmt.Free()

	require.NoError(t, inputFmt.OpenInput("testdata/video_with_subrip_subtitle.mkv", nil, nil))
	defer inputFmt.CloseInput()

	require.NoError(t, inputFmt.FindStreamInfo(nil))

	var inAudioStream *astiav.Stream

	for _, stream := range inputFmt.Streams() {
		if stream.CodecParameters().MediaType() == astiav.MediaTypeAudio {
			inAudioStream = stream
			break
		}
	}

	require.NotNil(t, inAudioStream, "test fixture must carry an audio stream")

	outputFmt, err := astiav.AllocOutputFormatContext(nil, "matroska", outputPath)
	require.NoError(t, err)

	defer outputFmt.Free()

	outStream := outputFmt.NewStream(nil)
	require.NotNil(t, outStream)
	require.NoError(t, inAudioStream.CodecParameters().Copy(outStream.CodecParameters()))
	outStream.CodecParameters().SetCodecTag(0)
	outStream.SetTimeBase(inAudioStream.TimeBase())

	ioCtx, err := astiav.OpenIOContext(outputPath, astiav.NewIOContextFlags(astiav.IOContextFlagWrite), nil, nil)
	require.NoError(t, err)

	defer func() { _ = ioCtx.Close() }()

	outputFmt.SetPb(ioCtx)

	require.NoError(t, outputFmt.WriteHeader(nil))

	css := &copyStreamState{inStream: inAudioStream}
	css.setOutputStream(outStream)

	pkt := astiav.AllocPacket()
	defer pkt.Free()

	readAudioPacket := func() {
		t.Helper()

		for {
			err := inputFmt.ReadFrame(pkt)
			require.NoError(t, err, "input must supply enough audio packets for the test")

			if pkt.StreamIndex() == inAudioStream.Index() {
				return
			}

			pkt.Unref()
		}
	}

	// Override the natural DTS on both packets so the regression on the second
	// is precisely controlled rather than dependent on whatever the demuxer
	// happens to emit.
	readAudioPacket()
	pkt.SetDts(100)
	pkt.SetPts(100)

	require.NoError(t, css.processPacket(pkt, outputFmt, nil, 0),
		"first packet (monotonic DTS) must be accepted by the muxer")

	pkt.Unref()

	readAudioPacket()
	pkt.SetDts(50)
	pkt.SetPts(50)

	require.NoError(t, css.processPacket(pkt, outputFmt, nil, 0),
		"non-monotonic source DTS must be repaired on the copy path so the matroska muxer accepts the packet")

	pkt.Unref()

	assert.Equal(t, int64(1), css.clampedCount,
		"clampedCount must increment on the first repaired packet")

	readAudioPacket()
	pkt.SetDts(60)
	pkt.SetPts(60)

	require.NoError(t, css.processPacket(pkt, outputFmt, nil, 0),
		"second non-monotonic packet must also be repaired and accepted")

	pkt.Unref()

	require.NoError(t, outputFmt.WriteTrailer())

	assert.Greater(t, css.lastWrittenDts, int64(100),
		"copyStreamState.lastWrittenDts must advance strictly past the previous packet's DTS after the repair")
	assert.Equal(t, int64(2), css.clampedCount,
		"clampedCount must accumulate across every repaired packet so the summary log reflects the true total")
}

// TestCopyStreamState_processPacket_RepairsPtsBehindDts verifies that the
// pure-copy packet path repairs a packet whose PTS is behind its own DTS
// before submitting it to the muxer, even when the DTS is monotonic (so the
// non-monotonic-DTS clamp does not fire). libavformat rejects a packet with
// pts < dts with EINVAL ("Invalid argument") in av_interleaved_write_frame.
//
// The motivating production failure is a copied HEVC video stream (stream 0)
// of a 2160p WEB-DL whose authored packet timestamps include pts < dts. The
// ffmpeg CLI tolerates this by clamping pts up to dts at its stream-output
// layer; our in-process transcoder reaches the muxer directly, so the activity
// failed with "ffmpeg: writing remuxed packet for stream 0: Invalid argument".
// The DTS here is strictly increasing, so the existing non-monotonic-DTS clamp
// leaves the packet untouched — only the dedicated pts >= dts repair prevents
// the rejection.
func TestCopyStreamState_processPacket_RepairsPtsBehindDts(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "out.mkv")

	inputFmt := astiav.AllocFormatContext()
	require.NotNil(t, inputFmt)

	defer inputFmt.Free()

	require.NoError(t, inputFmt.OpenInput("testdata/video_with_subrip_subtitle.mkv", nil, nil))
	defer inputFmt.CloseInput()

	require.NoError(t, inputFmt.FindStreamInfo(nil))

	var inAudioStream *astiav.Stream

	for _, stream := range inputFmt.Streams() {
		if stream.CodecParameters().MediaType() == astiav.MediaTypeAudio {
			inAudioStream = stream
			break
		}
	}

	require.NotNil(t, inAudioStream, "test fixture must carry an audio stream")

	outputFmt, err := astiav.AllocOutputFormatContext(nil, "matroska", outputPath)
	require.NoError(t, err)

	defer outputFmt.Free()

	outStream := outputFmt.NewStream(nil)
	require.NotNil(t, outStream)
	require.NoError(t, inAudioStream.CodecParameters().Copy(outStream.CodecParameters()))
	outStream.CodecParameters().SetCodecTag(0)
	outStream.SetTimeBase(inAudioStream.TimeBase())

	ioCtx, err := astiav.OpenIOContext(outputPath, astiav.NewIOContextFlags(astiav.IOContextFlagWrite), nil, nil)
	require.NoError(t, err)

	defer func() { _ = ioCtx.Close() }()

	outputFmt.SetPb(ioCtx)

	require.NoError(t, outputFmt.WriteHeader(nil))

	css := &copyStreamState{inStream: inAudioStream}
	css.setOutputStream(outStream)

	pkt := astiav.AllocPacket()
	defer pkt.Free()

	readAudioPacket := func() {
		t.Helper()

		for {
			err := inputFmt.ReadFrame(pkt)
			require.NoError(t, err, "input must supply enough audio packets for the test")

			if pkt.StreamIndex() == inAudioStream.Index() {
				return
			}

			pkt.Unref()
		}
	}

	// First packet: well-formed (pts == dts), establishes the baseline DTS.
	readAudioPacket()
	pkt.SetDts(100)
	pkt.SetPts(100)

	require.NoError(t, css.processPacket(pkt, outputFmt, nil, 0),
		"first packet (pts == dts) must be accepted by the muxer")

	pkt.Unref()

	// Second packet: DTS is strictly monotonic (150 > 100) so the
	// non-monotonic-DTS clamp does not fire, but PTS (120) is behind DTS (150).
	// Without the pts >= dts repair the matroska muxer rejects this with EINVAL.
	readAudioPacket()
	pkt.SetDts(150)
	pkt.SetPts(120)

	require.NoError(t, css.processPacket(pkt, outputFmt, nil, 0),
		"a packet whose PTS is behind its monotonic DTS must be repaired so the matroska muxer accepts it")

	pkt.Unref()

	assert.Equal(t, int64(1), css.clampedCount,
		"the pts < dts repair must be recorded as a clamp")

	require.NoError(t, outputFmt.WriteTrailer())
}

// TestCopyStreamState_processPacket_SynthesizesMissingDts verifies that the
// pure-copy packet path synthesizes a monotonic DTS for packets that arrive
// with no DTS at all (AV_NOPTS_VALUE) before submitting them to the muxer.
//
// The motivating production failure is a copied HEVC video stream (stream 0)
// of a 2160p NF WEB-DL whose leading reordered B-frames carry no DTS while
// their PTS is set. Forwarding the unset DTS made libavformat derive one from
// the PTS, which ran backwards across the reordered frames — the matroska muxer
// rejected the third packet with "Application provided invalid, non
// monotonically increasing dts to muxer in stream 0: 85 >= 83" (EINVAL,
// surfaced as "writing remuxed packet for stream 0: Invalid argument"). The
// ffmpeg CLI fills a running DTS estimate on its stream-copy path; our
// in-process transcoder reaches the muxer directly, so the synthesis must
// happen here.
func TestCopyStreamState_processPacket_SynthesizesMissingDts(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "out.mkv")

	inputFmt := astiav.AllocFormatContext()
	require.NotNil(t, inputFmt)

	defer inputFmt.Free()

	require.NoError(t, inputFmt.OpenInput("testdata/video_with_subrip_subtitle.mkv", nil, nil))
	defer inputFmt.CloseInput()

	require.NoError(t, inputFmt.FindStreamInfo(nil))

	var inAudioStream *astiav.Stream

	for _, stream := range inputFmt.Streams() {
		if stream.CodecParameters().MediaType() == astiav.MediaTypeAudio {
			inAudioStream = stream
			break
		}
	}

	require.NotNil(t, inAudioStream, "test fixture must carry an audio stream")

	outputFmt, err := astiav.AllocOutputFormatContext(nil, "matroska", outputPath)
	require.NoError(t, err)

	defer outputFmt.Free()

	outStream := outputFmt.NewStream(nil)
	require.NotNil(t, outStream)
	require.NoError(t, inAudioStream.CodecParameters().Copy(outStream.CodecParameters()))
	outStream.CodecParameters().SetCodecTag(0)
	outStream.SetTimeBase(inAudioStream.TimeBase())

	ioCtx, err := astiav.OpenIOContext(outputPath, astiav.NewIOContextFlags(astiav.IOContextFlagWrite), nil, nil)
	require.NoError(t, err)

	defer func() { _ = ioCtx.Close() }()

	outputFmt.SetPb(ioCtx)

	require.NoError(t, outputFmt.WriteHeader(nil))

	css := &copyStreamState{inStream: inAudioStream}
	css.setOutputStream(outStream)

	pkt := astiav.AllocPacket()
	defer pkt.Free()

	readAudioPacket := func() {
		t.Helper()

		for {
			err := inputFmt.ReadFrame(pkt)
			require.NoError(t, err, "input must supply enough audio packets for the test")

			if pkt.StreamIndex() == inAudioStream.Index() {
				return
			}

			pkt.Unref()
		}
	}

	// First packet: well-formed, establishes the baseline DTS.
	readAudioPacket()
	pkt.SetDts(0)
	pkt.SetPts(0)

	require.NoError(t, css.processPacket(pkt, outputFmt, nil, 0),
		"first packet (monotonic DTS) must be accepted by the muxer")

	pkt.Unref()

	// Next two packets carry no DTS, with PTS that would derive a regressing DTS
	// (167 then 83) if forwarded unset. The synthesis must keep them monotonic.
	for _, pts := range []int64{167, 83} {
		readAudioPacket()
		pkt.SetDts(astiav.NoPtsValue)
		pkt.SetPts(pts)

		require.NoError(t, css.processPacket(pkt, outputFmt, nil, 0),
			"a packet with no DTS must get a synthesized monotonic DTS so the matroska muxer accepts it")

		pkt.Unref()
	}

	require.NoError(t, outputFmt.WriteTrailer())

	assert.Equal(t, int64(2), css.clampedCount,
		"each missing-DTS packet must be recorded as a clamp")
	assert.Greater(t, css.lastWrittenDts, int64(0),
		"the synthesized DTS must advance strictly past the baseline packet's DTS")
}

// testADTSAACSourcePath is an MPEG-TS clip whose AAC track is ADTS-framed and
// therefore carries no AudioSpecificConfig extradata — the shape every off-air
// TS recording has. See testdata/README.md.
const testADTSAACSourcePath = "testdata/video_adts_aac.ts"

// firstAudioStream returns the first audio stream of an opened input, failing
// the test when the fixture has none.
func firstAudioStream(t *testing.T, inputFmt *astiav.FormatContext) *astiav.Stream {
	t.Helper()

	for _, stream := range inputFmt.Streams() {
		if stream.CodecParameters().MediaType() == astiav.MediaTypeAudio {
			return stream
		}
	}

	require.FailNow(t, "test fixture must carry an audio stream")

	return nil
}

// TestIsADTSFrame covers the syncword check that decides whether a copied AAC
// packet is a frame the ADTS → AudioSpecificConfig conversion can parse. The
// syncword is the top 12 bits of the first two bytes, so the low nibble of the
// second byte (MPEG version, layer, protection-absent) must not participate.
func TestIsADTSFrame(t *testing.T) {
	tests := []struct {
		name     string
		payload  []byte
		expected bool
	}{
		{
			name:     "MPEG-4 ADTS frame without CRC",
			payload:  []byte{0xFF, 0xF1, 0x50, 0x40, 0x23, 0xFF, 0xFC},
			expected: true,
		},
		{
			name:     "MPEG-2 ADTS frame with CRC",
			payload:  []byte{0xFF, 0xF8, 0x4C, 0xA0, 0x52, 0x41, 0x6C},
			expected: true,
		},
		{
			name:     "fragment of a frame captured mid-stream",
			payload:  []byte{0xD3, 0xAF, 0x79, 0x5B, 0x46, 0xEF, 0xFA, 0x69},
			expected: false,
		},
		{
			name:     "syncword truncated to a header too short to parse",
			payload:  []byte{0xFF, 0xF1, 0x50, 0x40, 0x23, 0xFF},
			expected: false,
		},
		{
			name:     "raw AAC frame from a matroska or mp4 source",
			payload:  []byte{0x21, 0x1A, 0x8F, 0x20, 0x63, 0xC0, 0x00},
			expected: false,
		},
		{
			name:     "empty payload",
			payload:  nil,
			expected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.expected, isADTSFrame(test.payload))
		})
	}
}

// TestNeedsADTSFraming verifies which copied AAC streams get their packets
// validated as ADTS frames. The distinction is the presence of extradata: an
// ADTS-framed source has none, while an mp4/matroska source carries the
// AudioSpecificConfig and holds raw AAC frames that would all be discarded if
// they were held to the syncword check.
func TestNeedsADTSFraming(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected bool
	}{
		{
			name:     "ADTS AAC in MPEG-TS has no extradata",
			path:     testADTSAACSourcePath,
			expected: true,
		},
		{
			name:     "AAC in matroska carries its AudioSpecificConfig",
			path:     "testdata/video_with_subrip_subtitle.mkv",
			expected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inputFmt := astiav.AllocFormatContext()
			require.NotNil(t, inputFmt)

			defer inputFmt.Free()

			require.NoError(t, inputFmt.OpenInput(test.path, nil, nil))
			defer inputFmt.CloseInput()

			require.NoError(t, inputFmt.FindStreamInfo(nil))

			assert.Equal(t, test.expected,
				needsADTSFraming(firstAudioStream(t, inputFmt).CodecParameters()))
		})
	}
}

// TestCopyStreamState_processPacket_DropsPartialLeadingADTSFrame verifies that
// the copy path discards a leading packet that is not a well-formed ADTS frame
// so that the AAC track still reaches matroska with a usable sample rate.
//
// The motivating production failure is an off-air MPEG-TS recording whose
// capture began mid-frame. The demuxer's AAC parser emits everything before the
// first syncword as a packet of its own, so the stream opens with a fragment of
// an ADTS frame. matroska needs an AudioSpecificConfig to record the track's
// sample rate and gets it by auto-inserting the aac_adtstoasc bitstream filter
// — but only when the first packet submitted for the stream carries a syncword.
// The fragment suppressed that, leaving the muxer with no sample rate, and it
// rejected the first packet it was asked to write with EINVAL ("Invalid
// argument"). Because the muxer interleaves, that rejection surfaced on an
// unrelated stream's write, as "ffmpeg: writing encoded packet: Invalid
// argument" from the video encoder.
func TestCopyStreamState_processPacket_DropsPartialLeadingADTSFrame(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "out.mkv")

	inputFmt := astiav.AllocFormatContext()
	require.NotNil(t, inputFmt)

	defer inputFmt.Free()

	require.NoError(t, inputFmt.OpenInput(testADTSAACSourcePath, nil, nil))
	defer inputFmt.CloseInput()

	require.NoError(t, inputFmt.FindStreamInfo(nil))

	inAudioStream := firstAudioStream(t, inputFmt)
	require.Empty(t, inAudioStream.CodecParameters().ExtraData(),
		"fixture's AAC track must be ADTS-framed, so it carries no extradata")

	outputFmt, err := astiav.AllocOutputFormatContext(nil, "matroska", outputPath)
	require.NoError(t, err)

	defer outputFmt.Free()

	outStream := outputFmt.NewStream(nil)
	require.NotNil(t, outStream)
	require.NoError(t, inAudioStream.CodecParameters().Copy(outStream.CodecParameters()))
	outStream.CodecParameters().SetCodecTag(0)
	outStream.SetTimeBase(inAudioStream.TimeBase())

	ioCtx, err := astiav.OpenIOContext(outputPath, astiav.NewIOContextFlags(astiav.IOContextFlagWrite), nil, nil)
	require.NoError(t, err)

	defer func() { _ = ioCtx.Close() }()

	outputFmt.SetPb(ioCtx)

	require.NoError(t, outputFmt.WriteHeader(nil))

	css := &copyStreamState{
		inStream:          inAudioStream,
		requireADTSFrames: needsADTSFraming(inAudioStream.CodecParameters()),
	}
	css.setOutputStream(outStream)

	pkt := astiav.AllocPacket()
	defer pkt.Free()

	readAudioPacket := func() bool {
		t.Helper()

		for {
			if err := inputFmt.ReadFrame(pkt); err != nil {
				return false
			}

			if pkt.StreamIndex() == inAudioStream.Index() {
				return true
			}

			pkt.Unref()
		}
	}

	// Rebuild the leading fragment the demuxer produces from a mid-frame
	// capture: a real ADTS frame with its header sliced off, so the payload
	// starts partway into the frame and carries no syncword.
	require.True(t, readAudioPacket(), "fixture must supply at least one audio packet")

	fragment := astiav.AllocPacket()
	require.NotNil(t, fragment)

	defer fragment.Free()

	require.NoError(t, fragment.CopyProperties(pkt))
	require.NoError(t, fragment.FromData(pkt.Data()[adtsHeaderSize+1:]))

	pkt.Unref()

	require.NoError(t, css.processPacket(fragment, outputFmt, nil, 0),
		"a leading packet that is not a well-formed ADTS frame must be dropped, not muxed")

	written := 0

	for readAudioPacket() {
		require.NoError(t, css.processPacket(pkt, outputFmt, nil, 0),
			"every intact ADTS frame must be accepted by the muxer")

		written++

		pkt.Unref()
	}

	require.NoError(t, outputFmt.WriteTrailer())

	assert.Equal(t, int64(1), css.droppedCount,
		"only the malformed leading packet must be dropped")
	assert.Positive(t, written, "the intact frames must still reach the muxer")

	// The muxed track must carry the AudioSpecificConfig that the ADTS
	// conversion derived; without it players have no sample rate to decode with.
	muxedFmt := astiav.AllocFormatContext()
	require.NotNil(t, muxedFmt)

	defer muxedFmt.Free()

	require.NoError(t, muxedFmt.OpenInput(outputPath, nil, nil))
	defer muxedFmt.CloseInput()

	require.NoError(t, muxedFmt.FindStreamInfo(nil))

	muxedAudio := firstAudioStream(t, muxedFmt)
	assert.NotEmpty(t, muxedAudio.CodecParameters().ExtraData(),
		"matroska output must carry the AAC AudioSpecificConfig")
	assert.Equal(t, inAudioStream.CodecParameters().SampleRate(), muxedAudio.CodecParameters().SampleRate(),
		"muxed track must record the source sample rate")
}
