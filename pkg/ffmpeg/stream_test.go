package ffmpeg

import (
	"testing"

	"github.com/asticode/go-astiav"
	"github.com/stretchr/testify/assert"
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

	assert.Equal(t, 3, clampedCount, "clamp must fire on every regressing packet")

	for i := 1; i < len(muxedDts); i++ {
		assert.Greater(t, muxedDts[i], muxedDts[i-1],
			"muxed DTS must be strictly monotonic at packet %d (got %d after %d)",
			i, muxedDts[i], muxedDts[i-1])
	}
}
