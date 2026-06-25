package ffmpeg

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/asticode/go-astiav"
)

// stream is the interface for all per-stream processing types.
// copyStreamState, videoStreamState, and audioStreamState implement it.
type stream interface {
	// inputStream returns the source stream from the input format context.
	inputStream() *astiav.Stream
	// outputStream returns the destination stream in the output format context.
	outputStream() *astiav.Stream
	// setOutputStream binds the output stream produced by the muxer to this state.
	setOutputStream(*astiav.Stream)
	// setupEncoder configures the encoder and allocates encoder resources.
	// For copy streams this is a no-op.
	setupEncoder(hwAccel HWAccel, outputFmt *astiav.FormatContext) error
	// encoderContext returns the encoder codec context used to populate output
	// stream parameters. Returns nil for copy streams.
	encoderContext() *astiav.CodecContext
	// processPacket handles an incoming demuxed packet.
	processPacket(packet *astiav.Packet, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error
	// flush drains any buffered encoder output. No-op for copy streams.
	flush(outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error
	// applyOutputOverrides allows the stream to mutate the output stream's
	// codec parameters after they have been populated from either the encoder
	// context (encoded streams) or the input stream (copy streams). The
	// default implementation in copyStreamState is a no-op; subtitleStreamState
	// uses it to switch the codec ID for the mov_text → ASS rewrite path.
	applyOutputOverrides(outStream *astiav.Stream) error
	// free releases all resources held by this stream state.
	free()
}

// streamDecoderState holds resources for the decoder side of a stream. Used by
// both videoStreamState and audioStreamState.
type streamDecoderState struct {
	codec        *astiav.Codec
	codecContext *astiav.CodecContext
	frame        *astiav.Frame
}

func (sd *streamDecoderState) free() {
	if sd.codecContext != nil {
		sd.codecContext.Free()
	}

	if sd.frame != nil {
		sd.frame.Free()
	}
}

// copyStreamState passes packets through to the output without re-encoding.
// Used for subtitle, attachment, data, and copy-codec audio/video streams.
// It is also embedded by videoStreamState and audioStreamState to provide the
// common inStream/outStream fields and the encoded-frame counter.
type copyStreamState struct {
	inStream  *astiav.Stream
	outStream *astiav.Stream
	frames    int64 // encoded frames written; used for progress reporting
	// lastWrittenDts is the DTS of the most recent packet handed to the muxer
	// for outStream, in outStream's timebase. Used to clamp non-monotonic DTS
	// before the muxer's strict monotonicity check sees it — both from
	// encoders that emit out-of-order DTS (hevc_qsv on VFR sources, where
	// Intel's libmfx runtime computes DecodeTimeStamp assuming uniform input
	// cadence) and from copy-path sources whose authored timestamps regress
	// (PGS subtitle streams from some BluRay-authoring tools, where Block
	// timestamps go briefly backwards between adjacent display sets).
	// NoPtsValue before the first write.
	lastWrittenDts int64
	// clampedCount caps log volume from repairNonMonotonicDts: only the first
	// clamp on a stream logs a detailed warn line, and logClampSummary later
	// emits the total. Pathological sources can regress on thousands of packets.
	clampedCount int64
}

func (css *copyStreamState) inputStream() *astiav.Stream  { return css.inStream }
func (css *copyStreamState) outputStream() *astiav.Stream { return css.outStream }
func (css *copyStreamState) setOutputStream(out *astiav.Stream) {
	css.outStream = out
	css.lastWrittenDts = astiav.NoPtsValue
	css.clampedCount = 0
}

func (css *copyStreamState) setupEncoder(_ HWAccel, _ *astiav.FormatContext) error { return nil }
func (css *copyStreamState) encoderContext() *astiav.CodecContext                  { return nil }

func (css *copyStreamState) processPacket(packet *astiav.Packet, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	packet.RescaleTs(css.inStream.TimeBase(), css.outStream.TimeBase())
	packet.SetStreamIndex(css.outStream.Index())
	css.repairNonMonotonicDts(packet)

	css.frames++
	// sendProgress and the DTS snapshot must read packet fields before
	// WriteInterleavedFrame, which takes ownership of the packet and zeroes
	// them.
	if progressCh != nil {
		sendProgress(progressCh, css.frames, packet, css.outStream, totalDuration)
	}

	writtenDts := packet.Dts()

	if err := outputFmt.WriteInterleavedFrame(packet); err != nil {
		return fmt.Errorf("ffmpeg: writing remuxed packet for stream %d: %w", css.outStream.Index(), err)
	}

	css.lastWrittenDts = writtenDts

	return nil
}

func (css *copyStreamState) flush(_ *astiav.FormatContext, _ chan<- Progress, _ int64) error {
	return nil
}

func (css *copyStreamState) applyOutputOverrides(_ *astiav.Stream) error { return nil }

// free must be invoked by every embedding type's free() — Go does not promote
// the embedded method once the outer type defines its own — so the DTS-clamp
// summary is emitted exactly once per stream.
func (css *copyStreamState) free() {
	css.logClampSummary()
}

// receiveAndWritePackets drains encoded packets from the encoder context and
// writes each to the output. css.frames is incremented per written packet.
func (css *copyStreamState) receiveAndWritePackets(encCtx *astiav.CodecContext, encPkt *astiav.Packet, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	for {
		if err := encCtx.ReceivePacket(encPkt); err != nil {
			if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
				return nil
			}

			return fmt.Errorf("ffmpeg: receiving encoded packet: %w", err)
		}

		css.frames++
		encPkt.SetStreamIndex(css.outStream.Index())
		encPkt.RescaleTs(encCtx.TimeBase(), css.outStream.TimeBase())
		css.repairNonMonotonicDts(encPkt)

		if progressCh != nil {
			sendProgress(progressCh, css.frames, encPkt, css.outStream, totalDuration)
		}

		// Snapshot DTS before WriteInterleavedFrame, which transfers ownership of
		// the packet to the muxer and leaves the AVPacket fields cleared.
		writtenDts := encPkt.Dts()

		if err := outputFmt.WriteInterleavedFrame(encPkt); err != nil {
			encPkt.Unref()
			return fmt.Errorf("ffmpeg: writing encoded packet: %w", err)
		}

		css.lastWrittenDts = writtenDts

		encPkt.Unref()
	}
}

// repairNonMonotonicDts rewrites pkt's DTS so that it is strictly greater than
// the last DTS written to the output stream, and rewrites pkt's PTS so that it
// is never behind its own DTS. Mirrors the clamp in
// fftools/ffmpeg_mux.c::mux_fixup_ts that lets the FFmpeg CLI tolerate both
// non-monotonic DTS — from encoders that emit out-of-order DTS (notably
// hevc_qsv on VFR sources) and from copy-path sources whose authored timestamps
// regress — and packets whose authored PTS sits behind their DTS, which
// libavformat otherwise rejects with EINVAL ("Invalid argument"). No-op when
// the incoming DTS is already monotonic and its PTS is not behind it — so
// well-formed sources produce bit-identical output.
func (css *copyStreamState) repairNonMonotonicDts(pkt *astiav.Packet) {
	newDts, newPts, clamped := monotonicDtsClamp(css.lastWrittenDts, pkt.Dts(), pkt.Pts())
	if !clamped {
		return
	}

	if css.clampedCount == 0 {
		slog.Warn("ffmpeg: clamping packet timestamps",
			slog.Int("stream", css.outStream.Index()),
			slog.Int64("packet_dts", pkt.Dts()),
			slog.Int64("previous_dts", css.lastWrittenDts),
			slog.Int64("corrected_dts", newDts),
			slog.Int64("packet_pts", pkt.Pts()),
			slog.Int64("corrected_pts", newPts),
		)
	}

	css.clampedCount++

	pkt.SetDts(newDts)

	if newPts != pkt.Pts() {
		pkt.SetPts(newPts)
	}
}

// logClampSummary skips the single-clamp case because repairNonMonotonicDts
// already logged that packet in detail — a summary saying "1 clamp" would
// just duplicate the existing line.
func (css *copyStreamState) logClampSummary() {
	if css.clampedCount <= 1 || css.outStream == nil {
		return
	}

	slog.Warn("ffmpeg: clamped packet timestamps on stream",
		slog.Int("stream", css.outStream.Index()),
		slog.Int64("total_clamps", css.clampedCount),
	)
}

// monotonicDtsClamp computes the DTS and PTS for an outgoing packet so that
// (1) its DTS is strictly greater than the previous packet's DTS on the same
// stream and (2) its PTS is not behind its own DTS. clamped is true when either
// invariant had to be repaired. Pure function — no I/O, no logging — so it can
// be unit-tested without a hardware encoder or a live FormatContext. Inputs and
// outputs are in the muxer's stream timebase. AV_NOPTS_VALUE sentinels
// short-circuit each repair (first-packet case and packets without DTS pass the
// monotonicity check unchanged; packets without PTS skip the pts >= dts check).
//
// The pts >= dts repair matters independently of the DTS clamp: libavformat
// rejects any packet with pts < dts (EINVAL, surfaced as "Invalid argument"),
// and a source stream can carry such packets with an already-monotonic DTS — in
// which case the monotonicity branch leaves the packet untouched and only this
// second repair keeps the muxer happy. The ffmpeg CLI performs the same clamp
// at its stream-output layer, which is why the CLI tolerates these sources.
// Well-formed sources (pts >= dts, monotonic DTS) are returned unchanged, so
// they still produce bit-identical output.
func monotonicDtsClamp(lastWrittenDts, encDts, encPts int64) (newDts, newPts int64, clamped bool) {
	newDts = encDts
	newPts = encPts

	if lastWrittenDts != astiav.NoPtsValue && encDts != astiav.NoPtsValue && encDts <= lastWrittenDts {
		newDts = lastWrittenDts + 1
		clamped = true
	}

	if newDts != astiav.NoPtsValue && newPts != astiav.NoPtsValue && newPts < newDts {
		newPts = newDts
		clamped = true
	}

	return newDts, newPts, clamped
}

// sendProgress emits a non-blocking progress update on ch.
func sendProgress(ch chan<- Progress, frames int64, packet *astiav.Packet, outStream *astiav.Stream, totalDuration int64) {
	var percentComplete float64

	if totalDuration > 0 {
		tb := outStream.TimeBase()
		ptsInMicros := float64(packet.Pts()) * float64(tb.Num()) / float64(tb.Den()) * 1e6
		percentComplete = ptsInMicros / float64(totalDuration) * 100

		if percentComplete > 100 {
			percentComplete = 100
		}

		if percentComplete < 0 {
			percentComplete = 0
		}
	}

	select {
	case ch <- Progress{FramesProcessed: frames, PercentComplete: percentComplete}:
	default:
	}
}

// freeStreams releases all resources held by a streams map.
func freeStreams(streams map[int]stream) {
	for _, s := range streams {
		s.free()
	}
}
