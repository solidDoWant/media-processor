package ffmpeg

import (
	"errors"
	"fmt"

	"github.com/asticode/go-astiav"
)

// subtitleStreamState transcodes a subtitle stream from the source codec to a
// matroska-compatible codec via libavcodec's subtitle decoder/encoder pair
// (subtitleConverter). It exists primarily to handle mov_text → ASS, since
// the matroska muxer has no codec mapping for mov_text and would otherwise
// reject the stream at WriteHeader.
//
// Targeting ASS (matroska's S_TEXT/ASS) rather than SubRip preserves the
// styling that survives libavcodec's text-subtitle pipeline — bold, italic,
// underline, colour, font/size — because both decoder and encoder use ASS
// dialogue strings as the in-memory intermediate. SubRip carries the same
// subset but ASS additionally has carriers for highlights, karaoke timing,
// and free-form positioning so that, when libavcodec gains broader support
// for the source-side modifier boxes, less is lost.
//
// Subtitle codecs other than the ones explicitly handled in buildStreamStates
// stay in the plain copyStreamState path with converter == nil.
type subtitleStreamState struct {
	copyStreamState

	// targetCodecID is the codec ID written to the output stream's codec
	// parameters. Zero means no rewrite (the stream behaves as a copy).
	targetCodecID astiav.CodecID

	// sourceCodecID identifies the input codec; passed to the converter on
	// setup so the right libavcodec decoder is selected.
	sourceCodecID astiav.CodecID

	// sourceExtraData is the input stream's CodecParameters.ExtraData. For
	// mov_text this is the mp4 TextSampleEntry box describing the source's
	// default font/style; the decoder reads it to populate subtitle_header.
	sourceExtraData []byte

	// sourceTimeBase is the input stream's AVStream.time_base, used as the
	// decoder's pkt_timebase so that AVPacket.pts/duration on inbound packets
	// are interpreted in the right units (mp4 subtitle streams typically use
	// 1/1000000, MKV typically 1/1000 — they cannot be assumed equal).
	sourceTimeBase astiav.Rational

	converter *subtitleConverter
}

// setupEncoder opens the libavcodec subtitle decoder/encoder pair so the
// converter is ready by the time setupOutputContext reads its extradata.
// Stream states whose targetCodecID is zero stay in pure copy mode.
func (sss *subtitleStreamState) setupEncoder(_ HWAccel, _ *astiav.FormatContext) error {
	if sss.targetCodecID == astiav.CodecIDNone {
		return nil
	}

	conv, err := newSubtitleConverter(sss.sourceCodecID, sss.targetCodecID, sss.sourceExtraData, sss.sourceTimeBase)
	if err != nil {
		return fmt.Errorf("opening subtitle converter (%v → %v): %w", sss.sourceCodecID, sss.targetCodecID, err)
	}

	sss.converter = conv

	return nil
}

// applyOutputOverrides switches the output stream's codec to the converter's
// target and attaches the encoder's extradata (the ASS [Script Info] +
// [V4+ Styles] header). Without the codec-id swap the muxer would still see
// the source codec; without the extradata, matroska players would render
// dialogue lines without their style table.
func (sss *subtitleStreamState) applyOutputOverrides(outStream *astiav.Stream) error {
	if sss.converter == nil {
		return nil
	}

	params := outStream.CodecParameters()
	params.SetCodecID(sss.targetCodecID)
	params.SetCodecTag(0)

	if err := params.SetExtraData(sss.converter.extraData()); err != nil {
		return fmt.Errorf("setting subtitle output extradata: %w", err)
	}

	return nil
}

// processPacket runs one input packet through the converter and writes the
// resulting ASS payload to the output, preserving the input packet's PTS,
// duration, and other timing fields via av_packet_copy_props.
func (sss *subtitleStreamState) processPacket(packet *astiav.Packet, outputFmt *astiav.FormatContext, progressCh chan<- Progress, totalDuration int64) error {
	if sss.converter == nil {
		return sss.copyStreamState.processPacket(packet, outputFmt, progressCh, totalDuration)
	}

	encoded, err := sss.converter.convert(packet.Data(), packet.Pts(), packet.Duration())
	if err != nil {
		return fmt.Errorf("ffmpeg: converting subtitle packet on stream %d: %w", sss.outStream.Index(), err)
	}

	if len(encoded) == 0 {
		// Decoder accepted the packet but produced no subtitle (e.g. mov_text
		// "clear subtitle" markers). Skipping is safe because matroska players
		// already hide a subtitle once the preceding Block's duration elapses.
		return nil
	}

	rewritten := astiav.AllocPacket()
	if rewritten == nil {
		return errors.New("ffmpeg: allocating subtitle rewrite packet")
	}

	defer rewritten.Free()

	if err := rewritten.CopyProperties(packet); err != nil {
		return fmt.Errorf("ffmpeg: copying properties to subtitle rewrite packet on stream %d: %w", sss.outStream.Index(), err)
	}

	if err := rewritten.FromData(encoded); err != nil {
		return fmt.Errorf("ffmpeg: setting subtitle rewrite packet payload on stream %d: %w", sss.outStream.Index(), err)
	}

	return sss.copyStreamState.processPacket(rewritten, outputFmt, progressCh, totalDuration)
}

func (sss *subtitleStreamState) free() {
	sss.converter.free()
}
