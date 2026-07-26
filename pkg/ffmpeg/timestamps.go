package ffmpeg

import (
	"log/slog"

	"github.com/asticode/go-astiav"
)

// timestampRebase shifts every demuxed packet onto a zero-based timeline by
// adding a single per-input offset to its PTS and DTS.
//
// Sources whose timestamps do not start near zero — off-air MPEG-TS recordings
// routinely open at an arbitrary PTS — otherwise produce an output whose
// timeline is shifted by the source's start time: players see hours of nothing
// before the content, the container reports a runtime several times the real
// one, and any consumer comparing the output's duration against the source's
// (the transcode reuse check) never matches. Progress reporting is skewed the
// same way, since sendProgress divides a packet's PTS by the input's duration.
//
// This mirrors the FFmpeg CLI's default behaviour: it derives a per-input
// ts_offset from the input's start time and adds it to each packet as it comes
// out of the demuxer (fftools/ffmpeg_demux.c; -copyts is what disables the
// rebasing). Applying it at the demuxer boundary — before any stream state sees
// the packet — is what makes one shift cover every path at once: copy streams
// get rebased values ahead of their rescale into the output timebase and the
// monotonic-DTS repair, encoded streams carry the rebased PTS through the
// decoder into the encoder, and the subtitle rewrite and downmix clone both
// derive from the same packet.
//
// The offset is deliberately shared by every stream rather than computed per
// stream: streams start at different times within a source (an off-air capture
// can open with audio a third of a second ahead of the first video frame), and
// only a common shift preserves that relationship. A per-stream offset would
// collapse each stream onto zero independently and silently destroy A/V sync.
type timestampRebase struct {
	// offset is added to every packet timestamp, in AV_TIME_BASE units. Zero
	// when the input already starts at zero or reports no start time at all,
	// which makes apply a no-op and leaves such sources bit-identical.
	offset int64
	// timeBases holds the input streams' timebases indexed by stream index, so
	// that apply can rescale offset without rebuilding the stream list for
	// every packet. AVStream.index is a stream's position in the container's
	// stream list, so position and index agree here.
	timeBases []astiav.Rational
}

// newTimestampRebase computes the rebasing offset for inputFmt and captures the
// per-stream timebases apply needs. inputFmt must have been through
// FindStreamInfo, which is what populates the container's start time.
func newTimestampRebase(inputFmt *astiav.FormatContext) *timestampRebase {
	streams := inputFmt.Streams()

	timeBases := make([]astiav.Rational, 0, len(streams))
	for _, stream := range streams {
		timeBases = append(timeBases, stream.TimeBase())
	}

	startTime := inputFmt.StartTime()

	rebase := &timestampRebase{
		offset:    rebaseOffset(startTime),
		timeBases: timeBases,
	}

	if rebase.offset != 0 {
		slog.Info("ffmpeg: rebasing output timestamps to zero",
			slog.Int64("input_start_time_micros", startTime),
		)
	}

	return rebase
}

// rebaseOffset returns the value to add to every packet timestamp, in
// AV_TIME_BASE units, for an input whose container start time is startTime.
// A start time of AV_NOPTS_VALUE means the container never reported one, and
// must be treated as a zero offset rather than shifting the whole file by the
// sentinel.
func rebaseOffset(startTime int64) int64 {
	if startTime == astiav.NoPtsValue {
		return 0
	}

	return -startTime
}

// apply shifts pkt's PTS and DTS onto the rebased timeline, in pkt's own input
// stream timebase. It is a no-op for a zero offset, for a stream index with no
// known timebase, and for timestamps the demuxer left as AV_NOPTS_VALUE — a
// missing timestamp stays missing so the repairs in monotonicDtsClamp still see
// it as such.
func (tr *timestampRebase) apply(pkt *astiav.Packet) {
	if tr.offset == 0 {
		return
	}

	index := pkt.StreamIndex()
	if index < 0 || index >= len(tr.timeBases) {
		return
	}

	delta := astiav.RescaleQ(tr.offset, astiav.TimeBaseQ, tr.timeBases[index])

	if pts := pkt.Pts(); pts != astiav.NoPtsValue {
		pkt.SetPts(pts + delta)
	}

	if dts := pkt.Dts(); dts != astiav.NoPtsValue {
		pkt.SetDts(dts + delta)
	}
}
