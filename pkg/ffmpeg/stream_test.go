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
			name:           "packet has no DTS — pass through unchanged",
			lastWrittenDts: 100,
			encDts:         astiav.NoPtsValue,
			encPts:         200,
			wantDts:        astiav.NoPtsValue,
			wantPts:        200,
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
