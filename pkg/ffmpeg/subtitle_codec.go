package ffmpeg

/*
#cgo pkg-config: libavcodec libavformat libavutil

#include "subtitle_codec.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/asticode/go-astiav"
)

// subtitleEncodeBufferSize is the staging buffer handed to the subtitle
// encoder on each call. ASS Dialogue lines are short (typically a few hundred
// bytes); 64 KiB leaves plenty of headroom even for unusually long subtitles
// or many rects per packet, while staying small enough that the buffer can
// live for the lifetime of the converter without being noticeable.
const subtitleEncodeBufferSize = 64 * 1024

// subtitleConverter wraps a libavcodec subtitle decoder/encoder pair. It is
// the Go-side equivalent of ffmpeg CLI's `-c:s <codec>` path: each input
// packet is decoded into the AVSubtitle intermediate (which normalises
// text-based subtitles to ASS dialogue) and re-encoded into the destination
// codec. Passing decoder.subtitle_header into the encoder ensures the encoded
// output and its extradata are internally consistent — the decoder generates
// the ASS [V4+ Styles] section from the source's defaults (e.g. an mp4
// TextSampleEntry box for mov_text) and the encoder embeds that exact header
// into its own extradata for the muxer to attach to the output stream.
//
// Conversion preserves the styling that both the source decoder and ASS can
// represent — bold, italic, underline, colour, font, size — by virtue of
// libavcodec's text-subtitle pipeline normalising on ASS dialogue strings.
// 3GPP-only features (highlight, karaoke timing, free-form positioning) are
// dropped because the AVSubtitle intermediate has no carrier for them.
type subtitleConverter struct {
	decoder    *C.AVCodecContext
	encoder    *C.AVCodecContext
	encBuf     unsafe.Pointer
	encBufSize C.int
}

// newSubtitleConverter opens decoder + encoder pair and threads the decoder's
// generated subtitle_header into the encoder before opening, so the encoder's
// extradata matches the decoder's understanding of the source's styling
// defaults. srcTimeBase is the input stream's AVStream.time_base; it is set
// as the decoder's pkt_timebase so the libavcodec subtitle decode wrapper
// can rescale packet timestamps into AVSubtitle fields without falling into
// its zero-pkt_timebase no-op branch.
func newSubtitleConverter(srcCodec, dstCodec astiav.CodecID, srcExtraData []byte, srcTimeBase astiav.Rational) (*subtitleConverter, error) {
	sc := &subtitleConverter{}

	var srcExtraPtr *C.uint8_t
	if len(srcExtraData) > 0 {
		srcExtraPtr = (*C.uint8_t)(unsafe.Pointer(&srcExtraData[0]))
	}

	var rc C.int

	sc.decoder = C.mpsub_decoder_open(
		C.int(srcCodec), srcExtraPtr, C.int(len(srcExtraData)),
		C.int(srcTimeBase.Num()), C.int(srcTimeBase.Den()),
		&rc,
	)
	if sc.decoder == nil {
		return nil, fmt.Errorf("opening subtitle decoder for codec %v: %w", srcCodec, avError(rc))
	}

	sc.encoder = C.mpsub_encoder_open(C.int(dstCodec), sc.decoder, &rc)
	if sc.encoder == nil {
		C.mpsub_codec_close(&sc.decoder)
		return nil, fmt.Errorf("opening subtitle encoder for codec %v: %w", dstCodec, avError(rc))
	}

	sc.encBuf = C.av_malloc(C.size_t(subtitleEncodeBufferSize))
	if sc.encBuf == nil {
		C.mpsub_codec_close(&sc.decoder)
		C.mpsub_codec_close(&sc.encoder)

		return nil, errors.New("allocating subtitle encode buffer")
	}

	sc.encBufSize = subtitleEncodeBufferSize

	return sc, nil
}

// extraData returns a copy of the encoder's extradata — the ASS
// [Script Info] + [V4+ Styles] sections the encoder produced when it was
// opened. This is the bytes that must be set on the output stream's
// CodecParameters.ExtraData so the matroska muxer attaches the script
// header to the stream.
func (sc *subtitleConverter) extraData() []byte {
	var size C.int

	ptr := C.mpsub_extradata(sc.encoder, &size)
	if ptr == nil || size == 0 {
		return nil
	}

	return C.GoBytes(unsafe.Pointer(ptr), size)
}

// convert decodes one input packet and returns the encoded payload, or nil
// when the input packet decoded to no subtitle (e.g. mov_text "clear"
// markers). pts and duration are passed through to the decoder so the
// AVSubtitle intermediate carries correct timestamps for the encoder to
// embed in the Dialogue line.
func (sc *subtitleConverter) convert(data []byte, pts, duration int64) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}

	written := C.mpsub_convert(
		sc.decoder, sc.encoder,
		(*C.uint8_t)(unsafe.Pointer(&data[0])), C.int(len(data)),
		C.int64_t(pts), C.int64_t(duration),
		(*C.uint8_t)(sc.encBuf), sc.encBufSize,
	)

	if written < 0 {
		return nil, avError(written)
	}

	if written == 0 {
		return nil, nil
	}

	return C.GoBytes(sc.encBuf, written), nil
}

func (sc *subtitleConverter) free() {
	if sc == nil {
		return
	}

	if sc.decoder != nil {
		C.mpsub_codec_close(&sc.decoder)
	}

	if sc.encoder != nil {
		C.mpsub_codec_close(&sc.encoder)
	}

	if sc.encBuf != nil {
		C.av_free(sc.encBuf)
		sc.encBuf = nil
	}
}

// avError wraps a libav AVERROR code in a Go error with av_strerror's
// human-readable description.
func avError(code C.int) error {
	var buf [256]C.char

	C.mpsub_strerror(code, &buf[0], C.int(len(buf)))

	return fmt.Errorf("%s (libav error %d)", C.GoString(&buf[0]), int(code))
}

// matroskaSupportsCodec reports whether the matroska muxer can write the
// given codec ID directly. It mirrors the muxer's own codec_tag lookup, so a
// "true" answer means a copy stream will not be rejected at WriteHeader.
func matroskaSupportsCodec(id astiav.CodecID) bool {
	return C.mpsub_matroska_supports(C.int(id)) == 1
}

// isTextSubtitleCodec reports whether the codec descriptor identifies the
// codec as text-based. False covers bitmap subtitle codecs (PGS, VobSub,
// DVB) and any non-subtitle codec — neither category can be transcoded to
// ASS via libavcodec's text-subtitle pipeline.
func isTextSubtitleCodec(id astiav.CodecID) bool {
	return C.mpsub_is_text_subtitle(C.int(id)) == 1
}
