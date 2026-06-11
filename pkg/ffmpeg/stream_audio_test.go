package ffmpeg

import (
	"testing"

	"github.com/asticode/go-astiav"
	"github.com/stretchr/testify/require"
)

// makeAudioFrame builds an allocated, silence-filled planar audio frame with the
// given format, layout, rate, and sample count. It stands in for a frame the
// decoder would hand to the encoder.
func makeAudioFrame(t *testing.T, format astiav.SampleFormat, layout astiav.ChannelLayout, sampleRate, nbSamples int) *astiav.Frame {
	t.Helper()

	frame := astiav.AllocFrame()
	t.Cleanup(frame.Free)

	frame.SetSampleFormat(format)
	frame.SetChannelLayout(layout)
	frame.SetSampleRate(sampleRate)
	frame.SetNbSamples(nbSamples)

	require.NoError(t, frame.AllocBuffer(0))
	require.NoError(t, frame.SamplesFillSilence())

	return frame
}

// TestResampleFrameRecoversFromMidStreamFormatChange reproduces a production
// failure transcoding a DTS-HD MA 5.1 source into an AC-3 2.1 downmix. The dca
// decoder emits the lossy core as planar float (fltp) and the lossless
// extension as planar 32-bit int (s32p), so the decoded sample format changes
// partway through the stream. swr_convert_frame locks its input configuration
// on the first frame and rejects any later frame whose format differs with
// AVERROR_INPUT_CHANGED ("Input changed") — which previously surfaced as
// "transcode: ffmpeg: resampling audio frame: Input changed" and aborted the
// entire transcode. The resampler must reinitialise and keep converting.
//
// A binary media fixture cannot exercise this: only a real DTS-HD MA stream
// switches decoded sample format mid-decode, and ffmpeg's dca encoder produces
// DTS core only (a single, constant sample format). So the decoder's output is
// reproduced directly as the sequence of frames the resampler would receive.
func TestResampleFrameRecoversFromMidStreamFormatChange(t *testing.T) {
	const (
		sampleRate = 48000
		nbSamples  = 1536 // AC-3 frame size
	)

	ass := &audioStreamState{}

	ass.encoder.resampleContext = astiav.AllocSoftwareResampleContext()
	require.NotNil(t, ass.encoder.resampleContext)
	t.Cleanup(func() {
		if ass.encoder.resampleContext != nil {
			ass.encoder.resampleContext.Free()
		}
	})

	// The resample output frame mirrors what setupEncoder configures for the
	// AC-3 2.1 downmix encoder: 2.1 layout, planar float, 48 kHz. Fixed-frame
	// encoders take the FIFO path and leave the buffer for swr to allocate, so
	// no AllocBuffer here. swr resets these fields on Unref, so they are
	// reapplied before each conversion exactly as encodeAudioFrame does.
	ass.encoder.frame = astiav.AllocFrame()
	t.Cleanup(ass.encoder.frame.Free)

	resetOutputFrame := func() {
		ass.encoder.frame.SetChannelLayout(astiav.ChannelLayout2Point1)
		ass.encoder.frame.SetSampleFormat(astiav.SampleFormatFltp)
		ass.encoder.frame.SetSampleRate(sampleRate)
	}
	resetOutputFrame()

	// The decoded frames the dca decoder hands over: the fltp core, then the
	// s32p lossless extension, then back to the core — same channel layout and
	// sample rate throughout, only the sample format changes.
	inputFormats := []astiav.SampleFormat{
		astiav.SampleFormatFltp,
		astiav.SampleFormatS32P,
		astiav.SampleFormatFltp,
	}

	for index, inputFormat := range inputFormats {
		source := makeAudioFrame(t, inputFormat, astiav.ChannelLayout5Point1, sampleRate, nbSamples)

		err := ass.resampleFrame(source)
		require.NoErrorf(t, err, "resampling frame %d (sample format %s)", index, inputFormat)
		require.Positivef(t, ass.encoder.frame.NbSamples(),
			"frame %d (sample format %s) produced no resampled samples", index, inputFormat)

		// Mirror encodeAudioFrame's per-frame lifecycle: the output frame is
		// consumed into the FIFO, then unref'd and its target params restored.
		ass.encoder.frame.Unref()
		resetOutputFrame()
	}
}
