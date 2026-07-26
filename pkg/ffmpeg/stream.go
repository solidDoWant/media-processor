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
	// requireADTSFrames makes processPacket discard packets that are not
	// well-formed ADTS frames. Set only for AAC copy streams carrying no
	// extradata; see needsADTSFraming.
	requireADTSFrames bool
	// droppedCount counts packets discarded by dropMalformedADTS, and caps its
	// log volume the same way clampedCount does for the DTS repair.
	droppedCount int64
}

func (css *copyStreamState) inputStream() *astiav.Stream  { return css.inStream }
func (css *copyStreamState) outputStream() *astiav.Stream { return css.outStream }
func (css *copyStreamState) setOutputStream(out *astiav.Stream) {
	css.outStream = out
	css.lastWrittenDts = astiav.NoPtsValue
	css.clampedCount = 0
	css.droppedCount = 0
}

func (css *copyStreamState) setupEncoder(_ HWAccel, _ *astiav.FormatContext) error { return nil }
func (css *copyStreamState) encoderContext() *astiav.CodecContext                  { return nil }

func (css *copyStreamState) processPacket(packet *astiav.Packet, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	if css.dropMalformedADTS(packet) {
		return nil
	}

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
	css.logDropSummary()
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

// adtsHeaderSize is the length in bytes of an ADTS frame header without the
// optional 2-byte CRC. It is the smallest payload that can carry the profile,
// sampling-frequency index, and channel configuration that the ADTS → ASC
// conversion needs.
const adtsHeaderSize = 7

// isADTSFrame reports whether payload begins with an ADTS frame header: the
// 12-bit syncword 0xFFF in the top bits of the first two bytes, followed by
// enough bytes to hold the rest of the fixed header.
func isADTSFrame(payload []byte) bool {
	return len(payload) >= adtsHeaderSize && payload[0] == 0xFF && payload[1]&0xF0 == 0xF0
}

// needsADTSFraming reports whether a stream copied with these codec parameters
// must have its packets validated as ADTS frames before they reach the muxer.
//
// AAC carried in MPEG-TS is ADTS-framed and so has no AudioSpecificConfig
// extradata, which matroska (and mp4) require in order to record the track's
// sample rate. libavformat covers that gap by auto-inserting the
// aac_adtstoasc bitstream filter, which derives the config from an ADTS header
// and hands it to the muxer as new extradata — but the matroska muxer only
// arms that filter when the *first* packet submitted for the stream carries an
// ADTS syncword (mkv_check_bitstream). Every packet after it must be
// ADTS-framed too, since the filter treats an unparsable header as fatal.
//
// An AAC stream that already carries extradata (any mp4/matroska source) holds
// raw AAC frames with no syncword, so it must be left alone — filtering there
// would discard the entire track.
func needsADTSFraming(params *astiav.CodecParameters) bool {
	return params.CodecID() == astiav.CodecIDAac && len(params.ExtraData()) == 0
}

// dropMalformedADTS discards pkt when the stream requires ADTS framing and pkt
// is not a well-formed ADTS frame, reporting whether it did so. It only ever
// fires on streams flagged by needsADTSFraming.
//
// The motivating source is an off-air MPEG-TS recording whose capture began
// mid-frame: the demuxer's AAC parser emits everything preceding the first
// syncword as a packet of its own, so the stream opens with a fragment of an
// ADTS frame. That fragment is undecodable audio on its own, and forwarding it
// leaves matroska with no way to learn the sample rate — the muxer then rejects
// the first packet it is asked to write with EINVAL ("Invalid argument").
// Dropping it lets the auto-inserted filter arm on the first intact frame.
func (css *copyStreamState) dropMalformedADTS(pkt *astiav.Packet) bool {
	if !css.requireADTSFrames || isADTSFrame(pkt.Data()) {
		return false
	}

	if css.droppedCount == 0 {
		slog.Warn("ffmpeg: dropping packet that is not a well-formed ADTS frame",
			slog.Int("stream", css.outStream.Index()),
			slog.Int("packet_size", pkt.Size()),
			slog.Int64("packet_dts", pkt.Dts()),
		)
	}

	css.droppedCount++

	return true
}

// logDropSummary mirrors logClampSummary: the single-drop case is skipped
// because dropMalformedADTS already logged that packet in detail.
func (css *copyStreamState) logDropSummary() {
	if css.droppedCount <= 1 || css.outStream == nil {
		return
	}

	slog.Warn("ffmpeg: dropped packets that were not well-formed ADTS frames on stream",
		slog.Int("stream", css.outStream.Index()),
		slog.Int64("total_dropped", css.droppedCount),
	)
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
// (1) it carries a DTS at all, (2) that DTS is strictly greater than the
// previous packet's DTS on the same stream, and (3) its PTS is not behind its
// own DTS. clamped is true when any invariant had to be repaired. Pure
// function — no I/O, no logging — so it can be unit-tested without a hardware
// encoder or a live FormatContext. Inputs and outputs are in the muxer's stream
// timebase.
//
// Three repairs, each independently necessary for the copy path to survive
// real-world sources, all mirroring clamps the FFmpeg CLI performs at its
// stream-output layer (which is why the CLI tolerates these sources):
//
//   - Missing DTS: a source packet can carry no DTS at all — seen on the leading
//     reordered frames of some WEB-DL HEVC streams, where the demuxer cannot yet
//     compute a decode time. Forwarding AV_NOPTS_VALUE to the muxer makes
//     libavformat derive a DTS from the PTS, which on reordered B-frames runs
//     backwards and is rejected ("non monotonically increasing dts"). We instead
//     synthesize a monotonic DTS just past the previous one — mirroring the CLI's
//     stream-copy path (of_streamcopy), which fills a running DTS estimate when
//     the source DTS is absent. The first packet, having no prior anchor, falls
//     back to its own PTS.
//   - Non-monotonic DTS: an encoder (notably hevc_qsv on VFR sources) or a
//     copy-path source can emit a DTS that regresses below the previous one;
//     bump it just past.
//   - PTS behind DTS: libavformat rejects any packet with pts < dts (EINVAL,
//     surfaced as "Invalid argument"); a source can carry such packets with an
//     already-monotonic DTS, so this repair stands on its own.
//
// matroska stores PTS as the block timecode and does not persist DTS, so the
// synthesized/bumped DTS values are invisible in the output. Well-formed sources
// (every packet has a DTS, DTS monotonic, pts >= dts) are returned unchanged, so
// they still produce bit-identical output.
func monotonicDtsClamp(lastWrittenDts, encDts, encPts int64) (newDts, newPts int64, clamped bool) {
	newDts = encDts
	newPts = encPts

	switch {
	case encDts == astiav.NoPtsValue:
		// Synthesize a DTS so the muxer never sees AV_NOPTS_VALUE. Advance past
		// the previous packet when there is one; otherwise anchor to the PTS.
		switch {
		case lastWrittenDts != astiav.NoPtsValue:
			newDts = lastWrittenDts + 1
			clamped = true
		case encPts != astiav.NoPtsValue:
			newDts = encPts
			clamped = true
		}
	case lastWrittenDts != astiav.NoPtsValue && encDts <= lastWrittenDts:
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
