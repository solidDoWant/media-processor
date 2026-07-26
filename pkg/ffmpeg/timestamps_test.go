package ffmpeg

import (
	"testing"

	"github.com/asticode/go-astiav"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRebaseOffset covers the offset derivation: the shift applied to every
// packet is the negation of the container's start time, and a container that
// reports no start time at all must produce no shift rather than one of
// AV_NOPTS_VALUE.
func TestRebaseOffset(t *testing.T) {
	tests := []struct {
		name      string
		startTime int64
		expected  int64
	}{
		{
			name:      "source already at zero is not shifted",
			startTime: 0,
			expected:  0,
		},
		{
			name:      "unknown start time is not shifted",
			startTime: astiav.NoPtsValue,
			expected:  0,
		},
		{
			name:      "off-air recording is shifted back to zero",
			startTime: 10169648700,
			expected:  -10169648700,
		},
		{
			name:      "sub-second start time is shifted back to zero",
			startTime: 21333,
			expected:  -21333,
		},
		{
			name:      "negative start time is shifted forward to zero",
			startTime: -500000,
			expected:  500000,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.expected, rebaseOffset(test.startTime))
		})
	}
}

// TestTimestampRebase_Apply verifies that the offset reaches a packet's PTS and
// DTS in that packet's own stream timebase, and that the cases which must be
// left alone — a zero offset, a stream the input never declared, and
// timestamps the demuxer could not determine — are untouched.
func TestTimestampRebase_Apply(t *testing.T) {
	// 90 kHz is the MPEG-TS timebase; 1/1000 stands in for a second stream
	// carrying a different one, so a shared offset must be rescaled per stream.
	timeBases := []astiav.Rational{
		astiav.NewRational(1, 90000),
		astiav.NewRational(1, 1000),
	}

	tests := []struct {
		name        string
		offset      int64
		streamIndex int
		pts         int64
		dts         int64
		expectedPts int64
		expectedDts int64
	}{
		{
			// 10001.4 s is 900126000 ticks at 90 kHz, so the stream's first
			// packet lands on zero and the DTS a tenth of a second ahead of it
			// lands just below.
			name:        "shifts both timestamps in the 90 kHz stream timebase",
			offset:      -10001400000,
			streamIndex: 0,
			pts:         900126000,
			dts:         900117000,
			expectedPts: 0,
			expectedDts: -9000,
		},
		{
			name:        "rescales the same offset into a millisecond timebase",
			offset:      -10001400000,
			streamIndex: 1,
			pts:         10001700,
			dts:         10001700,
			expectedPts: 300,
			expectedDts: 300,
		},
		{
			name:        "zero offset leaves the packet untouched",
			offset:      0,
			streamIndex: 0,
			pts:         900126000,
			dts:         900117000,
			expectedPts: 900126000,
			expectedDts: 900117000,
		},
		{
			name:        "missing pts stays missing",
			offset:      -10001400000,
			streamIndex: 0,
			pts:         astiav.NoPtsValue,
			dts:         900126000,
			expectedPts: astiav.NoPtsValue,
			expectedDts: 0,
		},
		{
			name:        "missing dts stays missing",
			offset:      -10001400000,
			streamIndex: 0,
			pts:         900126000,
			dts:         astiav.NoPtsValue,
			expectedPts: 0,
			expectedDts: astiav.NoPtsValue,
		},
		{
			name:        "unknown stream index leaves the packet untouched",
			offset:      -10001400000,
			streamIndex: 7,
			pts:         900126000,
			dts:         900117000,
			expectedPts: 900126000,
			expectedDts: 900117000,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			packet := astiav.AllocPacket()
			require.NotNil(t, packet)

			defer packet.Free()

			packet.SetStreamIndex(test.streamIndex)
			packet.SetPts(test.pts)
			packet.SetDts(test.dts)

			rebase := &timestampRebase{offset: test.offset, timeBases: timeBases}
			rebase.apply(packet)

			assert.Equal(t, test.expectedPts, packet.Pts(), "pts")
			assert.Equal(t, test.expectedDts, packet.Dts(), "dts")
		})
	}
}

// TestNewTimestampRebase verifies the offset and per-stream timebases derived
// from real containers: a source that opens ~10001.4 s in is shifted back by
// exactly that much, and a source that already starts at zero yields the
// no-op offset that keeps its output bit-identical.
func TestNewTimestampRebase(t *testing.T) {
	tests := []struct {
		name              string
		path              string
		expectedOffset    int64
		expectedTimeBases []astiav.Rational
	}{
		{
			// An MPEG-TS fixture whose container start time is 10001.4 s; see
			// testdata/README.md.
			name:           "off-air style MPEG-TS is shifted back to zero",
			path:           "testdata/video_shifted_ts.ts",
			expectedOffset: -10001400000,
			expectedTimeBases: []astiav.Rational{
				astiav.NewRational(1, 90000),
				astiav.NewRational(1, 90000),
			},
		},
		{
			name:           "source that already starts at zero is not shifted",
			path:           "../../pkg/ffprobe/testdata/video.mp4",
			expectedOffset: 0,
			expectedTimeBases: []astiav.Rational{
				astiav.NewRational(1, 12288),
				astiav.NewRational(1, 48000),
			},
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

			rebase := newTimestampRebase(inputFmt)

			assert.Equal(t, test.expectedOffset, rebase.offset)
			assert.Equal(t, test.expectedTimeBases, rebase.timeBases)
		})
	}
}
